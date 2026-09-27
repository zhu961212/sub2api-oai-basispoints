package protocol

// HasOriginalImageDetail checks typed image content only. Strings, tool
// arguments, and arbitrary nested objects are never interpreted as images.
func HasOriginalImageDetail(source map[string]any) bool {
	items, _ := source["input"].([]any)
	for _, raw := range items {
		item := objectValue(raw)
		field := imageDetailContentField(item)
		if field == "" {
			continue
		}
		parts, _ := item[field].([]any)
		for _, rawPart := range parts {
			part := objectValue(rawPart)
			if part["type"] == "input_image" && part["detail"] == "original" {
				return true
			}
		}
	}
	return false
}

// NormalizeImageDetails maps the client-only original hint to high, the highest
// BPS-supported detail. Image bytes, URLs, IDs, and other fields are unchanged.
// Edited containers are copied so shared history remains intact for tool replay.
// Callers must still validate and prepare inline user images before forwarding.
func NormalizeImageDetails(source map[string]any) bool {
	items, _ := source["input"].([]any)
	var updated []any
	for index, raw := range items {
		item := objectValue(raw)
		field := imageDetailContentField(item)
		if field == "" {
			continue
		}
		parts, _ := item[field].([]any)
		var updatedParts []any
		for partIndex, rawPart := range parts {
			part := objectValue(rawPart)
			if part["type"] != "input_image" || part["detail"] != "original" {
				continue
			}
			if updatedParts == nil {
				updatedParts = append([]any(nil), parts...)
			}
			image := copyImageDetailObject(part)
			image["detail"] = "high"
			updatedParts[partIndex] = image
		}
		if updatedParts == nil {
			continue
		}
		if updated == nil {
			updated = append([]any(nil), items...)
		}
		message := copyImageDetailObject(item)
		message[field] = updatedParts
		updated[index] = message
	}
	if updated == nil {
		return false
	}
	source["input"] = updated
	return true
}

func imageDetailContentField(item map[string]any) string {
	switch item["type"] {
	case nil, "", "message":
		return "content"
	case "function_call_output", "custom_tool_call_output":
		return "output"
	default:
		return ""
	}
}

func copyImageDetailObject(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
