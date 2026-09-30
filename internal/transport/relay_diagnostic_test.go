package transport

import (
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestMalformedRelayStreamIncludesSafeDiagnosticReason(t *testing.T) {
	const message = "Basis Points returned a malformed or ambiguous client tool relay envelope"
	encode := func(code any) string { return string(protocol.JSONBytes(map[string]any{"code": code})) }
	for _, test := range []struct{ name, arguments, want string }{
		{"outer", "PRIVATE invalid outer arguments", "relay_outer_arguments"},
		{"code", encode("PRIVATE invalid code"), "relay_code_envelope"},
		{"nested", encode(map[string]any{"tool": "run_officejs", "args": map[string]any{"code": "PRIVATE invalid nested code"}}), "relay_nested_envelope"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, attempts := runOuterSyntaxRelay(t, test.arguments, true, true)
			if attempts != 1 || result.errFrame != nil || !result.ended {
				t.Fatalf("failure lifecycle changed: attempts=%d result=%+v", attempts, result)
			}
			events := parsedStreamEvents(t, result)
			if len(events) != 2 || events[1]["type"] != "response.failed" {
				t.Fatalf("unexpected failure events: %#v", events)
			}
			failure := relayObject(relayObject(events[1]["response"])["error"])
			if failure["reason"] != test.want || failure["code"] != "invalid_tool_call" || failure["message"] != message || failure["type"] != "invalid_request_error" {
				t.Fatalf("failure diagnostic changed: %#v", failure)
			}
			if strings.Contains(string(result.body), "PRIVATE") || strings.Count(string(result.body), "data: [DONE]") != 1 {
				t.Fatal("failure leaked source or terminated incorrectly")
			}
			assertNoToolExecutionOnStreamError(t, result)
			buffered, attempts := runOuterSyntaxRelay(t, test.arguments, false, false)
			if attempts != 1 || buffered.errFrame == nil || buffered.errFrame.GetCode() != "invalid_tool_call" || buffered.errFrame.GetMessage() != message || !buffered.errFrame.GetRequestSent() {
				t.Fatalf("buffered contract changed: %+v", buffered)
			}
		})
	}
}

func TestCanonicalHTTPToolFailureKeepsOnlyKnownDiagnosticReasons(t *testing.T) {
	for _, reason := range []any{"relay_outer_arguments", "relay_code_envelope", "relay_nested_envelope", "relay_tool_identity", "PRIVATE SOURCE", map[string]any{"private": "PRIVATE"}} {
		payload := httpFailureTestPayload(httpFailureTestMessage)
		relayObject(relayObject(payload["response"])["error"])["reason"] = reason
		result := canonicalHTTPToolFailure(payload, "response.failed")
		if result == nil {
			t.Fatal("known tool failure lost canonicalization")
		}
		failure := relayObject(result["error"])
		want := protocol.ClientToolDiagnosticReason(protocol.StringValue(reason))
		if protocol.StringValue(failure["reason"]) != want || strings.Contains(string(protocol.JSONBytes(result)), "PRIVATE") {
			t.Fatalf("unsafe or missing diagnostic: %#v", result)
		}
		if want == "" {
			if _, exists := failure["reason"]; exists {
				t.Fatal("unknown reason should be omitted")
			}
		}
	}
}
