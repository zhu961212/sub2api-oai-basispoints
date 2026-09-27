package transport

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func runOuterSyntaxRelay(t *testing.T, arguments string, stream, upstreamSSE bool) (forwardResult, int32) {
	t.Helper()
	native := relayNativeCall("call_outer_syntax", "functions.exec", "unused")
	native["arguments"] = arguments
	response := map[string]any{"id": "resp_outer_syntax", "status": "completed", "output": []any{native}}
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if !upstreamSSE {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(protocol.JSONBytes(response))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			streamData(map[string]any{"type": "response.created", "response": map[string]any{"id": response["id"], "status": "in_progress", "output": []any{}}})+
				streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": native})+
				streamData(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": native["id"], "delta": arguments})+
				streamData(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": native})+
				streamData(map[string]any{"type": "response.completed", "response": response})+
				"data: [DONE]\n\n")
	}))
	defer upstream.Close()
	tr := New()
	defer tr.Shutdown()
	applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
	source := executorOnlyCatalogSource(t.Name())
	source["stream"] = stream
	result := runForward(t, tr, requestFrames(t, "https://host.invalid/v1/responses", token(t, "acct-outer-syntax"), nil, protocol.JSONBytes(source)))
	return result, attempts.Load()
}

func TestOuterRelayStringSyntaxRecoversAcrossResponseFormats(t *testing.T) {
	slash := string(byte(92))
	input := "  text(1);" + string([]byte{13, 10, 9}) + "// pattern " + slash + "d+" + slash + "s; literal " + slash + slash + "n  "
	for _, codeString := range []bool{false, true} {
		envelope := map[string]any{"tool": "functions.exec", "args": input}
		var code any = envelope
		layers := 1
		if codeString {
			code = string(protocol.JSONBytes(envelope))
			layers = 2
		}
		valid := string(protocol.JSONBytes(map[string]any{"summary": "first second", "code": code}))
		for _, syntax := range []struct{ name, arguments string }{
			{"summary_newline", strings.Replace(valid, "first second", "first"+string(byte(10))+"second", 1)},
			{"summary_tab", strings.Replace(valid, "first second", "first"+string(byte(9))+"second", 1)},
			{"summary_escape", strings.Replace(valid, "first second", "inspect C:"+slash+"project", 1)},
			{"input_newline", strings.Replace(valid, strings.Repeat(slash, layers)+"n", string(byte(10)), 1)},
			{"input_tab", strings.Replace(valid, strings.Repeat(slash, layers)+"t", string(byte(9)), 1)},
			{"input_escape", strings.Replace(valid, strings.Repeat(slash, 2*layers)+"d", strings.Repeat(slash, 2*layers-1)+"d", 1)},
		} {
			for _, stream := range []bool{false, true} {
				for _, upstreamSSE := range []bool{false, true} {
					t.Run(fmt.Sprintf("code_string=%t/%s/stream=%t/upstream_sse=%t", codeString, syntax.name, stream, upstreamSSE), func(t *testing.T) {
						if syntax.arguments == valid {
							t.Fatal("fixture did not introduce the intended malformed outer syntax")
						}
						result, attempts := runOuterSyntaxRelay(t, syntax.arguments, stream, upstreamSSE)
						if attempts != 1 || result.errFrame != nil || !result.ended || result.received != int64(len(result.body)) {
							t.Fatalf("deterministic recovery retried or failed: attempts=%d result=%+v", attempts, result)
						}
						if strings.Contains(string(result.body), "run_officejs") || strings.Contains(string(result.body), "response.failed") {
							t.Fatalf("native relay or failure leaked: %s", result.body)
						}
						response := outerSyntaxCompletedResponse(t, result, input, stream)
						output, _ := response["output"].([]any)
						if response["id"] != "resp_outer_syntax" || response["status"] != "completed" || len(output) != 1 {
							t.Fatalf("recovery lost response identity or returned multiple tools: %#v", response)
						}
						call := relayObject(output[0])
						if call["type"] != "custom_tool_call" || call["name"] != "functions.exec" || call["call_id"] != "call_outer_syntax" || call["input"] != input {
							t.Fatalf("recovery changed executable tool identity or input: %#v", call)
						}
					})
				}
			}
		}
	}
}

func outerSyntaxCompletedResponse(t *testing.T, result forwardResult, input string, stream bool) map[string]any {
	t.Helper()
	if !stream {
		response, err := protocol.RawObject(result.body)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	var response map[string]any
	created, completed, added, done, inputsDone := 0, 0, 0, 0, 0
	var deltas strings.Builder
	var doneInput string
	for _, event := range parsedStreamEvents(t, result) {
		switch event["type"] {
		case "response.created":
			created++
		case "response.completed":
			completed++
			response = relayObject(event["response"])
		case "response.output_item.added":
			added++
		case "response.output_item.done":
			done++
		case "response.custom_tool_call_input.delta":
			delta, _ := event["delta"].(string)
			deltas.WriteString(delta)
		case "response.custom_tool_call_input.done":
			inputsDone++
			doneInput, _ = event["input"].(string)
		}
	}
	if created != 1 || completed != 1 || added != 1 || done != 1 || inputsDone != 1 || doneInput != input || deltas.Len() > 0 && deltas.String() != input || strings.Count(string(result.body), "data: [DONE]") != 1 {
		t.Fatalf("recovered stream duplicated or changed the tool lifecycle: %s", result.body)
	}
	return response
}

func TestOuterRelayStringRecoveryRejectsUnsafeSyntaxAcrossResponseFormats(t *testing.T) {
	const private = "PRIVATE OUTER INPUT"
	quote := string(byte(34))
	for _, codeString := range []bool{false, true} {
		envelope := map[string]any{"tool": "functions.exec", "args": private}
		var code any = envelope
		if codeString {
			code = string(protocol.JSONBytes(envelope))
		}
		codeJSON := string(protocol.JSONBytes(code))
		valid := string(protocol.JSONBytes(map[string]any{"summary": "inspect C:PROJECT", "code": code}))
		valid = strings.Replace(valid, "C:PROJECT", "C:"+string(byte(92))+"project", 1)
		prefix := strings.TrimSuffix(valid, "}")
		for _, invalid := range []struct{ name, arguments string }{
			{"duplicate_code", prefix + "," + quote + "code" + quote + ":" + codeJSON + "}"},
			{"truncated_object", prefix},
			{"truncated_string", prefix + "," + quote + "detail" + quote + ":" + quote + "unfinished"},
			{"multiple_objects", valid + " " + valid},
		} {
			for _, stream := range []bool{false, true} {
				for _, upstreamSSE := range []bool{false, true} {
					t.Run(fmt.Sprintf("code_string=%t/%s/stream=%t/upstream_sse=%t", codeString, invalid.name, stream, upstreamSSE), func(t *testing.T) {
						result, attempts := runOuterSyntaxRelay(t, invalid.arguments, stream, upstreamSSE)
						if attempts != 1 || streamFailureCode(result) != "invalid_tool_call" {
							t.Fatalf("unsafe outer syntax was executed or retried: attempts=%d result=%+v", attempts, result)
						}
						assertNoToolExecutionOnStreamError(t, result)
						if strings.Contains(string(result.body), private) || strings.Contains(string(result.body), "response.completed") {
							t.Fatalf("rejected outer input leaked or completed: %s", result.body)
						}
						if stream && upstreamSSE {
							events := parsedStreamEvents(t, result)
							if len(events) != 2 || events[len(events)-1]["type"] != "response.failed" || strings.Count(string(result.body), "data: [DONE]") != 1 {
								t.Fatalf("rejected stream has an invalid failure lifecycle: %s", result.body)
							}
						}
					})
				}
			}
		}
	}
}

func TestQuotedTruncatedRelayCorrectionAcrossResponseFormats(t *testing.T) {
	const private = "PRIVATE TRUNCATED INPUT"
	input := "  text('corrected');" + string([]byte{13, 10, 9}) + "  "
	for _, missingQuote := range []bool{false, true} {
		for _, depth := range []int{1, 3} {
			// Put the tool first so truncating its args still identifies it.
			quote := string(byte(34))
			code := "{" + quote + "tool" + quote + ":" + quote + "functions.exec" + quote + "," + quote + "args" + quote + ":" + quote + private
			if !missingQuote {
				code += quote
			}
			for layer := 0; layer < depth; layer++ {
				code = string(protocol.JSONBytes(code))
			}
			for _, stream := range []bool{false, true} {
				for _, upstreamSSE := range []bool{false, true} {
					t.Run(fmt.Sprintf("missing_quote=%t/depth=%d/stream=%t/upstream_sse=%t", missingQuote, depth, stream, upstreamSSE), func(t *testing.T) {
						invalid := relayNativeCall("call_rejected", "functions.exec", private)
						invalid["arguments"] = string(protocol.JSONBytes(map[string]any{"summary": "relay", "code": code}))
						original := map[string]any{"id": "resp_outer_syntax", "status": "completed", "output": []any{invalid}}
						fixed := map[string]any{"id": "resp_internal_correction", "status": "completed", "output": []any{relayNativeCall("call_safe", "functions.exec", input)}}
						var attempts atomic.Int32
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							_, _ = io.Copy(io.Discard, r.Body)
							response := original
							if attempts.Add(1) == 2 {
								response = fixed
							}
							if !upstreamSSE {
								w.Header().Set("Content-Type", "application/json")
								_, _ = w.Write(protocol.JSONBytes(response))
								return
							}
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.created", "response": map[string]any{"id": response["id"], "status": "in_progress", "output": []any{}}})+
								streamData(map[string]any{"type": "response.completed", "response": response})+"data: [DONE]\n\n")
						}))
						defer upstream.Close()
						tr := New()
						defer tr.Shutdown()
						applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
						source := executorOnlyCatalogSource(t.Name())
						source["stream"] = stream
						result := runForward(t, tr, requestFrames(t, "https://host.invalid/v1/responses", token(t, "acct-quoted-correction"), nil, protocol.JSONBytes(source)))
						if attempts.Load() != 2 || result.errFrame != nil || !result.ended {
							t.Fatalf("quoted truncation was not corrected once: attempts=%d result=%+v", attempts.Load(), result)
						}
						for _, rejected := range []string{private, "call_rejected", "resp_internal_correction", "response.failed", "run_officejs"} {
							if strings.Contains(string(result.body), rejected) {
								t.Fatalf("correction leaked rejected call or internal lifecycle: %s", result.body)
							}
						}
						response := outerSyntaxCompletedResponse(t, result, input, stream)
						output, _ := response["output"].([]any)
						if response["id"] != "resp_outer_syntax" || response["status"] != "completed" || len(output) != 1 {
							t.Fatalf("correction changed visible response identity: %#v", response)
						}
						call := relayObject(output[0])
						if call["type"] != "custom_tool_call" || call["name"] != "functions.exec" || call["call_id"] != "call_safe" || call["input"] != input {
							t.Fatalf("correction returned an invalid client call: %#v", call)
						}
					})
				}
			}
		}
	}
}
