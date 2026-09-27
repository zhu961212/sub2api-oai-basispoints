package transport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestFunctionRelayCorrectionPreservesScopeAndRetryBound(t *testing.T) {
	for _, mode := range []struct {
		name   string
		stream bool
	}{{"json", false}, {"sse", true}} {
		for _, outcome := range []string{"success", "changed_workdir", "still_malformed", "upstream_failed"} {
			t.Run(mode.name+"/"+outcome, func(t *testing.T) {
				quote := string(byte(34))
				cmd := "printf(" + quote + "hello" + quote + ");"
				wantArgs := map[string]any{"workdir": "/safe/fixture", "timeout": 45, "cmd": cmd}
				field := func(key string, value any) string {
					return string(protocol.JSONBytes(key)) + ":" + string(protocol.JSONBytes(value))
				}
				code := "{" + field("tool", "exec_command") + "," + quote + "args" + quote + ":{" +
					field("workdir", wantArgs["workdir"]) + "," + field("timeout", wantArgs["timeout"]) + "," +
					quote + "cmd" + quote + ":" + quote + cmd + quote + "}}"
				initial := relayNativeCall("call_function_rejected", "exec_command", wantArgs)
				initial["arguments"] = string(protocol.JSONBytes(map[string]any{"code": code}))
				original := map[string]any{"id": "resp_function_original", "status": "completed", "output": []any{initial}}
				fixedArgs := map[string]any{"workdir": wantArgs["workdir"], "timeout": wantArgs["timeout"], "cmd": cmd}
				if outcome == "changed_workdir" {
					fixedArgs["workdir"] = "/other/fixture"
				}
				fixed := relayNativeCall("call_function_corrected", "exec_command", fixedArgs)
				if outcome == "still_malformed" {
					fixed["arguments"] = initial["arguments"]
				}
				if outcome == "upstream_failed" {
					original["status"] = "failed"
					original["error"] = map[string]any{"code": "invalid_tool_call", "message": "Basis Points returned a malformed or ambiguous client tool relay envelope", "type": "invalid_request_error"}
				}
				corrected := map[string]any{"id": "resp_function_internal", "status": "completed", "output": []any{fixed}}
				var attempts atomic.Int32
				requests := make(chan []byte, 4)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					raw, _ := io.ReadAll(r.Body)
					select {
					case requests <- raw:
					default:
					}
					response := original
					if attempts.Add(1) > 1 {
						response = corrected
					}
					if !mode.stream {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(protocol.JSONBytes(response))
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					call := relayObject(response["output"].([]any)[0])
					terminal := "response.completed"
					if response["status"] == "failed" {
						terminal = "response.failed"
					}
					_, _ = io.WriteString(w,
						streamData(map[string]any{"type": "response.created", "response": map[string]any{"id": response["id"], "status": "in_progress", "output": []any{}}})+
							streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": call})+
							streamData(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": call["id"], "delta": call["arguments"]})+
							streamData(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": call})+
							streamData(map[string]any{"type": terminal, "response": response})+"data: [DONE]"+string([]byte{10, 10}))
				}))
				defer upstream.Close()
				tr := New()
				defer tr.Shutdown()
				applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
				source := executorOnlyCatalogSource(t.Name())
				source["stream"] = mode.stream
				source["tools"] = []any{map[string]any{
					"type": "function", "name": "exec_command", "parameters": map[string]any{
						"type": "object", "required": []any{"cmd"}, "additionalProperties": false,
						"properties": map[string]any{"cmd": map[string]any{"type": "string"}, "workdir": map[string]any{"type": "string"}, "timeout": map[string]any{"type": "integer"}},
					},
				}}
				result := runForward(t, tr, requestFrames(t, "https://host.invalid/v1/responses", token(t, "acct-function-correction"), nil, protocol.JSONBytes(source)))
				wantAttempts := int32(2)
				if outcome == "upstream_failed" {
					wantAttempts = 1
				}
				if attempts.Load() != wantAttempts {
					t.Fatalf("attempts=%d, want %d", attempts.Load(), wantAttempts)
				}
				if wantAttempts == 2 {
					<-requests
					if body := string(<-requests); !strings.Contains(body, "No client tool was executed") || !strings.Contains(body, "call_function_rejected") {
						t.Fatal("correction lost its explicit rejected-call feedback")
					}
				}
				if outcome != "success" {
					if outcome == "upstream_failed" && !mode.stream {
						failed, err := protocol.RawObject(result.body)
						output, _ := failed["output"].([]any)
						if err != nil || result.errFrame != nil || !result.ended || failed["status"] != "failed" || relayObject(failed["error"])["code"] != "invalid_tool_call" || len(output) != 0 {
							t.Fatalf("upstream JSON failure was not preserved safely: %#v error=%v", failed, err)
						}
						return
					}
					if streamFailureCode(result) != "invalid_tool_call" {
						t.Fatalf("correction changed the failure code: %+v", result)
					}
					assertNoToolExecutionOnStreamError(t, result)
					if strings.Contains(string(result.body), "response.completed") {
						t.Fatal("rejected correction emitted completion")
					}
					return
				}
				if result.errFrame != nil || !result.ended || result.received != int64(len(result.body)) {
					t.Fatalf("correction did not complete: %+v", result)
				}
				for _, hidden := range []string{"call_function_rejected", "resp_function_internal", "run_officejs", "response.failed"} {
					if strings.Contains(string(result.body), hidden) {
						t.Fatalf("correction leaked its internal lifecycle: %s", hidden)
					}
				}
				var response map[string]any
				if mode.stream {
					counts := map[string]int{}
					for _, event := range parsedStreamEvents(t, result) {
						kind := protocol.StringValue(event["type"])
						counts[kind]++
						if kind == "response.completed" {
							response = relayObject(event["response"])
						}
					}
					for _, kind := range []string{"response.created", "response.completed", "response.output_item.added", "response.output_item.done", "response.function_call_arguments.done"} {
						if counts[kind] != 1 {
							t.Fatalf("incorrect corrected tool lifecycle: %s=%d", kind, counts[kind])
						}
					}
					if strings.Count(string(result.body), "data: [DONE]") != 1 {
						t.Fatal("correction did not close the stream once")
					}
				} else {
					var err error
					response, err = protocol.RawObject(result.body)
					if err != nil {
						t.Fatal(err)
					}
				}
				output, _ := response["output"].([]any)
				if response["id"] != "resp_function_original" || response["status"] != "completed" || len(output) != 1 {
					t.Fatalf("correction changed response identity: %#v", response)
				}
				call := relayObject(output[0])
				args, err := protocol.RawObject([]byte(protocol.StringValue(call["arguments"])))
				if err != nil || string(protocol.JSONBytes(args)) != string(protocol.JSONBytes(wantArgs)) || call["type"] != "function_call" || call["name"] != "exec_command" || call["call_id"] != "call_function_corrected" {
					t.Fatalf("correction changed function identity or unaffected arguments: %#v; error=%v", call, err)
				}
			})
		}
	}
}
