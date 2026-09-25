package protocol

import (
	"fmt"
	"sync"
	"testing"
)

func TestConcurrentSessionLateCompletionPreservesNewCatalog(t *testing.T) {
	for _, catalog := range []string{"extended", "empty", "null"} {
		t.Run(catalog, func(t *testing.T) {
			revoke := catalog != "extended"
			session := "late-catalog-" + t.Name()
			older := sessionTestSource(session)
			if _, err := PrepareResponsesBody(older, DefaultConfig()); err != nil {
				t.Fatal(err)
			}
			newer := sessionTestSource(session)
			if catalog == "null" {
				newer["tools"] = nil
			} else if revoke {
				newer["tools"] = []any{}
			} else {
				newer["tools"] = append(newer["tools"].([]any), map[string]any{"type": "function", "name": "new_tool", "parameters": map[string]any{"type": "object"}})
			}
			if _, err := PrepareResponsesBody(newer, DefaultConfig()); err != nil {
				t.Fatal(err)
			}
			// Deliberately complete the older request after the newer one.
			RememberResponseContext(newer, map[string]any{"id": "resp-new-" + session})
			RememberResponseContext(older, map[string]any{"id": "resp-old-" + session})
			follow := map[string]any{"session_id": session}
			got := clientToolSpecs(follow)
			if revoke {
				if len(got) != 0 {
					t.Fatalf("late completion restored revoked tools: %#v", got)
				}
			} else if _, ok := got["new_tool"]; !ok {
				t.Fatalf("late completion removed newly declared tool: %#v", got)
			} else if _, reason := decodeNativeClientToolCallFromItem(relayCompatNative(map[string]any{"tool": "new_tool", "args": map[string]any{}}), follow, false); reason != "" {
				t.Fatalf("next request cannot call newly declared tool: %s", reason)
			}
		})
	}
}

func TestConcurrentSessionDiscoveredCatalogUpdatesAreAtomic(t *testing.T) {
	const workers = 64
	session := t.Name()
	_ = sourceTools(sessionTestSource(session))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			source := map[string]any{"session_id": session, "input": []any{map[string]any{
				"type": "additional_tools", "tools": []any{map[string]any{
					"type": "function", "name": fmt.Sprintf("discovered_%d", index), "parameters": map[string]any{"type": "object"},
				}},
			}}}
			<-start
			_ = sourceTools(source)
		}(index)
	}
	close(start)
	wg.Wait()
	follow := map[string]any{"session_id": session}
	specs := clientToolSpecs(follow)
	if len(specs) != workers+1 {
		t.Fatalf("concurrent discoveries lost catalog updates: got %d tools, want %d", len(specs), workers+1)
	}
	for index := 0; index < workers; index++ {
		name := fmt.Sprintf("discovered_%d", index)
		if _, reason := decodeNativeClientToolCallFromItem(relayCompatNative(map[string]any{"tool": name, "args": map[string]any{}}), follow, false); reason != "" {
			t.Errorf("discovered tool %s cannot be called: %s", name, reason)
		}
	}
}

func TestRememberResponseContextRegistersMissingCatalog(t *testing.T) {
	for _, prepare := range []bool{false, true} {
		t.Run(fmt.Sprintf("prepared_%v", prepare), func(t *testing.T) {
			source := sessionTestSource("")
			if prepare {
				if _, err := PrepareResponsesBody(source, DefaultConfig()); err != nil {
					t.Fatal(err)
				}
			}
			responseID := "resp-register-" + t.Name()
			RememberResponseContext(source, map[string]any{"id": responseID})
			follow := map[string]any{"previous_response_id": responseID}
			if _, ok := clientToolSpecs(follow)["read"]; !ok {
				t.Fatal("initial response did not register its catalog")
			}
		})
	}
}

func TestConcurrentSessionCacheCopiesAndResponseIsolation(t *testing.T) {
	const workers = 64
	var wg sync.WaitGroup
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			session := fmt.Sprintf("cache-clone-%s-%d", t.Name(), index)
			source := sessionTestSource(session)
			if _, err := PrepareResponsesBody(source, DefaultConfig()); err != nil {
				t.Error(err)
				return
			}
			wantID := fmt.Sprintf("fc_clone_%d", index)
			native := sessionTestNative(wantID)
			if _, ok := extractNativeClientToolCall(map[string]any{"output": []any{native}}, source); !ok {
				t.Error("tool not transformed")
				return
			}
			responseID := "resp-" + session
			RememberResponseContext(source, map[string]any{"id": responseID})
			follow := map[string]any{"previous_response_id": responseID}
			if cacheNamespaceForSource(follow) != cacheNamespaceForSource(source) {
				t.Errorf("session %d resolved another response namespace", index)
			}
			// Mutating caller data or returned copies must not alter shared caches.
			native["id"] = "mutated_source"
			objectValue(source["tools"].([]any)[0])["name"] = "mutated_source"
			firstCatalog := rememberedToolCatalog(follow).([]any)
			objectValue(firstCatalog[0])["name"] = "mutated_result"
			firstNative := rememberedNativeCallInNamespace(nativeCallNamespace(follow), "shared_call")
			firstNative["id"] = "mutated_result"
			if _, ok := clientToolSpecs(follow)["read"]; !ok {
				t.Errorf("session %d catalog mutation leaked into cache", index)
			}
			items := translateInputItemsInNamespace([]any{map[string]any{"type": "function_call_output", "call_id": "shared_call", "output": session}}, nil, nativeCallNamespace(follow))
			if len(items) != 2 || objectValue(items[0])["id"] != wantID || objectValue(items[1])["output"] != session {
				t.Errorf("session %d replay was mutated or crossed sessions: %#v", index, items)
			}
		}(index)
	}
	wg.Wait()
}
