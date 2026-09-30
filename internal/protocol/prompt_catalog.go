package protocol

import (
	"sort"
	"strings"
)

// promptToolContext separates the initial client catalog from discoveries made
// later in a full conversation. Moving a later declaration into the prologue
// changes every earlier prompt prefix, even when the cache routing key is stable.
// Authorization still uses the original source's complete frozen catalog; these
// views are only a deterministic representation of declarations in the prompt.
func promptToolContext(source map[string]any) (map[string]any, any) {
	base, explicit := source["tools"]
	items, listInput := source["input"].([]any)
	if !explicit || !listInput || strings.EqualFold(strings.TrimSpace(stringValue(source["tool_choice"])), "none") {
		return source, source["input"]
	}

	var result []any
	state := base
	for index, value := range items {
		declarations := requestItemToolDeclarations(value)
		if len(declarations) == 0 {
			if result != nil {
				result = append(result, value)
			}
			continue
		}
		if result == nil {
			result = make([]any, 0, len(items))
			result = append(result, items[:index]...)
		}
		before := indexClientToolSpecs(state)
		state = mergeToolCatalog(state, declarations)
		if update := promptCatalogUpdate(before, indexClientToolSpecs(state)); update != "" {
			result = append(result, messageItem("developer", update))
		}
		if note := RequestCapabilityInstructions(map[string]any{"tools": declarations}); note != "" {
			result = append(result, messageItem("developer", note))
		}
	}
	if result == nil {
		return source, source["input"]
	}
	// Do not read or update the session catalog here: it may include discoveries
	// from later requests. The explicit base and each input position fully define
	// this request's historical prompt without creating another permission cache.
	catalogSource := map[string]any{
		"tools":                   base,
		"__bps_effective_tools":   base,
		"__bps_client_tool_specs": indexClientToolSpecs(base),
	}
	return catalogSource, result
}

func promptCatalogUpdate(before, after map[string]toolSpec) string {
	changed := make([]string, 0, len(after))
	withdrawn := make([]string, 0)
	for name, spec := range after {
		if spec.Ambiguous {
			continue
		}
		prior, existed := before[name]
		if !existed || prior.Ambiguous || prior.Type != spec.Type || !jsonValuesEqual(prior.Spec, spec.Spec) {
			changed = append(changed, name)
		}
	}
	for name, prior := range before {
		if next, remains := after[name]; !prior.Ambiguous && (!remains || next.Ambiguous) {
			withdrawn = append(withdrawn, name)
		}
	}
	if len(changed) == 0 && len(withdrawn) == 0 {
		return ""
	}
	sort.Strings(changed)
	sort.Strings(withdrawn)
	lines := []string{"Client tool catalog update at this point in the conversation. These declarations replace earlier declarations for the same exact tool names; other declarations remain unchanged. These newly declared tools are available even if the initial catalog was empty. Use the outer run_officejs relay for calls. The final request reminder determines which tools are currently callable."}
	if len(changed) > 0 && len(sortedCallableToolNames(before)) == 0 {
		// An empty/ambiguous initial catalog has no relay protocol prologue.
		// Introduce the full transport contract with the first callable discovery.
		lines = append(lines, clientToolProtocolInstructions(map[string]any{"__bps_client_tool_specs": after}))
		return strings.Join(lines, "\n")
	}
	for _, name := range changed {
		spec := after[name]
		line := "- " + spec.Key + " (" + spec.Type + ")"
		if spec.Type == "function" {
			if parameters := firstMap(spec.Spec, "parameters", "inputSchema", "input_schema"); parameters != nil {
				line += ". Its arguments are an object with " + describeParameterNames(parameters)
			}
			line += describeToolContract(spec) + "."
		} else {
			line += ". It receives raw text in code.args; the proxy emits it as custom_tool_call.input." + describeToolContract(spec)
		}
		lines = append(lines, line)
	}
	if len(withdrawn) > 0 {
		lines = append(lines, "These tools are no longer callable: "+string(jsonBytes(withdrawn))+".")
	}
	return strings.Join(lines, "\n")
}
