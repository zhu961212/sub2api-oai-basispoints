package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/config"
)

const (
	// Version / PluginID / Capability 必须与 manifest.json 保持一致。
	Version    = config.Version
	PluginID   = config.PluginID
	Capability = config.Capability

	DefaultResponsesURL  = config.DefaultResponsesURL
	DefaultUpstreamModel = config.DefaultUpstreamModel
	DefaultModelID       = config.DefaultModelID
)

// Config 是配置包中定义的结构别名，便于协议层直接使用而不重复定义。
type Config = config.Config

// DefaultConfig 返回默认配置。
func DefaultConfig() Config { return config.Default() }

// AvailableModels 返回配置页可选的 Basis Points 模型目录。
func AvailableModels() []string { return config.AvailableModels() }

// ParseConfig 严格解析配置，并把解析错误包装成对宿主安全的 APIError。
// 详细字段、默认值与范围校验见 internal/config。
func ParseConfig(raw []byte) (Config, error) {
	c, err := config.Parse(raw)
	if err != nil {
		return Config{}, fail(http.StatusBadRequest, "invalid_config", err.Error())
	}
	return c, nil
}

// APIError 是故意设计成可以安全返回给宿主的错误类型。
// 它从不携带请求体、Bearer Token、代理地址或上游校验输入。
type APIError struct {
	Status  int
	Kind    string
	Message string
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *APIError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.Status
}

func (e *APIError) Code() string {
	if e == nil || e.Kind == "" {
		return "plugin_error"
	}
	return e.Kind
}

func fail(status int, kind, message string) error {
	return &APIError{Status: status, Kind: kind, Message: message}
}

// NormalizeEffort 与 config.NormalizeEffort 同义，保留给协议层调用。
func NormalizeEffort(value any) string { return config.NormalizeEffort(value) }

func normalizeEffort(value any) string { return config.NormalizeEffort(value) }

// IsResponseModel 判断模型是否由本插件对外提供。
func IsResponseModel(model string, cfg Config) bool { return config.SupportsModel(model, cfg) }

func isResponseModel(model string, cfg Config) bool { return config.SupportsModel(model, cfg) }

// HandlesModel 判断该模型是否由本插件接管（详见 config.HandlesModel）。
func HandlesModel(model string, cfg Config) bool { return cfg.HandlesModel(model) }

// RequestedModel 从请求体里取顶层 model 字段。无法解析出字符串时返回空串，
// 调用方应据此按"不由本插件接管"处理（原样透传）。
func RequestedModel(body []byte) string {
	// Match the protocol's exact JSON key. A struct would also accept MODEL,
	// allowing a second case-variant field to change the routing decision.
	var probe map[string]json.RawMessage
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	var model string
	if json.Unmarshal(probe["model"], &model) != nil {
		return ""
	}
	return strings.TrimSpace(model)
}

// SupportedReasoningEfforts 返回上游可用的思考等级。
func SupportedReasoningEfforts() []string { return config.SupportedReasoningEfforts() }

// RawObject 把请求体解析为 JSON 对象，数字保留原始字面量。
func RawObject(raw []byte) (map[string]any, error) {
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, fail(http.StatusBadRequest, "invalid_request", "request body must be a JSON object")
	}
	return object, nil
}

func rawObject(raw []byte) (map[string]any, error) { return RawObject(raw) }

// JSONBytes 序列化为 JSON；协议内所有结构都保证可序列化。
func JSONBytes(value any) []byte { data, _ := json.Marshal(value); return data }

func jsonBytes(value any) []byte { return JSONBytes(value) }

// StringValue 取出字符串并去除首尾空白。
func StringValue(value any) string {
	s, _ := value.(string)
	return strings.TrimSpace(s)
}

func stringValue(value any) string { return StringValue(value) }

// ErrorMessage 从上游错误体中提取可以安全展示的原因。
// 校验错误只保留字段路径与原因，避免把 input 中的私有内容写进日志或错误消息。
func ErrorMessage(body []byte) string {
	var object map[string]any
	if json.Unmarshal(body, &object) == nil {
		if details, ok := object["detail"].([]any); ok && len(details) > 0 {
			safe := make([]map[string]any, 0, len(details))
			for _, value := range details {
				if entry, ok := value.(map[string]any); ok {
					safe = append(safe, map[string]any{"loc": entry["loc"], "msg": entry["msg"], "type": entry["type"]})
				}
			}
			if len(safe) > 0 {
				return string(JSONBytes(map[string]any{"detail": safe}))
			}
		}
		if detail := StringValue(object["detail"]); detail != "" {
			return detail
		}
		if nested, ok := object["error"].(map[string]any); ok {
			if message := StringValue(nested["message"]); message != "" {
				return message
			}
		}
		if message := StringValue(object["message"]); message != "" {
			return message
		}
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "Basis Points upstream request failed"
	}
	if len(text) > 500 {
		text = text[:500]
	}
	return text
}

func errorMessage(body []byte) string { return ErrorMessage(body) }

func timeoutError(c Config) error {
	return fail(http.StatusGatewayTimeout, "upstream_timeout",
		fmt.Sprintf("Basis Points request timed out after %d seconds", c.TimeoutSeconds))
}
