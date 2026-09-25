// Package config 定义插件身份常量与配置结构。
//
// 配置由插件自己拥有、由 Sub2API 加密保存：宿主只负责把 JSON 原样交给
// ValidateConfig / ApplyConfig，因此这里必须自行完成严格解析、默认值补全和
// 范围校验，并在空对象时产出一份完整的默认配置。
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
)

const (
	// Version 是插件自身版本，必须与 manifest.json 的 version 完全一致。
	Version = "0.5.17"
	// PluginID 必须与 manifest.json 的 id 完全一致。
	PluginID = "local.oai-basispoints"
	// Capability 是宿主当前唯一接受的传输能力标识。
	Capability = "openai.oauth.outbound_transport.v1"

	// DefaultResponsesURL 是 Basis Points 的 Responses 端点（Excel 网关）。
	DefaultResponsesURL = "https://bps.openai.com/basispoints/api/responses"
	// DefaultModelID 是插件唯一接管和对外提供的模型。
	DefaultModelID = "gpt-6-astra"
	// DefaultUpstreamModel 固定为同一个模型，旧配置不能覆盖。
	DefaultUpstreamModel = DefaultModelID

	// DefaultTimeoutSeconds 是单次上游请求的超时。
	DefaultTimeoutSeconds = 300
	// DefaultMaxResponseBytes 是上游响应体上限。
	DefaultMaxResponseBytes = 64 << 20
	// DefaultAuthMode 是 Basis Points 的鉴权模式。
	DefaultAuthMode = "chatgpt"
)

// defaultModels 与实际接管范围一致，不再提供旧版 Excel 别名。
var defaultModels = []string{DefaultModelID}

// defaultModelMap 保留空对象以兼容旧配置的字段形状。
var defaultModelMap = map[string]string{}

// supportedReasoningEfforts 是上游实际接受的思考等级。
// Basis Points 的 max/ultra 请求统一使用 xhigh，none/minimal 使用 low。
var supportedReasoningEfforts = map[string]struct{}{
	"low": {}, "medium": {}, "high": {}, "xhigh": {},
}

// Config 是插件的 JSON 配置。字段名统一使用 snake_case。
type Config struct {
	ResponsesURL  string   `json:"responses_url"`
	UpstreamModel string   `json:"upstream_model"`
	Models        []string `json:"models"`
	// ModelMap 仅为兼容旧配置保留；Normalize 会清空映射。
	ModelMap       map[string]string `json:"model_map"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	// AccountIDs 限定使用这些账号（在它们之间轮询）。为空表示不限制，
	// 完全跟随宿主调度。
	AccountIDs         []int64 `json:"account_ids"`
	MaxResponseBytes   int     `json:"max_response_bytes"`
	AuthMode           string  `json:"auth_mode"`
	ToolsVersionID     string  `json:"tools_version_id,omitempty"`
	RewriteTools       bool    `json:"rewrite_tools"`
	TransformResponses bool    `json:"transform_responses"`
	// Deprecated image settings are accepted only to migrate old saved configurations.
	ImageRelayEnabled    bool   `json:"image_relay_enabled,omitempty"`
	ImageRelayPublicURL  string `json:"image_relay_public_url,omitempty"`
	ImageRelayListen     string `json:"image_relay_listen,omitempty"`
	ImageRelayStorageDir string `json:"image_relay_storage_dir,omitempty"`
}

// Default 返回一份完整可用的默认配置。宿主极少提交空对象，但空对象必须
// 能规范化为完整配置，而不是解析失败。
func Default() Config {
	return Config{
		ResponsesURL:       DefaultResponsesURL,
		UpstreamModel:      DefaultUpstreamModel,
		Models:             append([]string(nil), defaultModels...),
		ModelMap:           cloneStringMap(defaultModelMap),
		TimeoutSeconds:     DefaultTimeoutSeconds,
		MaxResponseBytes:   DefaultMaxResponseBytes,
		AuthMode:           DefaultAuthMode,
		RewriteTools:       true,
		TransformResponses: true,
	}
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
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
	if len(strings.TrimSpace(string(raw))) != 0 {
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
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
	c.UpstreamModel = DefaultUpstreamModel
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
	c.AccountIDs = normalizeAccountIDs(c.AccountIDs)
	c.ToolsVersionID = strings.TrimSpace(c.ToolsVersionID)
	// Native attachments need no listener, storage directory or public origin.
	c.ImageRelayEnabled = false
	c.ImageRelayPublicURL, c.ImageRelayListen, c.ImageRelayStorageDir = "", "", ""
	// 平滑迁移已保存的旧配置：保留账号与传输选项，只收敛模型相关字段。
	c.ModelMap = cloneStringMap(defaultModelMap)
	c.Models = append([]string(nil), defaultModels...)
	return nil
}

// Clone 深复制配置，避免调用方共享 Models / ModelMap / AccountIDs 的底层结构。
func (c Config) Clone() Config {
	c.Models = append([]string(nil), c.Models...)
	c.ModelMap = cloneStringMap(c.ModelMap)
	c.AccountIDs = append([]int64(nil), c.AccountIDs...)
	return c
}

// SelectedAccountIDs 返回生效的账号限定列表（去重、过滤非法值）。
// 为空表示不限制账号，完全跟随宿主调度。
func (c Config) SelectedAccountIDs() []int64 {
	return normalizeAccountIDs(c.AccountIDs)
}

// HandlesModel 报告某个模型名是否由本插件接管（= 请求打到 Basis Points）。
//
// 只接管 gpt-6-astra；即使传入尚未规范化的旧配置，也不能扩大接管范围。
//
// 其余模型必须原样透传回宿主的上游：宿主会用普通 Codex 模型（默认 gpt-5.4）
// 对账号做连通性测试，这类请求也会被交给插件，而 Basis Points 只服务它自己的
// 模型集合，收到 gpt-5.4 会回 403 basispoints_model_access_changed —— 宿主随即
// 把账号判成异常/限流，表现为"一启用插件账号就限流"。未知模型一律不接管。
func (c Config) HandlesModel(model string) bool {
	return strings.TrimSpace(model) == DefaultModelID
}

// UpstreamModelFor 固定返回唯一允许的 Basis Points 模型。
// 调用方先用 HandlesModel 分流，旧映射和整体覆盖不能改写上游模型。
func (c Config) UpstreamModelFor(requested string) string {
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
