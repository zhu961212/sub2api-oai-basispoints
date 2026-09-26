package transport

import (
	"bytes"
	"container/list"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	hcplugin "github.com/hashicorp/go-plugin"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/attachments"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/auth"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"golang.org/x/net/proxy"
	"google.golang.org/grpc"
)

type Transport struct {
	pluginv1.UnimplementedTransportPluginServer
	mu sync.RWMutex
	// cfg and its slices are immutable after publication. New/ApplyConfig
	// own the storage; readers take a value snapshot under mu.
	cfg            protocol.Config
	client         *http.Client
	closed         bool
	broker         *hcplugin.GRPCBroker
	host           pluginv1.HostServiceClient
	hostConn       *grpc.ClientConn
	accounts       accountCache
	bpsAccounts    bpsAccountState
	attachments    *attachments.Uploader
	imageAdmission imageRequestAdmission
	// proxyClients 按账号代理 URL 复用独立的 Transport。此前每个请求都会
	// Clone 一个 Transport 并立即关闭连接池，带代理的并发请求无法复用连接。
	// 同时获取两把锁时始终先 mu 后 proxyMu；生命周期切换与缓存摘除
	// 必须原子完成，避免旧请求在配置切换或 Shutdown 后重建缓存。
	proxyMu      sync.Mutex
	proxyClients map[string]cachedProxyClient
	proxyOrder   list.List
}

type cachedProxyClient struct {
	base      *http.Client
	client    *http.Client
	transport *http.Transport
	lastUsed  time.Time
	order     *list.Element
}

// accountSummary 是账号目录中对配置页可见的最小字段集合。
type accountSummary struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Schedulable bool   `json:"schedulable"`
}

// accountCache 缓存宿主返回的账号目录。Health 可能被宿主健康检查和配置页轮询
// 同时调用，缓存可以避免每次探测都查一次账号表；失败时保留上一次成功结果，避免
// 面板因瞬时故障闪空。
type accountCache struct {
	mu      sync.Mutex
	expires time.Time
	items   []accountSummary
	refresh *accountRefresh
}

type accountRefresh struct {
	done   chan struct{}
	cancel context.CancelFunc
}

const (
	accountCacheTTL       = 5 * time.Second
	accountCacheRetry     = time.Second
	accountLookupTimeout  = 3 * time.Second
	maxCachedProxyClients = 128
	proxyClientIdleTTL    = 5 * time.Minute
)

var errTransportStopped = errors.New("plugin is stopped")

func New() *Transport {
	cfg := protocol.DefaultConfig()
	return &Transport{cfg: cfg, client: newClient(cfg), attachments: attachments.New(), proxyClients: make(map[string]cachedProxyClient)}
}

func (t *Transport) Shutdown() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	client, conn := t.client, t.hostConn
	t.hostConn = nil
	t.host = nil
	t.bindBPSAccountStore(nil)
	t.accounts.invalidate()
	proxyClients := t.detachProxyClients()
	t.mu.Unlock()
	closeCachedProxyClients(proxyClients)
	if client != nil {
		if tr, ok := client.Transport.(interface{ CloseIdleConnections() }); ok {
			tr.CloseIdleConnections()
		}
	}
	if conn != nil {
		_ = conn.Close()
	}
}

func newClient(cfg protocol.Config) *http.Client {
	return &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second, Transport: &http.Transport{
		Proxy:             nil, // Empty account proxy explicitly means direct egress.
		DialContext:       (&netDialer{}).DialContext,
		ForceAttemptHTTP2: true,
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:      100, MaxIdleConnsPerHost: 20, IdleConnTimeout: 90 * time.Second,
	}}
}

// closeProxyClients 关闭并清空按代理缓存的连接池。配置变更或插件退出时调用；
// 正常请求结束不应关闭连接池，否则 keep-alive 无法发挥作用。
func (t *Transport) closeProxyClients() {
	closeCachedProxyClients(t.detachProxyClients())
}

// detachProxyClients 摘除旧连接池；生命周期调用者持有 mu，实际关闭在
// 释放 mu 后进行，避免阻塞其它请求的配置快照读取。
func (t *Transport) detachProxyClients() map[string]cachedProxyClient {
	t.proxyMu.Lock()
	defer t.proxyMu.Unlock()
	clients := t.proxyClients
	t.proxyClients = make(map[string]cachedProxyClient)
	t.proxyOrder.Init()
	return clients
}

func closeCachedProxyClients(clients map[string]cachedProxyClient) {
	for _, cached := range clients {
		if cached.transport != nil {
			cached.transport.CloseIdleConnections()
		}
	}
}

// clientForProxy 返回可复用的代理 HTTP 客户端。代理 URL 是账号出站身份的一部分，
// 以完整 URL 作为 key 可同时安全支持多个账号和不同代理认证信息。
func (t *Transport) clientForProxy(base *http.Client, proxyURL string) (*http.Client, error) {
	var retired []*http.Transport
	defer func() {
		// Closing idle sockets can take time; never hold either cache/lifecycle
		// mutex while doing it. In-flight requests keep their live connections.
		for _, transport := range retired {
			transport.CloseIdleConnections()
		}
	}()
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.closed {
		return nil, errTransportStopped
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return base, nil
	}
	t.proxyMu.Lock()
	defer t.proxyMu.Unlock()
	retire := func(key string) {
		cached := t.proxyClients[key]
		delete(t.proxyClients, key)
		if cached.order != nil {
			t.proxyOrder.Remove(cached.order)
		}
		if cached.transport != nil {
			retired = append(retired, cached.transport)
		}
	}
	now := time.Now()
	// Access order also orders lastUsed, so expired entries are removed from
	// the tail in O(number expired), with O(1) work for an ordinary cache hit.
	for oldest := t.proxyOrder.Back(); oldest != nil; oldest = t.proxyOrder.Back() {
		key := oldest.Value.(string)
		if now.Sub(t.proxyClients[key].lastUsed) < proxyClientIdleTTL {
			break
		}
		retire(key)
	}
	// Forward snapshots the active client before resolving the account. An
	// ApplyConfig may replace it while that lookup is in flight; bind this
	// cache entry to the current base client so an old request cannot repopulate
	// a freshly reset cache with a stale transport.
	active := t.client
	if active != nil && active != base {
		base = active
	}
	if cached, ok := t.proxyClients[proxyURL]; ok && cached.base == base && cached.client != nil {
		cached.lastUsed = now
		t.proxyOrder.MoveToFront(cached.order)
		t.proxyClients[proxyURL] = cached
		return cached.client, nil
	}
	client, transport, err := clientWithProxy(base, proxyURL)
	if err != nil {
		return nil, err
	}
	if transport != nil && client != nil && client != base {
		if t.proxyClients == nil {
			t.proxyClients = make(map[string]cachedProxyClient)
		}
		if _, exists := t.proxyClients[proxyURL]; exists {
			retire(proxyURL)
		}
		for len(t.proxyClients) >= maxCachedProxyClients {
			retire(t.proxyOrder.Back().Value.(string))
		}
		t.proxyClients[proxyURL] = cachedProxyClient{base: base, client: client, transport: transport, lastUsed: now, order: t.proxyOrder.PushFront(proxyURL)}
	}
	return client, nil
}

// netDialer keeps the connection timeout separate from the whole request
// timeout. It is a tiny wrapper so the plugin can keep one reusable Transport.
type netDialer struct{}

var sharedNetDialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

func (*netDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return sharedNetDialer.DialContext(ctx, network, address)
}

func (t *Transport) SetHostBroker(b *hcplugin.GRPCBroker) { t.mu.Lock(); t.broker = b; t.mu.Unlock() }

func (t *Transport) InitHostServices(ctx context.Context, r *pluginv1.InitHostServicesRequest) (*pluginv1.InitHostServicesResponse, error) {
	if r == nil || r.GetHostServiceApiVersion() != pluginv1.HostServiceAPIVersion {
		return &pluginv1.InitHostServicesResponse{Message: "unsupported host service API"}, nil
	}
	t.mu.RLock()
	broker, closed := t.broker, t.closed
	t.mu.RUnlock()
	if broker == nil || closed {
		return &pluginv1.InitHostServicesResponse{Message: "host broker unavailable"}, nil
	}
	conn, err := broker.Dial(r.GetHostServiceId())
	if err != nil {
		return &pluginv1.InitHostServicesResponse{Message: "host service connection failed"}, nil
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = conn.Close()
		return &pluginv1.InitHostServicesResponse{Message: "plugin stopped"}, nil
	}
	old := t.hostConn
	t.hostConn = conn
	t.host = pluginv1.NewHostServiceClient(conn)
	t.bindBPSAccountStore(t.host)
	t.accounts.invalidate()
	t.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return &pluginv1.InitHostServicesResponse{Ready: true, Message: "host services connected"}, nil
}

func (t *Transport) GetInfo(context.Context, *pluginv1.GetInfoRequest) (*pluginv1.GetInfoResponse, error) {
	return &pluginv1.GetInfoResponse{PluginId: protocol.PluginID, PluginVersion: protocol.Version, ProtocolVersion: pluginv1.ProtocolVersion, TransportApiVersion: pluginv1.TransportAPIVersion, Capabilities: []string{protocol.Capability}}, nil
}

func (t *Transport) Health(ctx context.Context, _ *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	t.mu.RLock()
	cfg, closed := t.cfg, t.closed
	t.mu.RUnlock()
	// 账号目录是只读查询：不应用配置、不访问上游，满足 status_json 的无副作用要求。
	accounts := t.accountDirectory(ctx)
	healthy, message := !closed, healthMessage(closed)
	return &pluginv1.HealthResponse{Healthy: healthy, Message: message, StatusJson: t.bpsAccountStatusJSON(healthStatusJSON(cfg, accounts), cfg)}, nil
}

// healthStatusJSON 生成无副作用的只读状态快照：不应用配置、不访问上游。
func healthStatusJSON(c protocol.Config, accounts []accountSummary) string {
	if accounts == nil {
		accounts = []accountSummary{}
	}
	accountIDs := c.SelectedAccountIDs()
	if accountIDs == nil {
		accountIDs = []int64{}
	}
	status, _ := json.Marshal(map[string]any{
		"plugin_version":           protocol.Version,
		"capability":               protocol.Capability,
		"responses_url":            c.ResponsesURL,
		"enabled_models":           c.EnabledModels,
		"available_models":         protocol.AvailableModels(),
		"account_ids":              accountIDs,
		"auto_select_new_accounts": c.AutoSelectNewAccounts,
		"excluded_account_ids":     c.ExcludedAccountIDs,
		"accounts":                 accounts,
		"auth_mode":                c.AuthMode,
		"timeout_seconds":          c.TimeoutSeconds,
		"rewrite_tools":            c.RewriteTools,
		"transform_responses":      c.TransformResponses,
		"reasoning_efforts":        protocol.SupportedReasoningEfforts(),
		"image_input":              "automatic_attachments",
	})
	return string(status)
}

// accountDirectory 返回插件作用域内的 OpenAI OAuth 账号目录，供配置页选择固定账号。
// Cached results are immutable snapshots. A single bounded refresh runs outside
// the mutex; stale readers return immediately and cold readers share its result
// while remaining independently cancelable.
func (t *Transport) accountDirectory(ctx context.Context) []accountSummary {
	for {
		if ctx.Err() != nil {
			return nil
		}
		// Pair the host snapshot with its cache generation. Host rebinding uses
		// the same lock order and cannot start an old-host refresh after reset.
		t.mu.RLock()
		host := t.host
		if t.closed || host == nil {
			t.mu.RUnlock()
			return nil
		}
		t.accounts.mu.Lock()
		items := append([]accountSummary(nil), t.accounts.items...)
		if time.Now().Before(t.accounts.expires) {
			t.accounts.mu.Unlock()
			t.mu.RUnlock()
			return items
		}
		refresh := t.accounts.refresh
		var lookupCtx context.Context
		if refresh == nil {
			var cancel context.CancelFunc
			lookupCtx, cancel = context.WithTimeout(context.Background(), accountLookupTimeout)
			refresh = &accountRefresh{done: make(chan struct{}), cancel: cancel}
			t.accounts.refresh = refresh
		}
		loaded := t.accounts.items != nil
		t.accounts.mu.Unlock()
		t.mu.RUnlock()
		if lookupCtx != nil {
			go t.refreshAccountDirectory(lookupCtx, host, refresh)
		}
		if loaded {
			return items
		}
		select {
		case <-ctx.Done():
			return nil
		case <-refresh.done:
			// A rebind can wake an old generation. Recheck the current host and
			// cache instead of returning a result from the detached host.
		}
	}
}

func (t *Transport) refreshAccountDirectory(ctx context.Context, host pluginv1.HostServiceClient, refresh *accountRefresh) {
	defer refresh.cancel()
	response, err := host.ListAccounts(ctx, &pluginv1.ListAccountsRequest{Platform: "openai", AccountType: "oauth"})
	items := make([]accountSummary, 0, len(response.GetAccounts()))
	for _, account := range response.GetAccounts() {
		items = append(items, accountSummary{
			ID:          account.GetId(),
			Name:        account.GetName(),
			Status:      account.GetStatus(),
			Schedulable: account.GetSchedulable(),
		})
	}
	t.accounts.mu.Lock()
	defer t.accounts.mu.Unlock()
	if t.accounts.refresh != refresh {
		return // Host replacement or shutdown already invalidated this work.
	}
	if err == nil {
		t.accounts.items = items
		t.accounts.expires = time.Now().Add(accountCacheTTL)
	} else {
		// Keep the last successful snapshot and suppress sequential failures.
		t.accounts.expires = time.Now().Add(accountCacheRetry)
	}
	t.accounts.refresh = nil
	close(refresh.done)
}

// Called with Transport.mu held. The pointer is a generation token: a delayed
// RPC cannot publish into a replacement host's cache. Waiters wake immediately.
func (cache *accountCache) invalidate() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.refresh != nil {
		cache.refresh.cancel()
		close(cache.refresh.done)
		cache.refresh = nil
	}
	cache.items = nil
	cache.expires = time.Time{}
}

func healthMessage(closed bool) string {
	if closed {
		return "plugin stopped"
	}
	return "ready"
}

func (t *Transport) ValidateConfig(_ context.Context, r *pluginv1.ValidateConfigRequest) (*pluginv1.ValidateConfigResponse, error) {
	var raw []byte
	if r != nil {
		raw = r.GetConfigJson()
	}
	c, err := protocol.ParseConfig(raw)
	if err != nil {
		return &pluginv1.ValidateConfigResponse{Valid: false, Message: safeError(err)}, nil
	}
	return &pluginv1.ValidateConfigResponse{Valid: true, Message: "configuration valid", NormalizedConfigJson: protocol.JSONBytes(c)}, nil
}

func (t *Transport) ApplyConfig(_ context.Context, r *pluginv1.ApplyConfigRequest) (*pluginv1.ApplyConfigResponse, error) {
	var raw []byte
	if r != nil {
		raw = r.GetConfigJson()
	}
	c, err := protocol.ParseConfig(raw)
	if err != nil {
		return &pluginv1.ApplyConfigResponse{Applied: false, Message: safeError(err)}, nil
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return &pluginv1.ApplyConfigResponse{Applied: false, Message: "plugin is stopped"}, nil
	}
	var old *http.Client
	var proxyClients map[string]cachedProxyClient
	// Only timeout config affects newClient. Account selection, endpoint,
	// auth mode and response/tool settings are applied per request and must
	// not discard healthy pools on every configuration-page save.
	if t.client == nil || t.cfg.TimeoutSeconds != c.TimeoutSeconds {
		old = t.client
		t.client = newClient(c)
		proxyClients = t.detachProxyClients()
	}
	t.cfg = c
	t.mu.Unlock()
	// 代理客户端绑定旧配置的超时/TLS 参数，应用新配置后必须丢弃旧缓存。
	closeCachedProxyClients(proxyClients)
	if old != nil {
		if tr, ok := old.Transport.(interface{ CloseIdleConnections() }); ok {
			tr.CloseIdleConnections()
		}
	}
	return &pluginv1.ApplyConfigResponse{Applied: true, Message: "configuration applied"}, nil
}

func (t *Transport) TestConfig(ctx context.Context, r *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	var raw []byte
	if r != nil {
		raw = r.GetConfigJson()
	}
	c, err := protocol.ParseConfig(raw)
	if err != nil {
		return &pluginv1.TestConfigResponse{Success: false, Message: safeError(err)}, nil
	}
	if c.DegradationCheck {
		started := time.Now()
		check, checkErr := t.runDegradationCheck(ctx, c)
		statusJSON := mergeDegradationStatus(t.bpsAccountStatusJSON(healthStatusJSON(c, t.accountDirectory(ctx)), c), check)
		if checkErr != nil {
			return &pluginv1.TestConfigResponse{
				Success:    false,
				Message:    safeError(checkErr),
				LatencyMs:  time.Since(started).Milliseconds(),
				StatusJson: statusJSON,
			}, nil
		}
		// A completed check is a successful operation even when one or more
		// accounts answered incorrectly. The per-account verdicts are in
		// status_json; returning success=false would make UI Bridge discard them.
		return &pluginv1.TestConfigResponse{
			Success:    true,
			Message:    fmt.Sprintf("degradation check completed: %d degraded account(s)", len(check.DegradedAccountIDs)),
			LatencyMs:  time.Since(started).Milliseconds(),
			StatusJson: statusJSON,
		}, nil
	}
	started := time.Now()
	reachable, detail := probeEndpoint(ctx, c)
	return &pluginv1.TestConfigResponse{
		Success:    reachable,
		Message:    detail,
		LatencyMs:  time.Since(started).Milliseconds(),
		StatusJson: t.bpsAccountStatusJSON(healthStatusJSON(c, t.accountDirectory(ctx)), c),
	}, nil
}

// probeEndpoint 只做一次轻量的 TCP/TLS 可达性探测：不发送请求、不携带访问令牌，
// 因此不会触发上游计费或泄露凭据。它不经过账号级代理，所以探测失败不代表真实
// 请求一定失败，消息里会明确说明这一点。
func probeEndpoint(ctx context.Context, c protocol.Config) (bool, string) {
	parsed, err := url.Parse(c.ResponsesURL)
	if err != nil || parsed.Hostname() == "" {
		return false, "responses_url is not a valid absolute URL"
	}
	port := parsed.Port()
	if port == "" {
		if strings.EqualFold(parsed.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	address := net.JoinHostPort(parsed.Hostname(), port)
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", address)
	if err != nil {
		return false, fmt.Sprintf("cannot reach %s (direct connection, account proxy is not used): %v", address, err)
	}
	defer func() { _ = conn.Close() }()
	if strings.EqualFold(parsed.Scheme, "https") {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: parsed.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(probeCtx); err != nil {
			return false, fmt.Sprintf("TLS handshake with %s failed: %v", address, err)
		}
	}
	return true, fmt.Sprintf("%s is reachable. Direct connection only: the account proxy is used for real requests, and credentials are attached only when forwarding.", address)
}

func (t *Transport) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	t.mu.RLock()
	cfg, client, host, closed := t.cfg, t.client, t.host, t.closed
	uploader := t.attachments
	t.mu.RUnlock()
	if closed {
		return sendError(stream, "plugin_stopped", "plugin is stopped", false)
	}
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	start, body, err := receiveRequest(stream)
	if err != nil {
		var api *protocol.APIError
		if errors.As(err, &api) {
			return sendImageRelayError(stream, err)
		}
		return sendError(stream, errorCode(err), safeError(err), false)
	}
	if start.GetPlatform() != "" && !strings.EqualFold(start.GetPlatform(), "openai") {
		return sendError(stream, "unsupported_platform", "plugin only accepts OpenAI OAuth requests", false)
	}
	if start.GetAccountType() != "" && !strings.EqualFold(start.GetAccountType(), "oauth") {
		return sendError(stream, "unsupported_account_type", "plugin only accepts OAuth accounts", false)
	}

	// 自动模式下新增账号直接走 BPS，明确取消的账号原样透传。旧配置继续使用
	// 白名单，直到配置页保存完成迁移；无需账号轮询或打开页面才能接入新 ID。
	if !cfg.HandlesAccount(start.GetAccountId()) {
		return t.passthrough(stream, start, body, client, cfg.MaxResponseBytes)
	}

	// 只有本插件对外提供的模型才走 Basis Points。宿主会用普通 Codex 模型
	// （默认 gpt-5.4）对账号做连通性测试，这个请求同样会被交给插件；把它打到
	// Basis Points 会被上游以 403 basispoints_model_access_changed 拒绝，宿主
	// 随即把账号判成异常/限流。未知模型一律原样透传。
	if !protocol.HandlesModel(protocol.RequestedModel(body), cfg) {
		return t.passthrough(stream, start, body, client, cfg.MaxResponseBytes)
	}
	if t.isBPSAccountDisabled(start.GetAccountId(), cfg) {
		return t.passthrough(stream, start, body, client, cfg.MaxResponseBytes)
	}
	if start.GetAccountId() > 0 {
		if err := t.bpsAccountStoreError(); err != nil {
			return sendError(stream, "bps_account_state_unavailable", safeError(err), true)
		}
	}
	observeBPSStatus := newBasisPointsStatusObserver(func() {
		t.disableBPSAccount(stream.Context(), start.GetAccountId())
	})

	requestHeaders, proxyURL, err := prepareHeaders(stream.Context(), start, host, cfg.AuthMode)
	if err != nil {
		// 固定账号解析失败这类错误换账号也不会变好，必须上报为"已发出"，
		// 否则宿主会拿池子里每个账号各试一遍，最后把一个跟真实原因无关的
		// "no available accounts" 503 抛给客户端。
		return sendError(stream, errorCode(err), safeError(err), isUnreplayable(err))
	}
	requestClient, err := t.clientForProxy(client, proxyURL)
	if err != nil {
		if errors.Is(err, errTransportStopped) {
			return sendError(stream, "plugin_stopped", err.Error(), false)
		}
		return sendError(stream, "invalid_proxy", safeError(err), false)
	}
	if requestClient == nil {
		return sendError(stream, "invalid_proxy", "proxy client is unavailable", false)
	}
	requestBody := body
	var source map[string]any
	imagesRewritten := false
	{ // Image preparation is independent of tool and response conversion toggles.
		source, err = protocol.RawObject(body)
		if err != nil {
			return sendRequestValidationError(stream, err)
		}
		// Internal context keys may only originate in this process.
		for key := range source {
			if strings.HasPrefix(key, "__bps_") {
				delete(source, key)
			}
		}
		// The host has already isolated these identifiers by API key/account.
		// Keep the scope local: PrepareResponsesBody rebuilds an allowlisted body.
		if scope := hostSessionScope(start.GetHeaders()); scope != "" {
			source["__bps_session_scope"] = scope
		}
		inlineImages := hasInlineImages(source)
		validate := cfg.RewriteTools || cfg.TransformResponses || inlineImages
		if validate {
			if err := protocol.ValidateImageUploadCapabilities(source); err != nil {
				return sendRequestValidationError(stream, err)
			}
		}
		if inlineImages {
			release, err = t.imageAdmission.acquire(int64(len(body)))
			if err != nil {
				return sendImageRelayError(stream, err)
			}
		}
		prepared := source
		if cfg.RewriteTools {
			// Derive conversation identity from original images, not ephemeral
			// uploaded file IDs; retries must keep the same turn and task.
			if inlineImages {
				prepared, err = protocol.PrepareResponsesBodyForImageUpload(source, cfg)
			} else {
				prepared, err = protocol.PrepareResponsesBody(source, cfg)
			}
			if err != nil {
				return sendRequestValidationError(stream, err)
			}
		}
		imagesRewritten, err = uploader.Rewrite(stream.Context(), requestClient, cfg.ResponsesURL, requestHeaders, prepared, relayScope(start))
		if err != nil {
			var upstreamStatus interface{ StatusCode() int }
			if errors.As(err, &upstreamStatus) {
				observeBPSStatus(upstreamStatus.StatusCode())
			}
			if stream.Context().Err() != nil {
				return stream.Context().Err()
			}
			return sendImageRelayError(stream, err, observeBPSStatus)
		}
		if validate {
			if err := protocol.ValidateRequestCapabilities(prepared); err != nil {
				return sendRequestValidationError(stream, err)
			}
		}
		if cfg.RewriteTools {
			requestBody = protocol.JSONBytes(prepared)
		} else if imagesRewritten {
			// Local session markers are never part of the upstream wire body.
			// Keep them in source for response translation/replay.
			wireSource := make(map[string]any, len(source))
			for key, value := range source {
				if !strings.HasPrefix(key, "__bps_") {
					wireSource[key] = value
				}
			}
			requestBody = protocol.JSONBytes(wireSource)
		}
	}
	upstreamURL := cfg.ResponsesURL
	requestCtx, cancelRequest := context.WithTimeout(stream.Context(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancelRequest()
	req, err := http.NewRequestWithContext(requestCtx, start.GetMethod(), upstreamURL, bytes.NewReader(requestBody))
	if err != nil {
		return sendError(stream, "upstream_request", "cannot create upstream request", false)
	}
	for key, values := range requestHeaders {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if cfg.RewriteTools {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := doBasisPointsRequest(requestClient, req)
	if err != nil {
		return sendError(stream, "upstream_transport", safeTransportError(err), true)
	}
	defer resp.Body.Close()
	observeBPSStatus(resp.StatusCode)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return sendBasisPointsAccountStatus(stream, resp.StatusCode)
	}
	// The host also derives account health from quota headers and errors
	// inside HTTP 200 streams. Isolate only the BPS response before any
	// image conversion or optional response transformation can expose it.
	if err := prepareBasisPointsResponse(resp, cfg.MaxResponseBytes, observeBPSStatus); err != nil {
		return sendError(stream, errorCode(err), safeError(err), true)
	}
	if imagesRewritten && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		return sendImageUpstreamError(stream, resp, cfg.MaxResponseBytes)
	}
	if !cfg.TransformResponses || source == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if imagesRewritten {
			return sendImageSafeHTTPResponse(stream, resp, cfg.MaxResponseBytes)
		}
		return sendHTTPResponseStream(stream, resp, cfg.MaxResponseBytes)
	}
	// A streaming Responses request must not wait for response.completed before
	// emitting the first text delta. The adaptive relay passes ordinary SSE
	// events through as they arrive and withholds only native transport-tool
	// events until the completed response can be translated safely.
	var repair relayToolRepair
	if cfg.RewriteTools {
		repair = newRelayToolRepair(req, requestClient, requestBody, source, cfg.MaxResponseBytes, observeBPSStatus)
	}
	if wantsStream(source) && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return sendTransformedHTTPResponseStreamWithRepair(stream, resp, cfg.MaxResponseBytes, source, 15*time.Second, repair)
	}
	respBody, err := readLimited(resp.Body, cfg.MaxResponseBytes)
	if err != nil {
		return sendError(stream, errorCode(err), safeError(err), true)
	}
	if imagesRewritten {
		respBody, _ = redactImageFailureJSON(respBody, "")
	}
	responseBody, responseContentType, transformErr := transformResponse(respBody, resp.Header, source)
	if protocol.IsRepairableClientToolError(transformErr) && repair != nil {
		if original, parseErr := protocol.ParseFinalStreamResponse(respBody); parseErr == nil && protocol.ToolRepairEligible(source, original) {
			_ = resp.Body.Close()
			var fixed map[string]any
			fixed, transformErr = repair(stream.Context(), original)
			if transformErr == nil {
				headers := resp.Header.Clone()
				headers.Set("Content-Type", "application/json")
				responseBody, responseContentType, transformErr = transformResponse(protocol.JSONBytes(fixed), headers, source)
			}
		}
	}
	if transformErr != nil {
		return sendError(stream, errorCode(transformErr), safeError(transformErr), true)
	}
	if imagesRewritten {
		responseBody, _ = redactImageFailureJSON(responseBody, "")
	}
	return sendHTTPResponse(stream, resp, responseBody, responseContentType)
}

// passthrough 把宿主的请求原样送到宿主原本指定的上游，不做任何改写。
//
// 用途：账号白名单模式下宿主调度的账号不在勾选列表里 —— 这个账号不应该被插件
// 改写。宿主已经把请求交给了插件，而插件无法拒绝接管，所以这里做一次与"没有
// 启用插件"等价的转发：URL、方法、请求头、请求体原样发出，响应原样流回，
// 既不注入 Basis Points 头，也不改写工具目录，更不做响应转换。
func (t *Transport) passthrough(stream pluginv1.TransportPlugin_ForwardServer, start *pluginv1.ForwardRequestStart, body []byte, client *http.Client, max int) error {
	target := strings.TrimSpace(start.GetUrl())
	if target == "" {
		return sendError(stream, "invalid_request", "host did not provide an upstream URL for this account", false)
	}
	method := strings.TrimSpace(start.GetMethod())
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(stream.Context(), method, target, bytes.NewReader(body))
	if err != nil {
		return sendError(stream, "invalid_request", "cannot build the passthrough request", false)
	}
	for key, values := range start.GetHeaders() {
		if !passthroughRequestHeader(key) {
			continue
		}
		req.Header.Del(key)
		for _, value := range values.GetValues() {
			req.Header.Add(key, value)
		}
	}
	requestClient, err := t.clientForProxy(client, start.GetProxyUrl())
	if err != nil {
		if errors.Is(err, errTransportStopped) {
			return sendError(stream, "plugin_stopped", err.Error(), false)
		}
		return sendError(stream, "invalid_proxy", safeError(err), false)
	}
	if requestClient == nil {
		return sendError(stream, "invalid_proxy", "proxy client is unavailable", false)
	}
	resp, err := requestClient.Do(req)
	if err != nil {
		return sendError(stream, "upstream_transport", safeTransportError(err), true)
	}
	defer resp.Body.Close()
	return sendHTTPResponseStream(stream, resp, max)
}

// passthroughRequestHeader 过滤 hop-by-hop 头以及由 Go 传输层自行决定的头。
// Accept-Encoding 必须丢掉：回传响应时会剥掉 Content-Encoding，只有交给 Go 自己
// 协商压缩并透明解压，客户端才能拿到可解析的正文。
func passthroughRequestHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Host", "Content-Length", "Connection", "Transfer-Encoding", "Accept-Encoding",
		"Proxy-Authorization", "Proxy-Connection", "Keep-Alive", "Te", "Trailer", "Upgrade":
		return false
	}
	return true
}

func receiveRequest(stream pluginv1.TransportPlugin_ForwardServer) (*pluginv1.ForwardRequestStart, []byte, error) {
	return receiveAdmittedRequest(stream, 128<<20, nil)
}

func receiveAdmittedRequest(stream pluginv1.TransportPlugin_ForwardServer, max int, admit func(*pluginv1.ForwardRequestStart) error) (*pluginv1.ForwardRequestStart, []byte, error) {
	frame, err := stream.Recv()
	if err != nil {
		return nil, nil, fmt.Errorf("request start is missing")
	}
	start := frame.GetStart()
	if start == nil {
		return nil, nil, fmt.Errorf("first request frame must be start")
	}
	if start.GetMethod() == "" {
		return nil, nil, fmt.Errorf("request method is missing")
	}
	if start.GetContentLength() > int64(max) {
		return nil, nil, &protocol.APIError{Status: http.StatusRequestEntityTooLarge, Kind: "request_too_large", Message: "request body exceeds the configured limit"}
	}
	if admit != nil {
		if err := admit(start); err != nil {
			return nil, nil, err
		}
		if start.GetContentLength() > 0 {
			max = int(start.GetContentLength())
		}
	}
	var body bytes.Buffer
	ended := false
	for {
		frame, err = stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil, nil, fmt.Errorf("request body end is missing")
		}
		if err != nil {
			return nil, nil, fmt.Errorf("request body stream failed")
		}
		if chunk := frame.GetBodyChunk(); len(chunk) > 0 {
			if body.Len()+len(chunk) > max {
				return nil, nil, &protocol.APIError{Status: http.StatusRequestEntityTooLarge, Kind: "request_too_large", Message: "request body exceeds the configured or declared limit"}
			}
			_, _ = body.Write(chunk)
		}
		if frame.GetBodyEnd() {
			ended = true
			break
		}
	}
	if !ended {
		return nil, nil, fmt.Errorf("request body end is missing")
	}
	return start, body.Bytes(), nil
}

func (t *Transport) prepareHeaders(ctx context.Context, start *pluginv1.ForwardRequestStart, host pluginv1.HostServiceClient) (http.Header, string, error) {
	return prepareHeaders(ctx, start, host, currentAuthMode(t))
}

// prepareHeaders 使用请求开始时的 authMode 快照。身份查询期间即使热更新
// 配置，该请求的 URL、请求体与认证模式仍来自同一份配置。
func prepareHeaders(ctx context.Context, start *pluginv1.ForwardRequestStart, host pluginv1.HostServiceClient, authMode string) (http.Header, string, error) {
	h := make(http.Header)
	for key, values := range start.GetHeaders() {
		for _, value := range values.GetValues() {
			if allowedIncomingHeader(key) {
				h.Add(key, value)
			}
		}
	}
	proxyURL := start.GetProxyUrl()
	// 凭据与代理始终跟随宿主本次调度的账号：宿主给了出站令牌就直接用它；
	// 宿主只负责调度、令牌由插件取时，按【同一个账号】解析身份（连同该账号自己的代理）。
	// 账号白名单只决定"这个账号走不走插件"，不参与这里 —— 插件不会用别的账号的
	// 令牌或代理替换当前账号，因此宿主的并发计数、限流窗口与用量归属始终准确。
	if !isBearerToken(h.Get("Authorization")) && start.GetAccountId() != 0 {
		if host == nil {
			// 宿主反向服务不可用：换账号也不会变好，按不可重放上报。
			return nil, "", unreplayable(fmt.Errorf("host services are unavailable; cannot resolve account %d", start.GetAccountId()))
		}
		identity, err := host.ResolveOutboundIdentity(ctx, &pluginv1.ResolveOutboundIdentityRequest{AccountId: start.GetAccountId()})
		if err != nil {
			return nil, "", fmt.Errorf("host identity lookup failed")
		}
		if !identity.GetFound() || identity.GetToken() == "" {
			return nil, "", fmt.Errorf("account %d has no usable OAuth credential", start.GetAccountId())
		}
		// 只补空缺：影子账号会刻意透传母账号 ID，宿主透传的头更权威。
		applyOutboundIdentity(h, identity, false)
		// 代理跟随同一个账号；空值表示直连，不由进程环境变量改写。
		proxyURL = identity.GetProxyUrl()
	}
	authorization := h.Get("Authorization")
	if !isBearerToken(authorization) {
		return nil, "", fmt.Errorf("OAuth access token is missing")
	}
	// 账号 ID 以宿主透传的头为准（影子账号会刻意透传母账号 ID），
	// 头部缺失时才回退解析访问令牌的 JWT 声明。
	identity := auth.ParseToken(authorization, firstHeader(h, "ChatGPT-Account-ID", "X-OpenAI-Account-ID"))
	if identity.AccountID == "" {
		return nil, "", fmt.Errorf("ChatGPT account ID is missing")
	}
	h.Set("Authorization", "Bearer "+identity.AccessToken)
	h.Set("ChatGPT-Account-ID", identity.AccountID)
	h.Set("X-OpenAI-Account-ID", identity.AccountID)
	if authMode == "" {
		authMode = "chatgpt"
	}
	h.Set("X-Basispoints-Auth-Mode", authMode)
	h.Set("Content-Type", "application/json")
	h.Set("Origin", "https://bps.openai.com")
	for key, value := range basisPointsHeaders() {
		h.Set(key, value)
	}
	h.Del("Host")
	h.Del("Content-Length")
	h.Del("Proxy-Authorization")
	h.Del("Accept-Encoding")
	if strings.Contains(strings.ToLower(h.Get("Accept")), "text/event-stream") {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	return h, proxyURL, nil
}

func currentAuthMode(t *Transport) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.cfg.AuthMode == "" {
		return "chatgpt"
	}
	return t.cfg.AuthMode
}

func allowedIncomingHeader(key string) bool {
	k := http.CanonicalHeaderKey(key)
	switch k {
	case "Authorization", "Chatgpt-Account-Id", "X-Openai-Account-Id", "Accept", "User-Agent", "Content-Type", "Openai-Beta":
		return true
	}
	return strings.HasPrefix(strings.ToLower(key), "x-") && !strings.EqualFold(key, "x-forwarded-for") && !strings.EqualFold(key, "x-forwarded-host")
}

func firstHeader(h http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(h.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func isBearerToken(authorization string) bool {
	value := strings.TrimSpace(authorization)
	return len(value) > 7 && strings.EqualFold(value[:7], "bearer ") && strings.TrimSpace(value[7:]) != ""
}

func containsAccount(list []int64, id int64) bool {
	for _, candidate := range list {
		if candidate == id {
			return true
		}
	}
	return false
}

// applyOutboundIdentity 把宿主解析出的身份写到请求头上。
// overwrite 为真时覆盖传入的同名头（限定/固定账号场景：宿主解析值更权威，
// 影子账号会带上母账号 ID）；为假时只补空缺（跟随调度场景）。
func applyOutboundIdentity(h http.Header, identity *pluginv1.ResolveOutboundIdentityResponse, overwrite bool) {
	h.Set("Authorization", "Bearer "+identity.GetToken())
	for key, values := range identity.GetHeaders() {
		if !overwrite && h.Get(key) != "" {
			continue
		}
		h.Del(key)
		for _, value := range values.GetValues() {
			h.Add(key, value)
		}
	}
}

// unreplayableError 标记"换一个账号重放这条请求不会改变结果"的失败。
//
// 宿主的 ForwardResponseError.request_sent 决定它要不要换账号重试。插件按配置
// 固定了一组账号时，宿主换账号是白费的：插件永远只用自己解析出来的那组账号。
// 这类失败若照实上报 request_sent=false，宿主就会把整个账号池挨个试穿，
// 再把池子耗空后回一个 "no available accounts" 503 —— 客户端看到的错误
// 与真实原因完全无关。因此这类失败按"不可重放"上报。
type unreplayableError struct{ err error }

func (e *unreplayableError) Error() string { return e.err.Error() }

func (e *unreplayableError) Unwrap() error { return e.err }

func unreplayable(err error) error {
	if err == nil {
		return nil
	}
	return &unreplayableError{err: err}
}

func isUnreplayable(err error) bool {
	var target *unreplayableError
	return errors.As(err, &target)
}

var basisPointsHeaderValues = map[string]string{
	"X-OpenAI-Internal-Basispoints-Client-Agent-Profile": "excel", "X-OpenAI-Internal-Basispoints-Client-Editor": "excel", "X-OpenAI-Internal-Basispoints-Client-Host": "office", "X-OpenAI-Internal-Basispoints-Client-Platform": "excel", "X-OpenAI-Internal-Basispoints-Client-Platform-Class": "PC", "X-OpenAI-Internal-Basispoints-Client-Product": "basispoints-excel-plugin", "X-OpenAI-Internal-Basispoints-Client-Runtime": "desktop", "X-OpenAI-Internal-Basispoints-Office-Host": "Excel", "X-OpenAI-Internal-Basispoints-Office-Platform": "PC", "X-Stainless-Arch": "unknown", "X-Stainless-Lang": "js", "X-Stainless-OS": "Unknown", "X-Stainless-Package-Version": "6.31.0", "X-Stainless-Retry-Count": "0", "X-Stainless-Runtime": "browser:chrome", "User-Agent": "sub2api-oai-basispoints/" + protocol.Version,
}

// basisPointsHeaders 返回只读共享表，避免每个请求重复分配固定画像头。
func basisPointsHeaders() map[string]string { return basisPointsHeaderValues }

// transformResponse 把上游响应转换成客户端可用的形状，并返回需要覆盖的
// Content-Type（空字符串表示沿用上游响应头）。
//
// Basis Points 的 Excel 网关对 Responses 请求总是回 SSE，即使请求里 stream=false。
// 此时若下游没有要求流式，就必须还原成 JSON 正文，否则客户端会拿到它解析不了的 SSE。
func transformResponse(body []byte, headers http.Header, source map[string]any) ([]byte, string, error) {
	if strings.Contains(strings.ToLower(headers.Get("Content-Type")), "text/event-stream") {
		response, err := protocol.ParseFinalStreamResponse(body)
		if err != nil {
			return nil, "", err
		}
		body = protocol.JSONBytes(response)
	}
	translated, response, _, err := protocol.TransformResponseBody(body, source)
	if err != nil {
		return nil, "", err
	}
	if wantsStream(source) {
		return protocol.SyntheticStream(response), "text/event-stream", nil
	}
	return translated, "application/json", nil
}

func wantsStream(source map[string]any) bool {
	value, _ := source["stream"].(bool)
	return value
}

func sendHTTPResponse(stream pluginv1.TransportPlugin_ForwardServer, resp *http.Response, body []byte, contentType string) error {
	headers := responseHeaders(resp.Header, int64(len(body)))
	if contentType != "" {
		headers["Content-Type"] = &pluginv1.HeaderValues{Values: []string{contentType}}
	}
	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{StatusCode: int32(resp.StatusCode), Status: resp.Status, Protocol: resp.Proto, ProtocolMajor: int32(resp.ProtoMajor), ProtocolMinor: int32(resp.ProtoMinor), Headers: headers, ContentLength: int64(len(body))}}}); err != nil {
		return err
	}
	const chunkSize = 32 << 10
	for offset := 0; offset < len(body); offset += chunkSize {
		end := offset + chunkSize
		if end > len(body) {
			end = len(body)
		}
		if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: body[offset:end]}}); err != nil {
			return err
		}
	}
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{BytesReceived: int64(len(body))}}})
}

func sendHTTPResponseStream(stream pluginv1.TransportPlugin_ForwardServer, resp *http.Response, max int) error {
	headers := responseHeaders(resp.Header, resp.ContentLength)
	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{StatusCode: int32(resp.StatusCode), Status: resp.Status, Protocol: resp.Proto, ProtocolMajor: int32(resp.ProtoMajor), ProtocolMinor: int32(resp.ProtoMinor), Headers: headers, ContentLength: resp.ContentLength}}}); err != nil {
		return err
	}
	var received int64
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			received += int64(n)
			if received > int64(max) {
				return sendError(stream, "upstream_response_too_large", "upstream response exceeds configured limit", true)
			}
			chunk := append([]byte(nil), buf[:n]...)
			if sendErr := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: chunk}}); sendErr != nil {
				return sendErr
			}
		}
		if errors.Is(err, io.EOF) {
			return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{BytesReceived: received}}})
		}
		if err != nil {
			var api *protocol.APIError
			if errors.As(err, &api) {
				return sendError(stream, api.Code(), api.Error(), true)
			}
			return sendError(stream, "upstream_read", "upstream response could not be read", true)
		}
	}
}

func responseHeaders(input http.Header, contentLength int64) map[string]*pluginv1.HeaderValues {
	headers := make(map[string]*pluginv1.HeaderValues, len(input)+1)
	for key, values := range input {
		if strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Transfer-Encoding") || strings.EqualFold(key, "Connection") || strings.EqualFold(key, "Content-Encoding") {
			continue
		}
		clean := make([]string, len(values))
		copy(clean, values)
		headers[key] = &pluginv1.HeaderValues{Values: clean}
	}
	if contentLength >= 0 {
		headers["Content-Length"] = &pluginv1.HeaderValues{Values: []string{fmt.Sprint(contentLength)}}
	}
	return headers
}

// Request validation is an HTTP result, not a failed transport. Keeping its
// status prevents the host from converting a deterministic 4xx into a 502 and
// replaying the same invalid input against other accounts. Only safe protocol
// errors from request validation use this path; upstream and transport errors
// retain their existing error-frame behavior.
func sendRequestValidationError(stream pluginv1.TransportPlugin_ForwardServer, err error) error {
	var api *protocol.APIError
	if !errors.As(err, &api) || api.StatusCode() < 400 || api.StatusCode() >= 500 {
		return sendError(stream, errorCode(err), safeError(err), false)
	}
	body := protocol.JSONBytes(map[string]any{
		"error": map[string]any{
			"message": api.Error(),
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    api.Code(),
		},
	})
	status := api.StatusCode()
	return sendHTTPResponse(stream, &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
	}, body, "application/json")
}

func sendError(stream pluginv1.TransportPlugin_ForwardServer, code, message string, requestSent bool) error {
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{Code: code, Message: message, RequestSent: requestSent}}})
}
func errorCode(err error) string {
	var api *protocol.APIError
	if errors.As(err, &api) {
		return api.Code()
	}
	return "invalid_request"
}
func safeError(err error) string {
	if err == nil {
		return ""
	}
	var api *protocol.APIError
	if errors.As(err, &api) {
		return api.Error()
	}
	return err.Error()
}
func safeTransportError(err error) string {
	if errors.Is(err, context.Canceled) {
		return "request canceled"
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return safeUpstreamReadError(err)
	}
	return "upstream transport failed"
}

func readLimited(r io.Reader, max int) ([]byte, error) {
	if prepared, ok := r.(*basisPointsBufferedBody); ok {
		return prepared.readRemaining(max)
	}
	data, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		var api *protocol.APIError
		if errors.As(err, &api) {
			return nil, api
		}
		return nil, &protocol.APIError{Status: http.StatusBadGateway, Kind: "upstream_read", Message: safeUpstreamReadError(err)}
	}
	if len(data) > max {
		return nil, &protocol.APIError{Status: http.StatusBadGateway, Kind: "upstream_response_too_large", Message: "upstream response exceeds configured limit"}
	}
	return data, nil
}

func clientWithProxy(base *http.Client, proxyURL string) (*http.Client, *http.Transport, error) {
	u, err := url.Parse(proxyURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, nil, fmt.Errorf("proxy_url is invalid")
	}
	if strings.EqualFold(u.Scheme, "socks5") || strings.EqualFold(u.Scheme, "socks5h") {
		var auth *proxy.Auth
		if u.User != nil {
			auth = &proxy.Auth{User: u.User.Username()}
			if password, ok := u.User.Password(); ok {
				auth.Password = password
			}
		}
		dialer, dialErr := proxy.SOCKS5("tcp", u.Host, auth, &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second})
		if dialErr != nil {
			return nil, nil, fmt.Errorf("proxy_url is invalid")
		}
		transport, ok := base.Transport.(*http.Transport)
		if !ok || transport == nil {
			return base, nil, nil
		}
		clone := transport.Clone()
		clone.Proxy = nil
		clone.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if d, ok := dialer.(proxy.ContextDialer); ok {
				return d.DialContext(ctx, network, address)
			}
			return dialer.Dial(network, address)
		}
		return &http.Client{Transport: clone, Timeout: base.Timeout}, clone, nil
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return nil, nil, fmt.Errorf("proxy_url must use http or https")
	}
	transport, ok := base.Transport.(*http.Transport)
	if !ok || transport == nil {
		return base, nil, nil
	}
	clone := transport.Clone()
	clone.Proxy = http.ProxyURL(u)
	return &http.Client{Transport: clone, Timeout: base.Timeout}, clone, nil
}
