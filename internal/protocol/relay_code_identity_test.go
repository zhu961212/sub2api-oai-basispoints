package protocol

import "testing"

func codeIdentityMalformedNative(name string) map[string]any {
	return relayEnvelopeNative(`{"tool":` + string(jsonBytes(name)) + `,"args":"text("x");"}`)
}

func TestToolRepairUnderescapedCodeRejectsDifferentCatalogTarget(t *testing.T) {
	for _, kind := range []string{"custom", "function", "custom_args_first", "function_args_first"} {
		t.Run(kind, func(t *testing.T) {
			source := repairTestSource()
			source["session_id"] = t.Name()
			target, payload := "functions.other", any("text(2)")
			other := map[string]any{"type": "custom", "name": target}
			if kind == "function" || kind == "function_args_first" {
				other = relayCompatFunction(target)
				payload = map[string]any{"cmd": "inspect"}
			}
			source["tools"] = append(source["tools"].([]any), other)
			originalCall := codeIdentityMalformedNative("functions.exec")
			if kind == "custom_args_first" || kind == "function_args_first" {
				originalCall = relayEnvelopeNative(`{"args":"text("x");","tool":"functions.exec"}`)
			}
			original := map[string]any{"status": "completed", "output": []any{originalCall}}
			if !ToolRepairEligible(source, original) {
				t.Fatal("fixture must be eligible for code quoting correction")
			}
			corrected := relayCompatNative(map[string]any{"tool": target, "args": payload})
			corrected["id"], corrected["call_id"] = "fc_identity_corrected", "call_identity_corrected"
			if call, reason := decodeNativeClientToolCallFromItem(corrected, source, false); call == nil || reason != "" {
				t.Fatalf("replacement must be valid in the current catalog: %s", reason)
			}
			repaired := map[string]any{"status": "completed", "output": []any{corrected}}
			merged, err := MergeToolRepairResponse(source, original, repaired)
			if err == nil || merged != nil {
				t.Error("code quoting correction switched to another catalog target")
			}
			for _, callID := range []string{stringValue(originalCall["call_id"]), stringValue(corrected["call_id"])} {
				if rememberedNativeCallInNamespace(nativeCallNamespace(source), callID) != nil {
					t.Errorf("rejected target swap populated replay cache for %s", callID)
				}
			}
		})
	}
}

func TestToolRepairUnderescapedCodeAllowsCanonicalTargetAliases(t *testing.T) {
	for _, catalogName := range []string{"functions.exec", "exec"} {
		for _, repairedName := range []string{"functions.exec", "exec"} {
			t.Run(catalogName+"/"+repairedName, func(t *testing.T) {
				source := repairTestSource()
				source["session_id"] = t.Name()
				source["tools"] = []any{map[string]any{"type": "custom", "name": catalogName}}
				original := map[string]any{"status": "completed", "output": []any{codeIdentityMalformedNative(catalogName)}}
				corrected := relayCompatNative(map[string]any{"tool": repairedName, "args": `text("x");`})
				corrected["id"], corrected["call_id"] = "fc_alias_corrected", "call_alias_corrected"
				repaired := map[string]any{"status": "completed", "output": []any{corrected}}
				merged, err := MergeToolRepairResponse(source, original, repaired)
				if err != nil || merged == nil {
					t.Fatalf("canonical target alias was rejected: %v", err)
				}
				if rememberedNativeCallInNamespace(nativeCallNamespace(source), "call_alias_corrected") != nil {
					t.Fatal("merge validation published replay state before transformation")
				}
				_, translated, _, err := TransformResponseBody(jsonBytes(merged), source)
				if err != nil {
					t.Fatal(err)
				}
				call := objectValue(translated["output"].([]any)[0])
				if clientToolKey(call) != catalogName || call["type"] != "custom_tool_call" || call["input"] != `text("x");` {
					t.Fatalf("canonical target, type or input changed: %#v", call)
				}
			})
		}
	}
}

func TestToolRepairUnknownHelperStillAllowsKnownExecutor(t *testing.T) {
	source := repairTestSource()
	source["session_id"] = t.Name()
	original := repairTestResponse()
	if !ToolRepairEligible(source, original) {
		t.Fatal("unknown helper must remain repairable")
	}
	corrected := relayCompatNative(map[string]any{"tool": "functions.exec", "args": "text(1)"})
	corrected["id"], corrected["call_id"] = "fc_helper_corrected", "call_helper_corrected"
	repaired := map[string]any{"status": "completed", "output": []any{corrected}}
	merged, err := MergeToolRepairResponse(source, original, repaired)
	if err != nil || merged == nil {
		t.Fatalf("unknown helper correction to known executor was rejected: %v", err)
	}
	if rememberedNativeCallInNamespace(nativeCallNamespace(source), "call_helper_corrected") != nil {
		t.Fatal("merge validation published replay state before transformation")
	}
	_, translated, _, err := TransformResponseBody(jsonBytes(merged), source)
	if err != nil {
		t.Fatal(err)
	}
	call := objectValue(translated["output"].([]any)[0])
	if clientToolKey(call) != "functions.exec" || call["type"] != "custom_tool_call" {
		t.Fatalf("known executor correction did not preserve the expected target: %#v", call)
	}
}
