package transport

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestForwardDiscoveryPreservesPromptPrefixAndToolAuthorization(t *testing.T) {
	captured := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body, err := protocol.RawObject(raw)
		if err != nil {
			t.Error(err)
		}
		captured <- body
		w.Header().Set("Content-Type", "application/json")
		response := map[string]any{"id": "resp_discovery_cache", "status": "completed", "output": []any{}}
		if strings.Contains(string(raw), "late.executor") {
			response["output"] = []any{relayNativeCall("call_late_executor", "late.executor", "text(1)")}
		}
		_, _ = w.Write(protocol.JSONBytes(response))
	}))
	defer upstream.Close()
	tr := New()
	defer tr.Shutdown()
	applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
	source := map[string]any{
		"model": protocol.DefaultModelID, "stream": false,
		"tools": []any{map[string]any{"type": "function", "name": "base.read", "parameters": map[string]any{"type": "object"}}},
		"input": []any{map[string]any{"role": "user", "content": strings.Repeat("Keep the earlier context intact. ", 1024)}},
	}
	var prefix []any
	var cacheKey any
	for round := 0; round < 3; round++ {
		frames := requestFrames(t, upstream.URL, token(t, "acct-discovery-cache"), map[string]string{"conversation_id": t.Name()}, protocol.JSONBytes(source))
		frames[0].GetStart().AccountId = 7
		result := runForward(t, tr, frames)
		if result.errFrame != nil || result.status != http.StatusOK || !result.ended {
			t.Fatalf("forward failed: %+v", result)
		}
		body := <-captured
		items := body["input"].([]any)
		if round == 0 {
			cacheKey = body["prompt_cache_key"]
			if cacheKey == nil {
				t.Fatal("trusted scope did not provide cache routing")
			}
		} else {
			if body["prompt_cache_key"] != cacheKey {
				t.Fatal("discovery changed cache routing")
			}
			if len(items) < len(prefix) || !bytes.Equal(protocol.JSONBytes(items[:len(prefix)]), protocol.JSONBytes(prefix)) {
				t.Fatal("discovery changed the outbound historical prefix")
			}
			response, err := protocol.RawObject(result.body)
			if err != nil {
				t.Fatal(err)
			}
			outputs, _ := response["output"].([]any)
			if len(outputs) != 1 {
				t.Fatal("discovered tool was not forwarded")
			}
			call := relayObject(outputs[0])
			if call["type"] != "custom_tool_call" || call["name"] != "late.executor" || call["input"] != "text(1)" {
				t.Fatal("discovered custom tool lost authorization or raw input")
			}
		}
		prefix = items[:len(items)-1]
		source["input"] = append(source["input"].([]any),
			map[string]any{"type": "additional_tools", "tools": []any{map[string]any{"type": "custom", "name": "late.executor", "description": strings.Repeat("Updated executor contract. ", round+1)}}},
			map[string]any{"role": "user", "content": "Continue using the discovered executor"})
	}
}
