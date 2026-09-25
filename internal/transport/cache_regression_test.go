package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"google.golang.org/grpc"
)

type cacheDirectoryHost struct {
	pluginv1.HostServiceClient
	calls atomic.Int32
	list  func(context.Context) (*pluginv1.ListAccountsResponse, error)
}

func (h *cacheDirectoryHost) ListAccounts(ctx context.Context, request *pluginv1.ListAccountsRequest, _ ...grpc.CallOption) (*pluginv1.ListAccountsResponse, error) {
	h.calls.Add(1)
	if request.GetPlatform() != "openai" || request.GetAccountType() != "oauth" {
		return nil, fmt.Errorf("account directory scope changed")
	}
	return h.list(ctx)
}

func TestProxyCacheBoundsDistinctAccountProxies(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	for i := 0; i < 160; i++ {
		if _, err := tr.clientForProxy(tr.client, fmt.Sprintf("http://proxy-%d.example:8080", i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(tr.proxyClients) > 128 {
		t.Fatalf("proxy cache retained %d clients; want at most 128", len(tr.proxyClients))
	}
}

func TestApplyConfigPreservesPoolsForNonNetworkChanges(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	base := tr.client
	proxy, err := tr.clientForProxy(base, "http://proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	applyConfig(t, tr, map[string]any{"account_ids": []int64{7, 9}, "rewrite_tools": false})
	again, err := tr.clientForProxy(tr.client, "http://proxy.example:8080")
	if err != nil || tr.client != base || again != proxy {
		t.Fatalf("saving account/tool settings discarded pooled clients: %v", err)
	}
}

func TestAccountCacheBacksOffFailedRefresh(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	host := &cacheDirectoryHost{list: func(context.Context) (*pluginv1.ListAccountsResponse, error) {
		return nil, context.DeadlineExceeded
	}}
	tr.host = host
	for i := 0; i < 32; i++ {
		if got := tr.accountDirectory(context.Background()); len(got) != 0 {
			t.Fatal("failed cold lookup returned accounts")
		}
	}
	if host.calls.Load() != 1 {
		t.Fatalf("failed lookup made %d host RPCs; want one shared retry window", host.calls.Load())
	}
}

func TestAccountCacheServesStaleSnapshotDuringRefresh(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	release := make(chan struct{})
	defer close(release)
	tr.host = &cacheDirectoryHost{list: func(ctx context.Context) (*pluginv1.ListAccountsResponse, error) {
		select {
		case <-release:
			return &pluginv1.ListAccountsResponse{Accounts: []*pluginv1.AccountInfo{{Id: 8}}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	tr.accounts.items = []accountSummary{{ID: 7}}
	tr.accounts.expires = time.Now().Add(-time.Second)
	returned := make(chan []accountSummary, 1)
	go func() { returned <- tr.accountDirectory(context.Background()) }()
	select {
	case got := <-returned:
		if len(got) != 1 || got[0].ID != 7 {
			t.Fatalf("refresh lost existing snapshot: %+v", got)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("cached health lookup waited for the host refresh")
	}
}

func TestProxyCacheEvictsLeastRecentAndIdleClients(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	proxyURL := func(i int) string { return fmt.Sprintf("http://proxy-%d.example:8080", i) }
	for i := 0; i < maxCachedProxyClients; i++ {
		if _, err := tr.clientForProxy(tr.client, proxyURL(i)); err != nil {
			t.Fatal(err)
		}
	}
	first := tr.proxyClients[proxyURL(0)].client
	if got, err := tr.clientForProxy(tr.client, proxyURL(0)); err != nil || got != first {
		t.Fatalf("hot proxy client was replaced: %v", err)
	}
	if _, err := tr.clientForProxy(tr.client, proxyURL(maxCachedProxyClients)); err != nil {
		t.Fatal(err)
	}
	if _, exists := tr.proxyClients[proxyURL(1)]; exists || tr.proxyClients[proxyURL(0)].client != first {
		t.Fatal("capacity eviction did not preserve the recently used client")
	}
	oldest := tr.proxyOrder.Back().Value.(string)
	old := tr.proxyClients[oldest]
	expired := old
	expired.lastUsed = time.Now().Add(-proxyClientIdleTTL - time.Second)
	tr.proxyClients[oldest] = expired
	if _, err := tr.clientForProxy(tr.client, proxyURL(0)); err != nil {
		t.Fatal(err)
	}
	if _, exists := tr.proxyClients[oldest]; exists {
		t.Fatal("idle proxy remained cached")
	}
	if renewed, err := tr.clientForProxy(tr.client, oldest); err != nil || renewed == old.client {
		t.Fatalf("expired client was reused: %v", err)
	}
	// Authentication is part of the proxy identity even when host/port match.
	left, err := tr.clientForProxy(tr.client, "http://account-a:credential-a@proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	right, err := tr.clientForProxy(tr.client, "http://account-a:credential-b@proxy.example:8080")
	if err != nil || left == right {
		t.Fatalf("proxy credentials shared a cached client: %v", err)
	}
}

func TestProxyCacheEvictionPreservesActiveDownload(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(entered)
		<-release
		_, _ = io.WriteString(w, "complete")
	}))
	defer proxy.Close()
	defer unblock()
	tr := New()
	defer tr.Shutdown()
	client, err := tr.clientForProxy(tr.client, proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		response, err := client.Get("http://upstream.invalid/image")
		if err == nil {
			defer response.Body.Close()
			var body []byte
			body, err = io.ReadAll(response.Body)
			if err == nil && string(body) != "complete" {
				err = fmt.Errorf("active response was truncated")
			}
		}
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy request did not start")
	}
	for i := 0; i < maxCachedProxyClients; i++ {
		if _, err := tr.clientForProxy(tr.client, fmt.Sprintf("http://other-%d.example:8080", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, exists := tr.proxyClients[proxy.URL]; exists {
		t.Fatal("test proxy was not evicted")
	}
	unblock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("cache eviction interrupted active request: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active request did not finish after eviction")
	}
}

func TestProxyCacheConcurrentEvictionPreservesProxyIdentity(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	const workers, requests = 32, 32
	start := make(chan struct{})
	failures := make(chan error, workers)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			<-start
			for index := 0; index < requests; index++ {
				proxyURL := fmt.Sprintf("http://account-%d:credential-%d@proxy.example:8080", worker, index)
				client, err := tr.clientForProxy(tr.client, proxyURL)
				if err == nil {
					proxy, resolveErr := client.Transport.(*http.Transport).Proxy(&http.Request{})
					if resolveErr != nil || proxy.String() != proxyURL {
						err = fmt.Errorf("proxy identity changed during concurrent eviction")
					}
				}
				if err != nil {
					failures <- err
					return
				}
			}
		}(worker)
	}
	close(start)
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if len(tr.proxyClients) != maxCachedProxyClients || tr.proxyOrder.Len() != len(tr.proxyClients) {
		t.Fatalf("concurrent eviction corrupted capacity/order: %d/%d", len(tr.proxyClients), tr.proxyOrder.Len())
	}
	for key, entry := range tr.proxyClients {
		if entry.order == nil || entry.order.Value != key {
			t.Fatal("proxy cache index and eviction order diverged")
		}
	}
}

func waitAccountRefresh(t *testing.T, tr *Transport) {
	t.Helper()
	tr.accounts.mu.Lock()
	refresh := tr.accounts.refresh
	tr.accounts.mu.Unlock()
	if refresh == nil {
		return
	}
	select {
	case <-refresh.done:
	case <-time.After(2 * time.Second):
		t.Fatal("account cache refresh did not complete")
	}
}

func TestAccountCacheSharesColdRefreshAndHonorsCallerCancellation(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	host := &cacheDirectoryHost{list: func(ctx context.Context) (*pluginv1.ListAccountsResponse, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return &pluginv1.ListAccountsResponse{Accounts: []*pluginv1.AccountInfo{{Id: 7}}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	tr.host = host
	const readers = 32
	results := make(chan []accountSummary, readers)
	for i := 0; i < readers; i++ {
		go func() { results <- tr.accountDirectory(context.Background()) }()
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("cold refresh did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan []accountSummary, 1)
	go func() { canceled <- tr.accountDirectory(ctx) }()
	cancel()
	select {
	case got := <-canceled:
		if got != nil {
			t.Fatal("canceled cold reader received a result")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("cold reader cancellation waited for shared RPC")
	}
	unblock()
	for i := 0; i < readers; i++ {
		select {
		case got := <-results:
			if len(got) != 1 || got[0].ID != 7 {
				t.Fatalf("shared lookup returned %+v", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cold readers did not share completed refresh")
		}
	}
	if host.calls.Load() != 1 {
		t.Fatalf("cold readers dispatched %d RPCs", host.calls.Load())
	}
}

func TestAccountCacheFailurePreservesImmutableSnapshotAndCanRetry(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	var fail atomic.Bool
	host := &cacheDirectoryHost{list: func(context.Context) (*pluginv1.ListAccountsResponse, error) {
		if fail.Load() {
			return nil, context.DeadlineExceeded
		}
		return &pluginv1.ListAccountsResponse{Accounts: []*pluginv1.AccountInfo{{Id: 7, Name: "stable"}}}, nil
	}}
	tr.host = host
	first := tr.accountDirectory(context.Background())
	first[0].Name = "caller mutation"
	if got := tr.accountDirectory(context.Background()); got[0].Name != "stable" {
		t.Fatal("caller modified the cached account directory")
	}
	fail.Store(true)
	tr.accounts.mu.Lock()
	tr.accounts.expires = time.Now().Add(-time.Second)
	tr.accounts.mu.Unlock()
	if got := tr.accountDirectory(context.Background()); len(got) != 1 || got[0].Name != "stable" {
		t.Fatal("expired cache did not preserve its successful snapshot")
	}
	waitAccountRefresh(t, tr)
	for i := 0; i < 32; i++ {
		if got := tr.accountDirectory(context.Background()); len(got) != 1 || got[0].Name != "stable" {
			t.Fatal("failed refresh cleared the account directory")
		}
	}
	if host.calls.Load() != 2 {
		t.Fatalf("failed stale refresh caused %d RPCs", host.calls.Load())
	}
	fail.Store(false)
	tr.accounts.mu.Lock()
	tr.accounts.expires = time.Now().Add(-time.Second)
	tr.accounts.mu.Unlock()
	tr.accountDirectory(context.Background())
	waitAccountRefresh(t, tr)
	if host.calls.Load() != 3 {
		t.Fatal("failure backoff prevented a later refresh")
	}
}

func TestAccountCacheShutdownCancelsRefreshAndWakesColdReaders(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	entered, canceled := make(chan struct{}), make(chan struct{})
	host := &cacheDirectoryHost{list: func(ctx context.Context) (*pluginv1.ListAccountsResponse, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	}}
	tr.host = host
	finished := make(chan []accountSummary, 1)
	go func() { finished <- tr.accountDirectory(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("cold refresh did not start")
	}
	tr.Shutdown()
	select {
	case got := <-finished:
		if len(got) != 0 {
			t.Fatal("stopped transport retained account results")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("shutdown did not wake cold reader")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel account RPC")
	}
	tr.accountDirectory(context.Background())
	if host.calls.Load() != 1 {
		t.Fatal("shutdown triggered another account RPC")
	}
}

func TestAccountCacheRebindRejectsLateOldHostResult(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	oldCtx, oldCancel := context.WithCancel(context.Background())
	old := &accountRefresh{done: make(chan struct{}), cancel: oldCancel}
	tr.accounts.items = []accountSummary{{ID: 7}}
	tr.accounts.refresh = old
	newHost := &cacheDirectoryHost{list: func(context.Context) (*pluginv1.ListAccountsResponse, error) {
		return &pluginv1.ListAccountsResponse{Accounts: []*pluginv1.AccountInfo{{Id: 9}}}, nil
	}}
	// This is the same critical section used by InitHostServices after dialing.
	tr.mu.Lock()
	tr.host = newHost
	tr.accounts.invalidate()
	tr.mu.Unlock()
	select {
	case <-old.done:
	default:
		t.Fatal("host rebind did not wake old-generation waiters")
	}
	if oldCtx.Err() == nil {
		t.Fatal("host rebind did not cancel old lookup")
	}
	if got := tr.accountDirectory(context.Background()); len(got) != 1 || got[0].ID != 9 {
		t.Fatalf("host rebind reused old directory: %+v", got)
	}
	oldHost := &cacheDirectoryHost{list: func(context.Context) (*pluginv1.ListAccountsResponse, error) {
		return &pluginv1.ListAccountsResponse{Accounts: []*pluginv1.AccountInfo{{Id: 7}}}, nil
	}}
	// A response racing cancellation may still arrive successfully. Its
	// detached refresh token must not overwrite the replacement directory.
	tr.refreshAccountDirectory(context.Background(), oldHost, old)
	if got := tr.accountDirectory(context.Background()); len(got) != 1 || got[0].ID != 9 || newHost.calls.Load() != 1 {
		t.Fatalf("late old-host response repopulated the cache: %+v", got)
	}
}
