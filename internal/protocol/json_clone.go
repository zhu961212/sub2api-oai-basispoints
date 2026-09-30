package protocol

import (
	"encoding/json"
	"unicode/utf8"
)

// cloneCanonicalJSONValue copies request-local JSON containers without encoding
// their contents first. Strings are immutable and can share storage for the
// request lifetime; retained cache entries still use protocolCacheSnapshot.
// Noncanonical values fall back to encoding/json so custom marshalers, typed
// containers, invalid UTF-8 and cycles retain the existing normalization.
func cloneCanonicalJSONValue(value any, depth int) (any, bool) {
	if depth > 64 {
		return nil, false
	}
	switch typed := value.(type) {
	case map[string]any:
		if typed == nil {
			return nil, true
		}
		copy := make(map[string]any, len(typed))
		for key, value := range typed {
			if !utf8.ValidString(key) {
				return nil, false
			}
			cloned, ok := cloneCanonicalJSONValue(value, depth+1)
			if !ok {
				return nil, false
			}
			copy[key] = cloned
		}
		return copy, true
	case []any:
		if typed == nil {
			return nil, true
		}
		copy := make([]any, len(typed))
		for index, value := range typed {
			cloned, ok := cloneCanonicalJSONValue(value, depth+1)
			if !ok {
				return nil, false
			}
			copy[index] = cloned
		}
		return copy, true
	case string:
		return value, utf8.ValidString(typed)
	case json.Number:
		if typed == "" {
			return json.Number("0"), true
		}
		first, last := typed[0], typed[len(typed)-1]
		if (first != '-' && (first < '0' || first > '9')) || last < '0' || last > '9' || !json.Valid([]byte(typed)) {
			return nil, false
		}
		return value, true
	case nil, bool:
		return value, true
	default:
		return nil, false
	}
}
