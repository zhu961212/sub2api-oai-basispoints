package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func repairTestSource() map[string]any {
	return map[string]any{"session_id": "repair-first-turn", "tools": []any{map[string]any{"type": "custom", "name": "functions.exec", "description": "Run JavaScript with tools.exec_command as a nested helper"}}, "input": []any{messageItem("user", "Inspect my project")}}
}

func repairTestResponse() map[string]any {
	return map[string]any{"id": "resp_original", "status": "completed", "output": []any{relayCompatNative(map[string]any{"tool": "exec_command", "args": map[string]any{"cmd": "pwd"}})}}
}

func TestToolRepairPreservesPreparedContextAndProvidesCatalogShape(t *testing.T) {
	source := repairTestSource()
	prepared, err := PrepareResponsesBody(source, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	before := string(jsonBytes(prepared))
	rejected := repairTestResponse()
	fixed, ok := PrepareToolRepairBody(prepared, source, rejected)
	if !ok || string(jsonBytes(prepared)) != before {
		t.Fatal("correction rejected or modified the prepared request")
	}
	for key, value := range prepared {
		if key != "input" && !jsonValuesEqual(value, fixed[key]) {
			t.Fatalf("correction changed %s", key)
		}
	}
	prior := prepared["input"].([]any)
	items := fixed["input"].([]any)
	if len(items) != len(prior)+3 || !jsonValuesEqual(items[:len(prior)], prior) {
		t.Fatal("correction lost original history")
	}
	feedback := objectValue(items[len(items)-2])
	if feedback["type"] != "function_call_output" || feedback["call_id"] != "call_compat" || !strings.Contains(stringValue(feedback["output"]), "No client tool was executed") {
		t.Fatalf("missing explicit nonexecution feedback: %#v", feedback)
	}
	prompt := relayGuidanceText(items[len(items)-1])
	shape := string(jsonBytes(map[string]any{"tool": "functions.exec", "args": "<raw custom-tool input>"}))
	if !strings.Contains(prompt, shape) || !strings.Contains(prompt, "Client tools (exact code.tool allowlist)") {
		t.Fatal("repair omitted the actual custom envelope shape")
	}
	if rememberedNativeCallInNamespace(nativeCallNamespace(source), "call_compat") != nil {
		t.Fatal("rejected call polluted replay cache")
	}
}

func TestToolRepairEligibilityNeverReplaysSideEffectsOrOtherFailures(t *testing.T) {
	for _, name := range []string{"history_call", "history_output", "previous_response", "conversation", "mixed_tools", "not_last", "direct_unknown", "schema_error", "ambiguous", "tool_choice_none", "failed"} {
		t.Run(name, func(t *testing.T) {
			source, response := repairTestSource(), repairTestResponse()
			output := response["output"].([]any)
			switch name {
			case "history_call":
				source["input"] = []any{map[string]any{"type": "custom_tool_call", "name": "functions.exec", "input": "side effect"}}
			case "history_output":
				source["input"] = []any{map[string]any{"type": "function_call_output", "output": "already executed"}}
			case "previous_response":
				source["previous_response_id"] = "resp_prior"
			case "conversation":
				source["conversation"] = "conv_prior"
			case "mixed_tools":
				response["output"] = append([]any{relayCompatNative(map[string]any{"tool": "functions.exec", "args": "text(1)"})}, output...)
			case "not_last":
				response["output"] = append(output, messageItem("assistant", "extra"))
			case "direct_unknown":
				response["output"] = []any{map[string]any{"type": "function_call", "name": "exec_command", "call_id": "call_direct", "arguments": "{}"}}
			case "schema_error":
				source["tools"] = []any{relayCompatFunction("exec_command")}
				response["output"] = []any{relayCompatNative(map[string]any{"tool": "exec_command", "args": map[string]any{"wrong": true}})}
			case "ambiguous":
				source["tools"] = []any{relayCompatFunction("exec_command"), map[string]any{"type": "custom", "name": "exec_command"}}
			case "tool_choice_none":
				source["tool_choice"] = "none"
			case "failed":
				response["status"] = "failed"
			}
			if ToolRepairEligible(source, response) {
				t.Fatal("unsafe or unrelated failure is repairable")
			}
		})
	}
}

func TestToolRepairMergeValidatesNamespacesPreservesReplayAndExactUsage(t *testing.T) {
	for _, name := range []string{"flat_custom", "namespace_custom", "namespace_function"} {
		t.Run(name, func(t *testing.T) {
			source := repairTestSource()
			source["session_id"] = t.Name()
			tool, payload := "functions.exec", any("text(await tools.exec_command({cmd: 'pwd'}));")
			if name == "namespace_custom" {
				source["tools"] = []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{map[string]any{"type": "custom", "name": "exec"}}}}
			}
			if name == "namespace_function" {
				source["tools"] = []any{map[string]any{"type": "namespace", "name": "files", "tools": []any{relayCompatFunction("inspect")}}}
				tool, payload = "files.inspect", map[string]any{"cmd": "pwd"}
			}
			original := repairTestResponse()
			original["usage"] = map[string]any{"input_tokens": json.Number("9007199254740993"), "input_tokens_details": map[string]any{"cached_tokens": json.Number("11")}}
			native := relayCompatNative(map[string]any{"tool": tool, "input": payload})
			native["call_id"], native["id"] = "call_fixed", "fc_fixed"
			repaired := map[string]any{"id": "resp_repair", "status": "completed", "output": []any{native}, "usage": map[string]any{"input_tokens": json.Number("2"), "input_tokens_details": map[string]any{"cached_tokens": json.Number("3")}}}
			merged, err := MergeToolRepairResponse(source, original, repaired)
			if err != nil {
				t.Fatal(err)
			}
			usage := objectValue(merged["usage"])
			if merged["id"] != original["id"] || usage["input_tokens"] != json.Number("9007199254740995") || objectValue(usage["input_tokens_details"])["cached_tokens"] != json.Number("14") {
				t.Fatalf("response identity or exact usage lost: %#v", merged)
			}
			_, translated, _, err := TransformResponseBody(jsonBytes(merged), source)
			if err != nil {
				t.Fatal(err)
			}
			call := objectValue(translated["output"].([]any)[0])
			if clientToolKey(call) != tool {
				t.Fatalf("qualified name changed: %#v", call)
			}
			replay := translateInputItemsInNamespace([]any{call}, clientToolSpecs(source), nativeCallNamespace(source))
			if len(replay) != 1 || !jsonValuesEqual(replay[0], native) {
				t.Fatalf("corrected call did not replay its exact native identity: %#v", replay)
			}
		})
	}
}

func TestToolRepairRejectsConflictingOutputIdentities(t *testing.T) {
	for _, duplicate := range []string{"prefix_id", "repair_id", "repair_call_id"} {
		t.Run(duplicate, func(t *testing.T) {
			source, original := repairTestSource(), repairTestResponse()
			message := messageItem("assistant", "working")
			message["id"] = "msg_original"
			original["output"] = append([]any{message}, original["output"].([]any)...)
			call := relayCompatNative(map[string]any{"tool": "functions.exec", "args": "text(1)"})
			call["id"], call["call_id"] = "fc_fixed", "call_fixed"
			reasoning := map[string]any{"type": "reasoning", "id": "rs_fixed"}
			switch duplicate {
			case "prefix_id":
				call["id"] = "msg_original"
			case "repair_id":
				reasoning["id"] = "fc_fixed"
			case "repair_call_id":
				reasoning["call_id"] = "call_fixed"
			}
			repaired := map[string]any{"status": "completed", "output": []any{reasoning, call}}
			if merged, err := MergeToolRepairResponse(source, original, repaired); err == nil || merged != nil {
				t.Fatalf("conflicting identity accepted: %#v", merged)
			}
		})
	}
}
