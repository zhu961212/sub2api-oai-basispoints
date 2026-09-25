package transport

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestStreamNumericToolValidationIsAtomic(t *testing.T) {
	for _, test := range []struct {
		name, value string
		wantValid   bool
	}{
		{"upper boundary", "64", true},
		{"integer decimal", "64.0", true},
		{"integer exponent", "6.4e1", true},
		{"fraction", "1.5", false},
		{"below minimum", "0", false},
		{"above maximum", "65", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := map[string]any{"count": json.Number(test.value)}
			valid := relayNativeCall("call_valid", "numeric_tool", map[string]any{"count": 1})
			candidate := relayNativeCall("call_candidate", "numeric_tool", args)
			body := streamData(map[string]any{"type": "response.output_text.delta", "delta": "working"}) +
				streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": valid}) +
				streamData(map[string]any{"type": "response.output_item.added", "output_index": 1, "item": candidate}) +
				streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_numeric", "status": "completed", "output": []any{valid, candidate}}})
			parameters := map[string]any{
				"type": "object", "required": []any{"count"}, "additionalProperties": false,
				"properties": map[string]any{"count": map[string]any{"type": "integer", "minimum": 1, "maximum": 64}},
			}
			request := protocol.JSONBytes(map[string]any{
				"model": "gpt-6-astra", "stream": true, "input": "inspect",
				"tools": []any{map[string]any{"type": "function", "name": "numeric_tool", "parameters": parameters}},
			})
			result := runSSE(t, body, requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), map[string]string{"session_id": t.Name()}, request))
			if result.errFrame != nil || !result.ended || strings.Count(string(result.body), "data: [DONE]") != 1 {
				t.Fatalf("invalid stream lifecycle: %#v", result)
			}
			events := parsedStreamEvents(t, result)
			if !test.wantValid {
				if streamFailureCode(result) != "invalid_tool_call" {
					t.Fatalf("invalid numeric call accepted: %s", result.body)
				}
				for _, event := range events {
					kind := protocol.StringValue(event["type"])
					if kind == "response.completed" || strings.HasPrefix(kind, "response.function_call_arguments.") || relayToolItem(relayObject(event["item"])) {
						t.Fatalf("tool event leaked before all numeric calls were validated: %s", result.body)
					}
				}
				return
			}
			if streamFailureCode(result) != "" || len(events) == 0 || events[len(events)-1]["type"] != "response.completed" {
				t.Fatalf("valid numeric call failed: %s", result.body)
			}
			output, _ := relayObject(events[len(events)-1]["response"])["output"].([]any)
			if len(output) != 2 || relayObject(output[1])["arguments"] != string(protocol.JSONBytes(args)) {
				t.Fatalf("numeric argument changed: %#v", output)
			}
		})
	}
}
