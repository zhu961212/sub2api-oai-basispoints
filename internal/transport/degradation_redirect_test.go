package transport

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestDegradationProbeDoesNotFollowRedirectOrRetry(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	for _, code := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("ChatGPT-Account-ID") != "probe-redirect-account" {
					t.Error("probe did not use selected account")
				}
				http.Redirect(w, r, destination.URL, code)
			}))
			defer upstream.Close()
			tr := New()
			defer tr.Shutdown()
			host := &degradationTestHost{fakeHost: &fakeHost{token: token(t, "probe-redirect-account")}}
			cfg := protocol.DefaultConfig()
			cfg.ResponsesURL = upstream.URL
			status, answer, err := tr.checkDegradationAccount(context.Background(), cfg, host, tr.client, 7, protocol.DefaultModelID)
			if status != "error" || answer != "" || err == nil || !strings.Contains(err.Error(), fmt.Sprint(code)) {
				t.Fatalf("redirect produced a classification: status=%s answer=%q err=%v", status, answer, err)
			}
			if calls.Load() != 1 || redirected.Load() != 0 {
				t.Fatalf("probe replayed or followed redirect: calls=%d redirected=%d", calls.Load(), redirected.Load())
			}
		})
	}
}
