package protocol

import (
	"encoding/json"
	"io"
	"strings"
)

const maxRecoveredEnvelopeBytes = 1 << 20

// recoverTransportEnvelope only decodes data. It never evaluates a JavaScript
// wrapper. Every caller must still resolve the name against its client catalog
// and validate the arguments before returning a usable client tool call.
func recoverTransportEnvelope(raw string) map[string]any {
	if len(raw) > maxRecoveredEnvelopeBytes {
		return nil
	}
	for depth := 0; depth < 4; depth++ {
		raw = strings.TrimSpace(raw)
		value, _, ok := relayJSONValue(raw, true)
		if ok {
			switch value := value.(type) {
			case map[string]any:
				return unambiguousEnvelope(value)
			case string:
				raw = value
				continue
			default:
				return nil
			}
		}
		// Repair only characters that JSON forbids inside strings. Valid JSON
		// escapes, quotes, separators, and structural delimiters are never guessed.
		fixed := repairEnvelopeStrings(raw)
		if fixed != raw {
			raw = fixed
			continue
		}
		return recoverEmbeddedEnvelope(raw)
	}
	return nil
}

func recoveryJSONValue(raw string, complete bool) (any, int, bool) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, 0, false
	}
	end := int(decoder.InputOffset())
	if complete {
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			return nil, 0, false
		}
	}
	return value, end, true
}

func unambiguousEnvelope(item map[string]any) map[string]any {
	for _, field := range []string{"name", "tool", "namespace"} {
		if value, exists := item[field]; exists {
			if _, ok := value.(string); !ok {
				return nil
			}
		}
	}
	name, _ := item["name"].(string)
	alias, _ := item["tool"].(string)
	if (name == "" && alias == "") || (name != "" && alias != "" && name != alias) {
		return nil
	}
	if _, ok := envelopePayload(item); !ok {
		return nil
	}
	return item
}

// The relay has one payload, even when a host uses a different field name.
// Never choose between duplicate fields: even equal values are ambiguous.
func envelopePayload(item map[string]any) (any, bool) {
	var result any
	present := false
	for _, field := range []string{"args", "arguments", "input"} {
		if value, exists := item[field]; exists {
			if present {
				return nil, false
			}
			result, present = value, true
		}
	}
	return result, present
}

func recoverEmbeddedEnvelope(raw string) map[string]any {
	var found map[string]any
	for index := 0; index < len(raw); {
		if strings.HasPrefix(raw[index:], "```") {
			// Fence markers are formatting, not quoted source. Scan all fenced
			// bodies so two independent examples cannot become one tool call.
			index += 3
			continue
		}
		switch raw[index] {
		case '"', '\'', '`':
			end, ok := recoveryQuotedEnd(raw, index)
			if !ok {
				return nil
			}
			index = end
			continue
		case '[':
			// Arrays are never one transport envelope; do not pluck a nested
			// object out of a batch or an otherwise malformed outer value.
			return nil
		case '{':
			value, consumed, ok := relayJSONValue(raw[index:], false)
			if !ok || found != nil {
				return nil
			}
			item, _ := value.(map[string]any)
			found = unambiguousEnvelope(item)
			if found == nil {
				return nil
			}
			index += consumed
			continue
		}
		if recoveryNameStart(raw[index]) {
			start := index
			for index < len(raw) && recoveryNameByte(raw[index]) {
				index++
			}
			name := raw[start:index]
			opening := recoverySkipSpace(raw, index)
			if opening == len(raw) || raw[opening] != '(' {
				continue
			}
			value, consumed, ok := relayJSONValue(raw[opening+1:], false)
			if !ok || found != nil {
				return nil
			}
			closing := recoverySkipSpace(raw, opening+1+consumed)
			if closing == len(raw) || raw[closing] != ')' {
				return nil
			}
			switch value := value.(type) {
			case map[string]any:
				envelope := unambiguousEnvelope(value)
				if envelope == nil && recoveryLooksLikeEnvelope(value) {
					return nil
				}
				if envelope != nil && recoveryEnvelopeName(envelope) == name {
					found = envelope
				} else {
					found = map[string]any{"name": name, "arguments": value}
				}
			case string:
				found = map[string]any{"name": name, "args": value}
			default:
				return nil
			}
			index = closing + 1
			continue
		}
		index++
	}
	return found
}

func recoveryLooksLikeEnvelope(item map[string]any) bool {
	_, name := item["name"]
	_, tool := item["tool"]
	_, args := item["args"]
	_, arguments := item["arguments"]
	_, input := item["input"]
	return (name || tool) && (args || arguments || input)
}

func recoveryEnvelopeName(item map[string]any) string {
	name, _ := item["name"].(string)
	if name == "" {
		name, _ = item["tool"].(string)
	}
	if namespace, _ := item["namespace"].(string); namespace != "" && !strings.HasPrefix(name, namespace+".") {
		name = namespace + "." + name
	}
	return name
}

func recoveryQuotedEnd(raw string, start int) (int, bool) {
	quote := raw[start]
	for index := start + 1; index < len(raw); index++ {
		if raw[index] == '\\' {
			index++
			continue
		}
		if raw[index] == quote {
			return index + 1, true
		}
	}
	return 0, false
}

func recoveryNameStart(ch byte) bool {
	return ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z'
}

func recoveryNameByte(ch byte) bool {
	return recoveryNameStart(ch) || ch >= '0' && ch <= '9' || ch == '.' || ch == '-'
}

func recoverySkipSpace(raw string, index int) int {
	for index < len(raw) && strings.ContainsRune(" \r\n\t", rune(raw[index])) {
		index++
	}
	return index
}

func repairEnvelopeStrings(raw string) string {
	var out strings.Builder
	out.Grow(len(raw))
	quoted := false
	for index := 0; index < len(raw); index++ {
		ch := raw[index]
		if ch == '"' {
			quoted = !quoted
		}
		if quoted && (ch == '\n' || ch == '\r' || ch == '\t') {
			switch ch {
			case '\n':
				out.WriteString(`\n`)
			case '\r':
				out.WriteString(`\r`)
			case '\t':
				out.WriteString(`\t`)
			}
			continue
		}
		if ch != '\\' || !quoted || index+1 >= len(raw) {
			out.WriteByte(ch)
			continue
		}
		next := raw[index+1]
		valid := strings.ContainsRune(`"\/bfnrt`, rune(next))
		if next == 'u' && index+5 < len(raw) {
			valid = true
			for _, digit := range raw[index+2 : index+6] {
				if !strings.ContainsRune("0123456789abcdefABCDEF", digit) {
					valid = false
				}
			}
		}
		out.WriteByte('\\')
		if valid {
			out.WriteByte(next)
			index++
		} else {
			out.WriteByte('\\')
		}
	}
	return out.String()
}
