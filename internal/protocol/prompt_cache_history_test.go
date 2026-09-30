package protocol

import (
	"bytes"
	"testing"
)

// Historical calls are data. Current declarations cannot change their bytes.
func TestPromptCacheHistoricalEnvelopeSurvivesCatalogChanges(t *testing.T) {
	source := sessionTestSource(t.Name())
	source["__bps_session_scope"] = t.Name()
	native := sessionTestNative("fc_historical_cache")
	native["status"] = "completed"
	call, ok := extractNativeClientToolCall(map[string]any{"output": []any{native}}, source)
	if !ok {
		t.Fatal("fixture did not produce a translated client call")
	}
	history := []any{call, map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "original result"}}
	namespace := nativeCallNamespace(source)
	want := jsonBytes(translateInputItemsInNamespace(history, clientToolSpecs(source), namespace))
	for _, tc := range []struct {
		name  string
		tools []any
	}{
		{"removed", []any{relayCompatFunction("other")}},
		{"conflicting", []any{relayCompatFunction("read"), map[string]any{"type": "custom", "name": "read"}}},
		{"revoked", []any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := indexClientToolSpecs(tc.tools)
			if _, callable := resolveClientTool(current, "read"); callable {
				t.Fatal("fixture must revoke the historical tool")
			}
			got := jsonBytes(translateInputItemsInNamespace(history, current, namespace))
			if !bytes.Equal(got, want) {
				t.Fatal("active catalog change rewrote historical native call or result")
			}
			for _, change := range []map[string]any{
				{"name": "another_tool"},
				{"arguments": string(jsonBytes(map[string]any{"path": "different"}))},
				{"id": "different_id"},
			} {
				mismatched := cloneObject(call)
				for key, value := range change {
					mismatched[key] = value
				}
				if nativeCallMatchesClientItem(native, mismatched, current) {
					t.Fatal("history replay accepted mismatched identity or arguments")
				}
			}
		})
	}
}
