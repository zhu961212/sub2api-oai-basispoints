// Package config 定义插件身份常量与配置结构。
//
// 配置由插件自己拥有、由 Sub2API 加密保存：宿主只负责把 JSON 原样交给
// ValidateConfig / ApplyConfig，因此这里必须自行完成严格解析、默认值补全和
// 范围校验，并在空对象时产出一份完整的默认配置。
package config

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/url"
	"strconv"
	"strings"
)

const (
	// Version 是插件自身版本，必须与 manifest.json 的 version 完全一致。
	Version = "0.6.1"
	// PluginID 必须与 manifest.json 的 id 完全一致。
	PluginID = "local.oai-basispoints"
	// Capability 是宿主当前唯一接受的传输能力标识。
	Capability = "openai.oauth.outbound_transport.v1"

	// DefaultResponsesURL 是 Basis Points 的 Responses 端点（Excel 网关）。
	DefaultResponsesURL = "https://bps.openai.com/basispoints/api/responses"
	// DefaultModelID 是插件默认的 Basis Points 模型。
	DefaultModelID = "gpt-6-astra"
	// SolModelID 是插件默认同时接管的 GPT-5.6 Sol 模型。
	SolModelID = "gpt-5.6-sol"
	// DefaultUpstreamModel 用于没有明确模型的内部协议准备。
	DefaultUpstreamModel = DefaultModelID

	// DefaultTimeoutSeconds 是单次上游请求的超时。
	DefaultTimeoutSeconds = 300
	// DefaultMaxResponseBytes 是上游响应体上限。
	DefaultMaxResponseBytes = 64 << 20
	// DefaultAuthMode 是 Basis Points 的鉴权模式。
	DefaultAuthMode = "chatgpt"
	// MaxDegradationCheckAccountIDs bounds one explicit diagnostic snapshot.
	MaxDegradationCheckAccountIDs = 10000
)

// defaultModels 是未指定 enabled_models 时启用的模型。
var defaultModels = []string{DefaultModelID, SolModelID}

// availableModels 是上游已验证的可选模型目录，其顺序也用于配置规范化。
var availableModels = []string{
	DefaultModelID, SolModelID, "gpt-6-sol", "gpt-6-luna", "gpt-5.6-terra", "gpt-5.6-luna",
}

// supportedReasoningEfforts 是上游实际接受的思考等级。
// Basis Points 的 max/ultra 请求统一使用 xhigh，none/minimal 使用 low。
var supportedReasoningEfforts = map[string]struct{}{
	"low": {}, "medium": {}, "high": {}, "xhigh": {},
}

// Config 是插件的 JSON 配置。字段名统一使用 snake_case。
type Config struct {
	ResponsesURL string `json:"responses_url"`
	// EnabledModels 控制模型接管；nil 表示默认双模型，非 nil 空切片表示全部关闭。
	EnabledModels  []string `json:"enabled_models"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	// AccountIDs 在旧模式下是账号白名单；为空表示不限制。插件始终使用宿主
	// 调度的账号，不参与轮询。自动模式下这里只保存界面已选账号的快照。
	AccountIDs []int64 `json:"account_ids"`
	// AutoSelectNewAccounts 允许当前及未来新增账号，只有明确取消的账号例外。
	// 缺省 false 保留旧白名单，配置页保存时迁移为自动模式。
	AutoSelectNewAccounts bool    `json:"auto_select_new_accounts,omitempty"`
	ExcludedAccountIDs    []int64 `json:"excluded_account_ids,omitempty"`
	// BPSAutoDisableOn403 controls creation of new BPS account restrictions.
	// Disabling this policy never clears an existing restriction.
	BPSAutoDisableOn403 bool `json:"bps_auto_disable_on_403"`
	// BPSDeviceConvergence opts into account-scoped device identity on BPS.
	// Default off preserves the host/client device identity until enabled.
	BPSDeviceConvergence bool `json:"bps_device_convergence"`
	// Acknowledging one block never clears a newer persisted BPS 403.
	BPSReenabledAccounts map[string]string `json:"bps_reenabled_accounts,omitempty"`
	// Diagnostic commands are temporary fields. Legacy saved configurations
	// remain parseable, but only the request-scoped TestConfig bridge executes
	// commands bound to an explicit account or caller-supplied bulk snapshot.
	DegradationCheck bool `json:"degradation_check,omitempty"`
	// DegradationCheckAccountID selects one account for a request-scoped probe.
	// It cannot authorize an account request through legacy TestConfig.
	DegradationCheckAccountID int64 `json:"degradation_check_account_id,omitempty"`
	// DegradationCheckAccountIDs freezes the bulk directory visible to the caller.
	DegradationCheckAccountIDs []int64 `json:"degradation_check_account_ids,omitempty"`
	MaxResponseBytes           int     `json:"max_response_bytes"`
	AuthMode                   string  `json:"auth_mode"`
	ToolsVersionID             string  `json:"tools_version_id,omitempty"`
	RewriteTools               bool    `json:"rewrite_tools"`
	TransformResponses         bool    `json:"transform_responses"`
}

// Default 返回一份完整可用的默认配置。宿主极少提交空对象，但空对象必须
// 能规范化为完整配置，而不是解析失败。
func Default() Config {
	return Config{
		ResponsesURL:        DefaultResponsesURL,
		EnabledModels:       cloneStrings(defaultModels),
		TimeoutSeconds:      DefaultTimeoutSeconds,
		MaxResponseBytes:    DefaultMaxResponseBytes,
		AuthMode:            DefaultAuthMode,
		RewriteTools:        true,
		TransformResponses:  true,
		BPSAutoDisableOn403: true,
	}
}

// cloneStrings 保留 nil 与显式空列表的不同选择语义。
func cloneStrings(source []string) []string {
	if source == nil {
		return nil
	}
	result := make([]string, len(source))
	copy(result, source)
	return result
}

// AvailableModels 返回可选模型目录的独立副本。
func AvailableModels() []string {
	return cloneStrings(availableModels)
}

func isAvailableModel(model string) bool {
	for _, available := range availableModels {
		if model == available {
			return true
		}
	}
	return false
}

func normalizeEnabledModels(models []string) ([]string, error) {
	if models == nil {
		return cloneStrings(defaultModels), nil
	}
	selected := make(map[string]bool, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if !isAvailableModel(model) {
			return nil, fmt.Errorf("enabled_models contains an unsupported model")
		}
		selected[model] = true
	}
	result := make([]string, 0, len(selected))
	for _, model := range availableModels {
		if selected[model] {
			result = append(result, model)
		}
	}
	return result, nil
}

// normalizeAccountIDs 去掉非法值与重复项；空结果统一为 nil（表示不限制）。
func normalizeAccountIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Parse 严格解析配置 JSON：拒绝未知字段、拒绝多对象、拒绝非法范围。
func Parse(raw []byte) (Config, error) {
	c := Default()
	input := strings.TrimSpace(string(raw))
	if len(input) != 0 {
		// Decoding JSON null into a struct succeeds without changing defaults,
		// which would silently reset saved routing instead of rejecting input.
		if input[0] != '{' {
			return Config{}, fmt.Errorf("configuration JSON must contain one object")
		}
		decoder := json.NewDecoder(strings.NewReader(input))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&c); err != nil {
			return Config{}, fmt.Errorf("configuration JSON is invalid: %w", err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return Config{}, fmt.Errorf("configuration JSON must contain one object")
		}
	}
	if err := c.Normalize(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Normalize 补全默认值并校验取值范围。它可以被重复调用（幂等）。
func (c *Config) Normalize() error {
	if c == nil {
		return fmt.Errorf("configuration is missing")
	}
	c.ResponsesURL = strings.TrimSpace(c.ResponsesURL)
	if c.ResponsesURL == "" {
		c.ResponsesURL = DefaultResponsesURL
	}
	parsed, err := url.Parse(c.ResponsesURL)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return fmt.Errorf("responses_url must be an absolute HTTP(S) URL")
	}
	c.AuthMode = strings.TrimSpace(c.AuthMode)
	if c.AuthMode == "" {
		c.AuthMode = DefaultAuthMode
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if c.TimeoutSeconds < 10 || c.TimeoutSeconds > 1800 {
		return fmt.Errorf("timeout_seconds must be between 10 and 1800")
	}
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if c.MaxResponseBytes < 64<<10 || c.MaxResponseBytes > 128<<20 {
		return fmt.Errorf("max_response_bytes must be between 64 KiB and 128 MiB")
	}
	if c.DegradationCheckAccountID < 0 {
		return fmt.Errorf("degradation_check_account_id must be a positive account ID or zero")
	}
	if c.DegradationCheckAccountID != 0 && !c.DegradationCheck {
		return fmt.Errorf("degradation_check_account_id requires degradation_check")
	}
	if c.DegradationCheckAccountIDs != nil {
		if !c.DegradationCheck {
			return fmt.Errorf("degradation_check_account_ids requires degradation_check")
		}
		if c.DegradationCheckAccountID != 0 {
			return fmt.Errorf("degradation check must select either one account or a bulk snapshot")
		}
		if len(c.DegradationCheckAccountIDs) > MaxDegradationCheckAccountIDs {
			return fmt.Errorf("degradation_check_account_ids exceeds the maximum snapshot size")
		}
		for _, id := range c.DegradationCheckAccountIDs {
			if id <= 0 {
				return fmt.Errorf("degradation_check_account_ids must contain positive account IDs")
			}
		}
		c.DegradationCheckAccountIDs = normalizeAccountIDs(c.DegradationCheckAccountIDs)
	}
	c.AccountIDs = normalizeAccountIDs(c.AccountIDs)
	c.ExcludedAccountIDs = normalizeAccountIDs(c.ExcludedAccountIDs)
	for key, blockID := range c.BPSReenabledAccounts {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != key || len(blockID) != 32 || strings.ToLower(blockID) != blockID {
			return fmt.Errorf("bps_reenabled_accounts must map positive account IDs to block IDs")
		}
		if _, err := hex.DecodeString(blockID); err != nil {
			return fmt.Errorf("bps_reenabled_accounts contains an invalid block ID")
		}
	}
	c.BPSReenabledAccounts = maps.Clone(c.BPSReenabledAccounts)
	c.ToolsVersionID = strings.TrimSpace(c.ToolsVersionID)
	models, err := normalizeEnabledModels(c.EnabledModels)
	if err != nil {
		return err
	}
	c.EnabledModels = models
	return nil
}

// Clone 深复制配置，并保留显式空模型选择。
func (c Config) Clone() Config {
	c.EnabledModels = cloneStrings(c.EnabledModels)
	c.AccountIDs = append([]int64(nil), c.AccountIDs...)
	c.ExcludedAccountIDs = append([]int64(nil), c.ExcludedAccountIDs...)
	c.DegradationCheckAccountIDs = append([]int64(nil), c.DegradationCheckAccountIDs...)
	c.BPSReenabledAccounts = maps.Clone(c.BPSReenabledAccounts)
	return c
}

// SelectedAccountIDs 返回规范化的账号选择。旧模式下为空表示不限制账号；
// 自动模式下它只是界面快照，实际路由必须使用 HandlesAccount。
func (c Config) SelectedAccountIDs() []int64 {
	return normalizeAccountIDs(c.AccountIDs)
}

// HandlesAccount 只判断当前调度账号是否使用 BPS，不读取或改写宿主账号目录。
// 自动模式下，未见过的新 ID 默认接入；排除项随配置持久化，重启后仍生效。
func (c Config) HandlesAccount(accountID int64) bool {
	if accountID == 0 {
		return true // 兼容未提供账号 ID 的旧宿主。
	}
	if c.AutoSelectNewAccounts {
		for _, excluded := range c.ExcludedAccountIDs {
			if excluded == accountID {
				return false
			}
		}
		return true
	}
	// Routing only needs membership. Normalizing here allocated a map and
	// copied the entire account list for every request. Keep invalid-only
	// legacy selections equivalent to an empty (unrestricted) selection.
	hasSelection := false
	for _, selected := range c.AccountIDs {
		if selected <= 0 {
			continue
		}
		hasSelection = true
		if selected == accountID {
			return true
		}
	}
	return !hasSelection
}

// HandlesModel 报告某个模型名是否由本插件接管（= 请求打到 Basis Points）。
//
// 只接管六个已知目录项中显式启用的模型；默认启用 Astra 与 5.6 Sol。
//
// 其余模型必须原样透传回宿主的上游：宿主会用普通 Codex 模型（默认 gpt-5.4）
// 对账号做连通性测试，这类请求也会被交给插件，而 Basis Points 只服务它自己的
// 模型集合，收到 gpt-5.4 会回 403 basispoints_model_access_changed —— 宿主随即
// 把账号判成异常/限流，表现为"一启用插件账号就限流"。未知模型一律不接管。
func (c Config) HandlesModel(model string) bool {
	model = strings.TrimSpace(model)
	if !isAvailableModel(model) {
		return false
	}
	models := c.EnabledModels
	if models == nil {
		models = defaultModels
	}
	for _, selected := range models {
		if model == strings.TrimSpace(selected) {
			return true
		}
	}
	return false
}

// UpstreamModelFor 保留受支持的请求模型名，其余输入沿用默认值。
// 调用方先用 HandlesModel 分流，选中的模型按自身名称转发。
func (c Config) UpstreamModelFor(requested string) string {
	requested = strings.TrimSpace(requested)
	if isAvailableModel(requested) {
		return requested
	}
	return DefaultUpstreamModel
}

// NormalizeEffort 把下游传入的思考等级归一化为上游可接受的值。
// 请求校验拒绝未知取值；内部默认取 medium，max/ultra 映射为 xhigh。
func NormalizeEffort(value any) string {
	s, _ := value.(string)
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "x-high", "extra-high", "extra_high", "max", "ultra":
		s = "xhigh"
	case "none", "minimal":
		s = "low"
	}
	if _, ok := supportedReasoningEfforts[s]; ok {
		return s
	}
	return "medium"
}

// SupportsModel 判断模型是否由本插件对外提供。
func SupportsModel(model string, cfg Config) bool {
	return cfg.HandlesModel(model)
}

// SupportedReasoningEfforts 返回上游可用的思考等级，供状态面板展示。
func SupportedReasoningEfforts() []string {
	return []string{"low", "medium", "high", "xhigh"}
}
