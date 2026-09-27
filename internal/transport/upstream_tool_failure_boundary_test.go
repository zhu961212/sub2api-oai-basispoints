package transport

import (
	"bytes"
	"fmt"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOrdinaryHTTPServerFailureRetainsStatusAndBytes(t *testing.T) {
	for _, transformed := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, sse := range []bool{false, true} {
				t.Run(fmt.Sprintf("transform=%t/stream=%t/sse=%t", transformed, stream, sse), func(t *testing.T) {
					payload := map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "output": []any{}, "error": map[string]any{"code": "server_error", "type": "server_error", "message": "ordinary failure; invalid_tool_call is a quoted example"}}}
					want := protocol.JSONBytes(payload)
					contentType := "application/json"
					if sse {
						want = []byte(streamData(payload))
						contentType = "text/event-stream"
					}
					var attempts atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						attempts.Add(1)
						_, _ = io.Copy(io.Discard, r.Body)
						w.Header().Set("Content-Type", contentType)
						w.Header().Set("X-Should-Retry", "false")
						w.Header().Set("X-Request-Id", "ordinary-server-failure")
						w.WriteHeader(http.StatusBadGateway)
						_, _ = w.Write(want)
					}))
					defer upstream.Close()
					tr := New()
					defer tr.Shutdown()
					applyConfig(t, tr, map[string]any{"responses_url": upstream.URL, "transform_responses": transformed})
					source := executorOnlyCatalogSource(t.Name())
					source["stream"] = stream
					result := runForward(t, tr, requestFrames(t, "https://host.invalid/v1/responses", token(t, "acct-ordinary-failure"), nil, protocol.JSONBytes(source)))
					if attempts.Load() != 1 || result.errFrame != nil || result.status != http.StatusBadGateway || !result.ended {
						t.Fatalf("ordinary server failure changed: attempts=%d status=%d rpc=%v ended=%t", attempts.Load(), result.status, result.errFrame, result.ended)
					}
					if !bytes.Equal(result.body, want) || !strings.Contains(headerValue(result.headers, "Content-Type"), contentType) || headerValue(result.headers, "X-Request-Id") != "ordinary-server-failure" {
						t.Fatalf("ordinary failure bytes or headers changed: %s", result.body)
					}
				})
			}
		}
	}
}

func TestKnownHTTPToolFailureStopsRetryingBeforeSecondPost(t *testing.T) {
	const message = "Basis Points returned a malformed or ambiguous client tool relay envelope"
	for _, retryHeader := range []string{"", "true", "false"} {
		for _, sse := range []bool{false, true} {
			t.Run(fmt.Sprintf("retry=%s/sse=%t", retryHeader, sse), func(t *testing.T) {
				payload := map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "error": map[string]any{"code": "invalid_tool_call", "message": message}}}
				var attempts atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attempts.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					if retryHeader != "" {
						w.Header().Set("X-Should-Retry", retryHeader)
					}
					if sse {
						w.Header().Set("Content-Type", "text/event-stream")
						w.WriteHeader(http.StatusBadGateway)
						_, _ = io.WriteString(w, streamData(payload))
					} else {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadGateway)
						_, _ = w.Write(protocol.JSONBytes(payload))
					}
				}))
				defer upstream.Close()
				tr := New()
				defer tr.Shutdown()
				applyConfig(t, tr, map[string]any{"responses_url": upstream.URL, "transform_responses": false})
				source := executorOnlyCatalogSource(t.Name())
				source["stream"] = true
				result := runForward(t, tr, requestFrames(t, "https://host.invalid/v1/responses", token(t, "acct-known-failure"), nil, protocol.JSONBytes(source)))
				if attempts.Load() != 1 {
					t.Fatalf("known tool terminal reposted %d times", attempts.Load())
				}
				assertCanonicalHTTPToolFailure(t, result, true, message)
			})
		}
	}
}
