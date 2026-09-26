package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestInlineImageFailureDiagnosticsDoNotExposeScreenshot(t *testing.T) {
	_, dataURL := relayTestImage(t)
	for _, event := range []string{"error", "response.failed", "response.incomplete"} {
		payload := map[string]any{"type": event, "message": "invalid screenshot: " + dataURL, "error": map[string]any{"message": dataURL}}
		normalizeRelayFailure(event, payload)
		encoded := string(protocol.JSONBytes(payload))
		if strings.Contains(encoded, dataURL) || !strings.Contains(encoded, "data:image/[redacted]") || !strings.Contains(encoded, "invalid screenshot") {
			t.Fatalf("inline screenshot escaped %s diagnostics", event)
		}
	}
	output := []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": dataURL}}}}
	raw := protocol.JSONBytes(map[string]any{"status": "failed", "error": map[string]any{"message": dataURL}, "output": output})
	redacted, changed := redactImageFailureJSON(raw, "")
	if !changed || strings.Count(string(redacted), dataURL) != 1 {
		t.Fatal("image redaction changed ordinary output or left the diagnostic unredacted")
	}
}

func TestInlineToolImageUpstream422RedactsImageWithoutRetry(t *testing.T) {
	_, dataURL := relayTestImage(t)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/responses" {
			t.Error("tool screenshot unexpectedly uploaded")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write(protocol.JSONBytes(map[string]any{"error": map[string]any{"message": "invalid screenshot: " + dataURL}}))
	}))
	defer upstream.Close()
	transport := New()
	defer transport.Shutdown()
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL + "/responses"})
	body := protocol.JSONBytes(map[string]any{"model": protocol.DefaultModelID, "input": []any{
		map[string]any{"type": "function_call", "name": "screenshot", "call_id": "call_image_redaction", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call_image_redaction", "output": []any{map[string]any{"type": "input_image", "image_url": dataURL}}},
	}})
	result := runForward(t, transport, requestFrames(t, upstream.URL, token(t, "acct-tool-image"), nil, body))
	if result.errFrame != nil || result.status != http.StatusUnprocessableEntity || !result.ended || calls != 1 {
		t.Fatalf("unexpected failure framing or retry: status=%d calls=%d", result.status, calls)
	}
	if strings.Contains(string(result.body), dataURL) || !strings.Contains(string(result.body), "data:image/[redacted]") {
		t.Fatal("upstream validation exposed tool screenshot bytes")
	}
}

func TestImageScopeDoesNotTrustAccountFingerprintSession(t *testing.T) {
	start := &pluginv1.ForwardRequestStart{AccountId: 7, Headers: map[string]*pluginv1.HeaderValues{
		"session_id": {Values: []string{"shared-account-fingerprint"}},
	}}
	first, second := relayScope(start), relayScope(start)
	if first != "" || second != "" {
		t.Fatal("account fingerprint was trusted for image deduplication")
	}
	start.Headers["conversation_id"] = &pluginv1.HeaderValues{Values: []string{"host-isolated-api-key-one"}}
	first = relayScope(start)
	if first != relayScope(start) {
		t.Fatal("isolated conversation cannot reuse its own image")
	}
	start.Headers["conversation_id"].Values[0] = "host-isolated-api-key-two"
	if first == relayScope(start) {
		t.Fatal("different API keys shared an image scope")
	}
}

func TestImageTokensDoNotEscapeTerminalFailure(t *testing.T) {
	const token = "PRIVATE_DOWNLOAD_TOKEN"
	message := "download failed: https://images.example/api/bps-images/" + token
	for _, event := range []string{"error", "response.failed", "response.incomplete"} {
		payload := map[string]any{
			"type": event, "message": message,
			"error":    map[string]any{"code": "download_failed", "message": message},
			"response": map[string]any{"id": "resp_image", "error": map[string]any{"message": message}, "output": []any{}},
		}
		normalizeRelayFailure(event, payload)
		encoded := string(protocol.JSONBytes(payload))
		if strings.Contains(encoded, token) || !strings.Contains(encoded, "[redacted]") || !strings.Contains(encoded, "download_failed") {
			t.Fatalf("failed to redact %s diagnostics", event)
		}
	}
}
