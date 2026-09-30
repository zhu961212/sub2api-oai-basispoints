package protocol

import (
	"errors"
	"strings"
	"testing"
)

func TestMalformedRelayDiagnosticPreservesRejectionSemantics(t *testing.T) {
	outer := relayEnvelopeNative(nil)
	outer["arguments"] = "PRIVATE invalid outer arguments"
	nested := relayEnvelopeNative(map[string]any{"tool": transportName, "args": map[string]any{"code": "PRIVATE invalid nested code"}})
	for _, test := range []struct {
		name   string
		native map[string]any
		want   string
	}{
		{"outer", outer, "relay_outer_arguments"},
		{"code", relayEnvelopeNative("PRIVATE invalid code"), "relay_code_envelope"},
		{"nested", nested, "relay_nested_envelope"},
		{"identity", map[string]any{"type": "function_call", "name": ""}, "relay_tool_identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := repairTestSource()
			response := map[string]any{"status": "completed", "output": []any{test.native}}
			before := ToolRepairEligible(source, response)
			_, _, _, err := transformResponseBody(jsonBytes(response), source)
			var api *APIError
			if !errors.As(err, &api) || api.Code() != "invalid_tool_call" || api.Error() != malformedClientToolMessage || api.DiagnosticReason() != test.want {
				t.Fatalf("unexpected safe diagnostic: %#v", err)
			}
			if !IsRepairableClientToolError(err) || before != ToolRepairEligible(source, response) || strings.Contains(api.Error()+api.DiagnosticReason(), "PRIVATE") {
				t.Fatal("diagnostics changed repair semantics or exposed arguments")
			}
		})
	}
}

func TestClientToolDiagnosticAllowlistAcrossTerminalShapes(t *testing.T) {
	for _, reason := range []any{"relay_outer_arguments", "relay_code_envelope", "relay_nested_envelope", "relay_tool_identity", "PRIVATE SOURCE", nil, map[string]any{"source": "PRIVATE"}} {
		for _, shape := range []string{"nested", "outer", "direct"} {
			failure := map[string]any{"code": "invalid_tool_call", "message": malformedClientToolMessage, "reason": reason}
			payload := map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "output": []any{}}}
			switch shape {
			case "nested":
				objectValue(payload["response"])["error"] = failure
			case "outer":
				payload["error"] = failure
			case "direct":
				payload["code"], payload["message"], payload["reason"] = failure["code"], failure["message"], reason
			}
			NormalizeClientToolFailure(payload, "response.failed")
			NormalizeResponseFailure(payload, TerminalFailed)
			api := ResponseTerminalError(TerminalFailed, payload).(*APIError)
			want := ClientToolDiagnosticReason(stringValue(reason))
			if api.DiagnosticReason() != want || api.Error() != malformedClientToolMessage {
				t.Fatalf("%s: reason=%#v diagnostic=%#v", shape, reason, api)
			}
			normalized := objectValue(objectValue(payload["response"])["error"])
			if got := stringValue(normalized["reason"]); got != want {
				t.Fatalf("%s: unsafe reason retained or safe stage lost: %#v", shape, normalized)
			}
			if want == "" {
				if _, exists := normalized["reason"]; exists {
					t.Fatal("unknown diagnostic should be omitted")
				}
			}
		}
	}
	if got := (&APIError{Kind: "server_error", DiagnosticCode: "relay_code_envelope"}).DiagnosticReason(); got != "" {
		t.Fatal("non-tool error acquired relay diagnostic")
	}
}
