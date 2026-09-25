package protocol

import (
	"strings"
	"testing"
)

func TestRelayGuidanceUsesActiveCatalogWithoutInventingShellTools(t *testing.T) {
	const nestedDescription = "Run JavaScript with tools.exec_command({cmd: 'pwd'}); nested tools are available only inside this executor."
	for _, test := range []struct {
		name   string
		tools  []any
		want   string
		custom bool
	}{
		{"flat custom", []any{map[string]any{"type": "custom", "name": "functions.exec", "description": nestedDescription}}, "functions.exec", true},
		{"namespaced custom", []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{map[string]any{"type": "custom", "name": "exec", "description": nestedDescription}}}}, "functions.exec", true},
		{"unrelated function", []any{relayCompatFunction("files.read")}, "files.read", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := map[string]any{"tools": test.tools, "input": []any{messageItem("user", "Inspect the project")}}
			body, err := PrepareResponsesBody(source, DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			items := body["input"].([]any)
			prologue := relayGuidanceText(items[0])
			reminder := relayGuidanceText(items[len(items)-1])
			for _, text := range []string{prologue, reminder} {
				for _, invalid := range []string{"\"tool\":\"exec_command\"", "\"tool\":\"functions.exec_command\"", "TOOL_NAME", "RAW_INPUT"} {
					if strings.Contains(text, invalid) {
						t.Fatalf("relay guidance advertises an undeclared or placeholder target %q: %s", invalid, text)
					}
				}
				if !strings.Contains(text, "Set code.tool to an exact function/custom tool name") || !strings.Contains(text, "mentioned only inside another tool description is not a direct relay target") {
					t.Fatalf("relay guidance does not distinguish callable tools from nested helpers: %s", text)
				}
			}
			if !strings.Contains(reminder, "Client tools (exact code.tool allowlist): "+string(jsonBytes([]string{test.want}))) {
				t.Fatalf("reminder does not preserve the exact active catalog key: %s", reminder)
			}
			if test.custom {
				if !strings.Contains(prologue, nestedDescription) {
					t.Fatal("nested executor documentation was removed")
				}
				if !strings.Contains(prologue, "- "+test.want+" (custom). It receives raw text in code.args; the proxy emits it as custom_tool_call.input.") {
					t.Fatalf("custom catalog entry confuses relay input with the emitted call: %s", prologue)
				}
				if !strings.Contains(reminder, "Custom tool "+test.want+" takes raw text in code.args") || strings.Contains(reminder, "uses input, not arguments") {
					t.Fatalf("custom payload guidance confuses the relay envelope with client output: %s", reminder)
				}
			}
		})
	}
}

func TestRelayGuidanceFollowsCachedReplacedAndRevokedCatalogs(t *testing.T) {
	session := t.Name()
	requests := []map[string]any{
		{"session_id": session, "tools": []any{map[string]any{"type": "custom", "name": "functions.exec"}}},
		{"session_id": session},
		{"session_id": session, "tools": []any{relayCompatFunction("files.read")}},
		{"session_id": session, "tools": []any{}},
		{"session_id": session},
		{"session_id": session, "tools": []any{relayCompatFunction("files.read")}, "tool_choice": "none"},
	}
	wants := []string{"functions.exec", "functions.exec", "files.read", "", "", ""}
	for index, source := range requests {
		if _, err := PrepareResponsesBody(source, DefaultConfig()); err != nil {
			t.Fatal(err)
		}
		reminder := clientToolProtocolReminder(source)
		if wants[index] == "" {
			if reminder != "" {
				t.Fatalf("request %d advertised unavailable tools: %s", index, reminder)
			}
			continue
		}
		if !strings.Contains(reminder, "Client tools (exact code.tool allowlist): "+string(jsonBytes([]string{wants[index]}))) {
			t.Fatalf("request %d used stale catalog guidance: %s", index, reminder)
		}
		if wants[index] != "functions.exec" && strings.Contains(reminder, "functions.exec") {
			t.Fatalf("request %d retained withdrawn executor guidance: %s", index, reminder)
		}
	}
}

func relayGuidanceText(raw any) string {
	item := objectValue(raw)
	parts, _ := item["content"].([]any)
	var text strings.Builder
	for _, part := range parts {
		text.WriteString(stringValue(objectValue(part)["text"]))
	}
	return text.String()
}
