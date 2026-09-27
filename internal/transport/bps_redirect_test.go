package transport

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestBPSForwardDoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var redirected atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				redirected.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer destination.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", destination.URL+"/collect")
				w.WriteHeader(status)
			}))
			defer upstream.Close()
			tr := New()
			defer tr.Shutdown()
			applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
			headers := map[string]string{"X-Codex-Installation-ID": "client-installation", "X-Codex-Session-ID": "client-session"}
			result := runForward(t, tr, requestFrames(t, "https://unused.invalid/responses", token(t, "acct-redirect"), headers, protocol.JSONBytes(map[string]any{"model": protocol.DefaultModelID, "input": "private user content"})))
			if redirected.Load() != 0 {
				t.Fatal("BPS request followed redirect with account identity or body")
			}
			if result.errFrame == nil || result.errFrame.GetCode() != "upstream_redirect" || !result.errFrame.GetRequestSent() {
				t.Fatalf("redirect was not isolated: %+v", result)
			}
			if result.status != 0 || len(result.headers) != 0 || len(result.body) != 0 {
				t.Fatal("redirect response escaped to the client")
			}
		})
	}
}

func TestBPSRedirectPolicyKeepsSharedClientUntouched(t *testing.T) {
	var originalCalls atomic.Int32
	base := &http.Client{Timeout: 17 * time.Second, Transport: http.DefaultTransport, CheckRedirect: func(*http.Request, []*http.Request) error { originalCalls.Add(1); return nil }}
	client := withoutBPSRedirects(base)
	if client == base || client.Transport != base.Transport || client.Timeout != base.Timeout {
		t.Fatal("BPS isolation rebuilt the shared transport or lost timeout")
	}
	if client.CheckRedirect(nil, nil) != http.ErrUseLastResponse || originalCalls.Load() != 0 {
		t.Fatal("BPS redirect policy delegated to the original client")
	}
	if base.CheckRedirect(nil, nil) != nil || originalCalls.Load() != 1 {
		t.Fatal("BPS redirect policy mutated shared client")
	}
}
