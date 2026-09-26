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

func TestMalformedRelayEnvelopeCanBeCorrectedWithoutExecutingIt(t *testing.T) {
	for _, malformed := range []string{"truncated_string", "truncated_object"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", malformed, stream), func(t *testing.T) {
				source := executorOnlyCatalogSource(t.Name())
				source["stream"] = stream
				invalid := relayNativeCall("call_rejected", "functions.exec", "PRIVATE INVALID INPUT")
				code := "{\"tool\":\"functions.exec\",\"args\":\"PRIVATE INVALID INPUT"
				if malformed == "truncated_object" {
					code += "\""
				}
				invalid["arguments"] = string(protocol.JSONBytes(map[string]any{"summary": "inspect", "code": code}))
				original := map[string]any{"id": "resp_first", "status": "completed", "output": []any{invalid}, "usage": map[string]any{"input_tokens": 9, "output_tokens": 2}}
				fixed := map[string]any{"id": "resp_second", "status": "completed", "output": []any{relayNativeCall("call_safe", "functions.exec", "text('corrected');")}, "usage": map[string]any{"input_tokens": 5, "output_tokens": 1}}
				var attempts atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					response := original
					if attempts.Add(1) == 2 {
						response = fixed
					}
					_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.created", "response": map[string]any{"id": response["id"], "status": "in_progress", "output": []any{}}})+streamData(map[string]any{"type": "response.completed", "response": response}))
				}))
				defer upstream.Close()
				tr := New()
				defer tr.Shutdown()
				applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
				result := runForward(t, tr, requestFrames(t, "https://host.invalid/v1/responses", token(t, "acct-envelope"), nil, protocol.JSONBytes(source)))
				if attempts.Load() != 2 || result.errFrame != nil || !result.ended {
					t.Fatalf("bounded correction failed: attempts=%d result=%+v", attempts.Load(), result)
				}
				body := string(result.body)
				for _, invalid := range []string{"PRIVATE INVALID INPUT", "AMBIGUOUS INPUT", "call_rejected", "response.failed"} {
					if strings.Contains(body, invalid) {
						t.Fatalf("rejected envelope leaked: %s", body)
					}
				}
				if !strings.Contains(body, "call_safe") || !strings.Contains(body, "text('corrected');") {
					t.Fatalf("corrected declared tool missing: %s", body)
				}
			})
		}
	}
}

func TestMalformedRelayCorrectionRemainsBoundedAndHistoryScoped(t *testing.T) {
	for _, priorTool := range []bool{false, true} {
		t.Run(fmt.Sprintf("prior_tool=%t", priorTool), func(t *testing.T) {
			source := executorOnlyCatalogSource(t.Name())
			source["stream"] = true
			if priorTool {
				source["input"] = append(source["input"].([]any), map[string]any{"type": "custom_tool_call_output", "call_id": "call_prior", "output": "executed"})
			}
			invalid := relayNativeCall("call_rejected", "run_officejs", "PRIVATE INVALID INPUT")
			invalid["arguments"] = string(protocol.JSONBytes(map[string]any{"summary": "inspect", "code": "{\"tool\":\"functions.exec\",\"args\":\"PRIVATE INVALID INPUT"}))
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_invalid", "status": "completed", "output": []any{invalid}}}))
			}))
			defer upstream.Close()
			tr := New()
			defer tr.Shutdown()
			applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
			result := runForward(t, tr, requestFrames(t, "https://host.invalid/v1/responses", token(t, "acct-envelope"), nil, protocol.JSONBytes(source)))
			expected := int32(2)
			if priorTool {
				expected = 1
			}
			if attempts.Load() != expected || streamFailureCode(result) != "invalid_tool_call" {
				t.Fatalf("correction bound/history changed: attempts=%d expected=%d result=%+v", attempts.Load(), expected, result)
			}
			assertNoToolExecutionOnStreamError(t, result)
		})
	}
}

func TestAmbiguousRelayEnvelopesNeverTriggerCorrection(t *testing.T) {
	for _, code := range []string{
		string(protocol.JSONBytes(map[string]any{"tool": "functions.exec", "args": "PRIVATE INPUT", "input": "OTHER INPUT"})),
		string(protocol.JSONBytes(map[string]any{"tool": "run_officejs", "args": map[string]any{"code": "PRIVATE INPUT"}})),
	} {
		source := executorOnlyCatalogSource(t.Name())
		source["stream"] = true
		invalid := relayNativeCall("call_rejected", "functions.exec", nil)
		invalid["arguments"] = string(protocol.JSONBytes(map[string]any{"summary": "inspect", "code": code}))
		var attempts atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_ambiguous", "status": "completed", "output": []any{invalid}}}))
		}))
		tr := New()
		applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
		result := runForward(t, tr, requestFrames(t, "https://host.invalid/v1/responses", token(t, "acct-envelope"), nil, protocol.JSONBytes(source)))
		tr.Shutdown()
		upstream.Close()
		if attempts.Load() != 1 || streamFailureCode(result) != "invalid_tool_call" {
			t.Fatalf("ambiguous envelope retried: attempts=%d result=%+v", attempts.Load(), result)
		}
		assertNoToolExecutionOnStreamError(t, result)
	}
}
