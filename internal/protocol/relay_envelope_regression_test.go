package protocol

import (
	"strings"
	"testing"
)

func relayEnvelopeNative(code any) map[string]any {
	native := relayCompatNative(map[string]any{"tool": "functions.exec", "args": "text(1)"})
	native["arguments"] = string(jsonBytes(map[string]any{"code": code}))
	return native
}

func TestRelayOuterArgumentsRecoverBoundedJSONStrings(t *testing.T) {
	source := repairTestSource()
	for _, code := range []any{map[string]any{"tool": "functions.exec", "args": " text(1) "}, `{"tool":"functions.exec","args":" text(1) "}`} {
		for depth := 0; depth < 5; depth++ {
			native := relayEnvelopeNative(code)
			for layer := 0; layer < depth; layer++ {
				native["arguments"] = string(jsonBytes(native["arguments"]))
			}
			call, reason := decodeNativeClientToolCallFromItem(native, source, false)
			if depth < 4 {
				if reason != "" || call["input"] != " text(1) " {
					t.Fatalf("depth %d: call=%#v, reason=%s", depth, call, reason)
				}
			} else if reason == "" {
				t.Fatal("unbounded outer encoding accepted")
			}
		}
	}
}

func TestRelayRejectsDuplicateKeysAndConflictingPayloads(t *testing.T) {
	source := repairTestSource()
	for _, code := range []string{
		`{"tool":"functions.exec","tool":"functions.exec","args":"text(1)"}`,
		`{"tool":"functions.exec","args":"text(1)","args":"text(2)"}`,
		`{"tool":"functions.exec","args":"text(1)","input":"text(1)"}`,
		`functions.exec({"tool":"functions.exec","tool":"functions.exec","args":"text(1)"})`,
	} {
		for _, raw := range []string{code, "Relay: " + code} {
			native := relayEnvelopeNative(raw)
			_, reason := decodeNativeClientToolCallFromItem(native, source, false)
			response := map[string]any{"status": "completed", "output": []any{native}}
			if reason == "" || ToolRepairEligible(source, response) {
				t.Fatalf("ambiguous envelope accepted or repaired: %q", raw)
			}
		}
	}
	native := relayEnvelopeNative(`{"tool":"functions.exec","args":"text(1)"}`)
	original := native["arguments"].(string)
	for _, outer := range []string{strings.TrimSuffix(original, "}") + `,"code":"broken"}`, `{"summary":"missing code"}`} {
		native["arguments"] = outer
		response := map[string]any{"status": "completed", "output": []any{native}}
		if _, reason := decodeNativeClientToolCallFromItem(native, source, false); reason == "" || ToolRepairEligible(source, response) {
			t.Fatal("ambiguous or missing outer code accepted")
		}
	}
}

func TestToolRepairOnlyRegeneratesKnownTruncatedEnvelope(t *testing.T) {
	for _, test := range []struct {
		name, code string
		want       bool
	}{
		{"missing closing brace", `{"tool":"functions.exec","args":"text(1)"`, true},
		{"truncated custom input", `{"tool":"functions.exec","args":"text(1)`, true},
		{"payload before tool", `{"args":"text(1)","tool":"functions.exec"`, true},
		{"unknown truncated tool", `{"tool":"exec_command","args":{`, false},
		{"missing payload", `{"tool":"functions.exec"`, false},
		{"truncated name", `{"tool":"functions.ex`, false},
		{"conflicting name", `{"tool":"functions.exec","name":"exec_command","args":"x`, false},
		{"conflicting payload", `{"tool":"functions.exec","args":"x","input":"y`, false},
		{"duplicate name", `{"tool":"functions.exec","tool":"functions.exec","args":"x`, false},
		{"duplicate nested key", `{"tool":"functions.exec","args":{"x":1,"x":`, false},
		{"invalid delimiter", `{"tool":"functions.exec","args"="x`, false},
		{"second candidate", `{"tool":"functions.exec","args":"x"} {`, false},
		{"nested self", `{"tool":"functions.run_officejs","args":{"code":`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := repairTestSource()
			source["session_id"] = t.Name()
			native := relayEnvelopeNative(test.code)
			response := map[string]any{"status": "completed", "output": []any{native}}
			if _, reason := decodeNativeClientToolCallFromItem(native, source, true); reason == "" {
				t.Fatal("partial envelope was executable")
			}
			if got := ToolRepairEligible(source, response); got != test.want {
				value, _, parseErr := strictRelayJSONValue(test.code, true)
				t.Fatalf("eligible=%v, want %v; partial=%#v parse error=%T %v", got, test.want, value, parseErr, parseErr)
			}
			if rememberedNativeCallInNamespace(nativeCallNamespace(source), "call_compat") != nil {
				t.Fatal("partial call entered replay cache")
			}
			if test.want {
				prepared, err := PrepareResponsesBody(source, DefaultConfig())
				if err != nil {
					t.Fatal(err)
				}
				fixed, ok := PrepareToolRepairBody(prepared, source, response)
				if !ok {
					t.Fatal("eligible partial envelope was not repairable")
				}
				items := fixed["input"].([]any)
				feedback := stringValue(objectValue(items[len(items)-2])["output"])
				if !strings.Contains(feedback, "incomplete JSON") || !strings.Contains(feedback, "No client tool was executed") {
					t.Fatalf("wrong feedback: %q", feedback)
				}
			}
		})
	}
}

func TestToolRepairAllowsOnlyCompletePairedExplicitHistory(t *testing.T) {
	for _, kind := range []string{"function", "custom_tool"} {
		source := repairTestSource()
		source["input"] = []any{
			map[string]any{"type": kind + "_call", "name": "functions.exec", "call_id": "call_prior", "input": "text(1)", "arguments": "{}"},
			map[string]any{"type": kind + "_call_output", "call_id": "call_prior", "output": "already executed"},
			messageItem("user", "continue"),
		}
		if !ToolRepairEligible(source, repairTestResponse()) {
			t.Fatal("complete explicit history prevented regeneration")
		}
		for _, variation := range []string{"orphan", "pending", "duplicate", "wrong kind", "previous alias", "reused call"} {
			altered := objectValue(cloneJSONValue(source))
			items := altered["input"].([]any)
			switch variation {
			case "orphan":
				altered["input"] = items[1:]
			case "pending":
				altered["input"] = items[:1]
			case "duplicate":
				altered["input"] = append(items, items[1])
			case "wrong kind":
				objectValue(items[1])["type"] = "unrelated_call_output"
			case "previous alias":
				altered["previousResponseId"] = "resp_opaque"
			case "reused call":
				objectValue(items[0])["call_id"] = "call_compat"
				objectValue(items[1])["call_id"] = "call_compat"
			}
			if ToolRepairEligible(altered, repairTestResponse()) {
				t.Fatalf("unsafe history accepted: %s", variation)
			}
		}
	}
}

func TestToolRepairDoesNotReuseCompletedHistoryCallID(t *testing.T) {
	source, original := repairTestSource(), repairTestResponse()
	source["input"] = []any{
		map[string]any{"type": "custom_tool_call", "name": "functions.exec", "call_id": "call_prior", "input": "text(1)"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_prior", "output": "already executed"},
	}
	repaired := repairTestResponse()
	native := relayCompatNative(map[string]any{"tool": "functions.exec", "args": "text(2)"})
	native["call_id"] = "call_prior"
	repaired["output"] = []any{native}
	if merged, err := MergeToolRepairResponse(source, original, repaired); err == nil || merged != nil {
		t.Fatal("repair reused a completed call identity")
	}
}
