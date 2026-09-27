package protocol

import (
	"encoding/json"
	"strings"
)

// customTextRelayTarget classifies a rejected call, never reconstructs
// or executes its text. Metadata must parse strictly either before the final
// custom string payload or as a single tool field after first-position args.
// Any possible object-member or second-value boundary
// inside the damaged text remains ineligible for regeneration.
func customTextRelayTarget(raw string, source map[string]any) (toolSpec, bool) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return toolSpec{}, false
	}
	header := map[string]any{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return toolSpec{}, false
		}
		if _, exists := header[key]; exists {
			return toolSpec{}, false
		}
		switch key {
		case "tool", "name", "namespace":
			value, err := decoder.Token()
			text, ok := value.(string)
			if err != nil || !ok {
				return toolSpec{}, false
			}
			header[key] = text
		case "args", "arguments", "input":
			header[key] = nil
			if key == "args" && len(header) == 1 {
				return argsFirstCustomTextRelayTarget(raw, int(decoder.InputOffset()), source)
			}
			envelope := unambiguousEnvelope(header)
			if envelope == nil || isTransportName(recoveryEnvelopeName(envelope)) {
				return toolSpec{}, false
			}
			spec, known := resolveClientTool(clientToolSpecs(source), recoveryEnvelopeName(envelope))
			if !known || spec.Type != "custom" {
				return toolSpec{}, false
			}
			offset := recoverySkipSpace(raw, int(decoder.InputOffset()))
			if offset >= len(raw) || raw[offset] != ':' {
				return toolSpec{}, false
			}
			offset = recoverySkipSpace(raw, offset+1)
			if offset >= len(raw) || raw[offset] != '"' {
				return toolSpec{}, false
			}
			tail := strings.TrimRight(raw, " \r\n\t")
			if len(tail) < offset+3 || tail[len(tail)-1] != '}' {
				return toolSpec{}, false
			}
			tail = strings.TrimRight(tail[:len(tail)-1], " \r\n\t")
			if len(tail) <= offset+1 || tail[len(tail)-1] != '"' {
				return toolSpec{}, false
			}
			end := len(tail) - 1
			slashes := 0
			for index := end - 1; index > offset && raw[index] == '\\'; index-- {
				slashes++
			}
			if slashes%2 != 0 {
				return toolSpec{}, false
			}
			return spec, customTextHasOnlyInteriorQuoteErrors(raw[offset+1 : end])
		default:
			return toolSpec{}, false
		}
	}
	return toolSpec{}, false
}

// The only trailing-metadata shape accepted here is args followed by exactly
// one strict tool:string field. Its literal key gives a bounded suffix scan;
// source text is inspected for ambiguity but never decoded or reconstructed.
func argsFirstCustomTextRelayTarget(raw string, start int, source map[string]any) (toolSpec, bool) {
	offset := recoverySkipSpace(raw, start)
	if offset >= len(raw) || raw[offset] != ':' {
		return toolSpec{}, false
	}
	offset = recoverySkipSpace(raw, offset+1)
	if offset >= len(raw) || raw[offset] != '"' {
		return toolSpec{}, false
	}
	keyOffset := strings.LastIndex(raw, `"tool"`)
	if keyOffset <= offset+1 {
		return toolSpec{}, false
	}
	metadata, _, err := strictRelayJSONValue("{"+raw[keyOffset:], true)
	tail := objectValue(metadata)
	if err != nil || len(tail) != 1 {
		return toolSpec{}, false
	}
	name, ok := tail["tool"].(string)
	if !ok || isTransportName(name) {
		return toolSpec{}, false
	}
	spec, known := resolveClientTool(clientToolSpecs(source), name)
	if !known || spec.Type != "custom" {
		return toolSpec{}, false
	}
	before := strings.TrimRight(raw[:keyOffset], " \r\n\t")
	if len(before) <= offset+1 || before[len(before)-1] != ',' {
		return toolSpec{}, false
	}
	before = strings.TrimRight(before[:len(before)-1], " \r\n\t")
	end := len(before) - 1
	if end <= offset || before[end] != '"' {
		return toolSpec{}, false
	}
	slashes := 0
	for index := end - 1; index > offset && raw[index] == '\\'; index-- {
		slashes++
	}
	if slashes%2 != 0 {
		return toolSpec{}, false
	}
	return spec, customTextHasOnlyInteriorQuoteErrors(raw[offset+1 : end])
}

func customTextHasOnlyInteriorQuoteErrors(text string) bool {
	quote := false
	for index := 0; index < len(text); index++ {
		if text[index] == '\\' {
			index++
			continue
		}
		if text[index] != '"' {
			continue
		}
		quote = true
		next := recoverySkipSpace(text, index+1)
		if next == len(text) {
			return false
		}
		switch text[next] {
		case ':', '{', '}', '[', ']':
			return false
		case ',':
			// printf("%d", value) is not a member boundary, but any
			// quoted or bare key followed by a colon stays ambiguous.
			next = recoverySkipSpace(text, next+1)
			if next == len(text) {
				return false
			}
			if strings.ContainsRune("{}[]", rune(text[next])) {
				return false
			}
			if customTextCouldStartMember(text, next) {
				return false
			}
		}
	}
	return quote
}

func customTextCouldStartMember(text string, start int) bool {
	end := start
	switch text[start] {
	case '/':
		return true
	case '"', '\'', '`':
		var valid bool
		end, valid = recoveryQuotedEnd(text, start)
		if !valid {
			return true
		}
		if text[start] == '"' {
			key, _, valid := relayJSONValue(text[start:end], true)
			if _, ok := key.(string); !valid || !ok {
				return true
			}
		}
	default:
		// Include Unicode and escaped bare names; do not infer identifiers.
		for end < len(text) && !strings.ContainsRune(" \r\n\t:,;()[]{}=+-*%!?|&<>/'\"`", rune(text[end])) {
			end++
		}
		if end == start {
			return false
		}
	}
	after := recoverySkipSpace(text, end)
	return after == len(text) || text[after] == ':' || text[after] == '/'
}
