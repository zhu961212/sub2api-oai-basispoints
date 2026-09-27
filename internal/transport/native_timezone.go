package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// This feature changes the model-visible Codex environment context. It does
// not claim that an undocumented HTTP header changes server-side behavior.
func isNativeTimezoneRequest(req *http.Request) bool {
	if req == nil || req.URL == nil || req.Method != http.MethodPost ||
		!strings.EqualFold(req.URL.Scheme, "https") ||
		!strings.EqualFold(req.URL.Hostname(), "chatgpt.com") ||
		(req.URL.Port() != "" && req.URL.Port() != "443") || req.URL.User != nil {
		return false
	}
	if req.Host != "" && !strings.EqualFold(req.Host, "chatgpt.com") && !strings.EqualFold(req.Host, "chatgpt.com:443") {
		return false
	}
	return req.URL.EscapedPath() == "/backend-api/codex/responses" ||
		req.URL.EscapedPath() == "/backend-api/codex/responses/compact"
}

func (t *Transport) applyNativeTimezone(c protocol.Config, accountID int64, proxyURL string, client *http.Client, req *http.Request) {
	if !c.NativeTimezoneByIP || accountID <= 0 || !isNativeTimezoneRequest(req) || req.GetBody == nil ||
		req.Header.Get("Content-Encoding") != "" || req.Context().Err() != nil {
		return
	}
	// Read a separate copy so every failure leaves the original body intact.
	copyBody, err := req.GetBody()
	if err != nil {
		return
	}
	body, err := io.ReadAll(io.LimitReader(copyBody, (128<<20)+1))
	_ = copyBody.Close()
	if err != nil || len(body) > 128<<20 {
		return
	}
	updated := t.applyAccountTimezoneBody(req.Context(), c, accountID, proxyURL, client, body, true)
	if bytes.Equal(updated, body) {
		return
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(updated))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(updated)), nil }
	req.ContentLength = int64(len(updated))
	req.Header.Del("Content-Length")
}

// Both native forwarding and the explicit BPS branch use the identity's
// resolved proxy client. BPS calls this before protocol preparation, so its
// retries, tool repair and request context all retain the same timezone.
func (t *Transport) applyAccountTimezoneBody(ctx context.Context, c protocol.Config, accountID int64, proxyURL string, client *http.Client, body []byte, annotateNative bool) []byte {
	if !c.NativeTimezoneByIP || accountID <= 0 || ctx.Err() != nil || len(body) > 128<<20 || !nativeTimezoneInputValid(body) {
		return body
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return body
	}
	if t.nativeTimezone == nil {
		t.nativeTimezone = newNativeTimezoneResolver()
	}
	resolver := t.nativeTimezone
	t.mu.Unlock()
	location, ok := resolver.resolve(ctx, accountID, proxyURL, client)
	if !ok || ctx.Err() != nil {
		return body
	}
	updated, ok := timezoneBodyWithFormat(body, location, time.Now(), annotateNative)
	if !ok {
		return body
	}
	return updated
}

func nativeTimezoneInputValid(body []byte) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return false
	}
	input := bytes.TrimSpace(object["input"])
	return len(input) > 0 && (input[0] == '[' || input[0] == '"')
}

func nativeTimezoneBody(body []byte, location *time.Location, now time.Time) ([]byte, bool) {
	return timezoneBodyWithFormat(body, location, now, true)
}

func timezoneBodyWithFormat(body []byte, location *time.Location, now time.Time, annotateNative bool) ([]byte, bool) {
	if location == nil || location.String() == "Local" {
		return body, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return body, false
	}
	var input []json.RawMessage
	raw := bytes.TrimSpace(object["input"])
	if len(raw) == 0 {
		return body, false
	}
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return body, false
		}
		input = []json.RawMessage{nativeTimezoneMessage(text)}
	} else if raw[0] != '[' || json.Unmarshal(raw, &input) != nil {
		return body, false
	}
	zone, date := location.String(), now.In(location).Format("2006-01-02")
	changed := false
	// Only a complete, standalone environment block in a user message is
	// eligible. Tool output, assistant text, examples and older blocks survive.
	for i := len(input) - 1; i >= 0; i-- {
		var message map[string]json.RawMessage
		if json.Unmarshal(input[i], &message) != nil || !nativeTimezoneUserMessage(message) {
			continue
		}
		var text string
		if json.Unmarshal(message["content"], &text) == nil && nativeTimezoneEnvironmentKind(message, 0) {
			if replacement, ok := nativeTimezoneEnvironment(text, zone, date); ok {
				message["content"], _ = json.Marshal(replacement)
				input[i], _ = json.Marshal(message)
				changed = true
				break
			}
		}
		var parts []json.RawMessage
		if json.Unmarshal(message["content"], &parts) != nil {
			continue
		}
		for p := len(parts) - 1; p >= 0; p-- {
			var part map[string]json.RawMessage
			var kind string
			if json.Unmarshal(parts[p], &part) != nil || json.Unmarshal(part["type"], &kind) != nil || kind != "input_text" || json.Unmarshal(part["text"], &text) != nil || !nativeTimezoneEnvironmentKind(message, p) {
				continue
			}
			if replacement, ok := nativeTimezoneEnvironment(text, zone, date); ok {
				part["text"], _ = json.Marshal(replacement)
				parts[p], _ = json.Marshal(part)
				message["content"], _ = json.Marshal(parts)
				input[i], _ = json.Marshal(message)
				changed = true
				break
			}
		}
		if changed {
			break
		}
	}
	if !changed {
		contextText := "<environment_context>\n  <current_date>" + date + "</current_date>\n  <timezone>" + zone + "</timezone>\n</environment_context>"
		index := len(input)
		if index > 0 {
			var last map[string]json.RawMessage
			if json.Unmarshal(input[index-1], &last) == nil && nativeTimezoneUserMessage(last) {
				index--
			}
		}
		input = append(input, nil)
		copy(input[index+1:], input[index:])
		input[index] = nativeTimezoneContextMessage(contextText)
		if !annotateNative {
			// Native content annotations are not a BPS protocol requirement.
			input[index] = nativeTimezoneMessage(contextText)
		}
	}
	object["input"], _ = json.Marshal(input)
	result, err := json.Marshal(object)
	if err != nil {
		return body, false
	}
	return result, true
}

func nativeTimezoneUserMessage(message map[string]json.RawMessage) bool {
	var role, kind string
	_ = json.Unmarshal(message["role"], &role)
	if raw, exists := message["type"]; exists && json.Unmarshal(raw, &kind) != nil {
		return false
	}
	return role == "user" && (kind == "" || kind == "message")
}

// Current Codex annotates each message's content parts individually. When
// present, respect this evidence instead of treating quoted user input as context.
func nativeTimezoneEnvironmentKind(message map[string]json.RawMessage, index int) bool {
	raw, exists := message["internal_chat_message_metadata_passthrough"]
	if !exists {
		return true
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(raw, &metadata) != nil {
		return false
	}
	kindsRaw, exists := metadata["content_item_kinds"]
	if !exists {
		return true
	}
	var kinds []string
	return json.Unmarshal(kindsRaw, &kinds) == nil && index < len(kinds) && kinds[index] == "environments.environment_context"
}

func nativeTimezoneContextMessage(text string) json.RawMessage {
	return protocol.JSONBytes(map[string]any{
		"type": "message", "role": "user",
		"content": []map[string]string{{"type": "input_text", "text": text}},
		"internal_chat_message_metadata_passthrough": map[string]any{"content_item_kinds": []string{"environments.environment_context"}},
	})
}

func nativeTimezoneMessage(text string) json.RawMessage {
	return protocol.JSONBytes(map[string]any{"role": "user", "content": []map[string]string{{"type": "input_text", "text": text}}})
}

func nativeTimezoneEnvironment(text, zone, date string) (string, bool) {
	const opening, closing = "<environment_context>", "</environment_context>"
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, opening+"\n") && !strings.HasPrefix(trimmed, opening+"\r\n") {
		return text, false
	}
	if !strings.HasSuffix(trimmed, closing) || strings.Count(trimmed, opening) != 1 || strings.Count(trimmed, closing) != 1 {
		return text, false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(trimmed, opening), closing)
	lines := strings.Split(strings.ReplaceAll(inner, "\r\n", "\n"), "\n")
	for _, field := range []struct{ name, value string }{{"current_date", date}, {"timezone", zone}} {
		begin, end := "<"+field.name+">", "</"+field.name+">"
		if strings.Count(inner, begin)+strings.Count(inner, "<"+field.name+" ") > 1 || strings.Count(inner, end) > 1 {
			return text, false
		}
		replaced := false
		for i, line := range lines {
			content := strings.TrimSpace(line)
			if strings.Contains(line, begin) || strings.Contains(line, end) || strings.Contains(line, "<"+field.name+" ") {
				unavailable := "<" + field.name + " status=\"unavailable\" />"
				unavailableCompact := "<" + field.name + " status=\"unavailable\"/>"
				if content != unavailable && content != unavailableCompact && (!strings.HasPrefix(content, begin) || !strings.HasSuffix(content, end) || strings.Contains(strings.TrimSuffix(strings.TrimPrefix(content, begin), end), "<")) {
					return text, false
				}
				prefix := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				lines[i] = prefix + begin + field.value + end
				replaced = true
			}
		}
		if !replaced {
			lines = append(lines, "  "+begin+field.value+end)
		}
	}
	return opening + "\n" + strings.Trim(strings.Join(lines, "\n"), "\n") + "\n" + closing, true
}
