package protocol

import "strings"

// RequiresNativeWebSearch reports requests that need the host's native search
// service. Basis Points has no hosted web-search executor; converting such a
// request either rejects its options or silently removes the declared search
// capability. Route it before BPS validation, attachment uploads or rewriting
// so tools, tool_choice, search options and citations retain their semantics.
// A client function/custom tool merely named web_search remains a relay tool.
func RequiresNativeWebSearch(source map[string]any) bool {
	// Hosted search calls are native conversation records even if this turn
	// disables new tool calls. Preserve their history on the native route.
	items, _ := source["input"].([]any)
	for _, raw := range items {
		item := objectValue(raw)
		if strings.EqualFold(stringValue(item["type"]), "web_search_call") {
			return true
		}
	}
	if strings.EqualFold(stringValue(source["tool_choice"]), "none") {
		return false
	}
	choice := objectValue(source["tool_choice"])
	if isHostedSearch(strings.ToLower(stringValue(choice["type"]))) {
		return true
	}
	if hasHostedWebSearch(source["tools"]) {
		return true
	}
	for _, raw := range items {
		item := objectValue(raw)
		switch strings.ToLower(stringValue(item["type"])) {
		case "tool_search_output":
			if status, exists := item["status"]; exists && !strings.EqualFold(stringValue(status), "completed") {
				continue
			}
		case "additional_tools":
		default:
			continue
		}
		if hasHostedWebSearch(item["tools"]) {
			return true
		}
	}
	return false
}

func hasHostedWebSearch(value any) bool {
	tools, _ := value.([]any)
	for _, raw := range tools {
		tool := objectValue(raw)
		kind := strings.ToLower(stringValue(tool["type"]))
		if isHostedSearch(kind) || (kind == "namespace" && hasHostedWebSearch(namespaceChildren(tool))) {
			return true
		}
	}
	return false
}
