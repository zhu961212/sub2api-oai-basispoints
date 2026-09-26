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

type bpsForbiddenForwardFixture struct {
	transport *Transport
	hostURL   string
	hostCalls atomic.Int32
	bpsCalls  atomic.Int32
}

func newBPSForbiddenForwardFixture(t *testing.T, transform bool, handler http.HandlerFunc) *bpsForbiddenForwardFixture {
	t.Helper()
	fixture := &bpsForbiddenForwardFixture{transport: New()}
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fixture.hostCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(protocol.JSONBytes(map[string]any{"id": "resp_host_passthrough", "status": "completed", "output": []any{}}))
	}))
	basis := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.bpsCalls.Add(1)
		handler(w, r)
	}))
	fixture.hostURL = host.URL
	t.Cleanup(host.Close)
	t.Cleanup(basis.Close)
	t.Cleanup(fixture.transport.Shutdown)
	applyConfig(t, fixture.transport, map[string]any{"responses_url": basis.URL, "transform_responses": transform, "account_ids": []int64{7, 8}})
	return fixture
}

func (f *bpsForbiddenForwardFixture) forward(t *testing.T, accountID int64, source map[string]any) forwardResult {
	t.Helper()
	frames := requestFrames(t, f.hostURL, token(t, fmt.Sprintf("fixture-account-%d", accountID)), nil, protocol.JSONBytes(source))
	frames[0].GetStart().AccountId = accountID
	return runForward(t, f.transport, frames)
}

func (f *bpsForbiddenForwardFixture) disabled(accountID int64) bool {
	f.transport.mu.RLock()
	cfg := f.transport.cfg
	f.transport.mu.RUnlock()
	return f.transport.isBPSAccountDisabled(accountID, cfg)
}

func TestBPSForbiddenForwardFirstHTTP403DisablesOnlyItsAccount(t *testing.T) {
	for _, body := range []string{"", "arbitrary text", "{\"error\":{\"code\":\"forbidden\"}}", "{\"error\":{\"code\":\"basispoints_model_access_changed\"}}", "{\"error\":{\"code\":\"basispoints_upstream_error\"}}"} {
		t.Run(fmt.Sprintf("body_%q", body), func(t *testing.T) {
			fixture := newBPSForbiddenForwardFixture(t, true, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.Header.Get("Chatgpt-Account-Id"), "fixture-account-8") {
					_, _ = w.Write(protocol.JSONBytes(map[string]any{"id": "resp_bps_account8", "status": "completed", "output": []any{}}))
					return
				}
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, body)
			})
			source := map[string]any{"model": "gpt-6-astra", "input": "hi"}
			first := fixture.forward(t, 7, source)
			if first.errFrame == nil || !first.errFrame.GetRequestSent() || !fixture.disabled(7) || fixture.disabled(8) {
				t.Fatalf("first403 did not isolate account7 immediately: result=%+v disabled7=%t disabled8=%t", first, fixture.disabled(7), fixture.disabled(8))
			}
			second := fixture.forward(t, 7, source)
			if second.errFrame != nil || !strings.Contains(string(second.body), "resp_host_passthrough") || fixture.hostCalls.Load() != 1 || fixture.bpsCalls.Load() != 1 {
				t.Fatalf("second request did not bypass BPS: result=%+v host=%d bps=%d", second, fixture.hostCalls.Load(), fixture.bpsCalls.Load())
			}
			other := fixture.forward(t, 8, source)
			if other.errFrame != nil || !strings.Contains(string(other.body), "resp_bps_account8") || fixture.disabled(8) || fixture.hostCalls.Load() != 1 || fixture.bpsCalls.Load() != 2 {
				t.Fatalf("account8 routing was affected: result=%+v host=%d bps=%d", other, fixture.hostCalls.Load(), fixture.bpsCalls.Load())
			}
		})
	}
}

func TestBPSForbiddenForwardKeepsNon403AccountsSelected(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   map[string]any
	}{
		{"http401", 401, map[string]any{"error": map[string]any{"code": "invalid_api_key"}}},
		{"http429", 429, map[string]any{"error": map[string]any{"code": "rate_limit_exceeded"}}},
		{"http200success", 200, map[string]any{"id": "resp_ok", "status": "completed", "output": []any{}}},
		{"ordinary_message403", 200, map[string]any{"id": "resp_ok", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "HTTP403 forbidden permission"}}}}}},
		{"keyword_only_failure", 200, map[string]any{"id": "resp_fail", "status": "failed", "error": map[string]any{"code": "workspace_suspended", "message": "HTTP403 permission"}, "output": []any{}}},
		{"error400_permission_word", 200, map[string]any{"id": "resp_fail", "status": "failed", "error": map[string]any{"status_code": 400, "message": "not a permission issue"}, "output": []any{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBPSForbiddenForwardFixture(t, true, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write(protocol.JSONBytes(test.body))
			})
			source := map[string]any{"model": "gpt-6-astra", "input": "hi"}
			fixture.forward(t, 7, source)
			fixture.forward(t, 7, source)
			if fixture.disabled(7) || fixture.bpsCalls.Load() != 2 || fixture.hostCalls.Load() != 0 {
				t.Fatalf("non403 changed routing: disabled=%t bps=%d host=%d", fixture.disabled(7), fixture.bpsCalls.Load(), fixture.hostCalls.Load())
			}
		})
	}
}

func TestBPSForbiddenForwardIgnoresNativePassthrough403(t *testing.T) {
	fixture := newBPSForbiddenForwardFixture(t, true, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(protocol.JSONBytes(map[string]any{"id": "resp_bps_ok", "status": "completed", "output": []any{}}))
	})
	var nativeCalls atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nativeCalls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "native host403")
	}))
	defer native.Close()
	fixture.hostURL = native.URL
	first := fixture.forward(t, 7, map[string]any{"model": "gpt-5.4", "input": "native request"})
	if first.status != 403 || first.errFrame != nil || fixture.disabled(7) || nativeCalls.Load() != 1 || fixture.bpsCalls.Load() != 0 {
		t.Fatalf("native403 changed BPS state: %+v", first)
	}
	second := fixture.forward(t, 7, map[string]any{"model": "gpt-6-astra", "input": "BPS request"})
	if second.errFrame != nil || !strings.Contains(string(second.body), "resp_bps_ok") || fixture.disabled(7) || fixture.bpsCalls.Load() != 1 {
		t.Fatalf("native403 blocked later BPS request: %+v", second)
	}
}

func TestBPSForbiddenForwardExplicitJSONAndSSE403Disable(t *testing.T) {
	for _, wire := range []string{"json", "sse"} {
		for _, transform := range []bool{false, true} {
			for _, status := range []any{403, "403"} {
				t.Run(fmt.Sprintf("%s/transform_%t/status_%v", wire, transform, status), func(t *testing.T) {
					fixture := newBPSForbiddenForwardFixture(t, transform, func(w http.ResponseWriter, _ *http.Request) {
						failure := map[string]any{"id": "resp_explicit403", "status": "failed", "output": []any{}, "error": map[string]any{"status_code": status, "code": "any_error_code"}}
						if wire == "sse" {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.failed", "response": failure}))
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write(protocol.JSONBytes(failure))
						}
					})
					source := map[string]any{"model": "gpt-6-astra", "input": "hi", "stream": wire == "sse"}
					fixture.forward(t, 7, source)
					if !fixture.disabled(7) {
						t.Fatal("explicit403 was not disabled on the first request")
					}
					second := fixture.forward(t, 7, source)
					if second.errFrame != nil || !strings.Contains(string(second.body), "resp_host_passthrough") || fixture.bpsCalls.Load() != 1 || fixture.hostCalls.Load() != 1 {
						t.Fatalf("explicit403 did not route the next request to the host: %+v bps=%d host=%d", second, fixture.bpsCalls.Load(), fixture.hostCalls.Load())
					}
				})
			}
		}
	}
}
