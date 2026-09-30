package transport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestForwardPromptCacheUsesTrustedHostScope(t *testing.T) {
	captured := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body, err := protocol.RawObject(raw)
		if err != nil {
			t.Error(err)
		}
		captured <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_cache_scope","status":"completed","output":[]}`)
	}))
	defer upstream.Close()
	tr := New()
	defer tr.Shutdown()
	applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
	var firstKey string
	for _, test := range []struct {
		name     string
		account  int64
		scope    string
		conflict bool
		explicit string
		want     string
	}{
		{"first turn", 7, "private-conversation", false, "", "first"},
		{"next turn", 7, "private-conversation", false, "", "same"},
		{"different account", 8, "private-conversation", false, "", "different"},
		{"different conversation", 7, "other-private-conversation", false, "", "different"},
		{"body aliases only", 7, "", false, "", "absent"},
		{"conflicting host scopes", 7, "private-conversation", true, "", "absent"},
		{"explicit scoped hint", 7, "private-conversation", false, "client-cache", "explicit"},
		{"explicit unscoped hint", 7, "", false, "client-cache", "explicit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := map[string]any{
				"model": protocol.DefaultModelID, "input": test.name,
				"session_id": "shared-body-alias", "conversation": "shared-body-alias",
				"__bps_session_scope": "spoofed-scope", "__bps_context_cache_disabled": false,
			}
			if test.explicit != "" {
				source["prompt_cache_key"] = test.explicit
			}
			headers := map[string]string{"session_id": "shared-account-fingerprint"}
			if test.scope != "" {
				headers["conversation_id"] = test.scope
			}
			frames := requestFrames(t, upstream.URL, token(t, "acct"), headers, protocol.JSONBytes(source))
			start := frames[0].GetStart()
			start.AccountId = test.account
			if test.conflict {
				start.Headers["Conversation_Id"] = &pluginv1.HeaderValues{Values: []string{"conflicting-scope"}}
			}
			result := runForward(t, tr, frames)
			if result.errFrame != nil || result.status != http.StatusOK {
				t.Fatalf("forward failed: %+v", result)
			}
			body := <-captured
			key, exists := body["prompt_cache_key"].(string)
			if strings.Contains(string(protocol.JSONBytes(body)), "__bps_") {
				t.Fatal("internal scope markers reached upstream")
			}
			switch test.want {
			case "first", "same", "different":
				if len(key) != 64 || strings.Contains(key, "private") {
					t.Fatalf("expected opaque key, got %q", key)
				}
				if test.want == "first" {
					firstKey = key
				} else if (key == firstKey) != (test.want == "same") {
					t.Fatal("key stability/account isolation mismatch")
				}
			case "absent":
				if exists {
					t.Fatal("untrusted identity generated a cache key")
				}
			case "explicit":
				if key != test.explicit {
					t.Fatal("client cache hint changed")
				}
			}
		})
	}
}
