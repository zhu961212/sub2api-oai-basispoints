package protocol

// FreezeRequestTools captures the request-local client catalog before any
// upstream work. Response-only translation needs the same snapshot as request
// rewriting, even when the original request body is forwarded unchanged.
func FreezeRequestTools(source map[string]any) {
	freezeToolCatalog(source)
}

func freezeToolCatalog(source map[string]any) {
	if _, frozen := source["__bps_effective_tools"]; !frozen {
		source["__bps_effective_tools"] = cloneJSONValue(sourceTools(source))
	}
	// Build eagerly before source is shared with response processing. Keeping
	// tool_choice outside the index preserves explicit none overrides.
	source["__bps_client_tool_specs"] = indexClientToolSpecs(source["__bps_effective_tools"])
}
