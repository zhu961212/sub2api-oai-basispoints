package protocol

import "strings"

// Relay names must come from the current request, never from a canned shell
// example or a nested API mentioned in a custom execution tool description.
// Codex can expose its shell only through a custom exec tool, so advertising
// a standalone shell call produces an unknown-tool failure at stream end.
const clientRelayContract = " Set code.tool to an exact function/custom tool name in the active catalog, preserving its namespace. Set code.args to the declared argument object for a function tool, or to a raw input string for a custom tool. A tool or helper mentioned only inside another tool description is not a direct relay target: invoke it through the declared execution tool using that tool description. Never use a nested helper name as code.tool unless it is separately declared in the active catalog."

// additionalRequestTools extracts runtime tool declarations carried by the
// Responses input stream. Codex places these in additional_tools items; a
// completed client-side tool_search_output can carry the same declarations.
// Malformed or in-progress discovery records are ignored so an unrelated
// request item cannot grant a tool that was not actually declared.
func additionalRequestTools(source map[string]any) []any {
	items, ok := source["input"].([]any)
	if !ok {
		return nil
	}
	var result []any
	for _, value := range items {
		item := objectValue(value)
		if item == nil {
			continue
		}
		typeName := strings.ToLower(strings.TrimSpace(stringValue(item["type"])))
		if typeName != "additional_tools" && typeName != "tool_search_output" {
			continue
		}
		if typeName == "tool_search_output" {
			if status, exists := item["status"]; exists {
				if strings.ToLower(strings.TrimSpace(stringValue(status))) != "completed" {
					continue
				}
			}
		}
		tools, ok := item["tools"].([]any)
		if !ok || len(tools) == 0 {
			continue
		}
		for _, tool := range tools {
			if objectValue(tool) == nil {
				continue
			}
			result = append(result, cloneJSONValue(tool))
		}
	}
	return result
}

// mergeToolCatalog overlays additions on a top-level Responses tool catalog.
// A function/custom tool is identified by its fully-qualified namespace path
// and leaf name. Namespace nodes with the same name are merged recursively so
// adding a child cannot replace unrelated children. Existing order is kept;
// newly discovered tools are appended in declaration order. Conflicting base
// declarations remain visible until an addition explicitly updates that key.
func mergeToolCatalog(base any, additions []any) []any {
	baseList, _ := base.([]any)
	return mergeToolList(baseList, additions, "")
}

func mergeToolList(base, additions []any, prefix string) []any {
	result := make([]any, 0, len(base)+len(additions))
	positions := make(map[string]int, len(base)+len(additions))
	duplicates := make(map[string][]int)
	all := make([]any, 0, len(base)+len(additions))
	all = append(all, base...)
	all = append(all, additions...)
	for inputIndex, value := range all {
		isAddition := inputIndex >= len(base)
		tool := objectValue(value)
		if tool == nil {
			continue
		}
		copy := objectValue(cloneJSONValue(tool))
		key := catalogToolKey(copy, prefix)
		if key == "" {
			continue
		}
		index, exists := positions[key]
		if isNamespaceTool(copy) && exists && isNamespaceTool(objectValue(result[index])) {
			prior := objectValue(result[index])
			namespacePrefix := prefix + stringValue(copy["name"]) + "."
			var merged map[string]any
			if isAddition {
				merged = mergeNamespaceNode(prior, copy, namespacePrefix)
			} else {
				// Repeated namespace nodes in one base catalog are declarations,
				// not updates. Preserve conflicting children from either node.
				children := append([]any{}, namespaceChildren(prior)...)
				children = append(children, namespaceChildren(copy)...)
				merged = cloneObject(prior)
				merged["tools"] = mergeToolList(children, nil, namespacePrefix)
				delete(merged, "children")
			}
			result[index] = merged
			continue
		}
		if isNamespaceTool(copy) {
			namespacePrefix := prefix + stringValue(copy["name"]) + "."
			if isAddition {
				copy["tools"] = mergeToolList(nil, namespaceChildren(copy), namespacePrefix)
			} else {
				copy["tools"] = mergeToolList(namespaceChildren(copy), nil, namespacePrefix)
			}
			delete(copy, "children")
		}
		if exists {
			if !isAddition {
				duplicates[key] = append(duplicates[key], len(result))
				result = append(result, copy)
				continue
			}
			// One explicit update replaces all declarations for this key; an
			// unrelated addition must not silently resolve a base conflict.
			result[index] = copy
			for _, duplicate := range duplicates[key] {
				result[duplicate] = nil
			}
			delete(duplicates, key)
			continue
		}
		positions[key] = len(result)
		result = append(result, copy)
	}
	compacted := make([]any, 0, len(result))
	for _, value := range result {
		if value != nil {
			compacted = append(compacted, value)
		}
	}
	return compacted
}

func mergeNamespaceNode(base, addition map[string]any, prefix string) map[string]any {
	result := cloneJSONValue(base).(map[string]any)
	baseChildren := namespaceChildren(base)
	additionChildren := namespaceChildren(addition)
	if len(additionChildren) == 0 {
		return result
	}
	merged := mergeToolList(baseChildren, additionChildren, prefix)
	// iterToolValues follows the `tools` field. Keep any other namespace
	// metadata but normalize children to that canonical field.
	result["tools"] = merged
	delete(result, "children")
	return result
}

func namespaceChildren(tool map[string]any) []any {
	if tool == nil {
		return nil
	}
	if children, ok := tool["tools"].([]any); ok {
		return children
	}
	if children, ok := tool["children"].([]any); ok {
		return children
	}
	return nil
}

func isNamespaceTool(tool map[string]any) bool {
	return tool != nil && strings.EqualFold(strings.TrimSpace(stringValue(tool["type"])), "namespace")
}

func catalogToolKey(tool map[string]any, prefix string) string {
	if tool == nil {
		return ""
	}
	name := strings.TrimSpace(stringValue(tool["name"]))
	if name == "" {
		return ""
	}
	if isNamespaceTool(tool) {
		return "namespace:" + prefix + name
	}
	if namespace := strings.TrimSpace(stringValue(tool["namespace"])); namespace != "" {
		return prefix + namespace + "." + name
	}
	return prefix + name
}

// describeToolContract renders the part of a client tool declaration needed by
// the model to form a valid call. Keeping the complete schema compact preserves
// nested properties, enums, and additionalProperties. Custom tools retain the
// declared format (including grammar definitions) verbatim.
func describeToolContract(spec toolSpec) string {
	parts := make([]string, 0, 3)
	if description := stringValue(spec.Spec["description"]); description != "" {
		parts = append(parts, description)
	}
	if spec.Type == "function" {
		if parameters := firstMap(spec.Spec, "parameters", "inputSchema", "input_schema"); parameters != nil {
			parts = append(parts, "parameters schema="+string(jsonBytes(parameters)))
		}
	} else if spec.Type == "custom" {
		if format := spec.Spec["format"]; format != nil {
			parts = append(parts, "input format="+string(jsonBytes(format)))
		}
		if grammar := spec.Spec["grammar"]; grammar != nil {
			parts = append(parts, "grammar="+string(jsonBytes(grammar)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return ": " + strings.Join(parts, "; ")
}
