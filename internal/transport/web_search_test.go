package transport

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestHostedWebSearchUsesNativeRoute(t *testing.T) {
	for _, test := range []struct {
		name   string
		fields map[string]any
	}{
		{"default", map[string]any{"tools": []any{map[string]any{"type": "web_search"}}}},
		{"live with filters", map[string]any{"tools": []any{map[string]any{"type": "web_search", "external_web_access": true, "filters": map[string]any{"allowed_domains": []string{"example.com"}}}}}},
		{"preview high context", map[string]any{"tools": []any{map[string]any{"type": "web_search_preview", "search_context_size": "high"}}}},
		{"forced", map[string]any{"tool_choice": map[string]any{"type": "web_search_preview"}}},
		{"namespace", map[string]any{"tools": []any{map[string]any{"type": "namespace", "name": "web", "tools": []any{map[string]any{"type": "web_search"}}}}}},
		{"additional tools", map[string]any{"input": []any{map[string]any{"type": "additional_tools", "tools": []any{map[string]any{"type": "web_search"}}}}}},
		{"discovered tool", map[string]any{"input": []any{map[string]any{"type": "tool_search_output", "status": "completed", "tools": []any{map[string]any{"type": "web_search"}}}}}},
		{"history with tools disabled", map[string]any{"tool_choice": "none", "input": []any{map[string]any{"type": "web_search_call", "id": "ws_history", "status": "completed", "action": map[string]any{"type": "search", "query": "historical query"}}}}},
	} {
		for _, rewrite := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/rewrite=%t/stream=%t", test.name, rewrite, stream), func(t *testing.T) {
					source := map[string]any{"model": protocol.DefaultModelID, "input": "search online", "stream": stream, "include": []string{"web_search_call.action.sources"}}
					for key, value := range test.fields {
						source[key] = value
					}
					body := protocol.JSONBytes(source)
					nativeResponse := map[string]any{
						"id": "resp_native_search", "status": "completed", "output": []any{
							map[string]any{"type": "web_search_call", "id": "ws_native", "status": "completed", "action": map[string]any{"type": "search", "query": "test"}},
							map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "source", "annotations": []any{map[string]any{"type": "url_citation", "url": "https://example.com", "title": "Source", "start_index": 0, "end_index": 6}}}}},
						},
					}
					response, contentType := protocol.JSONBytes(nativeResponse), "application/json"
					if stream {
						response = []byte(streamData(map[string]any{"type": "response.completed", "response": nativeResponse}))
						contentType = "text/event-stream"
					}
					var nativeHits, bpsHits atomic.Int32
					native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						nativeHits.Add(1)
						got, err := io.ReadAll(r.Body)
						if err != nil || !bytes.Equal(got, body) {
							t.Errorf("native search request changed: %s, %v", got, err)
						}
						if r.Header.Get("Authorization") != "Bearer host-token" || r.Header.Get("X-Basispoints-Auth-Mode") != "" {
							t.Error("native search received BPS authentication changes")
						}
						w.Header().Set("Content-Type", contentType)
						_, _ = w.Write(response)
					}))
					defer native.Close()
					basis := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						bpsHits.Add(1)
						w.WriteHeader(http.StatusBadGateway)
					}))
					defer basis.Close()
					tr := New()
					defer tr.Shutdown()
					applyConfig(t, tr, map[string]any{"responses_url": basis.URL, "rewrite_tools": rewrite, "transform_responses": true})
					result := runForward(t, tr, requestFrames(t, native.URL, "host-token", nil, body))
					if result.errFrame != nil || result.status != http.StatusOK || !result.ended || !bytes.Equal(result.body, response) || nativeHits.Load() != 1 || bpsHits.Load() != 0 {
						t.Fatalf("hosted search not forwarded intact: result=%+v native=%d bps=%d", result, nativeHits.Load(), bpsHits.Load())
					}
				})
			}
		}
	}
}

func TestHostedWebSearchNative403DoesNotDisableBPS(t *testing.T) {
	fixture := newBPSForbiddenForwardFixture(t, true, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(protocol.JSONBytes(map[string]any{"id": "resp_bps_search_client", "status": "completed", "output": []any{}}))
	})
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "native search forbidden")
	}))
	defer native.Close()
	fixture.hostURL = native.URL
	first := fixture.forward(t, 7, map[string]any{"model": protocol.DefaultModelID, "input": "search", "tools": []any{map[string]any{"type": "web_search"}}})
	if first.status != http.StatusForbidden || first.errFrame != nil || string(first.body) != "native search forbidden" || fixture.disabled(7) || fixture.bpsCalls.Load() != 0 {
		t.Fatalf("native search 403 affected BPS state: result=%+v disabled=%t bps=%d", first, fixture.disabled(7), fixture.bpsCalls.Load())
	}
	second := fixture.forward(t, 7, map[string]any{"model": protocol.DefaultModelID, "input": "client search", "tools": []any{map[string]any{"type": "function", "name": "web_search", "parameters": map[string]any{"type": "object"}}}})
	if second.errFrame != nil || second.status != http.StatusOK || !strings.Contains(string(second.body), "resp_bps_search_client") || fixture.disabled(7) || fixture.bpsCalls.Load() != 1 {
		t.Fatalf("client search failed to retain BPS after native 403: %+v", second)
	}
}

func TestHostedWebSearchBypassesUnavailableBPSRoutingState(t *testing.T) {
	fixture := newBPSForbiddenForwardFixture(t, true, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	fixture.transport.autoDegradation.mu.Lock()
	fixture.transport.autoDegradation.bound = true
	fixture.transport.autoDegradation.loaded = false
	fixture.transport.autoDegradation.mu.Unlock()
	fixture.transport.bpsAccounts.mu.Lock()
	fixture.transport.bpsAccounts.host = newBPSKVTestHost()
	fixture.transport.bpsAccounts.loaded = false
	fixture.transport.bpsAccounts.mu.Unlock()
	result := fixture.forward(t, 7, map[string]any{"model": protocol.DefaultModelID, "input": "search", "tools": []any{map[string]any{"type": "web_search"}}})
	if result.errFrame != nil || result.status != http.StatusOK || fixture.hostCalls.Load() != 1 || fixture.bpsCalls.Load() != 0 {
		t.Fatalf("BPS storage readiness blocked native search: result=%+v native=%d bps=%d", result, fixture.hostCalls.Load(), fixture.bpsCalls.Load())
	}
}

func TestDisabledHostedSearchAndClientSearchToolsRetainBPS(t *testing.T) {
	for _, test := range []struct {
		name   string
		source map[string]any
	}{
		{"disabled hosted search", map[string]any{"tool_choice": "none", "tools": []any{map[string]any{"type": "web_search", "external_web_access": true}}}},
		{"client function", map[string]any{"tools": []any{map[string]any{"type": "function", "name": "web_search", "parameters": map[string]any{"type": "object"}}}}},
		{"client custom", map[string]any{"tools": []any{map[string]any{"type": "custom", "name": "web_search"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBPSForbiddenForwardFixture(t, true, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(protocol.JSONBytes(map[string]any{"id": "resp_bps_ok", "status": "completed", "output": []any{}}))
			})
			test.source["model"], test.source["input"] = protocol.DefaultModelID, "test search routing"
			result := fixture.forward(t, 7, test.source)
			if result.errFrame != nil || result.status != http.StatusOK || fixture.bpsCalls.Load() != 1 || fixture.hostCalls.Load() != 0 {
				t.Fatalf("client/disabled search incorrectly left BPS: result=%+v bps=%d native=%d", result, fixture.bpsCalls.Load(), fixture.hostCalls.Load())
			}
		})
	}
}
