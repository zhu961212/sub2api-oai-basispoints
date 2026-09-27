package protocol

import (
	"strings"
	"testing"
)

func TestTerminalFailureDoesNotCopySSETypeIntoError(t *testing.T) {
	for _, event := range []string{"response.failed", "response.created", "error"} {
		for _, errorType := range []string{"", "server_error"} {
			failure := map[string]any{"code": "server_error", "message": "temporary failure"}
			if errorType != "" {
				failure["type"] = errorType
			}
			payload := map[string]any{"type": event, "response": map[string]any{"status": "failed", "error": failure}}
			NormalizeResponseFailure(payload, TerminalFailed)
			got := objectValue(objectValue(payload["response"])["error"])
			if stringValue(got["type"]) != errorType {
				t.Fatalf("SSE kind became an error type: %#v", got)
			}
		}
	}
}

func TestTerminalToolFailureRetainsOnlySafeDiagnostic(t *testing.T) {
	for _, message := range []string{malformedClientToolMessage, unknownClientToolMessage, "PRIVATE C SOURCE AND TOKEN"} {
		payload := map[string]any{"response": map[string]any{"status": "failed", "error": map[string]any{"code": "invalid_tool_call", "message": message}}}
		err := ResponseTerminalError(TerminalFailed, payload)
		api, ok := err.(*APIError)
		if !ok || api.Kind != "invalid_tool_call" {
			t.Fatalf("lost tool error kind: %v", err)
		}
		if strings.HasPrefix(message, "PRIVATE") {
			if strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("arbitrary diagnostic leaked through RPC")
			}
		} else if err.Error() != message {
			t.Fatalf("safe diagnostic lost: %v", err)
		}
	}
}

func TestNestedJSONFailureRetainsDiagnosticAndWithholdsTools(t *testing.T) {
	for _, location := range []string{"nested", "outer", "direct"} {
		t.Run(location, func(t *testing.T) {
			failure := map[string]any{"code": "invalid_tool_call", "message": malformedClientToolMessage}
			response := map[string]any{"id": "resp_failed_json", "status": "completed", "output": []any{
				map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "never_execute", "input": "PRIVATE SOURCE"},
			}}
			envelope := map[string]any{"type": "response.failed", "response": response}
			switch location {
			case "nested":
				response["error"] = failure
			case "outer":
				envelope["error"] = failure
			case "direct":
				envelope["code"], envelope["message"] = failure["code"], failure["message"]
			}
			raw, translated, _, err := TransformResponseBody(JSONBytes(envelope), map[string]any{})
			if err != nil {
				t.Fatal(err)
			}
			got := objectValue(translated["error"])
			if translated["id"] != "resp_failed_json" || translated["status"] != "failed" || got["code"] != "invalid_tool_call" || got["type"] != "invalid_request_error" || got["message"] != malformedClientToolMessage {
				t.Fatalf("failed envelope lost details: %s", raw)
			}
			if strings.Contains(string(raw), "never_execute") || strings.Contains(string(SyntheticStream(translated)), "PRIVATE SOURCE") {
				t.Fatal("failed response exposed callable output")
			}
		})
	}
}
