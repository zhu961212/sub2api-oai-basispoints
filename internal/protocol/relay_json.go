package protocol

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

var errRelayJSONStructure = errors.New("ambiguous or invalid relay JSON structure")

// Decode tokens explicitly: encoding/json otherwise silently accepts duplicate
// object keys. Return a partial tree only for classifying an unexecuted call.
func strictRelayJSONValue(raw string, complete bool) (any, int, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	value, err := strictRelayJSONToken(decoder, 0)
	end := int(decoder.InputOffset())
	if err != nil {
		return value, end, err
	}
	if complete {
		if _, err := decoder.Token(); err != io.EOF {
			return nil, end, errRelayJSONStructure
		}
	}
	return value, end, nil
}

func strictRelayJSONToken(decoder *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, errRelayJSONStructure
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return object, err
			}
			key, ok := token.(string)
			if !ok {
				return object, errRelayJSONStructure
			}
			if _, exists := object[key]; exists {
				return object, errRelayJSONStructure
			}
			// A known payload key may have a truncated value; partial input
			// is retained for classification only, never client execution.
			object[key] = nil
			value, err := strictRelayJSONToken(decoder, depth+1)
			object[key] = value
			if err != nil {
				return object, err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return object, err
		}
		if closing != json.Delim('}') {
			return object, errRelayJSONStructure
		}
		return object, nil
	case '[':
		array := []any{}
		for decoder.More() {
			value, err := strictRelayJSONToken(decoder, depth+1)
			if err != nil {
				return array, err
			}
			array = append(array, value)
		}
		closing, err := decoder.Token()
		if err != nil {
			return array, err
		}
		if closing != json.Delim(']') {
			return array, errRelayJSONStructure
		}
		return array, nil
	default:
		return nil, errRelayJSONStructure
	}
}

func relayJSONValue(raw string, complete bool) (any, int, bool) {
	value, end, err := strictRelayJSONValue(raw, complete)
	return value, end, err == nil
}

// Unwrap only complete JSON string layers; never recover prose or guess code.
// The outer arguments can contain the same literal control characters and
// invalid string escapes as code. Repair those without consuming an unwrap
// layer, then still require one complete object with no duplicate keys.
func parseTransportArguments(value any) map[string]any {
	for depth := 0; depth <= 4; depth++ {
		if object := objectValue(value); object != nil {
			if _, exists := object["code"]; exists {
				return object
			}
			return nil
		}
		raw, ok := value.(string)
		if !ok || len(raw) > maxRecoveredEnvelopeBytes || depth == 4 {
			return nil
		}
		decoded, _, valid := relayJSONValue(raw, true)
		if !valid {
			fixed := repairEnvelopeStrings(raw)
			if fixed != raw {
				decoded, _, valid = relayJSONValue(fixed, true)
			}
		}
		if !valid {
			decoded = recoverUnescapedTransportCode(raw)
			if decoded == nil {
				return nil
			}
		}
		value = decoded
	}
	return nil
}

// Recover a missing serialization layer around code only when its contents
// already form exactly one strict JSON envelope. Parse the surrounding object
// again after quoting that exact slice; duplicates, trailing values, damaged
// metadata, and ambiguous inner envelopes remain invalid. Never infer source
// quotes or execute a wrapper, and keep the original native call for replay.
func recoverUnescapedTransportCode(raw string) map[string]any {
	if len(raw) > maxRecoveredEnvelopeBytes {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return nil
		}
		seen[key] = true
		if key != "code" {
			if _, err := strictRelayJSONToken(decoder, 1); err != nil {
				return nil
			}
			continue
		}
		start := recoverySkipSpace(raw, int(decoder.InputOffset()))
		if start == len(raw) || raw[start] != ':' {
			return nil
		}
		start = recoverySkipSpace(raw, start+1)
		if start == len(raw) || raw[start] != byte(34) {
			return nil
		}
		inner := recoverySkipSpace(raw, start+1)
		if inner == len(raw) || raw[inner] != '{' {
			return nil
		}
		value, consumed, valid := relayJSONValue(raw[inner:], false)
		if !valid || unambiguousEnvelope(objectValue(value)) == nil {
			return nil
		}
		end := recoverySkipSpace(raw, inner+consumed)
		if end == len(raw) || raw[end] != byte(34) {
			return nil
		}
		fixed := raw[:start] + string(jsonBytes(raw[start+1:end])) + raw[end+1:]
		decoded, _, valid := relayJSONValue(fixed, true)
		if !valid {
			return nil
		}
		return objectValue(decoded)
	}
	return nil
}
