package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func functionStringRepairSource() map[string]any {
	return map[string]any{
		"tools": []any{map[string]any{
			"type": "function", "name": "exec_command",
			"parameters": map[string]any{
				"type": "object", "required": []any{"cmd"}, "additionalProperties": false,
				"properties": map[string]any{
					"cmd":     map[string]any{"type": "string"},
					"workdir": map[string]any{"type": "string"},
					"timeout": map[string]any{"type": "integer"},
					"options": map[string]any{"type": "object"},
				},
			},
		}},
		"input": []any{messageItem("user", "Inspect the fixture")},
	}
}

func functionStringRepairOriginal(code string) map[string]any {
	return map[string]any{"id": "resp_function_original", "status": "completed",
		"output": []any{messageItem("assistant", "Prior progress"), relayEnvelopeNative(code)}}
}

func functionStringRepairCorrected(name string, args any) map[string]any {
	call := relayCompatNative(map[string]any{"tool": name, "args": args})
	call["id"], call["call_id"] = "fc_function_corrected", "call_function_corrected"
	return map[string]any{"id": "resp_function_internal", "status": "completed", "output": []any{call}}
}

func TestToolRepairFunctionStringSyntaxCandidate(t *testing.T) {
	for _, test := range []struct {
		name, code string
		namespace  bool
	}{
		{"cmd only", `{"tool":"exec_command","args":{"cmd":"printf("hello");"}}`, false},
		{"strict prefix fields", `{"tool":"exec_command","args":{"workdir":"/fixture","timeout":12,"options":{"exact":9007199254740993},"cmd":"printf("hello");"}}`, false},
		{"arguments alias", `{"name":"exec_command","arguments":{"cmd":"printf("hello");"}}`, false},
		{"input alias", `{"tool":"exec_command","input":{"cmd":"printf("hello");"}}`, false},
		{"namespace", `{"namespace":"shell","tool":"exec_command","args":{"cmd":"printf("hello");"}}`, true},
	} {
		for depth, code := 0, test.code; depth < 5; depth++ {
			t.Run(fmt.Sprintf("%s/layers=%d", test.name, depth), func(t *testing.T) {
				source := functionStringRepairSource()
				source["session_id"] = t.Name()
				target := "exec_command"
				if test.namespace {
					source["tools"] = []any{map[string]any{"type": "namespace", "name": "shell", "tools": source["tools"]}}
					target = "shell.exec_command"
				}
				original := functionStringRepairOriginal(code)
				native := objectValue(original["output"].([]any)[1])
				if call, reason := decodeNativeClientToolCallFromItem(native, source, true); call != nil || reason != malformedClientToolMessage {
					t.Fatalf("damaged function arguments became executable: %v %s", call, reason)
				}
				if got := ToolRepairEligible(source, original); got != (depth < 4) {
					t.Fatalf("unexpected correction eligibility: %t", got)
				}
				if rememberedNativeCallInNamespace(nativeCallNamespace(source), "call_compat") != nil {
					t.Fatal("damaged function arguments polluted replay state")
				}
				if depth >= 4 {
					return
				}
				prepared, err := PrepareResponsesBody(source, DefaultConfig())
				if err != nil {
					t.Fatal(err)
				}
				before := string(jsonBytes(prepared))
				body, ok := PrepareToolRepairBody(prepared, source, original)
				if !ok || string(jsonBytes(prepared)) != before {
					t.Fatal("repair changed the prepared request")
				}
				history := prepared["input"].([]any)
				input := body["input"].([]any)
				if !jsonValuesEqual(input[:len(history)], history) || !jsonValuesEqual(input[len(input)-3], native) {
					t.Fatal("repair changed history or the rejected native call")
				}
				if !strings.Contains(stringValue(objectValue(input[len(input)-2])["output"]), "No client tool was executed") {
					t.Fatal("repair omitted explicit nonexecution feedback")
				}
				want := map[string]any{"cmd": `printf("hello");`}
				if test.name == "strict prefix fields" {
					want["workdir"], want["timeout"] = "/fixture", json.Number("12")
					want["options"] = map[string]any{"exact": json.Number("9007199254740993")}
				}
				repaired := functionStringRepairCorrected(target, want)
				merged, err := MergeToolRepairResponse(source, original, repaired)
				if err != nil {
					t.Fatal(err)
				}
				if merged["id"] != original["id"] || !jsonValuesEqual(merged["output"].([]any)[0], original["output"].([]any)[0]) {
					t.Fatal("correction changed visible response identity or progress")
				}
				if rememberedNativeCallInNamespace(nativeCallNamespace(source), "call_function_corrected") != nil {
					t.Fatal("merge published replay state")
				}
				_, translated, _, err := TransformResponseBody(jsonBytes(merged), source)
				if err != nil {
					t.Fatal(err)
				}
				call := objectValue(translated["output"].([]any)[1])
				if call["type"] != "function_call" || clientToolKey(call) != target || !jsonValuesEqual(parseArguments(call["arguments"]), want) {
					t.Fatalf("corrected function identity or arguments changed: %#v", call)
				}
			})
			code = string(jsonBytes(code))
		}
	}
}

func TestToolRepairFunctionStringRejectsAmbiguousOrUnrelatedDamage(t *testing.T) {
	for _, test := range []struct{ name, code string }{
		{"duplicate tool", `{"tool":"exec_command","tool":"exec_command","args":{"cmd":"printf("hello");"}}`},
		{"conflicting identity", `{"tool":"exec_command","name":"other","args":{"cmd":"printf("hello");"}}`},
		{"duplicate payload", `{"tool":"exec_command","args":{},"args":{"cmd":"printf("hello");"}}`},
		{"payload aliases", `{"tool":"exec_command","args":{"cmd":"printf("hello");"},"input":{}}`},
		{"metadata after payload", `{"tool":"exec_command","args":{"cmd":"printf("hello");"},"tool":"other"}`},
		{"unknown header", `{"extra":1,"tool":"exec_command","args":{"cmd":"printf("hello");"}}`},
		{"second envelope", `{"tool":"exec_command","args":{"cmd":"printf("hello");"}} {"tool":"exec_command","args":{"cmd":"other"}}`},
		{"array envelope", `[{"tool":"exec_command","args":{"cmd":"printf("hello");"}}]`},
		{"payload before identity", `{"args":{"cmd":"printf("hello");"},"tool":"exec_command"}`},
		{"payload string", `{"tool":"exec_command","args":"printf("hello");"}`},
		{"nested damaged field", `{"tool":"exec_command","args":{"cmd":"safe","options":{"text":"printf("hello");"}}}`},
		{"nonfinal damaged field", `{"tool":"exec_command","args":{"cmd":"printf("hello");","workdir":"/fixture"}}`},
		{"duplicate argument", `{"tool":"exec_command","args":{"cmd":"safe","cmd":"printf("hello");"}}`},
		{"duplicate nested key", `{"tool":"exec_command","args":{"options":{"x":1,"x":2},"cmd":"printf("hello");"}}`},
		{"unknown tool", `{"tool":"not_declared","args":{"cmd":"printf("hello");"}}`},
		{"recursive transport", `{"tool":"run_officejs","args":{"cmd":"printf("hello");"}}`},
		{"possible JSON member in text", `{"tool":"exec_command","args":{"cmd":"const x = {"tool":"other"};"}}`},
		{"bare injected member", `{"tool":"exec_command","args":{"cmd":"printf("hello");", other: "value"}}`},
		{"comment boundary", `{"tool":"exec_command","args":{"cmd":"printf("hello");", /* comment */ other: "value"}}`},
		{"wrong unaffected schema type", `{"tool":"exec_command","args":{"timeout":"wrong","cmd":"printf("hello");"}}`},
		{"unknown argument property", `{"tool":"exec_command","args":{"extra":1,"cmd":"printf("hello");"}}`},
		{"missing required property", `{"tool":"exec_command","args":{"workdir":"printf("hello");"}}`},
		{"schema mismatch without syntax error", `{"tool":"exec_command","args":{"cmd":2}}`},
		{"nonstring damaged property", `{"tool":"exec_command","args":{"cmd":"safe","timeout":"printf("hello");"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := functionStringRepairSource()
			source["session_id"] = t.Name()
			original := functionStringRepairOriginal(test.code)
			if ToolRepairEligible(source, original) {
				t.Fatalf("unsafe or unrelated function damage was repairable: %s", test.code)
			}
			native := objectValue(original["output"].([]any)[1])
			if call, reason := decodeNativeClientToolCallFromItem(native, source, true); call != nil || reason == "" {
				t.Fatal("damaged arguments became executable")
			}
			if rememberedNativeCallInNamespace(nativeCallNamespace(source), "call_compat") != nil {
				t.Fatal("rejected call polluted replay state")
			}
		})
	}
}

func TestToolRepairFunctionStringPreservesEligibilityGuards(t *testing.T) {
	for _, variation := range []string{"pending", "orphan", "previous response", "conversation", "compaction", "failed", "incomplete", "not last", "second tool", "missing call id", "reused call id", "tool choice none", "ambiguous catalog", "missing object schema", "missing property schema"} {
		t.Run(variation, func(t *testing.T) {
			source := functionStringRepairSource()
			original := functionStringRepairOriginal(`{"tool":"exec_command","args":{"cmd":"printf("hello");"}}`)
			native := objectValue(original["output"].([]any)[1])
			prior := map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "call_prior", "input": "done"}
			output := map[string]any{"type": "function_call_output", "call_id": "call_prior", "output": "done"}
			switch variation {
			case "pending":
				source["input"] = []any{prior}
			case "orphan":
				source["input"] = []any{output}
			case "previous response":
				source["previous_response_id"] = "resp_prior"
			case "conversation":
				source["conversation"] = "conv_prior"
			case "compaction":
				source["input"] = []any{map[string]any{"type": "compaction"}}
			case "failed", "incomplete":
				original["status"] = variation
			case "not last":
				original["output"] = append(original["output"].([]any), messageItem("assistant", "after"))
			case "second tool":
				original["output"] = append([]any{relayCompatNative(map[string]any{"tool": "exec_command", "args": map[string]any{"cmd": "safe"}})}, original["output"].([]any)...)
			case "missing call id":
				delete(native, "call_id")
			case "reused call id":
				prior["call_id"], output["call_id"] = native["call_id"], native["call_id"]
				source["input"] = []any{prior, output}
			case "tool choice none":
				source["tool_choice"] = "none"
			case "ambiguous catalog":
				source["tools"] = append(source["tools"].([]any), map[string]any{"type": "custom", "name": "exec_command"})
			case "missing object schema":
				delete(objectValue(source["tools"].([]any)[0]), "parameters")
			case "missing property schema":
				delete(objectValue(objectValue(source["tools"].([]any)[0])["parameters"]), "properties")
			}
			if ToolRepairEligible(source, original) {
				t.Fatal("function string repair bypassed an existing guard")
			}
		})
	}
}

func TestToolRepairFunctionStringPinsTargetAndValidatesOriginalSchema(t *testing.T) {
	for _, variation := range []string{"other function", "other namespace", "custom tool", "schema mismatch", "full string schema", "multiple tools", "failed repair"} {
		t.Run(variation, func(t *testing.T) {
			source := functionStringRepairSource()
			source["session_id"] = t.Name()
			other := objectValue(cloneJSONValue(source["tools"].([]any)[0]))
			other["name"] = "other"
			source["tools"] = append(source["tools"].([]any), other, map[string]any{"type": "custom", "name": "custom_exec"}, map[string]any{"type": "namespace", "name": "shell", "tools": []any{cloneJSONValue(source["tools"].([]any)[0])}})
			original := functionStringRepairOriginal(`{"tool":"exec_command","args":{"cmd":"printf("hello");"}}`)
			target, args := "exec_command", any(map[string]any{"cmd": "safe"})
			switch variation {
			case "other function":
				target = "other"
			case "other namespace":
				target = "shell.exec_command"
			case "custom tool":
				target, args = "custom_exec", "safe"
			case "schema mismatch":
				args = map[string]any{"cmd": 123}
			case "full string schema":
				objectValue(objectValue(objectValue(source["tools"].([]any)[0])["parameters"])["properties"])["cmd"] = map[string]any{"type": "string", "const": "required-command"}
			}
			if !ToolRepairEligible(source, original) {
				t.Fatal("original must remain eligible")
			}
			repaired := functionStringRepairCorrected(target, args)
			if variation == "multiple tools" {
				repaired["output"] = append(repaired["output"].([]any), relayCompatNative(map[string]any{"tool": "exec_command", "args": map[string]any{"cmd": "again"}}))
			}
			if variation == "failed repair" {
				repaired["status"] = "failed"
			}
			if merged, err := MergeToolRepairResponse(source, original, repaired); err == nil || merged != nil {
				t.Fatal("unsafe replacement was accepted")
			}
			if rememberedNativeCallInNamespace(nativeCallNamespace(source), "call_function_corrected") != nil {
				t.Fatal("rejected replacement polluted replay state")
			}
		})
	}
}

func TestToolRepairFunctionStringPreservesUnaffectedArguments(t *testing.T) {
	for _, variation := range []string{"unchanged", "delete", "change", "add", "nested change", "rounded number", "rename damaged field"} {
		t.Run(variation, func(t *testing.T) {
			source := functionStringRepairSource()
			original := functionStringRepairOriginal(`{"tool":"exec_command","args":{"workdir":"/safe","timeout":12,"options":{"exact":9007199254740993},"cmd":"printf("hello");"}}`)
			arguments := map[string]any{"workdir": "/safe", "timeout": json.Number("12"), "options": map[string]any{"exact": json.Number("9007199254740993")}, "cmd": `printf("hello");`}
			switch variation {
			case "delete":
				delete(arguments, "workdir")
			case "change":
				arguments["workdir"] = "/other"
			case "add":
				arguments["extra"] = true
				objectValue(objectValue(source["tools"].([]any)[0])["parameters"])["additionalProperties"] = true
			case "nested change":
				objectValue(arguments["options"])["extra"] = true
			case "rounded number":
				objectValue(arguments["options"])["exact"] = json.Number("9007199254740992")
			case "rename damaged field":
				delete(arguments, "cmd")
				arguments["extra"] = "text"
			}
			if !ToolRepairEligible(source, original) {
				t.Fatal("original must be eligible")
			}
			merged, err := MergeToolRepairResponse(source, original, functionStringRepairCorrected("exec_command", arguments))
			if variation == "unchanged" {
				if err != nil || merged == nil {
					t.Fatalf("exact known arguments rejected: %v", err)
				}
			} else if err == nil || merged != nil {
				t.Fatal("correction changed unaffected arguments")
			}
		})
	}
}
