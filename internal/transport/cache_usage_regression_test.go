package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// Cache usage is provider accounting, not a hit the relay can infer. Exercise
// every response conversion and retain explicit zeroes and unknown details.
func TestForwardPreservesCacheUsageAcrossResponseFormats(t *testing.T) {
	for _, usageCase := range []struct {
		name  string
		usage map[string]any
	}{
		{"read_and_write", map[string]any{
			"input_tokens": 1000, "output_tokens": 17, "total_tokens": 1017,
			"input_tokens_details":  map[string]any{"cached_tokens": 640, "cache_write_tokens": 256, "provider_detail": map[string]any{"label": "keep", "enabled": true}},
			"output_tokens_details": map[string]any{"reasoning_tokens": 11},
			"provider_usage":        map[string]any{"cost": "0.0025", "sequence": json.Number("9007199254740993"), "tags": []any{"cache", nil}},
		}},
		{"explicit_zero", map[string]any{
			"input_tokens": 1000, "output_tokens": 2, "total_tokens": 1002,
			"input_tokens_details":    map[string]any{"cached_tokens": 0, "cache_write_tokens": 0},
			"cache_read_input_tokens": 49, "cache_creation_input_tokens": 51,
		}},
		{"absent_cache_fields", map[string]any{
			"input_tokens": 1000, "output_tokens": 2, "total_tokens": 1002,
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		}},
	} {
		for _, clientStream := range []bool{false, true} {
			for _, upstreamSSE := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/client_stream=%t/upstream_sse=%t", usageCase.name, clientStream, upstreamSSE), func(t *testing.T) {
					response := map[string]any{
						"id": "resp_cache_usage", "status": "completed", "usage": usageCase.usage,
						"output": []any{relayNativeCall("call_cache_usage", "functions.exec", "text(1)")},
					}
					got, attempts := forwardCacheUsageResponses(t, []map[string]any{response}, clientStream, upstreamSSE)
					if attempts != 1 {
						t.Fatalf("ordinary cache accounting triggered %d upstream requests", attempts)
					}
					if !bytes.Equal(protocol.JSONBytes(got["usage"]), protocol.JSONBytes(usageCase.usage)) {
						t.Fatalf("cache usage changed: got %s, want %s", protocol.JSONBytes(got["usage"]), protocol.JSONBytes(usageCase.usage))
					}
					output, _ := got["output"].([]any)
					if len(output) != 1 || relayObject(output[0])["type"] != "custom_tool_call" || relayObject(output[0])["name"] != "functions.exec" {
						t.Fatalf("fixture did not exercise tool response conversion: %#v", output)
					}
				})
			}
		}
	}
}

// A real corrective inference must be billed once. Repeated SSE snapshots
// must not multiply either inference or convert cache writes into reads.
func TestToolRepairCacheUsageCountsEachInferenceOnce(t *testing.T) {
	for _, clientStream := range []bool{false, true} {
		for _, upstreamSSE := range []bool{false, true} {
			t.Run(fmt.Sprintf("client_stream=%t/upstream_sse=%t", clientStream, upstreamSSE), func(t *testing.T) {
				original := map[string]any{
					"id": "resp_cache_original", "status": "completed",
					"output": []any{relayNativeCall("call_cache_wrong", "exec_command", map[string]any{"cmd": "pwd"})},
					"usage":  map[string]any{"input_tokens": 1000, "output_tokens": 17, "total_tokens": 1017, "input_tokens_details": map[string]any{"cached_tokens": 640, "cache_write_tokens": 256}},
				}
				repaired := map[string]any{
					"id": "resp_cache_repair", "status": "completed",
					"output": []any{relayNativeCall("call_cache_fixed", "functions.exec", "text(1)")},
					"usage":  map[string]any{"input_tokens": 1040, "output_tokens": 13, "total_tokens": 1053, "input_tokens_details": map[string]any{"cached_tokens": 1000, "cache_write_tokens": 32}},
				}
				got, attempts := forwardCacheUsageResponses(t, []map[string]any{original, repaired}, clientStream, upstreamSSE)
				wantUsage := map[string]any{"input_tokens": 2040, "output_tokens": 30, "total_tokens": 2070, "input_tokens_details": map[string]any{"cached_tokens": 1640, "cache_write_tokens": 288}}
				if attempts != 2 || got["id"] != original["id"] || !bytes.Equal(protocol.JSONBytes(got["usage"]), protocol.JSONBytes(wantUsage)) {
					t.Fatalf("repair did not account for each inference once: attempts=%d response=%s", attempts, protocol.JSONBytes(got))
				}
			})
		}
	}
}

func forwardCacheUsageResponses(t *testing.T, responses []map[string]any, clientStream, upstreamSSE bool) (map[string]any, int32) {
	t.Helper()
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		index := int(attempts.Add(1)) - 1
		if index >= len(responses) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		response := responses[index]
		if !upstreamSSE {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(protocol.JSONBytes(response))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		progress := map[string]any{"id": response["id"], "status": "in_progress", "output": []any{}, "usage": response["usage"]}
		_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.created", "response": progress})+
			streamData(map[string]any{"type": "response.in_progress", "response": progress})+
			streamData(map[string]any{"type": "response.completed", "response": response})+"data: [DONE]\n\n")
	}))
	defer upstream.Close()
	tr := New()
	defer tr.Shutdown()
	applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
	source := executorOnlyCatalogSource(t.Name())
	source["stream"] = clientStream
	result := runForward(t, tr, requestFrames(t, "https://host.invalid/v1/responses", token(t, "acct-cache-usage"), nil, protocol.JSONBytes(source)))
	if result.errFrame != nil || result.status != http.StatusOK || !result.ended || result.received != int64(len(result.body)) {
		t.Fatalf("cache usage forwarding failed: %+v", result)
	}
	if !clientStream {
		response, err := protocol.RawObject(result.body)
		if err != nil {
			t.Fatal(err)
		}
		return response, attempts.Load()
	}
	var response map[string]any
	terminals := 0
	for _, event := range parsedStreamEvents(t, result) {
		if event["type"] == "response.completed" {
			terminals++
			response = relayObject(event["response"])
		}
	}
	if terminals != 1 {
		t.Fatalf("stream emitted %d completed responses, want one accounting terminal", terminals)
	}
	return response, attempts.Load()
}
