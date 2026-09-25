package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func relayCompatNative(envelope map[string]any) map[string]any {
	return map[string]any{
		"type": "function_call", "name": transportName,
		"id": "fc_compat", "call_id": "call_compat",
		"arguments": string(jsonBytes(map[string]any{"code": string(jsonBytes(envelope))})),
	}
}

func relayCompatFunction(name string) map[string]any {
	return map[string]any{
		"type": "function", "name": name,
		"parameters": map[string]any{
			"type": "object", "required": []any{"cmd"},
			"properties":           map[string]any{"cmd": map[string]any{"type": "string"}},
			"additionalProperties": false,
		},
	}
}

func TestRelayPayloadAliasesPreserveFunctionArgumentsAndReplay(t *testing.T) {
	for _, field := range []string{"args", "arguments", "input"} {
		for _, serialized := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/serialized=%v", field, serialized), func(t *testing.T) {
				want := map[string]any{"cmd": "read exact arguments"}
				var payload any = want
				if serialized {
					payload = string(jsonBytes(want))
				}
				source := map[string]any{"session_id": t.Name(), "tools": []any{relayCompatFunction("exec_command")}}
				native := relayCompatNative(map[string]any{"tool": "exec_command", field: payload})
				call, ok := extractNativeClientToolCallFromItem(native, source, true)
				if !ok || !jsonValuesEqual(parseArguments(call["arguments"]), want) {
					t.Fatalf("valid function payload rejected or altered: %#v", call)
				}
				items := translateInputItemsInNamespace([]any{call}, clientToolSpecs(source), nativeCallNamespace(source))
				if len(items) != 1 || !jsonValuesEqual(items[0], native) {
					t.Fatalf("original relay identity was lost: %#v", items)
				}
			})
		}
	}
}

func TestRelayPayloadAliasesPreserveCustomTextAndReplay(t *testing.T) {
	want := "  patch" + string([]byte{13, 10, 9}) + "{literal}  "
	for _, field := range []string{"args", "arguments", "input"} {
		t.Run(field, func(t *testing.T) {
			source := map[string]any{"session_id": t.Name(), "tools": []any{map[string]any{"type": "custom", "name": "functions.exec"}}}
			native := relayCompatNative(map[string]any{"tool": "exec", field: want})
			call, ok := extractNativeClientToolCallFromItem(native, source, true)
			if !ok || call["type"] != "custom_tool_call" || call["name"] != "functions.exec" || call["input"] != want {
				t.Fatalf("valid custom payload rejected or altered: %#v", call)
			}
			items := translateInputItemsInNamespace([]any{call}, clientToolSpecs(source), nativeCallNamespace(source))
			if len(items) != 1 || !jsonValuesEqual(items[0], native) {
				t.Fatalf("original custom relay identity was lost: %#v", items)
			}
		})
	}
}

func TestRelayRecoversBoundedNestedTransportAliases(t *testing.T) {
	source := map[string]any{"tools": []any{relayCompatFunction("exec_command")}}
	leaf := map[string]any{"tool": "exec_command", "args": map[string]any{"cmd": "pwd"}}
	for _, nameField := range []string{"tool", "name"} {
		for _, payloadField := range []string{"args", "arguments", "input"} {
			t.Run(nameField+"/"+payloadField, func(t *testing.T) {
				envelope := leaf
				for depth := 1; depth <= 3; depth++ {
					envelope = map[string]any{nameField: transportAlias, payloadField: map[string]any{"code": string(jsonBytes(envelope))}}
					call, ok := extractNativeClientToolCallFromItem(relayCompatNative(envelope), source, false)
					if ok != (depth <= 2) {
						t.Fatalf("depth %d: accepted=%v, call=%#v", depth, ok, call)
					}
				}
			})
		}
	}
}

func TestRelayFunctionsAliasRespectsExactCatalogAndNamespaces(t *testing.T) {
	for _, test := range []struct {
		name     string
		tools    []any
		envelope map[string]any
		want     string
	}{
		{"strip functions", []any{relayCompatFunction("exec_command")}, map[string]any{"tool": "functions.exec_command"}, "exec_command"},
		{"add functions", []any{relayCompatFunction("functions.exec_command")}, map[string]any{"tool": "exec_command"}, "functions.exec_command"},
		{"prefer exact qualified", []any{relayCompatFunction("exec_command"), relayCompatFunction("functions.exec_command")}, map[string]any{"tool": "functions.exec_command"}, "functions.exec_command"},
		{"prefer exact bare", []any{relayCompatFunction("exec_command"), relayCompatFunction("functions.exec_command")}, map[string]any{"tool": "exec_command"}, "exec_command"},
		{"nested functions namespace", []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{relayCompatFunction("exec_command")}}}, map[string]any{"tool": "exec_command"}, "functions.exec_command"},
		{"qualified collaboration", []any{relayCompatFunction("collaboration.send_message")}, map[string]any{"tool": "functions.collaboration.send_message"}, "collaboration.send_message"},
		{"namespace plus qualified leaf", []any{relayCompatFunction("functions.collaboration.send_message")}, map[string]any{"namespace": "functions", "tool": "collaboration.send_message"}, "functions.collaboration.send_message"},
		{"do not guess collaboration", []any{relayCompatFunction("collaboration.send_message")}, map[string]any{"tool": "send_message"}, ""},
		{"do not strip arbitrary namespace", []any{relayCompatFunction("exec_command")}, map[string]any{"tool": "unknown.exec_command"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.envelope["args"] = map[string]any{"cmd": "pwd"}
			call, ok := extractNativeClientToolCallFromItem(relayCompatNative(test.envelope), map[string]any{"tools": test.tools}, false)
			if ok != (test.want != "") || ok && clientToolKey(call) != test.want {
				t.Fatalf("accepted=%v, call=%#v, want=%q", ok, call, test.want)
			}
		})
	}
}

func TestRelayRejectsInvalidPayloadsAndConflictingCatalog(t *testing.T) {
	function := relayCompatFunction("exec_command")
	custom := map[string]any{"type": "custom", "name": "exec"}
	for _, test := range []struct {
		name     string
		tools    []any
		envelope map[string]any
	}{
		{"unknown", []any{function}, map[string]any{"tool": "unknown", "args": map[string]any{"cmd": "pwd"}}},
		{"function input still validates", []any{function}, map[string]any{"tool": "exec_command", "input": map[string]any{"wrong": "pwd"}}},
		{"raw function text", []any{function}, map[string]any{"tool": "exec_command", "input": "pwd"}},
		{"custom object", []any{custom}, map[string]any{"tool": "exec", "args": map[string]any{"input": "pwd"}}},
		{"custom null", []any{custom}, map[string]any{"tool": "exec", "input": nil}},
		{"missing payload", []any{custom}, map[string]any{"tool": "exec"}},
		{"multiple payloads", []any{custom}, map[string]any{"tool": "exec", "args": "pwd", "arguments": "pwd"}},
		{"conflicting function and custom", []any{function, map[string]any{"type": "custom", "name": "exec_command"}}, map[string]any{"tool": "exec_command", "args": "pwd"}},
		{"conflicting function schemas", []any{function, map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}}}, map[string]any{"tool": "exec_command", "args": map[string]any{"cmd": "pwd"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if call, ok := extractNativeClientToolCallFromItem(relayCompatNative(test.envelope), map[string]any{"tools": test.tools}, false); ok {
				t.Fatalf("invalid call passed validation: %#v", call)
			}
		})
	}
}

func TestRelayDoesNotReplayAnotherExactFunctionsTool(t *testing.T) {
	source := map[string]any{"tools": []any{relayCompatFunction("exec_command"), relayCompatFunction("functions.exec_command")}}
	native := relayCompatNative(map[string]any{"tool": "exec_command", "args": map[string]any{"cmd": "pwd"}})
	client := map[string]any{"type": "function_call", "name": "functions.exec_command", "call_id": native["call_id"], "arguments": map[string]any{"cmd": "pwd"}}
	if nativeCallMatchesClientItem(native, client, clientToolSpecs(source)) {
		t.Fatal("two separately declared tools were treated as aliases during replay")
	}
}

func TestParseArgumentsPreservesLargeIntegerAndRejectsTrailingData(t *testing.T) {
	want := map[string]any{"value": json.Number("9007199254740993")}
	raw := string(jsonBytes(want))
	if got := parseArguments(raw); !jsonValuesEqual(got, want) {
		t.Fatalf("function argument precision changed: %#v", got)
	}
	if got := parseArguments(raw + raw); got != nil {
		t.Fatalf("multiple argument objects accepted: %#v", got)
	}
}

func TestInvalidRelayDiagnosticsIdentifyCauseWithoutInputContents(t *testing.T) {
	const private = "private-call-content"
	function := relayCompatFunction("exec_command")
	for _, test := range []struct {
		name     string
		tools    []any
		envelope map[string]any
		want     string
	}{
		{"unknown tool", []any{function}, map[string]any{"tool": private, "args": map[string]any{"cmd": private}}, "unknown client tool"},
		{"schema validation", []any{function}, map[string]any{"tool": "exec_command", "args": map[string]any{"unexpected": private}}, "invalid client tool arguments"},
		{"catalog ambiguity", []any{function, map[string]any{"type": "custom", "name": "exec_command"}}, map[string]any{"tool": "exec_command", "args": private}, "ambiguous catalog declaration"},
		{"envelope ambiguity", []any{function}, map[string]any{"tool": "exec_command", "args": private, "input": private}, "malformed or ambiguous client tool relay envelope"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := map[string]any{"status": "completed", "output": []any{relayCompatNative(test.envelope)}}
			_, _, _, err := transformResponseBody(jsonBytes(response), map[string]any{"tools": test.tools})
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), private) {
				t.Fatalf("unsafe or unhelpful diagnostic: %v", err)
			}
		})
	}
}
