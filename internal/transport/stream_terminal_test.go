package transport

import (
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// Upstream variants must end with a terminal event understood by Responses
// clients; [DONE] alone cannot substitute for that event.
func TestStreamNormalizesTerminalEventNames(t *testing.T) {
	for _, tt := range []struct {
		name, upstreamEvent, status, wantEvent string
	}{
		{"done_completed", "response.done", "completed", "response.completed"},
		{"completed_failed", "response.completed", "failed", "response.failed"},
		{"completed_incomplete", "response.completed", "incomplete", "response.incomplete"},
		{"done_failed", "response.done", "failed", "response.failed"},
		{"done_incomplete", "response.done", "incomplete", "response.incomplete"},
		{"completed_cancelled", "response.completed", "cancelled", "response.failed"},
		{"completed_canceled", "response.completed", "canceled", "response.failed"},
		{"done_cancelled", "response.done", "cancelled", "response.failed"},
		{"done_canceled", "response.done", "canceled", "response.failed"},
		{"cancelled", "response.cancelled", "cancelled", "response.failed"},
		{"canceled", "response.canceled", "canceled", "response.failed"},
		{"failed", "response.failed", "failed", "response.failed"},
		{"incomplete", "response.incomplete", "incomplete", "response.incomplete"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := map[string]any{"id": "resp_terminal_normalization", "status": tt.status, "output": []any{}}
			if tt.status == "failed" {
				response["error"] = map[string]any{"code": "upstream_failure", "message": "The upstream could not complete this response"}
			}
			if tt.status == "incomplete" {
				response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
			}
			result := runRelayReader(t, strings.NewReader(streamData(map[string]any{"type": tt.upstreamEvent, "response": response})), 1<<20)
			events := parsedStreamEvents(t, result)
			if len(events) != 1 || events[0]["type"] != tt.wantEvent {
				t.Fatalf("terminal event = %s, want %s", result.body, tt.wantEvent)
			}
			if strings.Count(string(result.body), "data: [DONE]") != 1 {
				t.Fatalf("expected one stream end marker: %s", result.body)
			}
			if tt.wantEvent == "response.failed" {
				terminalResponse := relayObject(events[0]["response"])
				failure := relayObject(terminalResponse["error"])
				if terminalResponse["status"] != "failed" || protocol.StringValue(failure["code"]) == "" || protocol.StringValue(failure["message"]) == "" {
					t.Fatalf("failure terminal must carry failed status and a readable code/message: %s", result.body)
				}
			}
		})
	}
}

func TestStreamNormalizedFailureDiscardsPendingToolCalls(t *testing.T) {
	for _, tt := range []struct{ event, status string }{
		{"response.done", "failed"},
		{"response.completed", "incomplete"},
		{"response.done", "cancelled"},
		{"response.canceled", "canceled"},
	} {
		t.Run(tt.event+"_"+tt.status, func(t *testing.T) {
			native := relayNativeCall("call_failed_terminal", "get_weather", map[string]any{})
			body := streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": native}) +
				streamData(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": native}) +
				streamData(map[string]any{"type": tt.event, "response": map[string]any{"id": "resp_failed_terminal", "status": tt.status, "output": []any{native}}})
			result := runSSE(t, body, streamToolRequest(t, "get_weather", "{}"))
			events := parsedStreamEvents(t, result)
			want := "response.failed"
			if tt.status == "incomplete" {
				want = "response.incomplete"
			}
			if len(events) != 1 || events[0]["type"] != want {
				t.Fatalf("failed response must emit exactly one recognized terminal: %s", result.body)
			}
			output, _ := relayObject(events[0]["response"])["output"].([]any)
			if len(output) != 0 || strings.Contains(string(result.body), "run_officejs") {
				t.Fatalf("failed terminal retained pending tool execution: %s", result.body)
			}
		})
	}
}

func TestStreamExplicitFailureAlwaysCarriesReadableError(t *testing.T) {
	for _, tt := range []struct {
		name          string
		payload       map[string]any
		code, message string
	}{
		{"missing_response", map[string]any{"type": "response.failed"}, "", ""},
		{"missing_error", map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "output": []any{}}}, "", ""},
		{"top_level_error", map[string]any{"type": "error", "code": "rate_limit_exceeded", "message": "Please retry later"}, "rate_limit_exceeded", "Please retry later"},
		{"nested_error", map[string]any{"type": "error", "error": map[string]any{"code": "server_error", "message": "The upstream request failed"}}, "server_error", "The upstream request failed"},
		{"top_level_failed_details", map[string]any{"type": "response.failed", "code": "overloaded", "message": "Please wait before retrying"}, "overloaded", "Please wait before retrying"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := runRelayReader(t, strings.NewReader(streamData(tt.payload)), 1<<20)
			events := parsedStreamEvents(t, result)
			if len(events) != 1 || events[0]["type"] != "response.failed" {
				t.Fatalf("expected a recognized failure terminal: %s", result.body)
			}
			assertNoToolExecutionOnStreamError(t, result)
			failure := relayObject(relayObject(events[0]["response"])["error"])
			if tt.code != "" && failure["code"] != tt.code {
				t.Fatalf("upstream failure code lost: %s", result.body)
			}
			if tt.message != "" && failure["message"] != tt.message {
				t.Fatalf("upstream failure message lost: %s", result.body)
			}
		})
	}
}
