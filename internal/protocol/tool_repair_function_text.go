package protocol

import (
	"encoding/json"
	"strings"
)

// Classify only a final string field in a known function's argument object.
// Identity metadata must be strict and precede the argument object; all prior
// argument fields must also be strict. No damaged text is decoded or executed.
func functionTextRelayTarget(raw string, source map[string]any) (toolRepairTarget, bool) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return toolRepairTarget{}, false
	}
	header := map[string]any{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return toolRepairTarget{}, false
		}
		if _, exists := header[key]; exists {
			return toolRepairTarget{}, false
		}
		switch key {
		case "tool", "name", "namespace":
			value, err := decoder.Token()
			text, ok := value.(string)
			if err != nil || !ok {
				return toolRepairTarget{}, false
			}
			header[key] = text
		case "args", "arguments", "input":
			header[key] = nil
			envelope := unambiguousEnvelope(header)
			if envelope == nil || isTransportName(recoveryEnvelopeName(envelope)) {
				return toolRepairTarget{}, false
			}
			spec, known := resolveClientTool(clientToolSpecs(source), recoveryEnvelopeName(envelope))
			if !known || spec.Type != "function" {
				return toolRepairTarget{}, false
			}
			schema := firstMap(spec.Spec, "parameters", "inputSchema", "input_schema")
			if stringValue(schema["type"]) != "object" {
				return toolRepairTarget{}, false
			}
			offset := recoverySkipSpace(raw, int(decoder.InputOffset()))
			if offset >= len(raw) || raw[offset] != ':' {
				return toolRepairTarget{}, false
			}
			offset = recoverySkipSpace(raw, offset+1)
			knownArguments, field, eligible := functionTextArgumentsCandidate(raw[offset:], schema)
			return toolRepairTarget{toolSpec: spec, field: field, knownArguments: knownArguments}, eligible
		default:
			return toolRepairTarget{}, false
		}
	}
	return toolRepairTarget{}, false
}

func functionTextArgumentsCandidate(raw string, schema map[string]any) (map[string]any, string, bool) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, "", false
	}
	arguments := map[string]any{}
	properties := objectValue(schema["properties"])
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, "", false
		}
		if _, exists := arguments[key]; exists {
			return nil, "", false
		}
		offset := recoverySkipSpace(raw, int(decoder.InputOffset()))
		if offset >= len(raw) || raw[offset] != ':' {
			return nil, "", false
		}
		offset = recoverySkipSpace(raw, offset+1)
		if stringValue(objectValue(properties[key])["type"]) == "string" && finalFunctionTextCandidate(raw, offset) {
			// The damaged value is unknown. Check every unaffected field and
			// required/additional-property rule using a string placeholder only
			// in a private schema copy. The replacement uses the full schema.
			arguments[key] = ""
			probeSchema, probeProperties := cloneObject(schema), cloneObject(properties)
			probeProperties[key] = map[string]any{"type": "string"}
			probeSchema["properties"] = probeProperties
			if !schemaMatches(arguments, probeSchema) {
				return nil, "", false
			}
			delete(arguments, key)
			return arguments, key, true
		}
		value, err := strictRelayJSONToken(decoder, 1)
		if err != nil {
			return nil, "", false
		}
		arguments[key] = value
	}
	return nil, "", false
}

func finalFunctionTextCandidate(raw string, start int) bool {
	if start >= len(raw) || raw[start] != '"' {
		return false
	}
	space := string([]byte{32, 13, 10, 9})
	tail := strings.TrimRight(raw, space)
	// Exactly the argument object and relay object may follow the string.
	for range 2 {
		if len(tail) <= start+1 || tail[len(tail)-1] != '}' {
			return false
		}
		tail = strings.TrimRight(tail[:len(tail)-1], space)
	}
	end := len(tail) - 1
	if end <= start || tail[end] != '"' {
		return false
	}
	slashes := 0
	for index := end - 1; index > start && raw[index] == byte(92); index-- {
		slashes++
	}
	return slashes%2 == 0 && customTextHasOnlyInteriorQuoteErrors(raw[start+1:end])
}
