package transport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func executorOnlyCatalogSource(session string) map[string]any {
	return map[string]any{
		"model": "gpt-6-astra", "stream": true, "session_id": session,
		"input": []any{map[string]any{"role": "user", "content": "inspect the workspace"}},
		"tools": []any{map[string]any{
			"type": "custom", "name": "functions.exec",
			"description": "Run JavaScript. Nested tools are available only on the tools object, for example await tools.exec_command({cmd: \"pwd\"}).",
		}},
	}
}

func TestStreamExecutorOnlyCatalogPreservesCustomRelayAndReplay(t *testing.T) {
	const input = "  const result = await tools.exec_command({cmd: \"pwd\"});\r\ntext(result.output);  "
	native := relayNativeCall("call_exec_catalog", "functions.exec", input)
	body := streamData(map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "resp_exec_catalog", "status": "completed", "output": []any{native},
	}})
	captured := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		captured <- raw
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(body))
	}))
	defer upstream.Close()
	transport := New()
	defer transport.Shutdown()
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL})
	source := executorOnlyCatalogSource(t.Name())
	result := runForward(t, transport, requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, protocol.JSONBytes(source)))
	events := parsedStreamEvents(t, result)
	if len(events) == 0 || events[len(events)-1]["type"] != "response.completed" || streamFailureCode(result) != "" {
		t.Fatalf("custom executor relay did not complete: %s", result.body)
	}
	output, _ := relayObject(events[len(events)-1]["response"])["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected one executable custom call, got %#v", output)
	}
	call := relayObject(output[0])
	if call["type"] != "custom_tool_call" || call["name"] != "functions.exec" || call["input"] != input || call["call_id"] != native["call_id"] {
		t.Fatalf("custom executor identity or raw input changed: %#v", call)
	}
	var deltas strings.Builder
	for _, event := range events {
		if event["type"] == "response.custom_tool_call_input.delta" {
			delta, _ := event["delta"].(string)
			deltas.WriteString(delta)
		}
	}
	if deltas.String() != input || strings.Contains(string(result.body), "run_officejs") || strings.Count(string(result.body), "data: [DONE]") != 1 {
		t.Fatalf("custom executor stream is incomplete or exposes the native relay: %s", result.body)
	}

	prepared, err := protocol.RawObject(<-captured)
	if err != nil {
		t.Fatal(err)
	}
	var instructions strings.Builder
	items, _ := prepared["input"].([]any)
	for _, value := range items {
		item := relayObject(value)
		if item["role"] != "developer" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, part := range content {
			instructions.WriteString(protocol.StringValue(relayObject(part)["text"]))
		}
	}
	prompt := instructions.String()
	if !strings.Contains(prompt, "functions.exec") || !strings.Contains(prompt, "tools.exec_command") {
		t.Fatalf("prepared request omitted the actual custom executor contract: %s", prompt)
	}
	for _, forbidden := range []string{"\"tool\":\"exec_command\"", "\"tool\":\"functions.exec_command\"", "- exec_command (function)", "- functions.exec_command (function)"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prepared request advertises undeclared standalone exec_command: %s", prompt)
		}
	}

	followup := executorOnlyCatalogSource(t.Name())
	followup["input"] = []any{call, map[string]any{"type": "custom_tool_call_output", "call_id": call["call_id"], "output": "workspace path"}}
	replay, err := protocol.PrepareResponsesBody(followup, protocol.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	replayed := 0
	items, _ = replay["input"].([]any)
	for _, value := range items {
		item := relayObject(value)
		if item["type"] == "function_call" && item["call_id"] == native["call_id"] {
			replayed++
			if string(protocol.JSONBytes(item)) != string(protocol.JSONBytes(native)) {
				t.Fatalf("follow-up did not preserve the original native relay: got %#v, want %#v", item, native)
			}
		}
	}
	if replayed != 1 {
		t.Fatalf("follow-up replayed %d native calls, want one", replayed)
	}
}

func TestStreamExecutorOnlyCatalogRejectsNestedToolAsDirectCall(t *testing.T) {
	const private = "private-shell-argument"
	native := relayNativeCall("call_undeclared_nested", "exec_command", map[string]any{"cmd": private})
	body := streamData(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_undeclared_nested", "status": "in_progress", "output": []any{}}}) +
		streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": native}) +
		streamData(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": native}) +
		streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_undeclared_nested", "status": "completed", "output": []any{native}}})
	source := executorOnlyCatalogSource(t.Name())
	result := runSSE(t, body, requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, protocol.JSONBytes(source)))
	events := parsedStreamEvents(t, result)
	assertNoToolExecutionOnStreamError(t, result)
	if streamFailureCode(result) != "invalid_tool_call" || len(events) != 2 || events[len(events)-1]["type"] != "response.failed" {
		t.Fatalf("undeclared nested tool did not terminate as a protocol failure: %s", result.body)
	}
	failure := relayObject(relayObject(events[len(events)-1]["response"])["error"])
	if failure["message"] != "Basis Points returned an unknown client tool absent from the active catalog" {
		t.Fatalf("unexpected failure diagnostic: %#v", failure)
	}
	if strings.Contains(string(result.body), private) || strings.Contains(string(result.body), "exec_command") || strings.Contains(string(result.body), "response.completed") || strings.Count(string(result.body), "data: [DONE]") != 1 {
		t.Fatalf("rejected nested tool leaked content or an invalid terminal: %s", result.body)
	}
}
