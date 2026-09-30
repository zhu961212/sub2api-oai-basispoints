package protocol

// RequiresNativeToolChoice preserves explicit function/custom selection that
// the BPS relay cannot enforce. Include runtime declarations in this request,
// but never use BPS caches or invent missing choices. Index declarations
// together so conflicting definitions cannot override one another.
func RequiresNativeToolChoice(source map[string]any) bool {
	tools, _ := source["tools"].([]any)
	declared := append([]any(nil), tools...)
	declared = append(declared, additionalRequestTools(source)...)
	specs := indexClientToolSpecs(declared)
	if stringValue(source["tool_choice"]) == "required" {
		for _, spec := range specs {
			if !spec.Ambiguous {
				return true
			}
		}
		return false
	}
	choice := objectValue(source["tool_choice"])
	kind := stringValue(choice["type"])
	if kind != "function" && kind != "custom" {
		return false
	}
	name := stringValue(choice["name"])
	if namespace := stringValue(choice["namespace"]); namespace != "" {
		name = namespace + "." + name
	}
	spec, exists := specs[name]
	return exists && !spec.Ambiguous && spec.Type == kind
}
