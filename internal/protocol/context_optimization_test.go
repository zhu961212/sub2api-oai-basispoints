package protocol

import (
	"strings"
	"testing"
)

func TestContextCatalogPrefixStableAcrossOrderAndDuplicates(t *testing.T) {
	read := relayCompatFunction("files.read")
	exec := map[string]any{"type": "custom", "name": "functions.exec", "description": "Run the complete program", "format": map[string]any{"type": "text"}}
	var want string
	for index, tools := range [][]any{{read, exec}, {exec, read}, {read, exec, cloneJSONValue(read)}} {
		source := map[string]any{"tools": tools, "input": "Keep the same history"}
		body, err := PrepareResponsesBody(source, DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		got := string(jsonBytes(body["input"]))
		if index == 0 {
			want = got
		} else if got != want {
			t.Fatalf("equivalent catalog %d changed the prepared context", index)
		}
	}
}

func TestContextCatalogNeverAdvertisesConflictingTools(t *testing.T) {
	source := map[string]any{"tools": []any{relayCompatFunction("conflict"), map[string]any{"type": "custom", "name": "conflict"}, relayCompatFunction("safe")}}
	if _, err := PrepareResponsesBody(source, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{clientToolProtocolInstructions(source), clientToolProtocolReminder(source)} {
		if strings.Contains(prompt, "conflict") || !strings.Contains(prompt, "safe") {
			t.Fatalf("prompt does not match the callable catalog: %s", prompt)
		}
	}
	if _, ok := resolveClientTool(clientToolSpecs(source), "conflict"); ok {
		t.Fatal("conflicting declarations became callable")
	}
}

func TestContextPreparedCatalogRemainsFrozenAndHonorsToolChoice(t *testing.T) {
	source := sessionTestSource(t.Name())
	source["tool_choice"] = "none"
	if _, err := PrepareResponsesBody(source, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	if len(clientToolSpecs(source)) != 0 {
		t.Fatal("tool_choice none exposed a tool")
	}
	source["tool_choice"] = "auto"
	objectValue(source["tools"].([]any)[0])["name"] = "changed_after_prepare"
	if _, ok := clientToolSpecs(source)["read"]; !ok {
		t.Fatal("prepared catalog changed or tool_choice none permanently revoked it")
	}
	if _, ok := clientToolSpecs(source)["changed_after_prepare"]; ok {
		t.Fatal("source mutation changed the prepared catalog")
	}
	source["tool_choice"] = "none"
	if clientToolProtocolReminder(source) != "" {
		t.Fatal("prepared catalog bypassed tool_choice none")
	}
}

func TestContextDirectCallWithoutIDReplaysNativeEnvelope(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		t.Run(kind, func(t *testing.T) {
			spec := relayCompatFunction("direct")
			native := map[string]any{"type": "function_call", "name": "direct", "arguments": string(jsonBytes(map[string]any{"cmd": "pwd"}))}
			if kind == "custom" {
				spec = map[string]any{"type": "custom", "name": "direct"}
				native = map[string]any{"type": "custom_tool_call", "name": "direct", "input": "  raw input  "}
			}
			source := map[string]any{"session_id": t.Name(), "tools": []any{spec}}
			if _, err := PrepareResponsesBody(source, DefaultConfig()); err != nil {
				t.Fatal(err)
			}
			call, reason := decodeNativeClientToolCallFromItem(native, source, true)
			if reason != "" || stringValue(call["call_id"]) == "" {
				t.Fatalf("direct call failed to acquire an ID: %s", reason)
			}
			output := map[string]any{"type": stringValue(call["type"]) + "_output", "call_id": call["call_id"], "output": "done"}
			items := translateInputItemsInNamespace([]any{output}, clientToolSpecs(source), nativeCallNamespace(source))
			if len(items) != 2 {
				t.Fatalf("missing historical call/output pair: %#v", items)
			}
			replay := objectValue(items[0])
			if replay["name"] != transportName || replay["call_id"] != call["call_id"] || replay["type"] != "function_call" {
				t.Fatalf("history did not restore the native relay: %#v", replay)
			}
			if !nativeCallMatchesClientItem(replay, call, clientToolSpecs(source)) || objectValue(items[1])["type"] != "function_call_output" {
				t.Fatal("historical call arguments or result type changed")
			}
			if _, exists := native["call_id"]; exists {
				t.Fatal("upstream response was mutated")
			}
		})
	}
}

func BenchmarkContextPreparedToolLookup(b *testing.B) {
	source := map[string]any{"tools": cacheBenchmarkCatalog(), "input": "Inspect the project"}
	if _, err := PrepareResponsesBody(source, DefaultConfig()); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if len(clientToolSpecs(source)) != 64 {
			b.Fatal("catalog lost tools")
		}
	}
}

func BenchmarkContextPrepareResponses(b *testing.B) {
	tools := cacheBenchmarkCatalog()
	b.ReportAllocs()
	for range b.N {
		source := map[string]any{"tools": tools, "input": "Inspect the project"}
		if _, err := PrepareResponsesBody(source, DefaultConfig()); err != nil {
			b.Fatal(err)
		}
	}
}
