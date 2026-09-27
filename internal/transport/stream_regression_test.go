package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func relayNativeCall(callID, tool string, args any) map[string]any {
	return map[string]any{
		"type": "function_call", "id": "fc_" + callID, "call_id": callID,
		"name": "run_officejs", "status": "completed",
		"arguments": string(protocol.JSONBytes(map[string]any{
			"summary": "relay", "code": string(protocol.JSONBytes(map[string]any{"tool": tool, "args": args})),
		})),
	}
}

func streamData(value any) string { return "data: " + string(protocol.JSONBytes(value)) + "\n\n" }

func streamToolRequest(t *testing.T, tool string, parameters string) []*pluginv1.ForwardRequest {
	t.Helper()
	body := fmt.Sprintf(`{"model":"gpt-6-astra","input":[{"role":"user","content":"inspect"}],"stream":true,"tools":[{"type":"function","name":%q,"parameters":%s}]}`, tool, parameters)
	return requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, []byte(body))
}

func runSSE(t *testing.T, body string, request []*pluginv1.ForwardRequest) forwardResult {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(body))
	}))
	defer upstream.Close()
	transport := New()
	defer transport.Shutdown()
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL})
	return runForward(t, transport, request)
}

func TestHostSessionScopeUsesOnlyIsolatedConversation(t *testing.T) {
	headers := map[string]*pluginv1.HeaderValues{
		"conversation_id": {Values: []string{"conversation"}},
		"session_id":      {Values: []string{"session"}},
	}
	if got := hostSessionScope(headers); got != "conversation" {
		t.Fatalf("scope = %q, want isolated conversation_id value", got)
	}
	for _, alias := range []string{"session_id", "session-id", "conversation-id", "sessionId", "conversationId", "thread_id", "X-Conversation-ID", "X-Codex-Session-Id"} {
		if got := hostSessionScope(map[string]*pluginv1.HeaderValues{alias: {Values: []string{"untrusted-alias"}}}); got != "" {
			t.Errorf("untrusted %s supplied a persistent scope %q", alias, got)
		}
	}
	if got := hostSessionScope(map[string]*pluginv1.HeaderValues{"Conversation_ID": {Values: []string{" ", " conversation ", "conversation"}}}); got != "conversation" {
		t.Fatalf("case-insensitive header scope = %q", got)
	}
	if got := hostSessionScope(map[string]*pluginv1.HeaderValues{"conversation_id": {Values: []string{"first", "second"}}}); got != "" {
		t.Fatalf("ambiguous multi-value scope = %q", got)
	}
	if got := hostSessionScope(map[string]*pluginv1.HeaderValues{"conversation_id": {Values: []string{"first"}}, "Conversation_ID": {Values: []string{"second"}}}); got != "" {
		t.Fatalf("ambiguous header casing selected scope = %q", got)
	}
	if got := relayScope(&pluginv1.ForwardRequestStart{AccountId: 7, Headers: map[string]*pluginv1.HeaderValues{"conversation_id": {Values: []string{"first", "second"}}}}); got != "" {
		t.Fatalf("ambiguous image cache scope = %q", got)
	}
}

func TestStreamDataOnlyNativeItemIDDeltaIsConverted(t *testing.T) {
	native := relayNativeCall("call_weather", "get_weather", map[string]any{"city": "Tokyo"})
	response := map[string]any{"id": "resp_stream", "status": "completed", "output": []any{native}}
	body := strings.Join([]string{
		streamData(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_stream", "status": "in_progress", "output": []any{}}}),
		streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": native}),
		streamData(map[string]any{"type": "response.function_call_arguments.delta", "item_id": native["id"], "delta": `{"city":"Tokyo"}`}),
		streamData(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": native}),
		streamData(map[string]any{"type": "response.completed", "response": response}),
		"data: [DONE]\n\n",
	}, "")
	result := runSSE(t, body, streamToolRequest(t, "get_weather", `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`))
	if result.errFrame != nil {
		t.Fatalf("unexpected error: %s", result.errFrame.GetMessage())
	}
	stream := string(result.body)
	if strings.Contains(stream, "run_officejs") || !strings.Contains(stream, "function_call_arguments.delta") || !strings.Contains(stream, `"name":"get_weather"`) {
		t.Fatalf("native stream was not cleanly converted: %s", stream)
	}
}

func TestStreamCompleteOnlyParallelNativeCallsAreConverted(t *testing.T) {
	one := relayNativeCall("call_weather", "get_weather", map[string]any{"city": "Tokyo"})
	two := relayNativeCall("call_time", "get_time", map[string]any{"timezone": "UTC"})
	response := map[string]any{"id": "resp_parallel", "status": "completed", "output": []any{one, two}}
	body := streamData(map[string]any{"type": "response.completed", "response": response}) + "data: [DONE]\n\n"
	request := requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"inspect"}],"stream":true,"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}},{"type":"function","name":"get_time","parameters":{"type":"object"}}]}`))
	result := runSSE(t, body, request)
	if result.errFrame != nil {
		t.Fatalf("unexpected error: %s", result.errFrame.GetMessage())
	}
	stream := string(result.body)
	if strings.Contains(stream, "run_officejs") || !strings.Contains(stream, `"name":"get_weather"`) || !strings.Contains(stream, `"name":"get_time"`) {
		t.Fatalf("parallel complete-only calls were not converted: %s", stream)
	}
}

func TestStreamCustomToolPreservesWhitespace(t *testing.T) {
	input := "  patch line 1\npatch line 2  "
	native := relayNativeCall("call_patch", "apply_patch", input)
	response := map[string]any{"id": "resp_custom", "status": "completed", "output": []any{native}}
	body := streamData(map[string]any{"type": "response.completed", "response": response}) + "data: [DONE]\n\n"
	request := requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"patch"}],"stream":true,"tools":[{"type":"custom","name":"apply_patch"}]}`))
	result := runSSE(t, body, request)
	if result.errFrame != nil {
		t.Fatalf("unexpected error: %s", result.errFrame.GetMessage())
	}
	stream := string(result.body)
	if !strings.Contains(stream, `"type":"custom_tool_call"`) || !strings.Contains(stream, `  patch line 1\npatch line 2  `) {
		t.Fatalf("custom input whitespace was lost: %s", stream)
	}
}

func TestStreamTruncatedBeforeTerminalReturnsFailedEvent(t *testing.T) {
	body := streamData(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_truncated", "status": "in_progress"}})
	result := runSSE(t, body, requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"inspect"}],"stream":true}`)))
	if got := streamFailureCode(result); got != "invalid_upstream_response" {
		t.Fatalf("truncated stream error = %q", got)
	}
	parsedStreamEvents(t, result)
	assertNoToolExecutionOnStreamError(t, result)
	if strings.Contains(string(result.body), "response.completed") || strings.Count(string(result.body), "data: [DONE]") != 1 {
		t.Fatalf("truncated stream did not terminate as a failure: %s", result.body)
	}
}

func parsedStreamEvents(t *testing.T, result forwardResult) []map[string]any {
	t.Helper()
	if result.errFrame != nil {
		t.Fatalf("unexpected stream error: %s", result.errFrame.GetMessage())
	}
	if !result.ended || result.received != int64(len(result.body)) {
		t.Fatalf("stream end metadata: ended=%v bytes=%d actual=%d", result.ended, result.received, len(result.body))
	}
	var events []map[string]any
	decoder := &sseRelayDecoder{}
	err := decoder.feed(result.body, func(event sseRelayEvent) error {
		if event.data == "" || event.data == "[DONE]" {
			return nil
		}
		object, err := protocol.RawObject([]byte(event.data))
		if err != nil {
			return err
		}
		events = append(events, object)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// streamFailureCode 返回失败原因码：优先 error 帧（尚未给下游发过内容时），
// 其次协议内的 response.failed 终止事件（已经发过内容时）。
func streamFailureCode(result forwardResult) string {
	if result.errFrame != nil {
		return result.errFrame.GetCode()
	}
	code := ""
	decoder := &sseRelayDecoder{}
	_ = decoder.feed(result.body, func(event sseRelayEvent) error {
		if event.data == "" || event.data == "[DONE]" {
			return nil
		}
		payload, err := protocol.RawObject([]byte(event.data))
		if err != nil {
			return nil
		}
		if protocol.StringValue(payload["type"]) == "response.failed" {
			code = protocol.StringValue(relayObject(relayObject(payload["response"])["error"])["code"])
		}
		return nil
	})
	return code
}

// 失败有两种合法形态：error 帧（还没给下游发过内容）或 response.failed 终止
// 事件（已经发过内容、HTTP 状态码无法再改）。两者都必须带可读原因，且都不得
// 释放任何工具调用。
func assertNoToolExecutionOnStreamError(t *testing.T, result forwardResult) {
	t.Helper()
	failed := false
	decoder := &sseRelayDecoder{}
	if err := decoder.feed(result.body, func(event sseRelayEvent) error {
		if event.data == "" || event.data == "[DONE]" {
			return nil
		}
		payload, err := protocol.RawObject([]byte(event.data))
		if err != nil {
			t.Fatal(err)
		}
		event.payload = payload
		event.event = protocol.StringValue(payload["type"])
		if event.event == "response.failed" {
			failure := relayObject(relayObject(payload["response"])["error"])
			if protocol.StringValue(failure["code"]) == "" || protocol.StringValue(failure["message"]) == "" {
				t.Fatalf("response.failed 必须带 code/message：%s", event.data)
			}
			failed = true
		}
		if relayToolEvent(event) || strings.Contains(event.data, "run_officejs") {
			t.Fatalf("failed stream released a tool event: %s", result.body)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if result.errFrame == nil && !failed {
		t.Fatalf("expected an explicit protocol failure, got %#v", result)
	}
	if result.errFrame != nil && result.errFrame.GetCode() == "" {
		t.Fatalf("error frame must carry a code: %#v", result.errFrame)
	}
}

// 已经给客户端发过内容之后再失败：HTTP 状态码无法再改，此时必须用协议内的
// response.failed 收尾 —— 否则客户端只看到 "stream disconnected before
// completion"，真实原因（这里是 invalid_tool_call）被完全吞掉。
func TestStreamFailureAfterOutputUsesFailedEvent(t *testing.T) {
	native := relayNativeCall("call_unknown_after_text", "unknown_tool", map[string]any{})
	body := streamData(map[string]any{"type": "response.output_text.delta", "delta": "working on it"}) +
		streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": native}) +
		streamData(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": native}) +
		streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_after_text", "status": "completed", "output": []any{native}}})
	result := runSSE(t, body, streamToolRequest(t, "get_weather", "{}"))

	if result.errFrame != nil {
		t.Fatalf("流已经开始，不应再回 error 帧：%+v", result.errFrame)
	}
	if !result.ended {
		t.Fatal("失败路径也要干净收尾（End 帧），否则宿主会认为流没结束")
	}
	stream := string(result.body)
	if !strings.Contains(stream, "working on it") {
		t.Fatalf("已转发的文本丢失：%s", stream)
	}
	if !strings.Contains(stream, "response.failed") || !strings.Contains(stream, `"code":"invalid_tool_call"`) {
		t.Fatalf("response.failed 未带真实原因：%s", stream)
	}
	if !strings.Contains(stream, "data: [DONE]") {
		t.Fatalf("缺少 [DONE]：%s", stream)
	}
	if strings.Contains(stream, `"name":"run_officejs"`) {
		t.Fatalf("泄漏了原生传输调用：%s", stream)
	}
}

func TestStreamFailedTranslationNeverReleasesOriginalToolEvents(t *testing.T) {
	native := relayNativeCall("call_unknown", "unknown_tool", map[string]any{})
	body := streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": native}) +
		streamData(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": native}) +
		streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_unknown", "status": "completed", "output": []any{native}}})
	result := runSSE(t, body, streamToolRequest(t, "get_weather", "{}"))
	assertNoToolExecutionOnStreamError(t, result)
}

func TestStreamDoneItemsMissingFromTerminalCannotExecute(t *testing.T) {
	native := relayNativeCall("call_recovered", "get_weather", map[string]any{})
	textItem := map[string]any{"type": "message", "id": "msg_recovered", "role": "assistant", "content": []any{}}
	body := streamData(map[string]any{"type": "response.output_item.done", "output_index": 1, "item": native}) +
		streamData(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": textItem}) +
		"event: response.completed\ndata: " + string(protocol.JSONBytes(map[string]any{"response": map[string]any{"id": "resp_recovered", "status": "completed", "output": []any{}}})) + "\n\n"
	result := runSSE(t, body, streamToolRequest(t, "get_weather", "{}"))
	assertNoToolExecutionOnStreamError(t, result)
}

func TestStreamMissingOutputIndexDoesNotSynthesizeWrongToolIndex(t *testing.T) {
	native := relayNativeCall("call_gap", "get_weather", map[string]any{})
	body := streamData(map[string]any{"type": "response.output_item.done", "output_index": 2, "item": native}) +
		streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{}}})
	result := runSSE(t, body, streamToolRequest(t, "get_weather", "{}"))
	assertNoToolExecutionOnStreamError(t, result)
}

func TestStreamSequencesAndEndBytesMatchEmittedRecords(t *testing.T) {
	native := relayNativeCall("call_sequence", "get_weather", map[string]any{})
	body := streamData(map[string]any{"type": "response.output_item.added", "sequence_number": 100, "output_index": 0, "item": native}) +
		streamData(map[string]any{"type": "response.output_text.delta", "sequence_number": 101, "delta": "Searching"}) +
		streamData(map[string]any{"type": "response.completed", "sequence_number": 102, "response": map[string]any{"status": "completed", "output": []any{native}}}) +
		"data: [DONE]\n\ndata: [DONE]\n\n"
	result := runSSE(t, body, streamToolRequest(t, "get_weather", "{}"))
	for index, event := range parsedStreamEvents(t, result) {
		if got, ok := relayOutputIndex(event["sequence_number"]); !ok || got != index {
			t.Fatalf("non-monotonic event sequence at %d: %v", index, event["sequence_number"])
		}
	}
	if strings.Count(string(result.body), "data: [DONE]") != 1 {
		t.Fatalf("expected one end marker: %s", result.body)
	}
}

func TestStreamFailedTerminalDiscardsPendingToolExecutionEvents(t *testing.T) {
	for kind, terminal := range map[string]string{
		"response.failed":     "response.failed",
		"response.incomplete": "response.incomplete",
		"response.cancelled":  "response.failed",
		"response.canceled":   "response.failed",
		"response.done":       "response.incomplete",
	} {
		t.Run(kind, func(t *testing.T) {
			native := relayNativeCall("call_failed", "get_weather", map[string]any{})
			body := streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": native}) +
				streamData(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": native}) +
				streamData(map[string]any{"type": kind, "response": map[string]any{"status": "incomplete", "output": []any{native}}})
			result := runSSE(t, body, streamToolRequest(t, "get_weather", "{}"))
			events := parsedStreamEvents(t, result)
			if len(events) != 1 || events[0]["type"] != terminal {
				t.Fatalf("failed terminal released an execution event: %s", result.body)
			}
			if strings.Contains(string(result.body), "run_officejs") || len(relayObject(events[0]["response"])["output"].([]any)) != 0 {
				t.Fatalf("failed terminal retained executable output: %s", result.body)
			}
		})
	}
}

func TestStreamTextIsImmediateWhileToolRecordsWaitForTerminal(t *testing.T) {
	native := relayNativeCall("call_first_text", "get_weather", map[string]any{})
	reader, writer := io.Pipe()
	defer reader.Close()
	release := make(chan struct{})
	go func() {
		defer writer.Close()
		_, _ = io.WriteString(writer, streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": native}))
		_, _ = io.WriteString(writer, streamData(map[string]any{"type": "response.output_text.delta", "delta": "Checking local files"}))
		<-release
		_, _ = io.WriteString(writer, streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{native}}}))
	}()
	stub := &timingStreamStub{streamStub: streamStub{ctx: context.Background()}, firstBodyChunk: make(chan time.Time, 1)}
	response := &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: make(http.Header), Body: reader}
	source := map[string]any{"tools": []any{map[string]any{"type": "function", "name": "get_weather", "parameters": map[string]any{}}}}
	finished := make(chan error, 1)
	go func() { finished <- sendTransformedHTTPResponseStream(stub, response, 1<<20, source) }()
	select {
	case <-stub.firstBodyChunk:
		close(release)
	case <-time.After(time.Second):
		close(release)
		t.Fatal("text was buffered while waiting for native tool completion")
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not finish after terminal was released")
	}
	for _, frame := range stub.responses {
		if chunk := frame.GetBodyChunk(); len(chunk) > 0 {
			if !strings.Contains(string(chunk), "Checking local files") {
				t.Fatalf("first visible event was not immediate text: %s", chunk)
			}
			return
		}
	}
	t.Fatal("no body chunk was sent")
}

func TestTransformResponseWorksWhenToolRewritingIsDisabled(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		t.Run(fmt.Sprint(rewrite), func(t *testing.T) {
			native := relayNativeCall("call_rewrite_switch", "get_weather", map[string]any{})
			requestBody := protocol.JSONBytes(map[string]any{"model": "gpt-6-astra", "input": []any{map[string]any{"role": "user", "content": "inspect"}}, "tools": []any{map[string]any{"type": "function", "name": "get_weather", "parameters": map[string]any{}}}})
			captured := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				data, err := io.ReadAll(request.Body)
				if err != nil {
					t.Error(err)
				}
				captured <- data
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write(protocol.JSONBytes(map[string]any{"id": "resp_rewrite_switch", "status": "completed", "output": []any{native}}))
			}))
			defer upstream.Close()
			transport := New()
			defer transport.Shutdown()
			applyConfig(t, transport, map[string]any{"responses_url": upstream.URL, "rewrite_tools": rewrite, "transform_responses": true})
			result := runForward(t, transport, requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), map[string]string{"session_id": "local-only-session"}, requestBody))
			if result.errFrame != nil {
				t.Fatalf("unexpected forward error: %s", result.errFrame.GetMessage())
			}
			transformed, err := protocol.RawObject(result.body)
			if err != nil {
				t.Fatal(err)
			}
			output := transformed["output"].([]any)
			if len(output) != 1 || relayObject(output[0])["name"] != "get_weather" {
				t.Fatalf("tool response not converted when rewrite=%v: %s", rewrite, result.body)
			}
			outgoing := <-captured
			if strings.Contains(string(outgoing), "__bps_session_scope") || strings.Contains(string(outgoing), "local-only-session") {
				t.Fatalf("private session metadata leaked into outbound body: %s", outgoing)
			}
			if !rewrite && string(outgoing) != string(requestBody) {
				t.Fatalf("disabled rewriting changed outbound body: %s", outgoing)
			}
		})
	}
}

func TestStreamRejectsInProgressTerminal(t *testing.T) {
	body := streamData(map[string]any{"type": "response.done", "response": map[string]any{"status": "in_progress", "output": []any{}}})
	result := runSSE(t, body, streamToolRequest(t, "get_weather", "{}"))
	if result.errFrame == nil || result.errFrame.GetCode() != "invalid_upstream_response" || result.ended {
		t.Fatalf("in-progress response falsely marked complete: %#v", result)
	}
}

func TestStreamDuplicateTerminalDoesNotExecuteToolTwice(t *testing.T) {
	native := relayNativeCall("call_duplicate_terminal", "get_weather", map[string]any{})
	response := map[string]any{"status": "completed", "output": []any{native}}
	body := streamData(map[string]any{"type": "response.completed", "response": response}) + streamData(map[string]any{"type": "response.done", "response": response})
	result := runSSE(t, body, streamToolRequest(t, "get_weather", "{}"))
	executions := 0
	for _, event := range parsedStreamEvents(t, result) {
		if event["type"] == "response.output_item.done" {
			executions++
		}
	}
	if executions != 1 {
		t.Fatalf("duplicate terminal emitted %d tool execution events", executions)
	}
}

func TestStreamDeltaOnlyClientReceivesFunctionAndCustomArguments(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		t.Run(kind, func(t *testing.T) {
			input := "  exact custom text\nwith {braces}  "
			var args any = map[string]any{"cmd": "pwd", "n": 1}
			if kind == "custom" {
				args = input
			}
			native := relayNativeCall("call_delta_"+kind, "local_tool", args)
			response := map[string]any{"status": "completed", "output": []any{native}}
			body := streamData(map[string]any{"type": "response.completed", "response": response})
			requestBody := protocol.JSONBytes(map[string]any{"model": "gpt-6-astra", "stream": true, "input": "inspect", "tools": []any{map[string]any{"type": kind, "name": "local_tool"}}})
			result := runSSE(t, body, requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, requestBody))
			events := parsedStreamEvents(t, result)
			prefix := "response.function_call_arguments"
			want := string(protocol.JSONBytes(args))
			if kind == "custom" {
				prefix, want = "response.custom_tool_call_input", input
			}
			var received strings.Builder
			doneCount := 0
			for _, event := range events {
				if event["type"] == prefix+".delta" {
					received.WriteString(event["delta"].(string))
				}
				if event["type"] == prefix+".done" {
					doneCount++
				}
			}
			if received.String() != want || doneCount != 1 {
				t.Fatalf("delta-only client arguments = %q, want %q; done count %d", received.String(), want, doneCount)
			}
		})
	}
}

func TestStreamTerminalToolIndexAndIdentityMustMatch(t *testing.T) {
	one := relayNativeCall("call_index_one", "get_weather", map[string]any{})
	two := relayNativeCall("call_index_two", "get_weather", map[string]any{})
	for _, earlier := range []map[string]any{
		{"type": "response.output_item.done", "output_index": 1, "item": one},
		{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": two["id"], "delta": "{}"},
		{"type": "response.function_call_arguments.done", "item_id": "fc_missing", "arguments": "{}"},
	} {
		body := streamData(earlier) + streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{one, two}}})
		result := runSSE(t, body, streamToolRequest(t, "get_weather", "{}"))
		assertNoToolExecutionOnStreamError(t, result)
		if result.errFrame.GetCode() != "basispoints_protocol_error" {
			t.Fatalf("unexpected protocol error code: %s", result.errFrame.GetCode())
		}
	}
}

func TestStreamParallelInvalidCallPreventsEveryToolExecution(t *testing.T) {
	one := relayNativeCall("call_valid_parallel", "get_weather", map[string]any{})
	two := relayNativeCall("call_invalid_parallel", "undeclared", map[string]any{})
	body := streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{one, two}}})
	assertNoToolExecutionOnStreamError(t, runSSE(t, body, streamToolRequest(t, "get_weather", "{}")))
}

func TestStreamDirectNamespacedClientToolIsCanonicalized(t *testing.T) {
	native := map[string]any{"type": "function_call", "id": "fc_direct_stream", "call_id": "call_direct_stream", "name": "functions.local_read", "arguments": `{"path":"README.md"}`}
	body := streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": native}) +
		streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{native}}})
	request := requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, []byte(`{"model":"gpt-6-astra","stream":true,"input":"read","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"local_read","parameters":{"type":"object"}}]}]}`))
	result := runSSE(t, body, request)
	executions := 0
	for _, event := range parsedStreamEvents(t, result) {
		if event["type"] == "response.output_item.done" {
			executions++
			item := relayObject(event["item"])
			if item["name"] != "local_read" || item["namespace"] != "functions" {
				t.Fatalf("direct tool not canonicalized: %#v", item)
			}
		}
	}
	if executions != 1 {
		t.Fatalf("direct tool executions = %d", executions)
	}
}

func structuredStreamRequest(t *testing.T) []*pluginv1.ForwardRequest {
	t.Helper()
	return requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, []byte(`{"model":"gpt-6-astra","stream":true,"input":"return JSON","text":{"format":{"type":"json_schema","name":"result","strict":true,"schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}}}}`))
}

func structuredMessage(text string) map[string]any {
	return map[string]any{"type": "message", "id": "msg_structured", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func TestStreamStructuredOutputPublishesOnlyValidatedTerminalText(t *testing.T) {
	message := structuredMessage(`{"ok":true}`)
	body := streamData(map[string]any{"type": "response.output_text.delta", "delta": "UNVALIDATED_DRAFT"}) +
		streamData(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": structuredMessage("UNVALIDATED_DRAFT")}) +
		streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{message}}})
	result := runSSE(t, body, structuredStreamRequest(t))
	var text strings.Builder
	for _, event := range parsedStreamEvents(t, result) {
		if event["type"] == "response.output_text.delta" {
			text.WriteString(event["delta"].(string))
		}
	}
	if text.String() != `{"ok":true}` || strings.Contains(string(result.body), "UNVALIDATED_DRAFT") {
		t.Fatalf("structured output released unvalidated text: %s", result.body)
	}
}

func TestStreamInvalidStructuredOutputEmitsNoAnswer(t *testing.T) {
	for _, text := range []string{"INVALID_JSON", `{"ok":"wrong_type"}`} {
		message := structuredMessage(text)
		body := streamData(map[string]any{"type": "response.created", "response": map[string]any{"status": "in_progress", "output": []any{message}}}) +
			streamData(map[string]any{"type": "response.output_text.delta", "delta": text}) +
			streamData(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": message}) +
			streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{message}}})
		result := runSSE(t, body, structuredStreamRequest(t))
		if code := streamFailureCode(result); code != "invalid_structured_output" {
			t.Fatalf("structured validation failure = %q / %#v", code, result)
		}
		if strings.Contains(string(result.body), text) || strings.Contains(string(result.body), "output_text") || strings.Contains(string(result.body), "wrong_type") {
			t.Fatalf("unvalidated structured answer leaked: %s", result.body)
		}
	}
}

func TestStreamCreatedAndFailedResponsesFilterExecutableTools(t *testing.T) {
	for _, kind := range []string{"function_call", "custom_tool_call"} {
		t.Run(kind, func(t *testing.T) {
			item := map[string]any{"type": kind, "id": "tool_failed", "call_id": "call_failed", "name": "get_weather", "arguments": "{}", "input": "not executable"}
			body := streamData(map[string]any{"type": "response.created", "response": map[string]any{"status": "in_progress", "output": []any{item}}}) +
				streamData(map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "output": []any{item}}})
			result := runSSE(t, body, streamToolRequest(t, "get_weather", "{}"))
			for _, event := range parsedStreamEvents(t, result) {
				output, _ := relayObject(event["response"])["output"].([]any)
				if len(output) != 0 {
					t.Fatalf("non-completed response retained tool: %s", result.body)
				}
			}
		})
	}
}
