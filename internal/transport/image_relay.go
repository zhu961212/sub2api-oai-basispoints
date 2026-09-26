package transport

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func relayScope(start *pluginv1.ForwardRequestStart) string {
	// The host isolates conversation_id by API key. Its optional account
	// fingerprint can overwrite session_id afterwards with an account-wide
	// UUID, so session_id (and client aliases) cannot identify an image tenant.
	scope := ""
	for key, values := range start.GetHeaders() {
		if strings.EqualFold(key, "conversation_id") {
			for _, value := range values.GetValues() {
				if value = strings.TrimSpace(value); value != "" {
					scope = value
					break
				}
			}
			break
		}
	}
	if scope == "" {
		// Empty scope explicitly disables persistent cache entries; per-request
		// duplicates are still coalesced by the uploader. Random scopes would
		// fill the shared cache with entries that can never be reused.
		return ""
	}
	return fmt.Sprintf("account:%d/session:%s", start.GetAccountId(), scope)
}

func sendImageRelayError(stream pluginv1.TransportPlugin_ForwardServer, err error) error {
	status, code, kind := http.StatusBadRequest, "invalid_image", "invalid_request_error"
	var api interface {
		error
		StatusCode() int
		Code() string
	}
	if errors.As(err, &api) {
		status, code = api.StatusCode(), api.Code()
		if status >= 500 {
			kind = "server_error"
		}
	}
	if status == http.StatusTooManyRequests {
		return sendBasisPointsRateLimit(stream)
	}
	headers := make(http.Header)
	if status == http.StatusServiceUnavailable {
		headers.Set("Retry-After", "1")
	}
	body := protocol.JSONBytes(map[string]any{"error": map[string]any{"message": err.Error(), "type": kind, "code": code, "param": nil}})
	return sendHTTPResponse(stream, &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: headers}, body, "application/json")
}

// Upstream validators can echo the temporary URL in a message. URLs are
// bearer capabilities and must not be returned to client logs as diagnostics.
var relayErrorToken = regexp.MustCompile("/api/bps-images/[A-Za-z0-9_-]+")
var attachmentErrorToken = regexp.MustCompile("file-[A-Za-z0-9_-]+")

func redactImageDiagnostic(text string) string {
	return attachmentErrorToken.ReplaceAllString(relayErrorToken.ReplaceAllString(text, "/api/bps-images/[redacted]"), "file-[redacted]")
}

func redactImageErrorValues(value any) {
	switch object := value.(type) {
	case map[string]any:
		for key, child := range object {
			if text, ok := child.(string); ok {
				object[key] = redactImageDiagnostic(text)
			} else {
				redactImageErrorValues(child)
			}
		}
	case []any:
		for index, child := range object {
			if text, ok := child.(string); ok {
				object[index] = redactImageDiagnostic(text)
			} else {
				redactImageErrorValues(child)
			}
		}
	}
}

func sendImageUpstreamError(stream pluginv1.TransportPlugin_ForwardServer, resp *http.Response, max int) error {
	body, err := readLimited(resp.Body, max)
	if err != nil {
		return sendError(stream, "upstream_read", safeError(err), true)
	}
	message := redactImageDiagnostic(protocol.ErrorMessage(body))
	encoded := protocol.JSONBytes(map[string]any{"error": map[string]any{"message": message, "type": "upstream_error", "code": "upstream_error"}})
	return sendHTTPResponse(stream, resp, encoded, "application/json")
}

const maxImageRequestBytes = 64 << 20
const imageRequestBudget = 512 << 20

// Reserve for typed inline-image requests before uploads and conversion.
// The body has already been received under the general request limit. This
// bounds admitted image work, not total process RSS or initial JSON buffers.
type imageRequestAdmission struct {
	mu       sync.Mutex
	requests int
	bytes    int64
}

func (a *imageRequestAdmission) acquire(length int64) (func(), error) {
	if length <= 0 {
		length = maxImageRequestBytes
	}
	if length > maxImageRequestBytes {
		return nil, &protocol.APIError{Status: http.StatusRequestEntityTooLarge, Kind: "request_too_large", Message: "image request body exceeds 64 MiB"}
	}
	if length < 1<<20 {
		length = 1 << 20
	}
	weight := length * 8
	a.mu.Lock()
	if a.requests >= 32 || a.bytes+weight > imageRequestBudget {
		a.mu.Unlock()
		return nil, &protocol.APIError{Status: http.StatusServiceUnavailable, Kind: "image_request_busy", Message: "image request budget is busy; retry later"}
	}
	a.requests++
	a.bytes += weight
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.requests--
		a.bytes -= weight
		a.mu.Unlock()
	}, nil
}

// hasInlineImages visits only typed message/tool-result image blocks. Text and
// function arguments are not interpreted as attachments.
func hasInlineImages(source map[string]any) bool {
	input, _ := source["input"].([]any)
	for _, value := range input {
		item, _ := value.(map[string]any)
		field := "content"
		if item["type"] == "function_call_output" || item["type"] == "custom_tool_call_output" {
			field = "output"
		}
		parts, _ := item[field].([]any)
		for _, value := range parts {
			part, _ := value.(map[string]any)
			if part["type"] != "input_image" {
				continue
			}
			raw, _ := part["image_url"].(string)
			if len(raw) >= 5 && strings.EqualFold(raw[:5], "data:") {
				return true
			}
		}
	}
	return false
}
