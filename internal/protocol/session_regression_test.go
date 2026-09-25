package protocol

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func sessionTestSource(id string) map[string]any {
	return map[string]any{"session_id": id, "model": "gpt-6-astra", "input": []any{messageItem("user", "same opening")}, "tools": []any{
		map[string]any{"type": "function", "name": "read", "parameters": map[string]any{"type": "object"}},
	}}
}

func sessionTestNative(id string) map[string]any {
	return map[string]any{"type": "function_call", "id": id, "call_id": "shared_call", "name": transportName, "arguments": string(jsonBytes(map[string]any{
		"code": string(jsonBytes(map[string]any{"tool": "read", "args": map[string]any{"path": "same.txt"}})),
	}))}
}

func TestSessionsRemainIsolatedUnderConcurrentIdenticalCalls(t *testing.T) {
	const workers = 24
	var wg sync.WaitGroup
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			source := sessionTestSource(fmt.Sprintf("parallel-%d", index))
			source["prompt_cache_key"] = "shared-prefill-cache"
			want := fmt.Sprintf("fc_session_%d", index)
			call, ok := extractNativeClientToolCall(map[string]any{"output": []any{sessionTestNative(want)}}, source)
			if !ok {
				t.Errorf("session %d did not transform", index)
				return
			}
			items := translateInputItemsInNamespace([]any{call}, clientToolSpecs(source), nativeCallNamespace(source))
			if len(items) != 1 || objectValue(items[0])["id"] != want {
				t.Errorf("session %d received another session's call", index)
			}
		}(index)
	}
	wg.Wait()
}

func TestAnonymousIdenticalRequestsCannotShareReplayState(t *testing.T) {
	a, b := sessionTestSource(""), sessionTestSource("")
	if cacheNamespaceForSource(a) != "" || cacheNamespaceForSource(b) != "" {
		t.Fatal("anonymous prompts became shared session identities")
	}
	_, _ = extractNativeClientToolCall(map[string]any{"output": []any{sessionTestNative("fc_other_session")}}, a)
	if rememberedNativeCallInNamespace(nativeCallNamespace(b), "shared_call") != nil {
		t.Fatal("anonymous replay crossed requests")
	}
}

func TestExplicitEmptyCatalogStaysRevokedOnNextTurn(t *testing.T) {
	source := sessionTestSource("catalog-revoke")
	if _, err := PrepareResponsesBody(source, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	clear := map[string]any{"session_id": "catalog-revoke", "tools": []any{}}
	if _, err := PrepareResponsesBody(clear, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	follow := map[string]any{"session_id": "catalog-revoke"}
	if got := clientToolSpecs(follow); len(got) != 0 {
		t.Fatalf("cleared tools came back: %#v", got)
	}
}

func TestResponseContextKeepsCanonicalNamespaceWithoutDoublePrefix(t *testing.T) {
	source := sessionTestSource("response-mapping")
	_ = sourceTools(source)
	RememberResponseContext(source, map[string]any{"id": "resp_scope_1"})
	next := map[string]any{"previous_response_id": "resp_scope_1"}
	if cacheNamespaceForSource(next) != cacheNamespaceForSource(source) {
		t.Fatal("response lookup prefixed a resolved namespace twice")
	}
	RememberResponseContext(next, map[string]any{"id": "resp_scope_2"})
	last := map[string]any{"previous_response_id": "resp_scope_2"}
	if cacheNamespaceForSource(last) != cacheNamespaceForSource(source) {
		t.Fatal("response namespace changed on the second turn")
	}
	// This is catalog bookkeeping only. Native BPS lacks the history itself.
	if _, err := PrepareResponsesBody(last, DefaultConfig()); err == nil {
		t.Fatal("unexpanded previous_response_id was silently accepted")
	}
}

func TestOutputOnlyFollowUpIncludesOriginalNativeCall(t *testing.T) {
	source := sessionTestSource("output-only")
	_, ok := extractNativeClientToolCall(map[string]any{"output": []any{sessionTestNative("fc_output_only")}}, source)
	if !ok {
		t.Fatal("tool not transformed")
	}
	items := translateInputItemsInNamespace([]any{map[string]any{"type": "function_call_output", "call_id": "shared_call", "output": "file contents"}}, nil, nativeCallNamespace(source))
	if len(items) != 2 || objectValue(items[0])["id"] != "fc_output_only" || objectValue(items[1])["output"] != "file contents" {
		t.Fatalf("missing original call/output pair: %#v", items)
	}
}

func TestNestedNamespacesAndHistoricalCustomWhitespace(t *testing.T) {
	source := map[string]any{"tools": []any{map[string]any{"type": "namespace", "name": "outer", "tools": []any{
		map[string]any{"type": "namespace", "name": "inner", "tools": []any{map[string]any{"type": "custom", "name": "patch"}}},
	}}}}
	if _, ok := clientToolSpecs(source)["outer.inner.patch"]; !ok {
		t.Fatal("parent namespace lost")
	}
	input := "  patch\n\n"
	items := translateInputItems([]any{map[string]any{"type": "custom_tool_call", "namespace": "outer.inner", "name": "patch", "call_id": "history", "input": input}}, nil)
	envelope := transportEnvelope(objectValue(items[0]))
	if envelope["tool"] != "outer.inner.patch" || envelope["args"] != input {
		t.Fatalf("history was changed: %#v", envelope)
	}
	if len(functionItemID(strings.Repeat("x", 200))) > 64 {
		t.Fatal("upstream function ID exceeds 64 bytes")
	}
}
