package protocol

import (
	"errors"
	"strings"
	"testing"
)

func terminalTestRecord(event string, payload map[string]any) string {
	var result strings.Builder
	writeSSE(&result, event, payload)
	return result.String()
}

func TestResponseTerminalFailureEvidencePrecedesCompletion(t *testing.T) {
	for _, test := range []struct {
		name, event string
		payload     map[string]any
		want        ResponseTerminal
	}{
		{"wire error", "error", map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}}, TerminalFailed},
		{"payload error", "response.completed", map[string]any{"type": "error", "response": map[string]any{"status": "completed"}}, TerminalFailed},
		{"outer error", "response.completed", map[string]any{"error": map[string]any{"code": "server_error"}, "response": map[string]any{"status": "completed"}}, TerminalFailed},
		{"inner error", "response.completed", map[string]any{"response": map[string]any{"status": "completed", "error": "failure"}}, TerminalFailed},
		{"outer failed", "response.completed", map[string]any{"status": "failed", "response": map[string]any{"status": "completed"}}, TerminalFailed},
		{"inner incomplete", "response.completed", map[string]any{"response": map[string]any{"status": "incomplete"}}, TerminalIncomplete},
		{"inner cancelled", "response.completed", map[string]any{"response": map[string]any{"status": "cancelled"}}, TerminalCancelled},
		{"outer HTTP status", "response.completed", map[string]any{"status_code": 403, "response": map[string]any{"status": "completed"}}, TerminalFailed},
		{"inner HTTP status", "response.completed", map[string]any{"response": map[string]any{"status": "completed", "http_status": 500}}, TerminalFailed},
		{"false operation", "response.completed", map[string]any{"ok": false, "response": map[string]any{"status": "completed"}}, TerminalFailed},
		{"progress status", "response.created", map[string]any{"type": "response.created", "response": map[string]any{"status": "completed"}}, TerminalNone},
		{"conflicting event types", "response.created", map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}}, TerminalInvalid},
		{"in progress completion", "response.completed", map[string]any{"response": map[string]any{"status": "in_progress"}}, TerminalInvalid},
		{"success", "response.completed", map[string]any{"response": map[string]any{"status": "completed", "error": nil}}, TerminalCompleted},
		{"output is not evidence", "response.completed", map[string]any{"response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "error": "text", "status_code": 403}}}}, TerminalCompleted},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyResponseTerminal(test.event, test.payload); got != test.want {
				t.Fatalf("terminal = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBufferedResponseLocksFirstTerminal(t *testing.T) {
	completed := terminalTestRecord("response.completed", map[string]any{"response": map[string]any{"id": "first", "status": "completed", "output": []any{}}})
	second := strings.ReplaceAll(completed, "first", "second")
	failure := terminalTestRecord("response.failed", map[string]any{"response": map[string]any{"status": "completed", "error": map[string]any{"code": "bps_service_rejected", "message": "isolated failure"}}})
	for _, suffix := range []string{second, failure} {
		got, err := ParseFinalStreamResponse([]byte(completed + suffix))
		if err != nil || got["id"] != "first" {
			t.Fatalf("first terminal replaced: got=%v err=%v", got, err)
		}
	}
	_, err := ParseFinalStreamResponse([]byte(failure + completed))
	var api *APIError
	if !errors.As(err, &api) || api.Kind != "bps_service_rejected" || api.Message != "isolated failure" {
		t.Fatalf("first failure classification lost: %v", err)
	}
	nl := string(rune(10))
	for _, first := range []string{"data: [DONE]", "event: error", "event: response.failed" + nl + "data: not-json", "event: error" + nl + "data: [DONE]", "event: error" + nl + "data: "} {
		if _, err := ParseFinalStreamResponse([]byte(first + nl + nl + completed)); err == nil {
			t.Fatalf("completed event resurrected stream after %q", first)
		}
	}
}

func TestFailedResponsesNeverPublishToolsOrReplayContext(t *testing.T) {
	for _, state := range []string{"failed", "incomplete", "cancelled", "completed_with_error"} {
		t.Run(state, func(t *testing.T) {
			source := repairTestSource()
			source["session_id"] = t.Name()
			response := repairTestResponse()
			response["id"] = t.Name()
			response["status"] = state
			if state == "completed_with_error" {
				response["status"] = "completed"
				response["error"] = map[string]any{"code": "server_error", "message": "failure"}
			}
			if ToolRepairEligible(source, response) {
				t.Fatal("failed response eligible for correction")
			}
			RememberResponseContext(source, response)
			if rememberedResponseContext(t.Name()) != "" {
				t.Fatal("failed response published context")
			}
			before := string(jsonBytes(response))
			stream := string(SyntheticStream(response))
			if strings.Contains(stream, "event: response.completed") || strings.Contains(stream, "run_officejs") || strings.Contains(stream, "function_call_arguments") {
				t.Fatalf("failed synthetic stream released completion or tools: %s", stream)
			}
			if string(jsonBytes(response)) != before {
				t.Fatal("synthetic failure mutated input")
			}
			_, translated, _, err := TransformResponseBody(jsonBytes(response), source)
			if err != nil || len(translated["output"].([]any)) != 0 {
				t.Fatalf("failed JSON retained tool output: %v %v", translated, err)
			}
			if rememberedResponseContext(t.Name()) != "" || rememberedNativeCallInNamespace(nativeCallNamespace(source), "call_compat") != nil {
				t.Fatal("failed transformation polluted replay caches")
			}
		})
	}
}

func TestToolRepairRejectsCompletedResponseWithFailure(t *testing.T) {
	response := repairTestResponse()
	response["error"] = map[string]any{"code": "bps_service_rejected", "message": "isolated failure"}
	_, err := MergeToolRepairResponse(repairTestSource(), repairTestResponse(), response)
	var api *APIError
	if !errors.As(err, &api) || api.Kind != "bps_service_rejected" {
		t.Fatalf("repair discarded completed response failure: %v", err)
	}
}

func TestSyntheticFailureRetainsFinalSemantics(t *testing.T) {
	for _, test := range []struct{ status, want string }{
		{"failed", "upstream_failed"},
		{"incomplete", "upstream_incomplete"},
		{"cancelled", "upstream_cancelled"},
	} {
		t.Run(test.status, func(t *testing.T) {
			response := map[string]any{"status": test.status, "output": []any{}, "status_code": 500, "ok": false, "error": map[string]any{"code": "server_error", "message": "diagnostic"}}
			raw := SyntheticStream(response)
			_, err := ParseFinalStreamResponse(raw)
			var api *APIError
			if !errors.As(err, &api) || api.Kind != test.want {
				t.Fatalf("synthetic %s lost semantics: err=%v raw=%s", test.status, err, raw)
			}
		})
	}
}
