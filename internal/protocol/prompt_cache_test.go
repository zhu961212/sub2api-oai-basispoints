package protocol

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestPreparePromptCachePrefixSurvivesToolRounds(t *testing.T) {
	var previous []any
	var cacheKey string
	history := []any{messageItem("user", "inspect the same file")}
	for round := 0; round < 3; round++ {
		source := sessionTestSource(t.Name())
		source["__bps_session_scope"] = "account:7/session:" + t.Name()
		source["instructions"] = "Keep the tool history intact."
		source["input"] = history
		body, err := prepareResponsesBody(source, DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		items := body["input"].([]any)
		if round == 0 {
			cacheKey = stringValue(body["prompt_cache_key"])
		} else {
			if body["prompt_cache_key"] != cacheKey {
				t.Fatal("tool round changed the cache routing key")
			}
			// The per-request reminder belongs at the new generation position.
			// Every earlier input item must retain its original bytes.
			if len(items) < len(previous) || !bytes.Equal(jsonBytes(items[:len(previous)]), jsonBytes(previous)) {
				t.Fatalf("round %d changed previously sent history", round)
			}
		}
		previous = items[:len(items)-1]
		native := sessionTestNative(fmt.Sprintf("fc_%s_%d", t.Name(), round))
		native["call_id"] = fmt.Sprintf("call_%s_%d", t.Name(), round)
		native["status"] = "completed"
		call, ok := extractNativeClientToolCall(map[string]any{"output": []any{native}}, source)
		if !ok {
			t.Fatal("fixture did not produce a translated tool call")
		}
		history = append(history, call, map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "file contents"})
		replayed := translateInputItemsInNamespace(history, clientToolSpecs(source), nativeCallNamespace(source))
		if !bytes.Equal(jsonBytes(replayed[len(replayed)-2]), jsonBytes(native)) {
			t.Fatal("tool replay changed the original native envelope")
		}
	}
}

func TestPreparePromptCacheKeyStableAcrossTurns(t *testing.T) {
	source := map[string]any{
		"model":               DefaultModelID,
		"__bps_session_scope": "account:7/session:isolated-private-session",
		"input":               []any{map[string]any{"role": "user", "content": "first turn"}},
	}
	first, err := prepareResponsesBody(source, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	key := stringValue(first["prompt_cache_key"])
	if len(key) != 64 || strings.Contains(key, "private") {
		t.Fatalf("expected an opaque stable cache key, got %q", key)
	}
	next := cloneObject(source)
	next["input"] = append(next["input"].([]any),
		map[string]any{"role": "assistant", "content": "first answer"},
		map[string]any{"role": "user", "content": "second turn"})
	next["metadata"] = map[string]any{"request_id": "another-request"}
	second, err := prepareResponsesBody(next, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if second["prompt_cache_key"] != key {
		t.Fatalf("cache key changed as history advanced: %v -> %v", key, second["prompt_cache_key"])
	}
	for _, scope := range []string{"account:8/session:isolated-private-session", "account:7/session:another-isolated-session"} {
		other := cloneObject(source)
		other["__bps_session_scope"] = scope
		body, err := prepareResponsesBody(other, DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		if body["prompt_cache_key"] == key {
			t.Fatalf("cache key shared with %q", scope)
		}
	}
}

func TestPreparePromptCacheKeyPreservesExplicitHint(t *testing.T) {
	for _, field := range []string{"prompt_cache_key", "promptCacheKey"} {
		t.Run(field, func(t *testing.T) {
			source := map[string]any{
				"model": DefaultModelID, "input": "test", field: "client-chosen-cache",
				"__bps_session_scope": "account:7/session:host", "__bps_context_cache_disabled": true,
			}
			body, err := prepareResponsesBody(source, DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			if body["prompt_cache_key"] != "client-chosen-cache" {
				t.Fatalf("explicit key was replaced: %v", body["prompt_cache_key"])
			}
		})
	}
}

func TestPreparePromptCacheKeyDoesNotInventAnonymousIdentity(t *testing.T) {
	for _, source := range []map[string]any{
		{"input": "same opening"},
		{"input": "same opening", "session_id": "client-session", "conversation": "client-conversation"},
		{"input": "same opening", "__bps_session_scope": "blocked-scope", "__bps_context_cache_disabled": true},
	} {
		body, err := prepareResponsesBody(source, DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := body["prompt_cache_key"]; exists {
			t.Fatalf("invented a cache key without trusted scope: %v", body["prompt_cache_key"])
		}
	}
}
