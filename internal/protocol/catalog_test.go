package protocol

import (
	"strings"
	"testing"
)

func TestAdditionalRequestToolsCollectsDeclaredAndCompletedDiscoveredTools(t *testing.T) {
	source := map[string]any{
		"input": []any{
			map[string]any{"type": "additional_tools", "tools": []any{
				map[string]any{"type": "custom", "name": "exec", "format": map[string]any{"type": "grammar", "syntax": "lark"}},
			}},
			map[string]any{"type": "tool_search_output", "status": "in_progress", "tools": []any{
				map[string]any{"type": "function", "name": "not_ready"},
			}},
			map[string]any{"type": "tool_search_output", "status": "completed", "tools": []any{
				map[string]any{"type": "namespace", "name": "files", "tools": []any{
					map[string]any{"type": "function", "name": "read"},
				}},
			}},
		},
	}
	got := additionalRequestTools(source)
	if len(got) != 2 {
		t.Fatalf("additional tools = %#v, want two declarations", got)
	}
	if stringValue(objectValue(got[0])["name"]) != "exec" || stringValue(objectValue(got[1])["name"]) != "files" {
		t.Fatalf("unexpected declarations: %#v", got)
	}
	// Returned declarations are copies; mutating one must not modify request input.
	objectValue(got[0])["name"] = "changed"
	if stringValue(objectValue(objectValue(source["input"].([]any)[0])["tools"].([]any)[0])["name"]) != "exec" {
		t.Fatal("additional tools share mutable request objects")
	}
}

func TestMergeToolCatalogOverlaysNestedNamespacesWithoutClobberingSiblings(t *testing.T) {
	base := []any{
		map[string]any{"type": "function", "name": "top", "description": "old"},
		map[string]any{"type": "namespace", "name": "files", "tools": []any{
			map[string]any{"type": "function", "name": "read", "description": "old read"},
			map[string]any{"type": "function", "name": "write", "description": "keep"},
		}},
	}
	additions := []any{
		map[string]any{"type": "function", "name": "top", "description": "new"},
		map[string]any{"type": "namespace", "name": "files", "tools": []any{
			map[string]any{"type": "function", "name": "read", "description": "new read"},
			map[string]any{"type": "function", "name": "list", "description": "new list"},
		}},
		map[string]any{"type": "namespace", "name": "other", "tools": []any{
			map[string]any{"type": "function", "name": "read", "description": "other read"},
		}},
	}
	got := mergeToolCatalog(base, additions)
	if len(got) != 3 {
		t.Fatalf("merged catalog length = %d, want 3: %#v", len(got), got)
	}
	if stringValue(objectValue(got[0])["description"]) != "new" {
		t.Fatalf("top-level replacement missing: %#v", got[0])
	}
	files := objectValue(got[1])
	children := files["tools"].([]any)
	if len(children) != 3 {
		t.Fatalf("files children = %#v, want read/write/list", children)
	}
	if stringValue(objectValue(children[0])["description"]) != "new read" || stringValue(objectValue(children[1])["description"]) != "keep" || stringValue(objectValue(children[2])["name"]) != "list" {
		t.Fatalf("nested merge order/replacement wrong: %#v", children)
	}
	other := objectValue(got[2])
	if stringValue(objectValue(other["tools"].([]any)[0])["description"]) != "other read" {
		t.Fatal("same leaf name in another namespace was overwritten")
	}
}

func TestMergeToolCatalogKeepsDeepNamespacePathsDistinct(t *testing.T) {
	base := []any{map[string]any{"type": "namespace", "name": "outer", "tools": []any{
		map[string]any{"type": "namespace", "name": "inner", "tools": []any{
			map[string]any{"type": "function", "name": "read", "description": "keep"},
		}},
	}}}
	additions := []any{map[string]any{"type": "namespace", "name": "outer", "tools": []any{
		map[string]any{"type": "namespace", "name": "inner", "tools": []any{
			map[string]any{"type": "function", "name": "write", "description": "append"},
		}},
	}}}
	got := mergeToolCatalog(base, additions)
	outer := objectValue(got[0])
	inner := objectValue(outer["tools"].([]any)[0])
	children := inner["tools"].([]any)
	if len(children) != 2 || stringValue(objectValue(children[0])["description"]) != "keep" || stringValue(objectValue(children[1])["name"]) != "write" {
		t.Fatalf("deep namespace merge lost children: %#v", got)
	}
}

func TestMergeToolCatalogKeepsExplicitNamespacesDistinct(t *testing.T) {
	base := []any{map[string]any{"type": "function", "namespace": "files", "name": "read", "description": "keep"}}
	additions := []any{map[string]any{"type": "function", "namespace": "network", "name": "read", "description": "append"}}
	got := mergeToolCatalog(base, additions)
	specs := clientToolSpecs(map[string]any{"tools": got})
	if len(got) != 2 || specs["files.read"].Spec["description"] != "keep" || specs["network.read"].Spec["description"] != "append" {
		t.Fatalf("explicit namespace tools clobbered each other: %#v", got)
	}
}

func TestUnrelatedDiscoveryPreservesExistingCatalogAmbiguity(t *testing.T) {
	for _, conflict := range []string{"type", "schema"} {
		for _, layout := range []string{"top-level", "nested", "split-namespaces"} {
			for _, record := range []string{"additional_tools", "tool_search_output"} {
				t.Run(conflict+"/"+layout+"/"+record, func(t *testing.T) {
					first := relayCompatFunction("exec_command")
					second := map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}}
					var payload any = map[string]any{}
					if conflict == "type" {
						second = map[string]any{"type": "custom", "name": "exec_command"}
						payload = "pwd"
					}
					base := []any{first, second}
					key := "exec_command"
					if layout == "nested" {
						base = []any{map[string]any{"type": "namespace", "name": "functions", "tools": base}}
						key = "functions.exec_command"
					} else if layout == "split-namespaces" {
						base = []any{
							map[string]any{"type": "namespace", "name": "functions", "tools": []any{first}},
							map[string]any{"type": "namespace", "name": "functions", "tools": []any{second}},
						}
						key = "functions.exec_command"
					}
					unrelated := []any{relayCompatFunction("unrelated")}
					if layout != "top-level" {
						unrelated = []any{map[string]any{"type": "namespace", "name": "functions", "tools": unrelated}}
					}
					source := map[string]any{"tools": base, "input": []any{map[string]any{
						"type": record, "status": "completed", "tools": unrelated,
					}}}
					specs := clientToolSpecs(source)
					if !specs[key].Ambiguous {
						t.Fatalf("unrelated discovery erased existing %s conflict: %#v", conflict, specs[key])
					}
					_, reason := decodeNativeClientToolCallFromItem(relayCompatNative(map[string]any{"tool": key, "args": payload}), source, false)
					if !strings.Contains(reason, "ambiguous catalog declaration") {
						t.Fatalf("ambiguous call was not rejected: %q", reason)
					}
				})
			}
		}
	}
}

func TestExplicitDiscoveryStillReplacesExistingConflictingDeclarations(t *testing.T) {
	for _, nested := range []bool{false, true} {
		base := []any{relayCompatFunction("exec_command"), map[string]any{"type": "custom", "name": "exec_command"}}
		addition := []any{relayCompatFunction("exec_command")}
		key := "exec_command"
		if nested {
			base = []any{map[string]any{"type": "namespace", "name": "functions", "tools": base}}
			addition = []any{map[string]any{"type": "namespace", "name": "functions", "tools": addition}}
			key = "functions.exec_command"
		}
		source := map[string]any{"tools": mergeToolCatalog(base, addition)}
		spec := clientToolSpecs(source)[key]
		if spec.Ambiguous || spec.Type != "function" {
			t.Fatalf("explicit update did not replace conflicting declarations: %#v", spec)
		}
		call, ok := extractNativeClientToolCallFromItem(relayCompatNative(map[string]any{"tool": key, "args": map[string]any{"cmd": "pwd"}}), source, false)
		if !ok || clientToolKey(call) != key {
			t.Fatalf("explicitly updated valid tool was rejected: %#v", call)
		}
	}
}

func TestMergeToolCatalogDeduplicatesEmptyBaseAndNormalizesChildren(t *testing.T) {
	got := mergeToolCatalog(nil, []any{
		map[string]any{"type": "function", "name": "read", "description": "old"},
		map[string]any{"type": "function", "name": "read", "description": "new"},
		map[string]any{"type": "namespace", "name": "files", "children": []any{
			map[string]any{"type": "function", "name": "read"},
		}},
	})
	if len(got) != 2 || stringValue(objectValue(got[0])["description"]) != "new" {
		t.Fatalf("empty-base merge did not deduplicate in place: %#v", got)
	}
	namespace := objectValue(got[1])
	if _, exists := namespace["children"]; exists || len(namespaceChildren(namespace)) != 1 {
		t.Fatalf("namespace children were not normalized: %#v", namespace)
	}
}

func TestDescribeToolContractPreservesNestedSchemaAndCustomGrammar(t *testing.T) {
	function := describeToolContract(toolSpec{Type: "function", Spec: map[string]any{
		"description": "Inspect a path",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
				"mode": map[string]any{"enum": []any{"fast", "deep"}},
			},
			"required":             []any{"path"},
			"additionalProperties": false,
		},
	}})
	if !strings.Contains(function, "Inspect a path") || !strings.Contains(function, `"properties"`) || !strings.Contains(function, `"enum":["fast","deep"]`) || !strings.Contains(function, `"additionalProperties":false`) {
		t.Fatalf("function contract lost schema: %s", function)
	}
	custom := describeToolContract(toolSpec{Type: "custom", Spec: map[string]any{
		"description": "Run a command",
		"format":      map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: /[a-z]+/"},
	}})
	if !strings.Contains(custom, "Run a command") || !strings.Contains(custom, "grammar") || !strings.Contains(custom, "start: /[a-z]+/") {
		t.Fatalf("custom contract lost format/grammar: %s", custom)
	}
}
