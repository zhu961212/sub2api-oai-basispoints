package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func rateBackoffTestHeaders() http.Header {
	return http.Header{"Authorization": {"Bearer PRIVATE-token"}, "Chatgpt-Account-Id": {"account-a"}, "X-Basispoints-Auth-Mode": {"chatgpt"}}
}

func TestBPSRetryAfterBackoffBounds(t *testing.T) {
	now := time.Date(2026, time.September, 30, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		values []string
		want   time.Duration
		valid  bool
	}{
		{"seconds", []string{"12"}, 12 * time.Second, true},
		{"trimmed", []string{" 12 "}, 12 * time.Second, true},
		{"maximum", []string{"86400"}, 24 * time.Hour, true},
		{"date", []string{now.Add(time.Minute).Format(http.TimeFormat)}, time.Minute, true},
		{"absent", nil, 0, false},
		{"zero", []string{"0"}, 0, false},
		{"negative", []string{"-2"}, 0, false},
		{"fraction", []string{"0.5"}, 0, false},
		{"invalid", []string{"PRIVATE-input"}, 0, false},
		{"multiple", []string{"5", "10"}, 0, false},
		{"duplicate", []string{"5", "5"}, 0, false},
		{"long", []string{"86401"}, 0, false},
		{"overflow", []string{"18446744073709551616"}, 0, false},
		{"huge", []string{strings.Repeat("9", 200)}, 0, false},
		{"past_date", []string{now.Add(-time.Second).Format(http.TimeFormat)}, 0, false},
		{"long_date", []string{now.Add(25 * time.Hour).Format(http.TimeFormat)}, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			delay, valid := bpsRetryAfterDelay(http.Header{"Retry-After": test.values}, now)
			if valid != test.valid || valid && delay != test.want {
				t.Fatalf("delay=%v valid=%v", delay, valid)
			}
			state := &bpsRateLimitBackoff{}
			key := bpsRateLimitScope(7, "https://bps.invalid/responses", "gpt-6-astra", "", rateBackoffTestHeaders())
			state.record(key, http.Header{"Retry-After": test.values}, now)
			if got := state.remainingSeconds(key, now); test.valid && got != int64(test.want/time.Second) || !test.valid && got != 0 {
				t.Fatalf("unexpected fallback or altered wait: %d", got)
			}
		})
	}
}

func TestBPSRateBackoffIdentityAndExpiry(t *testing.T) {
	now := time.Now()
	state := &bpsRateLimitBackoff{}
	base := bpsRateLimitScope(7, "https://bps.invalid/responses", "gpt-6-astra", "http://proxy.invalid", rateBackoffTestHeaders())
	state.record(base, http.Header{"Retry-After": {"10"}}, now)
	state.record(base, http.Header{"Retry-After": {"1"}}, now)
	if state.remainingSeconds(base, now.Add(time.Nanosecond)) != 10 {
		t.Fatal("shorter concurrent response shortened backoff")
	}
	for _, test := range []struct {
		name                   string
		account                int64
		endpoint, model, proxy string
		headers                http.Header
	}{
		{"account", 8, "https://bps.invalid/responses", "gpt-6-astra", "http://proxy.invalid", rateBackoffTestHeaders()},
		{"endpoint", 7, "https://other.invalid/responses", "gpt-6-astra", "http://proxy.invalid", rateBackoffTestHeaders()},
		{"model", 7, "https://bps.invalid/responses", "gpt-6-sol", "http://proxy.invalid", rateBackoffTestHeaders()},
		{"proxy", 7, "https://bps.invalid/responses", "gpt-6-astra", "http://different.invalid", rateBackoffTestHeaders()},
	} {
		if key := bpsRateLimitScope(test.account, test.endpoint, test.model, test.proxy, test.headers); key == base || state.remainingSeconds(key, now) != 0 {
			t.Fatalf("%s inherited another scope's pause", test.name)
		}
	}
	for _, header := range []string{"Authorization", "ChatGPT-Account-ID", "X-OpenAI-Account-ID", "X-Basispoints-Auth-Mode"} {
		changed := rateBackoffTestHeaders()
		changed.Set(header, "Bearer different-identity")
		if bpsRateLimitScope(7, "https://bps.invalid/responses", "gpt-6-astra", "http://proxy.invalid", changed) == base {
			t.Fatalf("identity field ignored: %s", header)
		}
	}
	if bpsRateLimitScope(0, "x", "x", "", rateBackoffTestHeaders()) != (bpsRateBackoffKey{}) || bpsRateLimitScope(7, "x", "x", "", nil) != (bpsRateBackoffKey{}) {
		t.Fatal("untrusted or absent account identity retained")
	}
	if state.remainingSeconds(base, now.Add(10*time.Second)) != 0 || len(state.until) != 0 {
		t.Fatal("expired pause not removed")
	}
	for n := 0; n < bpsRateBackoffMaxEntries+2; n++ {
		key := bpsRateLimitScope(int64(n+1), "endpoint", "model", "", rateBackoffTestHeaders())
		state.record(key, http.Header{"Retry-After": {"1"}}, now)
	}
	if len(state.until) != bpsRateBackoffMaxEntries {
		t.Fatal("rate state exceeded its bound")
	}
	state.record(base, http.Header{"Retry-After": {"1"}}, now.Add(2*time.Second))
	if len(state.until) != 1 || state.remainingSeconds(base, now.Add(2*time.Second)) != 1 {
		t.Fatal("expired capacity was not reclaimed")
	}
}

func TestBPSRateBackoffObservesOnlyExactUpstream429(t *testing.T) {
	const endpoint = "https://bps.invalid/api/responses"
	key := bpsRateLimitScope(7, endpoint, "model", "", rateBackoffTestHeaders())
	for _, test := range []struct {
		name, method, target, retry string
		status                      int
		want                        bool
	}{
		{"responses", "POST", endpoint, "10", 429, true},
		{"attachments", "POST", "https://bps.invalid/api/attachments", "10", 429, true},
		{"foreign", "POST", "https://other.invalid/api/responses", "10", 429, false},
		{"query", "POST", endpoint + "?download=1", "10", 429, false},
		{"get", "GET", endpoint, "10", 429, false},
		{"unauthorized", "POST", endpoint, "10", 401, false},
		{"forbidden", "POST", endpoint, "10", 403, false},
		{"server_error", "POST", endpoint, "10", 500, false},
		{"success", "POST", endpoint, "10", 200, false},
		{"missing", "POST", endpoint, "", 429, false},
		{"invalid", "POST", endpoint, "invalid", 429, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := &bpsRateLimitBackoff{}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, test.method, test.target, strings.NewReader("original request"))
			body := &retryPolicyBody{Reader: strings.NewReader("PRIVATE upstream content")}
			want := &http.Response{StatusCode: test.status, Header: http.Header{"Retry-After": {test.retry}}, Body: body}
			calls := 0
			original := &http.Client{Timeout: 20 * time.Second, Transport: retryPolicyRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r != req || r.Context() != ctx {
					t.Fatal("observer rewrote request or timeout context")
				}
				return want, nil
			})}
			copied := withBPSRateBackoff(original, state, key, endpoint)
			response, err := copied.Transport.RoundTrip(req)
			if err != nil || response != want || calls != 1 || copied.Timeout != original.Timeout || copied == original || body.closed.Load() != 0 {
				t.Fatal("observation changed original response or client")
			}
			raw, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if string(raw) != "PRIVATE upstream content" || (state.remainingSeconds(key, time.Now()) > 0) != test.want {
				t.Fatal("body consumed or unexpected rate pause")
			}
			if _, replaced := original.Transport.(*bpsRateBackoffTransport); replaced {
				t.Fatal("shared proxy pool transport mutated")
			}
		})
	}
}

func TestBPSRateBackoffForwardIsolation(t *testing.T) {
	var basisCalls, nativeCalls, proxyCalls atomic.Int32
	reject := func(count *atomic.Int32) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			count.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(429)
		}
	}
	basis := httptest.NewServer(reject(&basisCalls))
	defer basis.Close()
	proxy := httptest.NewServer(reject(&proxyCalls))
	defer proxy.Close()
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { nativeCalls.Add(1); w.WriteHeader(http.StatusTeapot) }))
	defer native.Close()
	tr := New()
	defer tr.Shutdown()
	applyConfig(t, tr, map[string]any{"responses_url": basis.URL + "/api/responses", "enabled_models": protocol.AvailableModels()})
	run := func(id int64, credential, model, proxyURL string) forwardResult {
		frames := requestFrames(t, native.URL, token(t, credential), nil, protocol.JSONBytes(map[string]any{"model": model, "input": "hi"}))
		frames[0].GetStart().AccountId = id
		frames[0].GetStart().ProxyUrl = proxyURL
		return runForward(t, tr, frames)
	}
	first := run(7, "identity-a", "gpt-6-astra", "")
	assertBasisPointsRateLimit(t, first)
	if first.errFrame.GetMessage() != "Basis Points returned HTTP 429; retry later." {
		t.Fatal("initial error changed")
	}
	paused := run(7, "identity-a", "gpt-6-astra", "")
	assertBasisPointsRateLimit(t, paused)
	if !strings.Contains(paused.errFrame.GetMessage(), "request was not sent") || basisCalls.Load() != 1 {
		t.Fatal("matching pause retried upstream or hid its local origin")
	}
	for _, other := range []struct {
		id                       int64
		credential, model, proxy string
	}{
		{8, "identity-a", "gpt-6-astra", ""}, {7, "identity-b", "gpt-6-astra", ""}, {7, "identity-a", "gpt-6-sol", ""}, {7, "identity-a", "gpt-6-astra", proxy.URL},
	} {
		assertBasisPointsRateLimit(t, run(other.id, other.credential, other.model, other.proxy))
	}
	if basisCalls.Load() != 4 || proxyCalls.Load() != 1 {
		t.Fatalf("unrelated identity/model/proxy throttled: direct=%d proxy=%d", basisCalls.Load(), proxyCalls.Load())
	}
	assertBasisPointsRateLimit(t, run(7, "identity-a", "gpt-6-astra", proxy.URL))
	if proxyCalls.Load() != 1 {
		t.Fatal("proxy-specific pause not retained")
	}
	if result := run(7, "identity-a", "gpt-5.4", ""); result.errFrame != nil || result.status != http.StatusTeapot || nativeCalls.Load() != 1 {
		t.Fatal("native path inherited BPS pause")
	}
	for n := 0; n < 2; n++ {
		assertBasisPointsRateLimit(t, run(0, "identity-a", "gpt-6-astra", ""))
	}
	if basisCalls.Load() != 6 {
		t.Fatal("missing scheduled account was cached")
	}
	tr.bpsRateBackoff.mu.Lock()
	for key := range tr.bpsRateBackoff.until {
		tr.bpsRateBackoff.until[key] = time.Now().Add(-time.Second)
	}
	tr.bpsRateBackoff.mu.Unlock()
	assertBasisPointsRateLimit(t, run(7, "identity-a", "gpt-6-astra", ""))
	if basisCalls.Load() != 7 {
		t.Fatal("expired scope did not resume ordinary BPS request")
	}
}

func TestBPSRateBackoffFromAttachmentDoesNotRepeatUpload(t *testing.T) {
	var uploads, conversations atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/attachments" {
			uploads.Add(1)
		} else {
			conversations.Add(1)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	}))
	defer upstream.Close()
	tr := New()
	defer tr.Shutdown()
	applyConfig(t, tr, map[string]any{"responses_url": upstream.URL + "/api/responses"})
	_, inline := relayTestImage(t)
	source, _ := protocol.RawObject(relayTestBody(inline))
	for n := 0; n < 2; n++ {
		result := runForward(t, tr, requestFrames(t, "https://native.invalid/responses", token(t, "image-account"), nil, protocol.JSONBytes(source)))
		assertBasisPointsRateLimit(t, result)
		if n == 1 && !strings.Contains(result.errFrame.GetMessage(), "request was not sent") {
			t.Fatal("attachment pause not applied before upload")
		}
	}
	if uploads.Load() != 1 || conversations.Load() != 0 {
		t.Fatalf("attachment429 repeated: uploads=%d conversations=%d", uploads.Load(), conversations.Load())
	}
}

func TestBPSRateBackoffAbsentOrInvalidRetryAfterDoesNotPause(t *testing.T) {
	for _, header := range []string{"", "invalid", "86401", "18446744073709551616"} {
		t.Run(fmt.Sprintf("header_%s", header), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", header)
				w.WriteHeader(429)
			}))
			defer upstream.Close()
			tr := New()
			defer tr.Shutdown()
			applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
			for n := 0; n < 2; n++ {
				result := runForward(t, tr, requestFrames(t, "https://native.invalid", token(t, "account"), nil, protocol.JSONBytes(map[string]any{"model": "gpt-6-astra", "input": "hi"})))
				assertBasisPointsRateLimit(t, result)
			}
			if calls.Load() != 2 {
				t.Fatal("invented backoff from unsupported Retry-After")
			}
		})
	}
}
