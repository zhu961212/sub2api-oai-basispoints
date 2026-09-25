package protocol

import (
	"encoding/json"
	"errors"
	"math/big"
	"strings"
)

const unknownClientToolMessage = "Basis Points returned an unknown client tool absent from the active catalog"

func IsUnknownClientToolError(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Kind == "invalid_tool_call" && api.Message == unknownClientToolMessage
}

// ToolRepairEligible permits regeneration of only one unexecuted relay on
// the first client turn. Description-only helpers never become client tools.
func ToolRepairEligible(source, response map[string]any) bool {
	if source == nil || response == nil || source["previous_response_id"] != nil || source["conversation"] != nil || len(clientToolSpecs(source)) == 0 {
		return false
	}
	items, _ := source["input"].([]any)
	for _, value := range items {
		item := objectValue(value)
		kind := stringValue(item["type"])
		if clientCallableItem(item) || strings.HasSuffix(kind, "_call_output") || kind == "compaction" {
			return false
		}
	}
	if status := stringValue(response["status"]); status != "" && status != "completed" {
		return false
	}
	output, _ := response["output"].([]any)
	if len(output) == 0 {
		return false
	}
	for index, value := range output {
		item := objectValue(value)
		if !clientCallableItem(item) {
			continue
		}
		// Earlier output positions may already have been streamed. Replace
		// only the last, still-withheld tool position.
		if index != len(output)-1 || transportEnvelope(item) == nil || stringValue(item["call_id"]) == "" {
			return false
		}
		_, reason := decodeNativeClientToolCallFromItem(item, source, false)
		return reason == unknownClientToolMessage
	}
	return false
}

// Preserve the prepared model, metadata, history, and image URLs. The invalid
// native call receives an explicit rejection output and is never executed.
func PrepareToolRepairBody(prepared, source, response map[string]any) (map[string]any, bool) {
	if !ToolRepairEligible(source, response) {
		return nil, false
	}
	body := objectValue(cloneJSONValue(prepared))
	items, ok := body["input"].([]any)
	if !ok {
		return nil, false
	}
	output := response["output"].([]any)
	call := objectValue(output[len(output)-1])
	feedback := "The proxy rejected the preceding relay because its inner tool name is absent from the active client catalog. No client tool was executed. A helper documented inside an executor is callable only from that executor's raw input. Regenerate exactly one tool call using an explicitly declared client tool. Do not repeat commentary or change the task, model, account, scope, or input."
	added := append([]any{}, cloneJSONValue(output).([]any)...)
	added = append(added, map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": feedback}, messageItem("developer", feedback+" "+clientToolProtocolReminder(source)))
	body["input"] = appendBeforeCompaction(items, added)
	return body, true
}

// Validate the regenerated call without publishing replay state. Preserve
// visible response identity and progress; append repair output in the former
// withheld tool slot. Normal transformation validates and caches everything.
func MergeToolRepairResponse(source, original, repaired map[string]any) (map[string]any, error) {
	if !ToolRepairEligible(source, original) || stringValue(repaired["status"]) != "completed" {
		return nil, fail(502, "invalid_tool_call", unknownClientToolMessage)
	}
	output, _ := repaired["output"].([]any)
	tools := 0
	for _, value := range output {
		item := objectValue(value)
		if !clientCallableItem(item) {
			continue
		}
		tools++
		if _, reason := decodeNativeClientToolCallFromItem(item, source, false); reason != "" {
			return nil, fail(502, "invalid_tool_call", reason)
		}
	}
	if tools != 1 {
		return nil, fail(502, "invalid_tool_call", "Basis Points tool correction did not return exactly one valid client tool")
	}
	merged := objectValue(cloneJSONValue(original))
	prefix := merged["output"].([]any)
	merged["output"] = append(prefix[:len(prefix)-1], cloneJSONValue(output).([]any)...)
	ids, calls := map[string]bool{}, map[string]bool{}
	for _, value := range merged["output"].([]any) {
		item := objectValue(value)
		id, callID := stringValue(item["id"]), stringValue(item["call_id"])
		if id != "" && ids[id] || callID != "" && calls[callID] {
			return nil, fail(502, "invalid_tool_call", "Basis Points tool correction returned conflicting output identities")
		}
		ids[id], calls[callID] = id != "", callID != ""
	}
	if original["usage"] != nil || repaired["usage"] != nil {
		merged["usage"] = sumRepairUsage(objectValue(original["usage"]), objectValue(repaired["usage"]))
	}
	return merged, nil
}

// Counts can exceed float64 precision; sum exact nonnegative integers and
// nested usage details without coercing opaque metadata.
func sumRepairUsage(first, second map[string]any) map[string]any {
	result := objectValue(cloneJSONValue(first))
	if result == nil {
		result = map[string]any{}
	}
	for key, value := range second {
		if nested := objectValue(value); nested != nil {
			result[key] = sumRepairUsage(objectValue(result[key]), nested)
			continue
		}
		left, leftOK := new(big.Int).SetString(string(jsonBytes(result[key])), 10)
		right, rightOK := new(big.Int).SetString(string(jsonBytes(value)), 10)
		if leftOK && rightOK && left.Sign() >= 0 && right.Sign() >= 0 {
			result[key] = json.Number(new(big.Int).Add(left, right).String())
		} else {
			result[key] = cloneJSONValue(value)
		}
	}
	return result
}
