package transport

import (
	"strings"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

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
