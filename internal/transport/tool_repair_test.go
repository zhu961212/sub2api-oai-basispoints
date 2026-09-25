package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestFirstTurnToolRepairPreservesLifecycleContextProxyAndUsage(t *testing.T) {
	for _, mode := range []string{"custom_stream", "namespace_function", "custom_json"} {
		t.Run(mode, func(t *testing.T) {
			source := executorOnlyCatalogSource(t.Name())
			source["stream"] = mode != "custom_json"
			tool, input := "functions.exec", any("text(await tools.exec_command({cmd: 'pwd'}));")
			if mode == "namespace_function" {
				source["tools"] = []any{map[string]any{"type": "namespace", "name": "files", "tools": []any{map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "object", "required": []any{"cmd"}, "properties": map[string]any{"cmd": map[string]any{"type": "string"}}, "additionalProperties": false}}}}}
				tool, input = "files.inspect", map[string]any{"cmd": "pwd"}
			}
			originalCall := relayNativeCall("call_wrong", "exec_command", map[string]any{"cmd": "pwd"})
			message := map[string]any{"type": "message", "id": "msg_original", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Inspecting project"}}}
			original := map[string]any{"id": "resp_visible", "status": "completed", "output": []any{message, originalCall}, "usage": map[string]any{"input_tokens": json.Number("9007199254740993"), "input_tokens_details": map[string]any{"cached_tokens": 3}}}
			fixedCall := relayNativeCall("call_repaired", tool, input)
			reasoning := map[string]any{"type": "reasoning", "id": "rs_repaired", "summary": []any{}}
			fixed := map[string]any{"id": "resp_internal_repair", "status": "completed", "output": []any{reasoning, fixedCall}, "usage": map[string]any{"input_tokens": 2, "input_tokens_details": map[string]any{"cached_tokens": 4}}}
			type capturedRequest struct {
				body   map[string]any
				header http.Header
				target string
			}
			captured := make(chan capturedRequest, 2)
			var requests atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				body, err := protocol.RawObject(raw)
				if err != nil {
					t.Error(err)
				}
				captured <- capturedRequest{body, r.Header.Clone(), r.RequestURI}
				w.Header().Set("Content-Type", "text/event-stream")
				if requests.Add(1) == 1 {
					_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_visible", "status": "in_progress", "output": []any{}}})+
						streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": message})+
						streamData(map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "msg_original", "delta": "Inspecting project"})+
						streamData(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": message})+
						streamData(map[string]any{"type": "response.output_item.added", "output_index": 1, "item": originalCall})+
						streamData(map[string]any{"type": "response.completed", "response": original}))
				} else {
					_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_internal_repair"}})+streamData(map[string]any{"type": "response.completed", "response": fixed}))
				}
			}))
			defer proxy.Close()
			transport := New()
			defer transport.Shutdown()
			applyConfig(t, transport, map[string]any{"responses_url": "http://upstream.invalid/v1/responses"})
			frames := requestFrames(t, "https://ignored.example/v1/responses", token(t, "acct"), map[string]string{"ChatGPT-Account-ID": "same-account"}, protocol.JSONBytes(source))
			frames[0].GetStart().ProxyUrl = proxy.URL
			result := runForward(t, transport, frames)
			if requests.Load() != 2 {
				t.Fatalf("upstream attempts=%d: %s", requests.Load(), result.body)
			}
			first, second := <-captured, <-captured
			if first.target != second.target || first.header.Get("Authorization") != second.header.Get("Authorization") || first.header.Get("ChatGPT-Account-ID") != second.header.Get("ChatGPT-Account-ID") {
				t.Fatal("repair changed account, proxy target or credentials")
			}
			for key, value := range first.body {
				if key != "input" && string(protocol.JSONBytes(value)) != string(protocol.JSONBytes(second.body[key])) {
					t.Fatalf("repair changed prepared %s", key)
				}
			}
			history := first.body["input"].([]any)
			if string(protocol.JSONBytes(second.body["input"].([]any)[:len(history)])) != string(protocol.JSONBytes(history)) {
				t.Fatal("repair replaced original context")
			}
			var response map[string]any
			if mode == "custom_json" {
				var err error
				response, err = protocol.RawObject(result.body)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				events := parsedStreamEvents(t, result)
				created, completed, addedReasoning, doneReasoning := 0, 0, 0, 0
				for _, event := range events {
					switch event["type"] {
					case "response.created":
						created++
					case "response.completed":
						completed++
						response = relayObject(event["response"])
					case "response.output_item.added", "response.output_item.done":
						if relayObject(event["item"])["id"] == "rs_repaired" {
							if event["type"] == "response.output_item.added" {
								addedReasoning++
							} else {
								doneReasoning++
							}
							if event["output_index"] != json.Number("1") {
								t.Fatalf("repair reasoning index changed: %#v", event)
							}
						}
					}
				}
				if created != 1 || completed != 1 || addedReasoning != 1 || doneReasoning != 1 || strings.Count(string(result.body), "data: [DONE]") != 1 || strings.Contains(string(result.body), "response.failed") || strings.Contains(string(result.body), "resp_internal_repair") {
					t.Fatalf("invalid repaired lifecycle: %s", result.body)
				}
			}
			if response["id"] != "resp_visible" || relayObject(response["usage"])["input_tokens"] != json.Number("9007199254740995") || relayObject(relayObject(response["usage"])["input_tokens_details"])["cached_tokens"] != json.Number("7") {
				t.Fatalf("lost response identity/usage: %#v", response)
			}
			output := response["output"].([]any)
			call := relayObject(output[2])
			if len(output) != 3 || relayObject(output[0])["id"] != "msg_original" || call["call_id"] != "call_repaired" {
				t.Fatalf("lost progress or replacement identity: %#v", output)
			}
			if mode != "namespace_function" && (call["type"] != "custom_tool_call" || call["name"] != "functions.exec" || call["input"] != input) {
				t.Fatalf("custom input changed: %#v", call)
			}
			if mode == "namespace_function" && (call["namespace"] != "files" || call["name"] != "inspect") {
				t.Fatalf("function namespace changed: %#v", call)
			}
		})
	}
}

func TestFirstTurnToolRepairBoundAndHistoryGuard(t *testing.T) {
	for _, history := range []bool{false, true} {
		t.Run(map[bool]string{false: "second_unknown", true: "prior_tool_output"}[history], func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_bad"}})+streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_bad", "status": "completed", "output": []any{relayNativeCall("call_bad", "exec_command", map[string]any{"cmd": "secret command"})}}}))
			}))
			defer upstream.Close()
			transport := New()
			defer transport.Shutdown()
			applyConfig(t, transport, map[string]any{"responses_url": upstream.URL})
			source := executorOnlyCatalogSource(t.Name())
			if history {
				source["input"] = append(source["input"].([]any), map[string]any{"type": "custom_tool_call_output", "call_id": "call_prior", "output": "already executed"})
			}
			result := runForward(t, transport, requestFrames(t, "https://ignored.example/v1/responses", token(t, "acct"), nil, protocol.JSONBytes(source)))
			want := int32(2)
			if history {
				want = 1
			}
			if requests.Load() != want || streamFailureCode(result) != "invalid_tool_call" || strings.Contains(string(result.body), "secret command") {
				t.Fatalf("invalid retry bound or failure: attempts=%d body=%s", requests.Load(), result.body)
			}
			assertNoToolExecutionOnStreamError(t, result)
		})
	}
}

func TestToolRepairWaitKeepsHeartbeatsAndCancelsHTTP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, canceled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		close(entered)
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	source := executorOnlyCatalogSource(t.Name())
	prepared, err := protocol.PrepareResponsesBody(source, protocol.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	repair := newRelayToolRepair(req, upstream.Client(), protocol.JSONBytes(prepared), source, 1<<20)
	original := map[string]any{"id": "resp_cancel", "status": "completed", "output": []any{relayNativeCall("call_cancel", "exec_command", map[string]any{"cmd": "pwd"})}}
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(streamData(map[string]any{"type": "response.completed", "response": original})))}
	stream := newKeepaliveStreamProbe(ctx)
	finished := make(chan error, 1)
	go func() {
		finished <- sendTransformedHTTPResponseStreamWithRepair(stream, resp, 1<<20, source, 10*time.Millisecond, repair)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("correction request did not start")
	}
	waitKeepaliveFrames(t, stream, finished, 2)
	cancel()
	if err := waitKeepaliveResult(t, finished); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled correction returned %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("correction HTTP request remained alive after cancellation")
	}
	if stream.concurrent.Load() {
		t.Fatal("correction and heartbeat used concurrent Send")
	}
	for _, frame := range stream.snapshot() {
		if strings.Contains(string(frame.GetBodyChunk()), "response.failed") {
			t.Fatal("cancellation was changed to a protocol failure")
		}
	}
}

func TestToolRepairNeverRetriesSchemaOrAmbiguousCatalogErrors(t *testing.T) {
	for _, name := range []string{"structured_stream", "structured_json", "arguments", "ambiguous"} {
		t.Run(name, func(t *testing.T) {
			source := executorOnlyCatalogSource(t.Name())
			source["stream"] = name != "structured_json"
			call := relayNativeCall("call_invalid", "exec_command", map[string]any{"cmd": "pwd"})
			output := []any{call}
			if strings.HasPrefix(name, "structured_") {
				source["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
				message := structuredMessage("draft")
				relayObject(message["content"].([]any)[0])["text"] = true
				output = []any{message, call}
			} else if name == "arguments" {
				source["tools"] = []any{map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object", "required": []any{"other"}, "properties": map[string]any{"other": map[string]any{"type": "string"}}, "additionalProperties": false}}}
			} else {
				source["tools"] = []any{map[string]any{"type": "function", "name": "exec_command"}, map[string]any{"type": "custom", "name": "exec_command"}}
			}
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_schema"}})+streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_schema", "status": "completed", "output": output}}))
			}))
			defer upstream.Close()
			transport := New()
			defer transport.Shutdown()
			applyConfig(t, transport, map[string]any{"responses_url": upstream.URL})
			result := runForward(t, transport, requestFrames(t, "https://ignored.example/v1/responses", token(t, "acct"), nil, protocol.JSONBytes(source)))
			if requests.Load() != 1 {
				t.Fatalf("unrelated validation failure caused %d requests", requests.Load())
			}
			if name != "structured_json" {
				assertNoToolExecutionOnStreamError(t, result)
			}
		})
	}
}
