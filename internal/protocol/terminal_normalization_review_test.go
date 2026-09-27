package protocol

import (
	"strings"
	"testing"
)

func TestToolFailureNormalizationLeavesUnrelatedResponsesUntouched(t *testing.T) {
	for _, payload := range []map[string]any{
		{"type": "response.failed", "error": map[string]any{"code": "server_error", "type": "server_error", "message": "invalid_tool_call in diagnostic"}},
		{"response": map[string]any{"status": "failed", "error": map[string]any{"code": "server_error", "message": "temporary failure"}}},
		{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "code": "invalid_tool_call", "error": map[string]any{"code": "invalid_tool_call"}}}}},
		{"type": "response.function_call_arguments.delta", "delta": `{"error":{"code":"invalid_tool_call"}}`},
		{"status": "completed", "code": "invalid_tool_call", "message": "ordinary metadata"},
	} {
		before := string(jsonBytes(payload))
		if NormalizeClientToolFailure(payload, "") || string(jsonBytes(payload)) != before {
			t.Fatalf("unrelated response rewritten: %s", before)
		}
	}
}

func TestToolFailureNormalizationKeepsAccountIsolationPriority(t *testing.T) {
	for _, isolationOuter := range []bool{false, true} {
		tool := map[string]any{"code": "invalid_tool_call", "message": "PRIVATE SOURCE printf(SECRET)"}
		isolated := map[string]any{"code": "bps_service_rejected", "type": "invalid_request_error", "message": "Basis Points HTTP 403 rejection"}
		payload := map[string]any{"type": "response.failed", "error": tool, "response": map[string]any{"status": "failed", "error": isolated}}
		if isolationOuter {
			payload["error"] = isolated
			objectValue(payload["response"])["error"] = tool
		}
		NormalizeClientToolFailure(payload, "response.failed")
		err := ResponseTerminalError(ClassifyResponseTerminal("response.failed", payload), payload)
		api, ok := err.(*APIError)
		if !ok || api.Kind != "bps_service_rejected" || api.Message != isolated["message"] {
			t.Fatalf("RPC isolation priority changed: %v", err)
		}
		NormalizeResponseFailure(payload, TerminalFailed)
		got := objectValue(objectValue(payload["response"])["error"])
		if !jsonValuesEqual(got, isolated) {
			t.Fatalf("stream isolation diagnostic changed: %#v", got)
		}
	}
}

func TestToolFailureRPCRejectsDiagnosticPrefixesAndSuffixes(t *testing.T) {
	for _, message := range []string{"PRIVATE source", malformedClientToolMessage + " PRIVATE source", "PRIVATE source " + unknownClientToolMessage} {
		for _, nested := range []bool{false, true} {
			failure := map[string]any{"code": "invalid_tool_call", "message": message, "details": "PRIVATE source"}
			payload := map[string]any{"status": "failed", "error": failure}
			if nested {
				payload = map[string]any{"type": "response.failed", "response": payload}
			}
			NormalizeClientToolFailure(payload, "response.failed")
			err := ResponseTerminalError(TerminalFailed, payload)
			api, ok := err.(*APIError)
			if !ok || api.Kind != "invalid_tool_call" || strings.Contains(api.Message, "PRIVATE") {
				t.Fatalf("RPC exposed untrusted diagnostic: %v", err)
			}
			before := string(jsonBytes(payload))
			if NormalizeClientToolFailure(payload, "response.failed") || string(jsonBytes(payload)) != before {
				t.Fatal("normalization is not idempotent")
			}
		}
	}
}
