package transport

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestRelaySerializationRecoveryAcrossResponseFormats(t *testing.T) {
	quote := string(byte(34))
	input := "  text(" + quote + "quoted" + quote + ");" + string([]byte{13, 10, 9}) + "// C:" + string(byte(92)) + "workspace; literal " + string(byte(92)) + "n  "
	code := string(protocol.JSONBytes(map[string]any{"tool": "functions.exec", "args": input}))
	fixtures := []struct{ name, arguments, input string }{
		{"unescaped_code", `{"summary":"inspect","code":"` + code + `","destructive":false,"references":[]}`, input},
	}
	for _, control := range []byte{0, 1, 8, 11, 12, 31} {
		want := "  text(1);" + string(control) + "  "
		inner := `{"tool":"functions.exec","args":"` + want + `"}`
		fixtures = append(fixtures, struct{ name, arguments, input string }{fmt.Sprintf("inner_control_%02x", control), string(protocol.JSONBytes(map[string]any{"code": inner})), want})
		outer := `{"code":{"tool":"functions.exec","args":"` + want + `"}}`
		fixtures = append(fixtures, struct{ name, arguments, input string }{fmt.Sprintf("outer_control_%02x", control), outer, want})
	}
	for _, fixture := range fixtures {
		for _, stream := range []bool{false, true} {
			for _, upstreamSSE := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/upstream_sse=%t", fixture.name, stream, upstreamSSE), func(t *testing.T) {
					result, attempts := runOuterSyntaxRelay(t, fixture.arguments, stream, upstreamSSE)
					if attempts != 1 || result.errFrame != nil || !result.ended || result.received != int64(len(result.body)) {
						t.Fatalf("recovery retried or failed: attempts=%d result=%+v", attempts, result)
					}
					if strings.Contains(string(result.body), "run_officejs") || strings.Contains(string(result.body), "response.failed") {
						t.Fatalf("relay or error leaked: %s", result.body)
					}
					response := outerSyntaxCompletedResponse(t, result, fixture.input, stream)
					output, _ := response["output"].([]any)
					if response["id"] != "resp_outer_syntax" || response["status"] != "completed" || len(output) != 1 {
						t.Fatalf("invalid response: %#v", response)
					}
					call := relayObject(output[0])
					if call["type"] != "custom_tool_call" || call["name"] != "functions.exec" || call["call_id"] != "call_outer_syntax" || call["input"] != fixture.input {
						t.Fatalf("recovery changed tool identity or input: %#v", call)
					}
				})
			}
		}
	}
}

func TestRelaySerializationRecoveryRejectsAmbiguousOuterCode(t *testing.T) {
	const private = "PRIVATE SERIALIZATION INPUT"
	code := string(protocol.JSONBytes(map[string]any{"tool": "functions.exec", "args": private}))
	for index, arguments := range []string{
		`{"code":"` + code + `","code":"` + code + `"}`,
		`{"code":"` + code + ` ` + code + `"}`,
		`{"code":"` + code + `"} {"code":"` + code + `"}`,
		`{"code":"` + code + `","summary":"unfinished`,
	} {
		for _, stream := range []bool{false, true} {
			for _, upstreamSSE := range []bool{false, true} {
				t.Run(fmt.Sprintf("case=%d/stream=%t/upstream_sse=%t", index, stream, upstreamSSE), func(t *testing.T) {
					result, attempts := runOuterSyntaxRelay(t, arguments, stream, upstreamSSE)
					if attempts != 1 || streamFailureCode(result) != "invalid_tool_call" {
						t.Fatalf("ambiguous relay executed or retried: attempts=%d result=%+v", attempts, result)
					}
					assertNoToolExecutionOnStreamError(t, result)
					if strings.Contains(string(result.body), private) || strings.Contains(string(result.body), "response.completed") {
						t.Fatalf("rejected input leaked or completed: %s", result.body)
					}
				})
			}
		}
	}
}
