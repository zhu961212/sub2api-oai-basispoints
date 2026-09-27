package protocol

import (
	"fmt"
	"strings"
	"testing"
)

func codeTextTestInput() string {
	patch := "*** Begin Patch\n*** Add File: main.c\n" +
		`+#include <stdio.h>` + "\n" +
		`+#include "local.h"` + "\n" +
		`+int main(void) { const char *path = "C:\\temp\\main.c"; printf("%s: %d\n\t\0\x41", path, 42); return 0; }` + "\n*** End Patch\n"
	return "  text(await tools.apply_patch(" + string(jsonBytes(patch)) + "));\r\n\t"
}

func TestRelayCodeTextPreservesCSourceAndNestedPatchBytes(t *testing.T) {
	input := codeTextTestInput()
	for _, extra := range []string{"", string([]byte{0, 1, 8, 12}), ` const data = {"args":"\n","quote":"\""};`} {
		want := input + extra
		source := repairTestSource()
		source["session_id"] = t.Name() + extra
		native := relayCompatNative(map[string]any{"tool": "functions.exec", "args": want})
		before := string(jsonBytes(native))
		call, reason := decodeNativeClientToolCallFromItem(native, source, true)
		if reason != "" || call["input"] != want {
			t.Fatalf("serialized C source changed: call=%#v reason=%s", call, reason)
		}
		if string(jsonBytes(native)) != before {
			t.Fatal("decoding mutated native arguments")
		}
		replay := translateInputItemsInNamespace([]any{call}, clientToolSpecs(source), nativeCallNamespace(source))
		if len(replay) != 1 || !jsonValuesEqual(replay[0], native) {
			t.Fatal("raw source or native identity changed on replay")
		}
	}
}

func TestToolRepairClassifiesUnderescapedCustomCodeWithoutExecuting(t *testing.T) {
	input := codeTextTestInput()
	broken := `{"tool":"functions.exec","args":"` + input + `"}`
	for depth, code := 0, broken; depth < 5; depth++ {
		t.Run(fmt.Sprintf("layers=%d", depth), func(t *testing.T) {
			source := repairTestSource()
			source["session_id"] = t.Name()
			prepared, err := PrepareResponsesBody(source, DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			native := relayEnvelopeNative(code)
			original := map[string]any{"id": "resp_bad_code", "status": "completed", "output": []any{native}}
			before := string(jsonBytes(native))
			if call, reason := decodeNativeClientToolCallFromItem(native, source, true); call != nil || reason != malformedClientToolMessage {
				t.Fatalf("underescaped source was executed: call=%#v reason=%s", call, reason)
			}
			if ToolRepairEligible(source, original) != (depth < 4) {
				t.Fatal("wrong eligibility for unique underescaped custom input")
			}
			if rememberedNativeCallInNamespace(nativeCallNamespace(source), "call_compat") != nil {
				t.Fatal("malformed source entered replay cache")
			}
			if depth >= 4 {
				return
			}
			request, ok := PrepareToolRepairBody(prepared, source, original)
			if !ok {
				t.Fatal("eligible syntax error could not request one correction")
			}
			items := request["input"].([]any)
			if !jsonValuesEqual(items[len(items)-3], native) || string(jsonBytes(native)) != before {
				t.Fatal("correction changed the malformed original source")
			}
			feedback := stringValue(objectValue(items[len(items)-2])["output"])
			if !strings.Contains(feedback, "No client tool was executed") || !strings.Contains(feedback, "quotes and backslashes") {
				t.Fatal("missing serialization guidance")
			}
			corrected := relayCompatNative(map[string]any{"tool": "functions.exec", "args": input})
			corrected["id"], corrected["call_id"] = "fc_corrected_code", "call_corrected_code"
			repaired := map[string]any{"status": "completed", "output": []any{corrected}}
			merged, err := MergeToolRepairResponse(source, original, repaired)
			if err != nil {
				t.Fatal(err)
			}
			_, translated, _, err := TransformResponseBody(jsonBytes(merged), source)
			if err != nil || objectValue(translated["output"].([]any)[0])["input"] != input {
				t.Fatalf("correction changed exact code bytes: %v", err)
			}
		})
		code = string(jsonBytes(code))
	}
}

func TestToolRepairUnderescapedCodeRejectsAmbiguousBoundaries(t *testing.T) {
	for _, code := range []string{
		`{"tool":"functions.exec","args":"text("x");", tool: "other"}`,
		`{"tool":"functions.exec","args":"text("x");", 'extra': "value"}`,
		`{"tool":"functions.exec","args":"text("x");", \u0074ool: "other"}`,
		`{"tool":"functions.exec","args":"text("x");", π: "other"}`,
		`{"tool":"functions.exec","args":"text("x");", "tool" /* comment */ : "other"}`,
		`{"tool":"functions.exec","args":"text("x");", /* comment */ tool: "other"}`,
		`{"tool":"functions.exec","args":"` + " \n}",
		`{"tool":"functions.exec","args":"text("x");\"}`,
		`{"namespace":"functions","args":"text("x");","tool":"exec"}`,
		`{"tool":"functions.exec","args":"const v = {"tool":"functions.exec"};"}`,
		`{"tool":"functions.exec","args":"text("x");","\u0074ool":"functions.exec"}`,
		`{"tool":"unknown","args":"text("x");"}`,
		`{"tool":"functions.exec","tool":"functions.exec","args":"text("x");"}`,
		`{"tool":"functions.exec","name":"other","args":"text("x");"}`,
		`{"tool":"functions.exec","args":"text("x");","tool":"functions.exec"}`,
		`{"tool":"functions.exec","args":"text("x");","args":"again"}`,
		`{"tool":"functions.exec","args":"text("x");","input":"again"}`,
		`{"tool":"functions.exec","args":"text("x");","extra":1}`,
		`{"tool":"functions.exec","args":"text("x");"} {"tool":"functions.exec","args":"again"}`,
		`[{"tool":"functions.exec","args":"text("x");"}]`,
		`run_officejs({"tool":"functions.exec","args":"text("x");"})`,
		`{"tool":"functions.run_officejs","args":"text("x");"}`,
		`{"tool":"functions.exec","args":{"text":"printf("x");"}}`,
		`{"tool":"functions.exec","args"="text("x");"}`,
		`{"tool":"functions.exec","args":"text("x");`,
	} {
		source := repairTestSource()
		native := relayEnvelopeNative(code)
		response := map[string]any{"status": "completed", "output": []any{native}}
		if call, reason := decodeNativeClientToolCallFromItem(native, source, false); call != nil || reason == "" {
			t.Fatalf("ambiguous code became executable: %s", code)
		}
		if ToolRepairEligible(source, response) {
			t.Fatalf("ambiguous code became repairable: %s", code)
		}
	}
}

func TestToolRepairUnderescapedCSourcePatterns(t *testing.T) {
	for _, input := range []string{
		`printf("%d\n", value);`,
		`printf("%s", "quoted");`,
		`#include "local.h"` + "\nint value;\n",
		`const char *path = "C:\\temp\\demo.c";`,
		"*** Begin Patch\n*** Add File: main.c\n" + `+printf("%d\n", value);` + "\n*** End Patch\n",
	} {
		source := repairTestSource()
		native := relayEnvelopeNative(`{"tool":"functions.exec","args":"` + input + `"}`)
		response := map[string]any{"status": "completed", "output": []any{native}}
		if call, reason := decodeNativeClientToolCallFromItem(native, source, false); call != nil || reason != malformedClientToolMessage {
			t.Fatalf("malformed source became executable: %q", input)
		}
		if !ToolRepairEligible(source, response) {
			t.Fatalf("unique C string quoting error rejected: %q", input)
		}
	}
}

func TestToolRepairUnderescapedCodeKeepsHistoryAndTerminalGuards(t *testing.T) {
	for _, variation := range []string{"first_turn", "complete_history", "pending", "orphan", "previous_response", "previous_alias", "conversation", "compaction", "not_last", "second_tool", "reused_id", "missing_id", "failed", "tool_choice_none", "function_catalog", "ambiguous_catalog", "malformed_outer"} {
		t.Run(variation, func(t *testing.T) {
			source := repairTestSource()
			source["session_id"] = t.Name()
			native := relayEnvelopeNative(`{"tool":"functions.exec","args":"printf("%d\n", value);"}`)
			response := map[string]any{"status": "completed", "output": []any{native}}
			prior := map[string]any{"type": "custom_tool_call", "name": "functions.exec", "call_id": "call_prior_code", "input": "text(1)"}
			output := map[string]any{"type": "custom_tool_call_output", "call_id": "call_prior_code", "output": "done"}
			switch variation {
			case "complete_history":
				source["input"] = []any{prior, output}
			case "pending":
				source["input"] = []any{prior}
			case "orphan":
				source["input"] = []any{output}
			case "previous_response":
				source["previous_response_id"] = "resp_prior"
			case "previous_alias":
				source["previousResponseId"] = "resp_prior"
			case "conversation":
				source["conversation"] = "conv_prior"
			case "compaction":
				source["input"] = []any{map[string]any{"type": "compaction"}}
			case "not_last":
				response["output"] = []any{native, messageItem("assistant", "after")}
			case "second_tool":
				response["output"] = []any{relayCompatNative(map[string]any{"tool": "functions.exec", "args": "text(1)"}), native}
			case "reused_id":
				prior["call_id"], output["call_id"] = native["call_id"], native["call_id"]
				source["input"] = []any{prior, output}
			case "missing_id":
				delete(native, "call_id")
			case "failed":
				response["status"] = "failed"
			case "tool_choice_none":
				source["tool_choice"] = "none"
			case "function_catalog":
				source["tools"] = []any{relayCompatFunction("functions.exec")}
			case "ambiguous_catalog":
				source["tools"] = append(source["tools"].([]any), relayCompatFunction("functions.exec"))
			case "malformed_outer":
				native["arguments"] = `{"code":"broken`
			}
			want := variation == "first_turn" || variation == "complete_history"
			if ToolRepairEligible(source, response) != want {
				t.Fatal("code quoting repair bypassed history, catalog, or terminal guards")
			}
		})
	}
}
func TestToolRepairArgsFirstCodeUsesStrictUniqueTail(t *testing.T) {
	for _, input := range []string{`printf("%d\n", value);`, codeTextTestInput()} {
		for depth, code := 0, `{"args":"`+input+`","tool":"functions.exec"}`; depth < 5; depth++ {
			t.Run(fmt.Sprintf("depth=%d/input=%d", depth, len(input)), func(t *testing.T) {
				source := repairTestSource()
				source["session_id"] = t.Name()
				prepared, err := PrepareResponsesBody(source, DefaultConfig())
				if err != nil {
					t.Fatal(err)
				}
				native := relayEnvelopeNative(code)
				response := map[string]any{"status": "completed", "output": []any{native}}
				if call, reason := decodeNativeClientToolCallFromItem(native, source, true); call != nil || reason != malformedClientToolMessage {
					t.Fatal("damaged args-first source became executable")
				}
				if ToolRepairEligible(source, response) != (depth < 4) {
					t.Fatal("unique args-first source has wrong repair eligibility")
				}
				if rememberedNativeCallInNamespace(nativeCallNamespace(source), stringValue(native["call_id"])) != nil {
					t.Fatal("malformed call entered replay cache")
				}
				if depth >= 4 {
					return
				}
				request, ok := PrepareToolRepairBody(prepared, source, response)
				if !ok {
					t.Fatal("eligible call cannot request correction")
				}
				items := request["input"].([]any)
				if !jsonValuesEqual(items[len(items)-3], native) {
					t.Fatal("original malformed source changed")
				}
				corrected := relayCompatNative(map[string]any{"args": input, "tool": "exec"})
				corrected["id"], corrected["call_id"] = "fc_args_first_corrected", "call_args_first_corrected"
				merged, err := MergeToolRepairResponse(source, response, map[string]any{"status": "completed", "output": []any{corrected}})
				if err != nil {
					t.Fatal(err)
				}
				_, translated, _, err := TransformResponseBody(jsonBytes(merged), source)
				if err != nil || objectValue(translated["output"].([]any)[0])["input"] != input {
					t.Fatal("corrected args-first source changed")
				}
			})
			code = string(jsonBytes(code))
		}
	}
}

func TestToolRepairArgsFirstCodeRejectsAmbiguousMetadata(t *testing.T) {
	for _, code := range []string{
		`{"args":"text("x");","input":"extra","tool":"functions.exec"}`,
		`{"args":"text("x");","tool":"functions.exec","tool":"functions.exec"}`,
		`{"args":"text("x");","\u0074ool":"functions.exec","tool":"functions.exec"}`,
		`{"args":"text("x");", extra:"other","tool":"functions.exec"}`,
		`{"args":"text("x");", 'extra':"other","tool":"functions.exec"}`,
		`{"args":"text("x");", /*c*/ extra:"other","tool":"functions.exec"}`,
		`{"args":"text("x");","tool":"functions.exec","namespace":"functions"}`,
		`{"namespace":"functions","args":"text("x");","tool":"exec"}`,
		`{"args":"text("x");"} {"tool":"functions.exec"}`,
		`{"args":"text("x");\","tool":"functions.exec"}`,
		`{"args":"text("x");","tool":"unknown"}`,
		`{"args":"text("x");","tool":"functions.run_officejs"}`,
	} {
		source := repairTestSource()
		native := relayEnvelopeNative(code)
		if call, reason := decodeNativeClientToolCallFromItem(native, source, false); call != nil || reason == "" {
			t.Fatalf("ambiguous args-first code became executable: %s", code)
		}
		if ToolRepairEligible(source, map[string]any{"status": "completed", "output": []any{native}}) {
			t.Fatalf("ambiguous args-first metadata became eligible: %s", code)
		}
	}
}
