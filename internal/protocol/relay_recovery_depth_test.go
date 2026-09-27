package protocol

import (
	"fmt"
	"strings"
	"testing"
)

func TestRelayRecoveryStringRepairsPreserveUnwrapBudgetAndInput(t *testing.T) {
	slash := string(byte(92))
	controls := string([]byte{13, 10, 9})
	input := "  text(" + string(byte(34)) + "quoted" + string(byte(34)) + ");" + controls + "const pattern = /" + slash + "d+/; literal " + slash + "n  "
	valid := string(jsonBytes(map[string]any{"tool": "functions.exec", "args": input}))
	literalControls := strings.ReplaceAll(valid, slash+"r"+slash+"n"+slash+"t", controls)
	invalidEscape := strings.ReplaceAll(valid, slash+slash+"d", slash+"d")
	for _, test := range []struct{ name, code string }{
		{"valid", valid},
		{"literal controls", literalControls},
		{"invalid escape", invalidEscape},
		{"controls and escape", strings.ReplaceAll(literalControls, slash+slash+"d", slash+"d")},
	} {
		for depth, code := 0, test.code; depth <= 4; depth++ {
			t.Run(fmt.Sprintf("%s/wrappers=%d", test.name, depth), func(t *testing.T) {
				source := repairTestSource()
				source["session_id"] = t.Name()
				native := relayEnvelopeNative(code)
				call, reason := decodeNativeClientToolCallFromItem(native, source, true)
				if depth == 4 {
					if call != nil || reason != malformedClientToolMessage {
						t.Fatalf("recovery exceeded the unwrap budget: call=%#v reason=%s", call, reason)
					}
					if rememberedNativeCallInNamespace(nativeCallNamespace(source), "call_compat") != nil {
						t.Fatal("overwrapped call entered the replay cache")
					}
					return
				}
				if reason != "" || call["input"] != input {
					t.Fatalf("repair consumed a layer or changed input: call=%#v reason=%s", call, reason)
				}
				replayed := translateInputItemsInNamespace([]any{call}, clientToolSpecs(source), nativeCallNamespace(source))
				if len(replayed) != 1 || !jsonValuesEqual(replayed[0], native) {
					t.Fatalf("recovery changed native call identity or arguments: %#v", replayed)
				}
			})
			code = string(jsonBytes(code))
		}
	}
}

func TestRelayRecoveryRepairedStringsStillRejectAmbiguityAtEveryDepth(t *testing.T) {
	slash := string(byte(92))
	field := func(name string, value any) string {
		return string(jsonBytes(name)) + ":" + string(jsonBytes(value))
	}
	one := strings.ReplaceAll(string(jsonBytes(map[string]any{"tool": "functions.exec", "args": slash + "d+"})), slash+slash+"d", slash+"d")
	if recovered := recoverTransportEnvelope(one); recovered == nil || recovered["args"] != slash+"d+" {
		t.Fatal("fixture must start with one recoverable unambiguous envelope")
	}
	nestedDuplicate := "{" + field("tool", "inspect") + "," + string(jsonBytes("args")) + ":{" + field("pattern", slash+"d+") + "," + field("pattern", "again") + "}}"
	nestedDuplicate = strings.ReplaceAll(nestedDuplicate, slash+slash+"d", slash+"d")
	for _, test := range []struct{ name, code string }{
		{"duplicate tool", strings.Replace(one, "{", "{"+field("tool", "functions.exec")+",", 1)},
		{"duplicate payload", strings.TrimSuffix(one, "}") + "," + field("args", "again") + "}"},
		{"conflicting payload", strings.TrimSuffix(one, "}") + "," + field("input", "again") + "}"},
		{"conflicting tool", strings.TrimSuffix(one, "}") + "," + field("name", "other") + "}"},
		{"multiple objects", one + " " + one},
		{"array", "[" + one + "]"},
		{"truncated object", strings.TrimSuffix(one, "}")},
		{"nested duplicate", nestedDuplicate},
	} {
		for depth, code := 0, test.code; depth <= 4; depth++ {
			t.Run(fmt.Sprintf("%s/wrappers=%d", test.name, depth), func(t *testing.T) {
				if got := recoverTransportEnvelope(code); got != nil {
					t.Fatalf("syntax repair accepted an ambiguous or incomplete envelope: %#v", got)
				}
			})
			code = string(jsonBytes(code))
		}
	}
}
