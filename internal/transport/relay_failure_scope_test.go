package transport

import (
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestInvalidRelayStreamFailureIsRequestScoped(t *testing.T) {
	const private = "PRIVATE INVALID TOOL INPUT"
	for _, test := range []struct {
		name     string
		envelope map[string]any
	}{
		{"ambiguous envelope", map[string]any{"tool": "functions.exec", "args": private, "input": private}},
		{"invalid custom input", map[string]any{"tool": "functions.exec", "args": map[string]any{"input": private}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			arguments := string(protocol.JSONBytes(map[string]any{"code": string(protocol.JSONBytes(test.envelope))}))
			result, attempts := runOuterSyntaxRelay(t, arguments, true, true)
			if attempts != 1 || result.errFrame != nil || !result.ended {
				t.Fatalf("invalid relay was retried or lost its stream terminal: attempts=%d result=%+v", attempts, result)
			}
			events := parsedStreamEvents(t, result)
			if len(events) != 2 || events[1]["type"] != "response.failed" {
				t.Fatalf("expected the original created event and one failure: %#v", events)
			}
			failure := relayObject(relayObject(events[1]["response"])["error"])
			if failure["code"] != "invalid_tool_call" || failure["type"] != "invalid_request_error" {
				t.Fatalf("relay failure can be mistaken for a retryable upstream outage: %#v", failure)
			}
			if !strings.HasPrefix(protocol.StringValue(failure["message"]), "Basis Points returned") || strings.Contains(string(result.body), private) {
				t.Fatalf("original safe diagnostic was lost or private input leaked: %#v", failure)
			}
			assertNoToolExecutionOnStreamError(t, result)
			if strings.Count(string(result.body), "data: [DONE]") != 1 || result.received != int64(len(result.body)) {
				t.Fatal("failure stream did not terminate exactly once")
			}
			// Before any response bytes, the RPC contract already prevents
			// replay with RequestSent. Preserve that path and its diagnostic.
			for _, stream := range []bool{false, true} {
				buffered, attempts := runOuterSyntaxRelay(t, arguments, stream, false)
				if attempts != 1 || buffered.errFrame == nil || !buffered.errFrame.GetRequestSent() || buffered.errFrame.GetCode() != "invalid_tool_call" {
					t.Fatalf("buffered tool failure lost its no-replay contract: %+v", buffered)
				}
				if buffered.errFrame.GetMessage() != failure["message"] {
					t.Fatal("buffered and streaming tool diagnostics disagree")
				}
			}
		})
	}
}
