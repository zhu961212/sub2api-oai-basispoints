package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNativeReplayCachePreservesLargeMetadataNumbers(t *testing.T) {
	native := sessionTestNative("fc_exact_cache")
	native["references"] = []any{map[string]any{"sheet_id": json.Number("9007199254740993")}}
	rememberNativeCallInNamespace(t.Name(), native)
	got := rememberedNativeCallInNamespace(t.Name(), "shared_call")
	reference := objectValue(got["references"].([]any)[0])
	if reference["sheet_id"] != json.Number("9007199254740993") {
		t.Fatalf("cached native metadata changed numeric identity: %#v", reference["sheet_id"])
	}
	reference["sheet_id"] = "changed"
	native["id"] = "changed"
	next := rememberedNativeCallInNamespace(t.Name(), "shared_call")
	if next["id"] != "fc_exact_cache" || objectValue(next["references"].([]any)[0])["sheet_id"] != json.Number("9007199254740993") {
		t.Fatal("caller mutation changed native cache snapshot")
	}
}

func TestActiveToolCatalogSurvivesInactiveSessionChurn(t *testing.T) {
	active := sessionTestSource(t.Name())
	rememberToolCatalog(active, active["tools"])
	for index := range 256 {
		other := sessionTestSource(fmt.Sprintf("%s-other-%d", t.Name(), index))
		rememberToolCatalog(other, other["tools"])
		if got := rememberedToolCatalog(active); got == nil {
			t.Fatalf("actively used tool catalog was evicted by inactive session %d", index)
		}
	}
}

func TestProtocolCacheLRUIdleExpiryAndByteAccounting(t *testing.T) {
	cache := newProtocolCache(2, 1024, 512, time.Minute)
	now := time.Now()
	cache.now = func() time.Time { return now }
	cache.put("active", "first", 21, true)
	cache.put("old", "second", 22, true)
	if _, ok := cache.get("active"); !ok {
		t.Fatal("initial cache miss")
	}
	cache.put("new", "third", 21, true)
	if _, ok := cache.get("old"); ok {
		t.Fatal("least recently used entry survived capacity eviction")
	}
	now = now.Add(59 * time.Second)
	if _, ok := cache.get("active"); !ok {
		t.Fatal("active cache expired early")
	}
	now = now.Add(2 * time.Second)
	if _, ok := cache.get("new"); ok {
		t.Fatal("idle cache survived its TTL")
	}
	if _, ok := cache.get("active"); !ok {
		t.Fatal("hit did not extend idle lifetime")
	}
	now = now.Add(time.Minute)
	if _, ok := cache.get("active"); ok {
		t.Fatal("entry survived exact idle expiry")
	}
	if cache.bytes != 0 || len(cache.items) != 0 || cache.recent.Len() != 0 {
		t.Fatal("expired entries retained accounting or references")
	}
}

func TestProtocolCacheByteQuotaReplacementAndExplicitNil(t *testing.T) {
	cache := newProtocolCache(10, 600, 400, time.Hour)
	if !cache.put("one", "value", 240, true) || !cache.put("two", "value", 240, true) {
		t.Fatal("valid entries rejected")
	}
	if _, ok := cache.get("one"); ok {
		t.Fatal("byte budget did not evict old entry")
	}
	if cache.bytes > 600 {
		t.Fatal("byte budget exceeded")
	}
	if cache.put("two", "oversized replacement", 401, true) {
		t.Fatal("per-entry limit ignored")
	}
	if _, ok := cache.get("two"); ok || cache.bytes != 0 {
		t.Fatal("oversized replacement left stale cached value")
	}
	if !cache.put("revoked", nil, 16, true) {
		t.Fatal("empty catalog not cached")
	}
	cache.put("revoked", "old tools", 25, false)
	if value, exists := cache.get("revoked"); !exists || value != nil {
		t.Fatal("late completion replaced explicit empty snapshot")
	}
}

func TestOversizedCatalogCannotRestoreStaleTools(t *testing.T) {
	older := sessionTestSource(t.Name())
	older["__bps_effective_tools"] = cloneJSONValue(older["tools"])
	rememberToolCatalog(older, older["tools"])
	newer := sessionTestSource(t.Name())
	objectValue(newer["tools"].([]any)[0])["description"] = strings.Repeat("x", protocolCacheEntryBytes+1)
	rememberToolCatalog(newer, newer["tools"])
	follow := map[string]any{"session_id": t.Name()}
	if len(clientToolSpecs(follow)) != 0 {
		t.Fatal("oversized catalog retained stale predecessor")
	}
	RememberResponseContext(older, map[string]any{"id": "resp-oversized-" + t.Name()})
	if len(clientToolSpecs(follow)) != 0 {
		t.Fatal("late completion restored stale tools after oversized update")
	}
}

func TestGeneratedCallIDCachePreservesNativeNumericMetadata(t *testing.T) {
	source := sessionTestSource(t.Name())
	native := sessionTestNative("fc_missing_call_id")
	delete(native, "call_id")
	native["references"] = []any{map[string]any{"sheet_id": json.Number("9007199254740993")}}
	call, ok := extractNativeClientToolCall(map[string]any{"output": []any{native}}, source)
	if !ok {
		t.Fatal("native call without call_id was not translated")
	}
	cached := rememberedNativeCallInNamespace(nativeCallNamespace(source), stringValue(call["call_id"]))
	if objectValue(cached["references"].([]any)[0])["sheet_id"] != json.Number("9007199254740993") {
		t.Fatal("assigning generated call_id rounded cached metadata")
	}
}
