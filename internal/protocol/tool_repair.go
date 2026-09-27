package protocol

import (
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"strings"
)

const unknownClientToolMessage = "Basis Points returned an unknown client tool absent from the active catalog"
const malformedClientToolMessage = "Basis Points returned a malformed or ambiguous client tool relay envelope"

func IsUnknownClientToolError(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Kind == "invalid_tool_call" && api.Message == unknownClientToolMessage
}

// The message identifies a candidate only; ToolRepairEligible must separately
// establish that the unexecuted relay can be regenerated without ambiguity.
func IsRepairableClientToolError(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Kind == "invalid_tool_call" && (api.Message == unknownClientToolMessage || api.Message == malformedClientToolMessage)
}

func completeToolHistory(items []any) bool {
	pending, seen := map[string]string{}, map[string]bool{}
	for _, value := range items {
		item := objectValue(value)
		kind, callID := stringValue(item["type"]), stringValue(item["call_id"])
		if kind == "compaction" {
			return false
		}
		if clientCallableItem(item) {
			if (kind != "function_call" && kind != "custom_tool_call") || callID == "" || seen[callID] {
				return false
			}
			pending[callID], seen[callID] = kind+"_output", true
		} else if strings.HasSuffix(kind, "_call_output") {
			// Client transports may replay a custom call result as a function
			// output. Request preparation already normalizes this explicit pair.
			customOutputAlias := pending[callID] == "custom_tool_call_output" && kind == "function_call_output"
			if callID == "" || pending[callID] != kind && !customOutputAlias {
				return false
			}
			delete(pending, callID)
		} else if strings.HasSuffix(kind, "_call") {
			return false
		}
	}
	return len(pending) == 0
}

func truncatedRelayCanRegenerate(native, source map[string]any) bool {
	eligible, _ := malformedRelayRepairTarget(native, source)
	return eligible
}

type toolRepairTarget struct {
	toolSpec
	field          string
	knownArguments map[string]any
}

// A nonempty target pins the known tool for complete code-string
// syntax repairs. Older unknown-name and truncated repairs keep their rules.
func malformedRelayRepairTarget(native, source map[string]any) (bool, toolRepairTarget) {
	if stringValue(native["type"]) != "function_call" || !isTransportName(stringValue(native["name"])) {
		return false, toolRepairTarget{}
	}
	arguments := parseTransportArguments(native["arguments"])
	raw, ok := arguments["code"].(string)
	if !ok || len(raw) > maxRecoveredEnvelopeBytes {
		return false, toolRepairTarget{}
	}
	raw = strings.TrimSpace(raw)
	// Match code recovery's four JSON layers, but unwrap only complete
	// strings. The inner partial tree is classification data, never a call.
	for depth := 0; depth < 3 && len(raw) > 0 && raw[0] == '"'; depth++ {
		value, _, valid := relayJSONValue(raw, true)
		quoted, ok := value.(string)
		if !valid || !ok {
			return false, toolRepairTarget{}
		}
		raw = strings.TrimSpace(quoted)
	}
	if !strings.HasPrefix(raw, "{") {
		return false, toolRepairTarget{}
	}
	value, _, err := strictRelayJSONValue(raw, true)
	var syntax *json.SyntaxError
	truncated := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &syntax) && syntax.Error() == "unexpected end of JSON input"
	if !truncated {
		// Complete but underescaped custom code is also eligible for the
		// existing one-shot correction, never for local quote guessing.
		if !errors.As(err, &syntax) {
			return false, toolRepairTarget{}
		}
		if target, eligible := customTextRelayTarget(raw, source); eligible {
			return true, toolRepairTarget{toolSpec: target}
		}
		target, eligible := functionTextRelayTarget(raw, source)
		return eligible, target
	}
	envelope := unambiguousEnvelope(objectValue(value))
	if envelope == nil {
		return false, toolRepairTarget{}
	}
	name := recoveryEnvelopeName(envelope)
	if isTransportName(name) {
		return false, toolRepairTarget{}
	}
	_, exists := resolveClientTool(clientToolSpecs(source), name)
	return exists, toolRepairTarget{}
}

// Regenerate only the final unexecuted relay. Explicit prior tool history must
// be complete; opaque server-side history and unfinished calls stay ineligible.
func ToolRepairEligible(source, response map[string]any) bool {
	if ClassifyResponseTerminal("", response).Failed() {
		return false
	}
	if source == nil || response == nil || source["previous_response_id"] != nil || source["previousResponseId"] != nil || source["conversation"] != nil || len(clientToolSpecs(source)) == 0 {
		return false
	}
	items, _ := source["input"].([]any)
	if !completeToolHistory(items) {
		return false
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
		if index != len(output)-1 || stringValue(item["call_id"]) == "" {
			return false
		}
		for _, prior := range items {
			if stringValue(objectValue(prior)["call_id"]) == stringValue(item["call_id"]) {
				return false
			}
		}
		_, reason := decodeNativeClientToolCallFromItem(item, source, false)
		return reason == unknownClientToolMessage && transportEnvelope(item) != nil || reason == malformedClientToolMessage && truncatedRelayCanRegenerate(item, source)
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
	if transportEnvelope(call) == nil {
		feedback = "The proxy rejected the preceding relay because its code contains incomplete JSON or invalid JSON string quoting. No client tool was executed. Regenerate exactly one complete relay for the declared client tool. Serialize the entire inner object as the code string, preserving quotes and backslashes. Use a JSON serializer for source code and patches; do not manually interpolate them into the envelope. For a function with a damaged string argument, correct only that string and preserve every other argument field and value exactly; do not add, remove, rename, or change unaffected arguments. Do not guess missing arguments, repeat commentary, or change the task, model, account, scope, or input."
		if _, target := malformedRelayRepairTarget(call, source); target.knownArguments != nil {
			feedback += " Repair only the damaged string argument. Preserve every other argument field and its exact value; do not add, remove, or rename fields."
		}
	}
	added := append([]any{}, cloneJSONValue(output).([]any)...)
	added = append(added, map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": feedback}, messageItem("developer", feedback+" "+clientToolProtocolReminder(source)))
	body["input"] = appendBeforeCompaction(items, added)
	return body, true
}

// Validate the regenerated call without publishing replay state. Preserve
// visible response identity and progress; append repair output in the former
// withheld tool slot. Normal transformation validates and caches everything.
func MergeToolRepairResponse(source, original, repaired map[string]any) (map[string]any, error) {
	if terminal := ClassifyResponseTerminal("", repaired); terminal.Failed() {
		return nil, ResponseTerminalError(terminal, repaired)
	}
	if !ToolRepairEligible(source, original) || stringValue(repaired["status"]) != "completed" {
		return nil, fail(502, "invalid_tool_call", unknownClientToolMessage)
	}
	output, _ := repaired["output"].([]any)
	originalOutput := original["output"].([]any)
	_, expected := malformedRelayRepairTarget(objectValue(originalOutput[len(originalOutput)-1]), source)
	tools := 0
	for _, value := range output {
		item := objectValue(value)
		if !clientCallableItem(item) {
			continue
		}
		tools++
		call, reason := decodeNativeClientToolCallFromItem(item, source, false)
		if reason != "" {
			return nil, fail(502, "invalid_tool_call", reason)
		}
		expectedType := "function_call"
		if expected.Type == "custom" {
			expectedType = "custom_tool_call"
		}
		if expected.Key != "" && (clientToolKey(call) != expected.Key || stringValue(call["type"]) != expectedType) {
			return nil, fail(502, "invalid_tool_call", "Basis Points code string correction changed the declared client tool")
		}
		if expected.knownArguments != nil {
			arguments := parseArguments(call["arguments"])
			if len(arguments) != len(expected.knownArguments)+1 {
				return nil, fail(502, "invalid_tool_call", "Basis Points code string correction changed unaffected arguments")
			}
			if _, ok := arguments[expected.field].(string); !ok {
				return nil, fail(502, "invalid_tool_call", "Basis Points code string correction changed the damaged argument field")
			}
			for key, value := range expected.knownArguments {
				actual, exists := arguments[key]
				if !exists || !jsonValuesEqual(actual, value) {
					return nil, fail(502, "invalid_tool_call", "Basis Points code string correction changed unaffected arguments")
				}
			}
		}
	}
	if tools != 1 {
		return nil, fail(502, "invalid_tool_call", "Basis Points tool correction did not return exactly one valid client tool")
	}
	merged := objectValue(cloneJSONValue(original))
	prefix := merged["output"].([]any)
	merged["output"] = append(prefix[:len(prefix)-1], cloneJSONValue(output).([]any)...)
	ids, calls := map[string]bool{}, map[string]bool{}
	if history, ok := source["input"].([]any); ok {
		for _, value := range history {
			if id := stringValue(objectValue(value)["call_id"]); id != "" {
				calls[id] = true
			}
		}
	}
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
