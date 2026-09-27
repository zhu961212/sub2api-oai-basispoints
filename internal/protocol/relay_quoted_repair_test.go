package protocol

import (
	"fmt"
	"testing"
)

func TestToolRepairRecognizesBoundedQuotedTruncatedEnvelope(t *testing.T) {
	for _, test := range []struct {
		name, code string
		eligible   bool
	}{
		{"truncated string", `{"tool":"functions.exec","args":"text(1)`, true},
		{"truncated object", `{"tool":"functions.exec","args":"text(1)"`, true},
		{"unknown tool", `{"tool":"unknown","args":"x`, false},
		{"duplicate key", `{"tool":"functions.exec","tool":"functions.exec","args":"x`, false},
		{"conflicting payloads", `{"tool":"functions.exec","args":"x","input":"y`, false},
		{"second candidate", `{"tool":"functions.exec","args":"x"} {`, false},
		{"truncated wrapper", `"{`, false},
	} {
		for depth, code := 0, test.code; depth < 5; depth++ {
			t.Run(fmt.Sprintf("%s/depth=%d", test.name, depth), func(t *testing.T) {
				source := repairTestSource()
				source["session_id"] = t.Name()
				native := relayEnvelopeNative(code)
				response := map[string]any{"status": "completed", "output": []any{native}}
				if call, reason := decodeNativeClientToolCallFromItem(native, source, true); reason == "" || call != nil {
					t.Fatal("truncated or ambiguous call became executable")
				}
				if eligible := ToolRepairEligible(source, response); eligible != (test.eligible && depth < 4) {
					t.Fatalf("unexpected repair eligibility: %t", eligible)
				}
				if rememberedNativeCallInNamespace(nativeCallNamespace(source), "call_compat") != nil {
					t.Fatal("invalid call entered replay cache")
				}
			})
			code = string(jsonBytes(code))
		}
	}
}
