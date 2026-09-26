package protocol

import (
	"encoding/json"
	"testing"
)

func TestFunctionPayloadStringsRejectDuplicateKeys(t *testing.T) {
	source := map[string]any{"tools": []any{relayCompatFunction("exec_command")}}
	for _, arguments := range []string{
		`{"cmd":"read","cmd":"write"}`,
		`{"cmd":"read","c` + string(rune(92)) + `u006dd":"write"}`,
	} {
		native := map[string]any{"type": "function_call", "name": "exec_command", "arguments": arguments}
		if _, reason := decodeNativeClientToolCallFromItem(native, source, false); reason == "" {
			t.Fatal("ambiguous direct function arguments were accepted")
		}
		for _, field := range []string{"args", "arguments", "input"} {
			native := relayCompatNative(map[string]any{"tool": "exec_command", field: arguments})
			if _, reason := decodeNativeClientToolCallFromItem(native, source, false); reason == "" {
				t.Fatalf("ambiguous serialized %s were accepted", field)
			}
		}
	}
	if parsed := parseArguments(`{"cmd":"read","nested":{"id":1,"id":2}}`); parsed != nil {
		t.Fatal("nested duplicate argument keys were accepted")
	}
	parsed := parseArguments(`{"n":9007199254740993}`)
	if parsed["n"] != json.Number("9007199254740993") {
		t.Fatal("strict argument parsing changed integer precision")
	}
}

func TestRawObjectRequiresOneUnambiguousObject(t *testing.T) {
	for _, raw := range []string{
		`{} {}`, `{} trailing`, `{} []`, `null`, `[]`,
		`{"output":[],"output":[{"type":"function_call"}]}`,
		`{"output":[{"arguments":{"code":{"tool":"read","tool":"write","args":{}}}}]}`,
	} {
		if got, err := RawObject([]byte(raw)); err == nil || got != nil {
			t.Fatalf("accepted ambiguous raw object %q: %#v", raw, got)
		}
	}
	got, err := RawObject([]byte(` {"n":9007199254740993,"nested":{"n":1}} `))
	if err != nil || got["n"] != json.Number("9007199254740993") {
		t.Fatalf("valid raw object changed: %#v, %v", got, err)
	}
}

func TestSyntheticStreamPreservesExactJSONNumbers(t *testing.T) {
	original := map[string]any{
		"id": "resp_exact_numbers", "status": "completed", "output": []any{},
		"usage": map[string]any{"input_tokens": json.Number("9007199254740993")},
		"text":  map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{"const": json.Number("9007199254740993")}}},
	}
	copied := cloneObject(original)
	if !jsonValuesEqual(copied, original) {
		t.Fatalf("copy lost numeric precision: %#v", copied)
	}
	objectValue(copied["usage"])["input_tokens"] = json.Number("1")
	if objectValue(original["usage"])["input_tokens"] != json.Number("9007199254740993") {
		t.Fatal("copy changed the original nested object")
	}
	completed, err := ParseFinalStreamResponse(SyntheticStream(original))
	if err != nil || !jsonValuesEqual(completed, original) {
		t.Fatalf("JSON/SSE numeric precision differs: %#v, %v", completed, err)
	}
}
