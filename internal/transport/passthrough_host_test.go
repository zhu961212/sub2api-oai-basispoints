package transport

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestPassthroughPreservesHostAuthority(t *testing.T) {
	for _, routing := range []struct {
		name   string
		models []string
		model  string
	}{
		{"no models selected", []string{}, "gpt-6-astra"},
		{"model not selected", []string{"gpt-5.6-sol"}, "gpt-6-astra"},
		{"ordinary host model", protocol.AvailableModels(), "gpt-5.4"},
	} {
		for _, proxied := range []bool{false, true} {
			name := routing.name + "/direct"
			if proxied {
				name = routing.name + "/account proxy"
			}
			t.Run(name, func(t *testing.T) {
				const authority = "chatgpt.com"
				body := protocol.JSONBytes(map[string]any{"model": routing.model, "input": "unchanged", "stream": true})
				response := []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Host != authority {
						t.Errorf("host authority = %q, want %q", r.Host, authority)
						http.Error(w, "wrong virtual host", http.StatusMisdirectedRequest)
						return
					}
					got, err := io.ReadAll(r.Body)
					if err != nil || !bytes.Equal(got, body) {
						t.Errorf("passthrough body changed: %q, %v", got, err)
					}
					if r.Header.Get("Authorization") != "Bearer host-token" {
						t.Errorf("host authorization changed")
					}
					if r.Header.Get("X-Basispoints-Auth-Mode") != "" {
						t.Errorf("BPS header leaked")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write(response)
				}))
				defer upstream.Close()
				tr := New()
				defer tr.Shutdown()
				applyConfig(t, tr, map[string]any{"enabled_models": routing.models, "responses_url": "https://unused.example.test/responses"})
				target := upstream.URL + "/backend-api/codex/responses"
				if proxied {
					target = "http://selected-egress.example.test/backend-api/codex/responses"
				}
				frames := requestFrames(t, target, "host-token", map[string]string{"Host": "ignored-header.example.test"}, body)
				frames[0].GetStart().Host = authority
				if proxied {
					frames[0].GetStart().ProxyUrl = upstream.URL
				}
				result := runForward(t, tr, frames)
				if result.errFrame != nil || result.status != http.StatusOK || !result.ended || !bytes.Equal(result.body, response) {
					t.Fatalf("passthrough failed: %+v, body %s", result, result.body)
				}
			})
		}
	}
}

func TestPassthroughDefaultsEmptyHostToURLAuthority(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "" {
			t.Error("empty upstream Host")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	tr := New()
	defer tr.Shutdown()
	applyConfig(t, tr, map[string]any{"enabled_models": []string{}})
	frames := requestFrames(t, upstream.URL, "host-token", nil, protocol.JSONBytes(map[string]any{"model": "gpt-6-astra"}))
	result := runForward(t, tr, frames)
	if result.errFrame != nil || result.status != http.StatusNoContent || !result.ended {
		t.Fatalf("empty host passthrough failed: %+v", result)
	}
}
