package protocol

import (
	"strings"
	"testing"
)

func TestDisabledContextCacheCannotRecoverOrPublishSharedState(t *testing.T) {
	trusted := sessionTestSource(t.Name())
	if _, err := PrepareResponsesBody(trusted, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	RememberResponseContext(trusted, map[string]any{"id": "resp-trusted-disabled-cache"})
	rememberNativeCallInNamespace(nativeCallNamespace(trusted), sessionTestNative("fc_trusted"))

	for _, carrier := range []string{"session", "response", "internal"} {
		t.Run(carrier, func(t *testing.T) {
			source := map[string]any{"__bps_context_cache_disabled": true}
			switch carrier {
			case "session":
				source["session_id"] = trusted["session_id"]
			case "response":
				source["previous_response_id"] = "resp-trusted-disabled-cache"
			case "internal":
				source["__bps_response_scope"] = cacheNamespaceForSource(trusted)
			}
			if cacheNamespaceForSource(source) != "" || len(clientToolSpecs(source)) != 0 {
				t.Fatal("disabled request recovered another request catalog")
			}
			if rememberedNativeCallInNamespace(nativeCallNamespace(source), "shared_call") != nil {
				t.Fatal("disabled request recovered a cached native call")
			}
			source["tools"] = []any{relayCompatFunction("local_tool")}
			if _, ok := clientToolSpecs(source)["local_tool"]; !ok {
				t.Fatal("cache disabling prevented request-local tool use")
			}
			responseID := "resp-disabled-" + t.Name()
			RememberResponseContext(source, map[string]any{"id": responseID})
			if rememberedResponseContext(responseID) != "" {
				t.Fatal("disabled request published a response scope")
			}
			if _, ok := clientToolSpecs(map[string]any{"session_id": trusted["session_id"]})["read"]; !ok {
				t.Fatal("disabled request replaced the existing catalog")
			}
		})
	}
}

func TestDisabledContextCachePreservesExpandedHistory(t *testing.T) {
	source := sessionTestSource(t.Name())
	source["__bps_context_cache_disabled"] = true
	call := map[string]any{"type": "function_call", "call_id": "history", "name": "read", "arguments": "{}"}
	source["input"] = []any{messageItem("user", "keep original history"), call, map[string]any{
		"type": "function_call_output", "call_id": "history", "output": "preserved result",
	}}
	body, err := PrepareResponsesBody(source, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	items := body["input"].([]any)
	if len(items) != 5 || itemText(objectValue(items[1])["content"]) != "keep original history" ||
		objectValue(items[2])["name"] != transportName || objectValue(items[3])["output"] != "preserved result" {
		t.Fatalf("expanded history lost content: %#v", items)
	}
	if strings.Contains(string(jsonBytes(body)), "__bps_") {
		t.Fatal("internal cache policy leaked into the upstream request")
	}
}

func TestLateResponseCannotRestoreEvictedCatalog(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		name := "replaced"
		if revoke {
			name = "revoked"
		}
		t.Run(name, func(t *testing.T) {
			older := sessionTestSource(t.Name())
			if _, err := PrepareResponsesBody(older, DefaultConfig()); err != nil {
				t.Fatal(err)
			}
			newer := sessionTestSource(t.Name())
			newer["tools"] = []any{relayCompatFunction("new_tool")}
			if revoke {
				newer["tools"] = []any{}
			}
			if _, err := PrepareResponsesBody(newer, DefaultConfig()); err != nil {
				t.Fatal(err)
			}
			// Model LRU eviction or idle expiry after a newer catalog update.
			namespace := cacheNamespaceForSource(newer)
			toolCatalogCache.forget(namespace)
			responseID := "resp-evicted-" + t.Name()
			RememberResponseContext(older, map[string]any{"id": responseID})
			if rememberedResponseContext(responseID) != namespace {
				t.Fatal("response lost its session mapping")
			}
			if _, exists := toolCatalogCache.get(namespace); exists {
				t.Fatal("late response republished an expired catalog")
			}
			follow := map[string]any{"session_id": t.Name()}
			if len(clientToolSpecs(follow)) != 0 {
				t.Fatal("late response restored stale tools")
			}
			// A fresh, explicit declaration remains able to restore the catalog.
			follow["tools"] = []any{relayCompatFunction("fresh_tool")}
			if _, err := PrepareResponsesBody(follow, DefaultConfig()); err != nil {
				t.Fatal(err)
			}
			if _, ok := clientToolSpecs(map[string]any{"session_id": t.Name()})["fresh_tool"]; !ok {
				t.Fatal("fresh request could not republish its catalog")
			}
		})
	}
}
