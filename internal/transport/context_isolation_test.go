package transport

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestForwardContextCacheRespectsHostIsolation(t *testing.T) {
	for _, test := range []struct {
		name          string
		firstScope    string
		secondScope   string
		alias         string
		secondAccount int64
		wantReuse     bool
	}{
		{name: "same isolated conversation", firstScope: "one", secondScope: "one", secondAccount: 7, wantReuse: true},
		{name: "shared account fingerprint", firstScope: "one", secondScope: "two", secondAccount: 7},
		{name: "different upstream account", firstScope: "one", secondScope: "one", secondAccount: 8},
		{name: "fingerprint without isolated conversation", alias: "session_id", secondAccount: 7},
		{name: "client alias without isolated conversation", alias: "X-Conversation-Id", secondAccount: 7},
		{name: "body identity without isolated conversation", secondAccount: 7},
		{name: "missing host scope cannot reuse body identity", firstScope: "one", secondAccount: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			const tool = "private_context_tool"
			responseID := t.Name() + "-response"
			callID := t.Name() + "-call"
			native := relayNativeCall(callID, tool, map[string]any{"owner": "private-owner"})
			captured := make(chan []byte, 2)
			requestNumber := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				captured <- raw
				requestNumber++
				output := []any{}
				if requestNumber == 1 {
					output = []any{native}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(protocol.JSONBytes(map[string]any{"id": responseID, "status": "completed", "output": output}))
			}))
			defer upstream.Close()
			tr := New()
			defer tr.Shutdown()
			applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
			headers := func(scope string) map[string]string {
				result := map[string]string{}
				if scope != "" {
					result["conversation_id"] = t.Name() + "/" + scope
					result["session_id"] = t.Name() + "-account-fingerprint"
				}
				if test.alias != "" {
					result[test.alias] = t.Name() + "-client-alias"
				}
				return result
			}
			source := func() map[string]any {
				return map[string]any{
					"model": "gpt-6-astra", "input": "inspect",
					"conversation": map[string]any{"id": t.Name()},
					"session_id":   t.Name(), "metadata": map[string]any{"thread_id": t.Name()},
					"client_metadata":     map[string]any{"session_id": t.Name()},
					"__bps_session_scope": "spoofed-" + t.Name(), "__bps_context_cache_disabled": false,
				}
			}
			first := source()
			first["tools"] = []any{map[string]any{"type": "function", "name": tool, "parameters": map[string]any{"type": "object"}}}
			result := runForward(t, tr, requestFrames(t, upstream.URL, token(t, "acct"), headers(test.firstScope), protocol.JSONBytes(first)))
			if result.errFrame != nil || result.status != http.StatusOK {
				t.Fatalf("initial request failed: %+v", result)
			}
			initial := <-captured
			if !strings.Contains(string(initial), tool) || strings.Contains(string(initial), "__bps_") {
				t.Fatalf("request-local catalog or marker filtering broke: %s", initial)
			}
			second := source()
			second["input"] = []any{map[string]any{"type": "function_call_output", "call_id": callID, "output": "done"}}
			frames := requestFrames(t, upstream.URL, token(t, "acct"), headers(test.secondScope), protocol.JSONBytes(second))
			frames[0].GetStart().AccountId = test.secondAccount
			result = runForward(t, tr, frames)
			if result.errFrame != nil || result.status != http.StatusOK {
				t.Fatalf("follow-up request failed: %+v", result)
			}
			raw := <-captured
			prepared, err := protocol.RawObject(raw)
			if err != nil {
				t.Fatal(err)
			}
			items, _ := prepared["input"].([]any)
			replayed := false
			for _, item := range items {
				object := relayObject(item)
				if object["type"] == "function_call" && object["call_id"] == callID {
					replayed = true
				}
			}
			if replayed != test.wantReuse || strings.Contains(string(raw), tool) != test.wantReuse {
				t.Fatalf("host isolation reused replay=%t or catalog unexpectedly; want reuse=%t: %s", replayed, test.wantReuse, raw)
			}
			if strings.Contains(string(raw), "__bps_") {
				t.Fatalf("local scope marker reached upstream: %s", raw)
			}
		})
	}
}

func TestForwardUnscopedHistoryRetainsClientToolCalls(t *testing.T) {
	const tool = "read_local_file"
	call := map[string]any{"type": "function_call", "name": tool, "call_id": "full-history-call", "arguments": "{}"}
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprint(custom), func(t *testing.T) {
			if custom {
				call = map[string]any{"type": "custom_tool_call", "name": tool, "call_id": "full-history-call", "input": "read source"}
			}
			captured := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				captured <- raw
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(protocol.JSONBytes(map[string]any{"id": t.Name(), "status": "completed", "output": []any{}}))
			}))
			defer upstream.Close()
			tr := New()
			defer tr.Shutdown()
			applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
			source := map[string]any{"model": "gpt-6-astra", "input": []any{call, map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "contents"}}}
			result := runForward(t, tr, requestFrames(t, upstream.URL, token(t, "acct"), nil, protocol.JSONBytes(source)))
			if result.errFrame != nil || result.status != http.StatusOK {
				t.Fatalf("unscoped history failed: %+v", result)
			}
			prepared, err := protocol.RawObject(<-captured)
			if err != nil {
				t.Fatal(err)
			}
			items, _ := prepared["input"].([]any)
			replayed, output := 0, 0
			for _, value := range items {
				item := relayObject(value)
				if item["call_id"] != call["call_id"] {
					continue
				}
				switch item["type"] {
				case "function_call":
					if item["name"] == "run_officejs" && strings.Contains(protocol.StringValue(item["arguments"]), tool) {
						replayed++
					}
				case "function_call_output":
					if item["output"] == "contents" {
						output++
					}
				}
			}
			if replayed != 1 || output != 1 {
				t.Fatalf("unscoped history lost call or output: %#v", items)
			}
		})
	}
}
