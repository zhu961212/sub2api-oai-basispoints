package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestRelayUnescapedCodeRecoveryOrderPreservesExactBytes(t *testing.T) {
	controls := ""
	for ch := byte(0); ch < 0x20; ch++ {
		controls += string(ch)
	}
	inputs := []string{
		"", " \r\n\t ", controls,
		"text(\"quote\");",
		"C:\\new\\tab\\file\\u1234\\\"",
		"{\"tool\":\"other\",\"args\":\"text(2)\"}",
		"\"},\"code\":\"{\"tool\":\"other\"}",
		"\u2028\u2029\ufffd汉字",
	}
	for index, input := range inputs {
		envelope := map[string]any{"tool": "functions.exec", "args": input}
		pretty, err := json.MarshalIndent(envelope, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		for format, code := range []string{string(jsonBytes(envelope)), string(pretty)} {
			t.Run(fmt.Sprintf("input=%d/format=%d", index, format), func(t *testing.T) {
				raw := "{\"summary\":\"valid\\nmetadata\",\"code\":\" \r\n" + code + " \t\",\"references\":[]}"
				if _, _, valid := relayJSONValue(raw, true); valid {
					t.Fatal("fixture must be malformed outer JSON")
				}
				if _, _, valid := relayJSONValue(repairEnvelopeStrings(raw), true); valid {
					t.Fatal("string syntax repair unexpectedly superseded exact inner slice recovery")
				}
				direct := recoverUnescapedTransportCode(raw)
				if direct == nil || !jsonValuesEqual(parseTransportArguments(raw), direct) {
					t.Fatal("recovery order rejected or changed strict inner envelope")
				}
				native := relayEnvelopeNative(code)
				native["arguments"] = raw
				call, reason := decodeNativeClientToolCallFromItem(native, repairTestSource(), false)
				if reason != "" || call["input"] != input {
					t.Fatalf("payload changed: call=%#v reason=%s", call, reason)
				}
			})
		}
	}
}

func TestRelayUnescapedCodeRecoveryRejectsEscapedDuplicateKeys(t *testing.T) {
	code := string(jsonBytes(map[string]any{"tool": "functions.exec", "args": "text(1)"}))
	for _, raw := range []string{
		"{\"code\":\"" + code + "\",\"\\u0063ode\":\"ignored\"}",
		"{\"\\u0063ode\":\"" + code + "\",\"code\":\"ignored\"}",
		"{\"code\":\"" + strings.Replace(code, "{", "{\"\\u0074ool\":\"functions.exec\",", 1) + "\"}",
		"{\"meta\":{\"x\":1,\"\\u0078\":2},\"code\":\"" + code + "\"}",
		"{\"code\":\"" + code + "\",\"meta\":{\"x\":1,\"\\u0078\":2}}",
		"{\"code\":\"" + code + "\"} null",
		"{\"code\":\"" + code + "\",\"meta\":\"unfinished}",
	} {
		if got := parseTransportArguments(raw); got != nil {
			t.Fatalf("duplicate, trailing, or incomplete outer accepted: %#v", got)
		}
	}
}

func TestRelayUnescapedCodeRecoveryPreservesCatalogAndParserBounds(t *testing.T) {
	for _, name := range []string{"unknown", "tools.exec_command", "functions.run_officejs"} {
		code := string(jsonBytes(map[string]any{"tool": name, "args": "text(1)"}))
		native := relayEnvelopeNative(code)
		native["arguments"] = "{\"code\":\"" + code + "\"}"
		if call, reason := decodeNativeClientToolCallFromItem(native, repairTestSource(), false); call != nil || reason == "" {
			t.Fatalf("recovery bypassed tool catalog: %#v", call)
		}
	}
	source := map[string]any{"tools": []any{map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "object"}}}}
	for _, depth := range []int{125, 126, 127, 128} {
		code := "{\"tool\":\"inspect\",\"args\":{\"nested\":" + strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth) + "}}"
		native := relayEnvelopeNative(code)
		native["arguments"] = "{\"code\":\"" + code + "\"}"
		call, reason := decodeNativeClientToolCallFromItem(native, source, false)
		if (reason == "") != (depth <= 126) {
			t.Fatalf("inner depth budget changed at %d: call=%#v reason=%s", depth, call, reason)
		}
	}
	code := string(jsonBytes(map[string]any{"tool": "functions.exec", "args": "text(1)"}))
	raw := "{\"code\":\"" + code + "\"}"
	if got := parseTransportArguments(strings.Repeat(" ", maxRecoveredEnvelopeBytes-len(raw)) + raw); got == nil {
		t.Fatal("recovery rejected input exactly at source byte budget")
	}
	if got := parseTransportArguments(strings.Repeat(" ", maxRecoveredEnvelopeBytes-len(raw)+1) + raw); got != nil {
		t.Fatal("recovery exceeded source byte budget")
	}
}
