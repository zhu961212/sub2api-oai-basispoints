package transport

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestForcedClientToolChoiceUsesNativeRoute(t *testing.T) {
	for _, declaration := range []string{"tools", "additional_tools", "tool_search_output"} {
		for _, kind := range []string{"function", "custom", "required"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", declaration, kind, stream), func(t *testing.T) {
					tool := map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "object"}}
					var choice any = map[string]any{"type": kind, "name": "inspect"}
					if kind == "required" {
						choice = "required"
					}
					if kind == "custom" {
						tool = map[string]any{"type": kind, "name": "inspect"}
					}
					source := map[string]any{"model": protocol.DefaultModelID, "input": "test exact selection", "stream": stream, "tools": []any{tool}, "tool_choice": choice}
					if declaration != "tools" {
						delete(source, "tools")
						source["input"] = []any{map[string]any{"type": declaration, "status": "completed", "tools": []any{tool}}}
					}
					body := protocol.JSONBytes(source)
					response := protocol.JSONBytes(map[string]any{"id": "resp_native_choice", "status": "completed", "output": []any{}})
					contentType := "application/json"
					if stream {
						contentType = "text/event-stream"
						response = []byte(streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_native_choice", "status": "completed", "output": []any{}}}) + "data: [DONE]\n\n")
					}
					var nativeHits, bpsHits atomic.Int32
					native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						nativeHits.Add(1)
						received, _ := io.ReadAll(r.Body)
						if !bytes.Equal(received, body) || r.Header.Get("X-Basispoints-Auth-Mode") != "" {
							t.Error("native choice request was rewritten")
						}
						w.Header().Set("Content-Type", contentType)
						_, _ = w.Write(response)
					}))
					defer native.Close()
					bps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bpsHits.Add(1); w.WriteHeader(502) }))
					defer bps.Close()
					tr := New()
					defer tr.Shutdown()
					applyConfig(t, tr, map[string]any{"responses_url": bps.URL})
					result := runForward(t, tr, requestFrames(t, native.URL, "host-token", nil, body))
					if result.errFrame != nil || result.status != 200 || !result.ended || !bytes.Equal(result.body, response) || nativeHits.Load() != 1 || bpsHits.Load() != 0 {
						t.Fatalf("forced choice lost native semantics: result=%+v native=%d bps=%d", result, nativeHits.Load(), bpsHits.Load())
					}
				})
			}
		}
	}
}

func TestForcedRuntimeToolChoiceRejectsInvalidDeclarations(t *testing.T) {
	for _, invalid := range []string{"in_progress", "failed", "conflicting_type", "conflicting_schema", "wrong_type", "undeclared"} {
		t.Run(invalid, func(t *testing.T) {
			function := map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "object"}}
			choice := map[string]any{"type": "function", "name": "inspect"}
			record := map[string]any{"type": "tool_search_output", "status": "completed", "tools": []any{function}}
			source := map[string]any{"model": protocol.DefaultModelID, "tool_choice": choice, "input": []any{record}}
			switch invalid {
			case "in_progress", "failed":
				record["status"] = invalid
			case "conflicting_type":
				source["tools"] = []any{map[string]any{"type": "custom", "name": "inspect"}}
			case "conflicting_schema":
				source["tools"] = []any{map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "string"}}}
			case "wrong_type":
				choice["type"] = "custom"
			case "undeclared":
				choice["name"] = "absent"
			}
			var upstreamHits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHits.Add(1)
				w.WriteHeader(502)
			}))
			defer upstream.Close()
			tr := New()
			defer tr.Shutdown()
			applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
			result := runForward(t, tr, requestFrames(t, upstream.URL, token(t, "native-choice-validation"), nil, protocol.JSONBytes(source)))
			if result.status != http.StatusBadRequest || result.errFrame != nil || !result.ended || upstreamHits.Load() != 0 || !bytes.Contains(result.body, []byte(`"code":"unsupported_capability"`)) {
				t.Fatalf("invalid declaration bypassed validation: result=%+v upstream=%d", result, upstreamHits.Load())
			}
		})
	}
}
