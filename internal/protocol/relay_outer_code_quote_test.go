package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestRelayOuterUnescapedCodeStringPreservesExactPayload(t *testing.T) {
	input := "  text(\"quoted\");\r\n\t// C:\\workspace\\demo; literal \\n  "
	code := string(jsonBytes(map[string]any{"tool": "functions.exec", "args": input}))
	for index, raw := range []string{
		`{"code":"` + code + `"}`,
		`{"summary":"inspect","code":"` + code + `","destructive":false,"references":[]}`,
		`{"code":" ` + code + ` ","summary":"inspect"}`,
	} {
		for depth := 0; depth < 5; depth++ {
			t.Run(fmt.Sprintf("case=%d/depth=%d", index, depth), func(t *testing.T) {
				source := repairTestSource()
				source["session_id"] = t.Name()
				native := relayEnvelopeNative(code)
				native["arguments"] = raw
				call, reason := decodeNativeClientToolCallFromItem(native, source, true)
				if depth == 4 {
					if reason == "" {
						t.Fatal("exceeded outer unwrap budget")
					}
					return
				}
				if reason != "" || call["input"] != input {
					t.Fatalf("recoverable outer quotes rejected or input changed: call=%#v reason=%s", call, reason)
				}
				replay := translateInputItemsInNamespace([]any{call}, clientToolSpecs(source), nativeCallNamespace(source))
				if len(replay) != 1 || !jsonValuesEqual(replay[0], native) {
					t.Fatal("native replay changed")
				}
			})
			raw = string(jsonBytes(raw))
		}
	}
}

func TestRelayOuterUnescapedCodeStringPreservesFunctionNumbers(t *testing.T) {
	want := map[string]any{"value": json.Number("9007199254740993"), "path": `C:\temp\demo`, "text": "\n\t"}
	code := string(jsonBytes(map[string]any{"tool": "inspect", "args": want}))
	source := map[string]any{"tools": []any{map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "object"}}}}
	native := relayEnvelopeNative(code)
	native["arguments"] = `{"code":"` + code + `"}`
	call, reason := decodeNativeClientToolCallFromItem(native, source, false)
	if reason != "" || !jsonValuesEqual(parseArguments(call["arguments"]), want) {
		t.Fatalf("exact function arguments changed: %#v reason=%s", call, reason)
	}
}

func TestRelayOuterUnescapedCodeStringRejectsAmbiguity(t *testing.T) {
	code := `{"tool":"functions.exec","args":"text(1)"}`
	for _, raw := range []string{
		`{"code":"` + code + `","code":"` + code + `"}`,
		`{"summary":"one","summary":"two","code":"` + code + `"}`,
		`{"code":"` + code + `","references":{"x":1,"x":2}}`,
		`{"code":"` + code + `"} {"code":"` + code + `"}`,
		`{"code":"` + code + ` ` + code + `"}`,
		`{"code":"[{"tool":"functions.exec","args":"text(1)"}]"}`,
		`{"code":"{"tool":"functions.exec","tool":"functions.exec","args":"text(1)"}"}`,
		`{"code":"{"tool":"functions.exec","args":"text(1)","input":"text(2)"}"}`,
		`{"code":"{"tool":"inspect","args":{"n":1,"n":2}}"}`,
		`{"code":"{"tool":"functions.exec","args":"text(1)""}`,
		`{"code":"run_officejs(` + code + `)"}`,
		`{"summary":"bad "quote"","code":"` + code + `"}`,
		`{"code":"` + code + `","summary":"bad "quote""}`,
		`{"code":"` + code + `"`,
		`{"code":"` + code + `","summary":"` + strings.Repeat("x", maxRecoveredEnvelopeBytes) + `"}`,
	} {
		if got := parseTransportArguments(raw); got != nil {
			t.Fatalf("ambiguous outer code accepted: %.200s", raw)
		}
	}
}
