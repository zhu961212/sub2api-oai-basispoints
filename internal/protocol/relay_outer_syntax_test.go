package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestRelayOuterArgumentsRecoverStringSyntaxWithoutChangingInput(t *testing.T) {
	slash := string(byte(92))
	input := `  text("quoted");` + string([]byte{13, 10, 9}) + "// C:" + slash + "project; pattern " + slash + "d+; literal " + slash + "n  "
	code := string(jsonBytes(map[string]any{"tool": "functions.exec", "args": input}))
	encoded := string(jsonBytes(code))
	formattedCode := "{" + string([]byte{13, 10}) + strings.TrimPrefix(code, "{")
	for _, test := range []struct{ name, arguments string }{
		{"literal newline in summary", `{"summary":"first` + string(byte(10)) + `second","code":` + encoded + `}`},
		{"literal tab in summary", `{"summary":"first` + string(byte(9)) + `second","code":` + encoded + `}`},
		{"invalid escape in summary", `{"summary":"inspect C:` + slash + `project","code":` + encoded + `}`},
		{"literal code formatting", strings.ReplaceAll(string(jsonBytes(map[string]any{"code": formattedCode})), slash+"r"+slash+"n", string([]byte{13, 10}))},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := repairTestSource()
			source["session_id"] = t.Name()
			native := relayEnvelopeNative(code)
			native["arguments"] = test.arguments
			call, reason := decodeNativeClientToolCallFromItem(native, source, true)
			if reason != "" || call["input"] != input {
				t.Fatalf("recoverable outer syntax rejected or input changed: call=%#v reason=%s", call, reason)
			}
			replayed := translateInputItemsInNamespace([]any{call}, clientToolSpecs(source), nativeCallNamespace(source))
			if len(replayed) != 1 || !jsonValuesEqual(replayed[0], native) {
				t.Fatalf("native identity or arguments changed during replay: %#v", replayed)
			}
		})
	}
}

func TestRelayOuterStringRecoveryPreservesObjectInputAndUnwrapLimit(t *testing.T) {
	slash := string(byte(92))
	want := map[string]any{"value": json.Number("9007199254740993"), "text": "first" + string(byte(9)) + "second", "pattern": slash + "d+"}
	envelope := map[string]any{"tool": "inspect", "args": want}
	source := map[string]any{"tools": []any{map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "object"}}}}
	for _, objectCode := range []bool{false, true} {
		var code any = string(jsonBytes(envelope))
		if objectCode {
			code = envelope
		}
		raw := string(jsonBytes(map[string]any{"code": code, "summary": "PLACEHOLDER"}))
		raw = strings.ReplaceAll(raw, "PLACEHOLDER", "C:"+slash+"project"+string(byte(10)))
		if objectCode {
			raw = strings.ReplaceAll(raw, slash+"t", string(byte(9)))
			raw = strings.ReplaceAll(raw, slash+slash+"d", slash+"d")
		}
		for depth := 0; depth < 5; depth++ {
			t.Run(fmt.Sprintf("object=%t/depth=%d", objectCode, depth), func(t *testing.T) {
				native := relayEnvelopeNative(code)
				native["arguments"] = raw
				call, reason := decodeNativeClientToolCallFromItem(native, source, false)
				if depth < 4 {
					if reason != "" || !jsonValuesEqual(parseArguments(call["arguments"]), want) {
						t.Fatalf("outer recovery changed exact input: call=%#v reason=%s", call, reason)
					}
				} else if reason == "" {
					t.Fatal("outer recovery exceeded the unwrap budget")
				}
			})
			raw = string(jsonBytes(raw))
		}
	}
}

func TestRelayOuterStringRecoveryStillRejectsAmbiguityAndTruncation(t *testing.T) {
	code := string(jsonBytes(`{"tool":"functions.exec","args":"text(1)"}`))
	prefix := `{"summary":"inspect C:` + string(byte(92)) + `project","code":` + code
	for _, raw := range []string{
		prefix + `,"code":` + code + `}`,
		prefix,
		prefix + `} {"code":` + code + `}`,
		`[{"code":` + code + `}]`,
		`run_officejs({"code":` + code + `})`,
		`{"code":` + code + `,"summary":"unfinished`,
		`{"summary":"literal` + string(byte(10)) + `newline","code":{"tool":"functions.exec","args":"one","args":"two"}}`,
		`{"summary":"literal` + string(byte(10)) + `newline","code":{"tool":"inspect","args":{"n":1,"n":2}}}`,
	} {
		if got := parseTransportArguments(raw); got != nil {
			t.Fatalf("unsafe outer arguments were accepted: %q", raw)
		}
	}
}

func TestSyntheticStreamPreservesCustomInputWhitespaceAndEmptyString(t *testing.T) {
	for _, input := range []string{"", "  " + string([]byte{13, 10, 9}), "  text(1);" + string([]byte{13, 10, 9})} {
		t.Run(fmt.Sprintf("input=%q", input), func(t *testing.T) {
			response := map[string]any{"status": "completed", "output": []any{map[string]any{"type": "custom_tool_call", "id": "ctc_exact", "call_id": "call_exact", "name": "functions.exec", "input": input}}}
			count := 0
			for _, line := range strings.Split(string(syntheticStream(response)), string(byte(10))) {
				if !strings.HasPrefix(line, "data: {") {
					continue
				}
				var event map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				if event["type"] == "response.custom_tool_call_input.done" {
					count++
					if event["input"] != input {
						t.Fatalf("input completion changed raw custom text: %#v", event["input"])
					}
				}
			}
			if count != 1 {
				t.Fatalf("expected one input completion, got %d", count)
			}
		})
	}
}
