package protocol

import (
	"fmt"
	"strings"
	"testing"
)

func relayControlEscape(ch byte) string {
	encoded := string(jsonBytes(string(ch)))
	return encoded[1 : len(encoded)-1]
}

func TestRelayLiteralControlsPreserveCustomInputAtEveryLayer(t *testing.T) {
	for ch := byte(0); ch < 0x20; ch++ {
		for _, layer := range []string{"inner", "outer object", "outer string"} {
			t.Run(fmt.Sprintf("%02x/%s", ch, layer), func(t *testing.T) {
				want := "  prefix" + string(ch) + "suffix\\n  "
				envelope := map[string]any{"tool": "functions.exec", "args": want}
				code := strings.Replace(string(jsonBytes(envelope)), relayControlEscape(ch), string(ch), 1)
				native := relayEnvelopeNative(code)
				if layer == "outer object" {
					native = relayEnvelopeNative(envelope)
				}
				if layer != "inner" {
					native["arguments"] = strings.Replace(native["arguments"].(string), relayControlEscape(ch), string(ch), 1)
				}
				source := repairTestSource()
				source["session_id"] = t.Name()
				before := string(jsonBytes(native))
				call, reason := decodeNativeClientToolCallFromItem(native, source, true)
				if reason != "" || call["input"] != want {
					t.Fatalf("literal control rejected or input changed: call=%#v reason=%s", call, reason)
				}
				if string(jsonBytes(native)) != before {
					t.Fatal("control recovery mutated original arguments")
				}
				replayed := translateInputItemsInNamespace([]any{call}, clientToolSpecs(source), nativeCallNamespace(source))
				if len(replayed) != 1 || !jsonValuesEqual(replayed[0], native) {
					t.Fatal("control recovery changed native replay identity")
				}
			})
		}
	}
}

func TestRelayLiteralControlsPreserveNestedFunctionDataAndValidEscapes(t *testing.T) {
	controls := ""
	for ch := byte(0); ch < 0x20; ch++ {
		controls += string(ch)
	}
	want := map[string]any{"text": "  " + controls + "  ", "nested": []any{map[string]any{"literal": "\\u0000\\b\\f\\n\\r\\t\\\\\""}}}
	valid := string(jsonBytes(map[string]any{"tool": "inspect", "args": want}))
	malformed := "{\"tool\":\"inspect\",\"args\":{\"text\":\"  " + controls + "  \",\"nested\":" + string(jsonBytes(want["nested"])) + "}}"
	source := map[string]any{"tools": []any{map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "object"}}}}
	for _, code := range []string{valid, malformed} {
		call, reason := decodeNativeClientToolCallFromItem(relayEnvelopeNative(code), source, false)
		if reason != "" || !jsonValuesEqual(parseArguments(call["arguments"]), want) {
			t.Fatalf("nested function controls or valid escapes changed: call=%#v reason=%s", call, reason)
		}
	}
}

func TestRelayLiteralControlRecoveryKeepsAmbiguityAndSizeLimits(t *testing.T) {
	one := strings.ReplaceAll(string(jsonBytes(map[string]any{"tool": "functions.exec", "args": "prefix" + string(byte(0)) + "suffix"})), relayControlEscape(0), string(byte(0)))
	for _, code := range []string{
		strings.Replace(one, "{", "{\"tool\":\"functions.exec\",", 1),
		strings.TrimSuffix(one, "}") + ",\"input\":\"other\"}",
		one + one,
		one + " []",
		"[" + one + "]",
		strings.TrimSuffix(one, "}"),
		strings.Repeat(" ", maxRecoveredEnvelopeBytes) + one,
	} {
		if got := recoverTransportEnvelope(code); got != nil {
			t.Fatalf("control recovery accepted ambiguous, truncated, or oversized data: %#v", got)
		}
	}
	for _, outer := range []string{
		"{\"code\":" + one + ",\"code\":" + one + "}",
		"{\"code\":" + one + "} false",
		strings.Repeat(" ", maxRecoveredEnvelopeBytes) + "{\"code\":" + one + "}",
	} {
		if got := parseTransportArguments(outer); got != nil {
			t.Fatal("outer control recovery accepted duplicate, trailing, or oversized data")
		}
	}
	for ch := byte(0); ch < 0x20; ch++ {
		if strings.ContainsRune(" \r\n\t", rune(ch)) {
			continue
		}
		raw := "{\"code\":" + string(ch) + string(jsonBytes(one)) + "}"
		if repairEnvelopeStrings(raw) != raw || parseTransportArguments(raw) != nil {
			t.Fatalf("unquoted control %02x was repaired", ch)
		}
	}
}

func TestRelayLiteralControlRecoveryKeepsStringUnwrapLimits(t *testing.T) {
	want := "a" + string(byte(0x0c)) + "b"
	leaf := strings.ReplaceAll(string(jsonBytes(map[string]any{"tool": "functions.exec", "args": want})), relayControlEscape(0x0c), string(byte(0x0c)))
	for _, outer := range []bool{false, true} {
		code := leaf
		if outer {
			code = "{\"code\":" + leaf + "}"
		}
		for depth := 0; depth <= 4; depth++ {
			native := relayEnvelopeNative(code)
			if outer {
				native["arguments"] = code
			}
			call, reason := decodeNativeClientToolCallFromItem(native, repairTestSource(), false)
			if depth < 4 {
				if reason != "" || call["input"] != want {
					t.Fatalf("outer=%t depth=%d: call=%#v reason=%s", outer, depth, call, reason)
				}
			} else if reason != malformedClientToolMessage {
				t.Fatal("control recovery exceeded unwrap budget")
			}
			code = string(jsonBytes(code))
		}
	}
}
