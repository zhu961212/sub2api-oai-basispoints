package transport

import (
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestStreamToolPayloadAliasesReachClient(t *testing.T) {
	for _, tt := range []struct {
		name, tool, field, kind, resultField, want string
		payload                                    any
		declaration                                map[string]any
	}{
		{
			name: "function_input_alias", tool: "get_weather", field: "input",
			kind: "function_call", resultField: "arguments", want: `{"city":"Tokyo"}`,
			payload:     map[string]any{"city": "Tokyo"},
			declaration: map[string]any{"type": "function", "name": "functions.get_weather", "parameters": map[string]any{"type": "object", "required": []any{"city"}, "properties": map[string]any{"city": map[string]any{"type": "string"}}, "additionalProperties": false}},
		},
		{
			name: "custom_arguments_alias", tool: "exec", field: "arguments",
			kind: "custom_tool_call", resultField: "input", want: "  first line\r\n\tsecond line\n",
			payload:     "  first line\r\n\tsecond line\n",
			declaration: map[string]any{"type": "custom", "name": "functions.exec"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			native := relayNativeCall("call_alias", tt.tool, nil)
			native["arguments"] = string(protocol.JSONBytes(map[string]any{"code": string(protocol.JSONBytes(map[string]any{"tool": tt.tool, tt.field: tt.payload}))}))
			body := streamData(map[string]any{"type": "response.output_text.delta", "delta": "working"}) +
				streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_alias", "status": "completed", "output": []any{native}}})
			request := protocol.JSONBytes(map[string]any{"model": "gpt-6-astra", "stream": true, "input": "inspect", "tools": []any{tt.declaration}})
			result := runSSE(t, body, requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, request))
			events := parsedStreamEvents(t, result)
			if len(events) == 0 || events[len(events)-1]["type"] != "response.completed" || streamFailureCode(result) != "" {
				t.Fatalf("compatible tool call failed: %s", result.body)
			}
			output := relayObject(events[len(events)-1]["response"])["output"].([]any)
			if len(output) != 1 {
				t.Fatalf("expected one converted tool call, got %v", output)
			}
			call := relayObject(output[0])
			if call["type"] != tt.kind || call["name"] != tt.declaration["name"] || call[tt.resultField] != tt.want || call["call_id"] != "call_alias" {
				t.Fatalf("converted tool call lost its identity or payload: %v", call)
			}
			if strings.Contains(string(result.body), "run_officejs") || strings.Count(string(result.body), "data: [DONE]") != 1 {
				t.Fatalf("invalid client stream: %s", result.body)
			}
		})
	}
}
