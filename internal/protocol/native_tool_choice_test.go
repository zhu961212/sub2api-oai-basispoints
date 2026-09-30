package protocol

import (
	"fmt"
	"testing"
)

func TestRequiresNativeToolChoiceMatchesDeclaredTools(t *testing.T) {
	function := map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "object"}}
	custom := map[string]any{"type": "custom", "name": "source"}
	for _, tc := range []struct {
		name   string
		choice any
		tools  []any
		want   bool
	}{
		{"required", "required", []any{function}, true},
		{"function", map[string]any{"type": "function", "name": "inspect"}, []any{function}, true},
		{"custom", map[string]any{"type": "custom", "name": "source"}, []any{custom}, true},
		{"auto", "auto", []any{function}, false},
		{"none", "none", []any{function}, false},
		{"missing tools", "required", nil, false},
		{"undeclared", map[string]any{"type": "function", "name": "absent"}, []any{function}, false},
		{"wrong type", map[string]any{"type": "custom", "name": "inspect"}, []any{function}, false},
		{"ambiguous", "required", []any{function, map[string]any{"type": "custom", "name": "inspect"}}, false},
		{"invalid scalar", true, []any{function}, false},
		{"alias not declared", map[string]any{"type": "function", "name": "functions.inspect"}, []any{function}, false},
		{"namespace", map[string]any{"type": "function", "namespace": "files", "name": "inspect"}, []any{map[string]any{"type": "namespace", "name": "files", "tools": []any{function}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := map[string]any{"tool_choice": tc.choice, "tools": tc.tools}
			before := string(jsonBytes(source))
			if got := RequiresNativeToolChoice(source); got != tc.want {
				t.Fatalf("native=%t want=%t", got, tc.want)
			}
			if string(jsonBytes(source)) != before {
				t.Fatal("routing mutated request")
			}
		})
	}
}

func TestRequiresNativeToolChoiceIncludesCurrentRuntimeDeclarations(t *testing.T) {
	for _, record := range []string{"additional_tools", "tool_search_output"} {
		for _, kind := range []string{"function", "custom"} {
			for _, choiceKind := range []string{"required", "specific", "namespaced"} {
				t.Run(fmt.Sprintf("%s/%s/%s", record, kind, choiceKind), func(t *testing.T) {
					tool := map[string]any{"type": kind, "name": "inspect"}
					if kind == "function" {
						tool["parameters"] = map[string]any{"type": "object"}
					}
					var choice any = "required"
					if choiceKind != "required" {
						choice = map[string]any{"type": kind, "name": "inspect"}
					}
					if choiceKind == "namespaced" {
						choice.(map[string]any)["namespace"] = "files"
						tool = map[string]any{"type": "namespace", "name": "files", "tools": []any{tool}}
					}
					source := map[string]any{"tool_choice": choice, "input": []any{map[string]any{"type": record, "status": "completed", "tools": []any{tool}}}}
					before := string(jsonBytes(source))
					if !RequiresNativeToolChoice(source) {
						t.Fatal("current runtime tool declaration lost native tool selection")
					}
					if string(jsonBytes(source)) != before {
						t.Fatal("routing mutated request")
					}
				})
			}
		}
	}
}

func TestRequiresNativeToolChoiceRuntimeDeclarationBoundaries(t *testing.T) {
	function := map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "object"}}
	custom := map[string]any{"type": "custom", "name": "inspect"}
	declaration := func(kind, status string, tools any) []any {
		return []any{map[string]any{"type": kind, "status": status, "tools": tools}}
	}
	for _, tc := range []struct {
		name   string
		source map[string]any
		want   bool
	}{
		{"in progress", map[string]any{"tool_choice": "required", "input": declaration("tool_search_output", "in_progress", []any{function})}, false},
		{"failed", map[string]any{"tool_choice": "required", "input": declaration("tool_search_output", "failed", []any{function})}, false},
		{"invalid tools", map[string]any{"tool_choice": "required", "input": declaration("additional_tools", "completed", function)}, false},
		{"unrelated record", map[string]any{"tool_choice": "required", "input": declaration("message", "completed", []any{function})}, false},
		{"wrong type", map[string]any{"tool_choice": map[string]any{"type": "custom", "name": "inspect"}, "input": declaration("additional_tools", "completed", []any{function})}, false},
		{"undeclared namespace", map[string]any{"tool_choice": map[string]any{"type": "function", "name": "inspect", "namespace": "files"}, "input": declaration("additional_tools", "completed", []any{function})}, false},
		{"conflicting top level", map[string]any{"tool_choice": "required", "tools": []any{custom}, "input": declaration("additional_tools", "completed", []any{function})}, false},
		{"conflicting runtime", map[string]any{"tool_choice": "required", "input": declaration("additional_tools", "completed", []any{function, custom})}, false},
		{"conflicting schema", map[string]any{"tool_choice": map[string]any{"type": "function", "name": "inspect"}, "tools": []any{function}, "input": declaration("tool_search_output", "completed", []any{map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "string"}}})}, false},
		{"identical repeated declaration", map[string]any{"tool_choice": "required", "tools": []any{function}, "input": declaration("additional_tools", "completed", []any{function})}, true},
		{"ignore internal snapshot", map[string]any{"tool_choice": "required", "__bps_effective_tools": []any{function}}, false},
		{"none", map[string]any{"tool_choice": "none", "input": declaration("additional_tools", "completed", []any{function})}, false},
		{"auto", map[string]any{"tool_choice": "auto", "input": declaration("additional_tools", "completed", []any{function})}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := string(jsonBytes(tc.source))
			if got := RequiresNativeToolChoice(tc.source); got != tc.want {
				t.Fatalf("native=%t want=%t", got, tc.want)
			}
			if string(jsonBytes(tc.source)) != before {
				t.Fatal("routing mutated request")
			}
		})
	}
}
