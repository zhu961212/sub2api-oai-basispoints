package protocol

import (
	"fmt"
	"strconv"
	"strings"
)

// ResponseTerminal is shared by streaming, buffered and repair consumers.
// A failed envelope must never be promoted by a completed nested response.
type ResponseTerminal string

const (
	TerminalNone       ResponseTerminal = ""
	TerminalCompleted  ResponseTerminal = "completed"
	TerminalFailed     ResponseTerminal = "failed"
	TerminalIncomplete ResponseTerminal = "incomplete"
	TerminalCancelled  ResponseTerminal = "cancelled"
	TerminalInvalid    ResponseTerminal = "invalid"
)

func (terminal ResponseTerminal) Failed() bool {
	return terminal != TerminalNone && terminal != TerminalCompleted
}

func terminalKind(kind string) ResponseTerminal {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "error", "failed", "response.failed":
		return TerminalFailed
	case "incomplete", "response.incomplete":
		return TerminalIncomplete
	case "cancelled", "canceled", "response.cancelled", "response.canceled":
		return TerminalCancelled
	case "completed", "response.completed", "response.done":
		return TerminalCompleted
	default:
		return TerminalNone
	}
}

// ClassifyResponseTerminal checks the actual SSE event name before any caller
// rewrites it from data.type. Only envelope fields are inspected: ordinary
// output text and tool arguments cannot become failure evidence. An explicit
// progress event cannot be completed by a contradictory nested status.
func ClassifyResponseTerminal(event string, payload map[string]any) ResponseTerminal {
	response := objectValue(payload["response"])
	types := [...]string{event, stringValue(payload["type"])}
	statuses := [...]string{stringValue(payload["status"]), stringValue(response["status"])}
	failure := TerminalNone
	for _, value := range [...]string{types[0], types[1], statuses[0], statuses[1]} {
		kind := terminalKind(value)
		if kind == TerminalFailed {
			return kind
		}
		if kind.Failed() && failure == TerminalNone {
			failure = kind
		}
	}
	if failure != TerminalNone {
		return failure
	}
	for _, object := range []map[string]any{payload, response} {
		if object["error"] != nil {
			return TerminalFailed
		}
		for _, key := range []string{"success", "ok"} {
			if success, ok := object[key].(bool); ok && !success {
				return TerminalFailed
			}
		}
		for _, key := range []string{"status", "status_code", "http_status"} {
			if object[key] == nil {
				continue
			}
			status, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(object[key])))
			if err == nil && status >= 400 && status <= 599 {
				return TerminalFailed
			}
		}
	}
	completion := false
	for _, kind := range types {
		completion = completion || terminalKind(kind) == TerminalCompleted
	}
	if completion {
		for _, kind := range types {
			if kind != "" && kind != "message" && terminalKind(kind) != TerminalCompleted {
				return TerminalInvalid
			}
		}
		for _, status := range statuses {
			if status != "" && terminalKind(status) != TerminalCompleted {
				return TerminalInvalid
			}
		}
		return TerminalCompleted
	}
	// Bare JSON responses have no SSE lifecycle. SSE envelopes must supply a
	// completion event, even if their nested object says completed.
	if event == "" && response == nil && terminalKind(stringValue(payload["status"])) == TerminalCompleted {
		return TerminalCompleted
	}
	return TerminalNone
}

// ResponseTerminalError retains request-scoped isolation without reflecting
// arbitrary upstream diagnostics into the host error channel. Stream payloads
// retain their original error details through the normal response path.
func ResponseTerminalError(terminal ResponseTerminal, payload map[string]any) error {
	if !terminal.Failed() {
		return nil
	}
	response := objectValue(payload["response"])
	failures := []map[string]any{objectValue(response["error"]), objectValue(payload["error"])}
	for _, failure := range failures {
		if stringValue(failure["code"]) == "bps_service_rejected" {
			return fail(502, "bps_service_rejected", stringValue(failure["message"]))
		}
	}
	for _, failure := range failures {
		if stringValue(failure["code"]) == "invalid_tool_call" {
			message := stringValue(failure["message"])
			if message != malformedClientToolMessage && message != unknownClientToolMessage {
				message = "Basis Points returned an invalid client tool call"
			}
			return fail(502, "invalid_tool_call", message)
		}
		if stringValue(failure["code"]) == "upstream_cancelled" {
			terminal = TerminalCancelled
		}
	}
	switch terminal {
	case TerminalIncomplete:
		return fail(502, "upstream_incomplete", "Basis Points returned an incomplete response")
	case TerminalCancelled:
		return fail(502, "upstream_cancelled", "Basis Points canceled the response before completion")
	case TerminalInvalid:
		return fail(502, "invalid_upstream_response", "Basis Points returned conflicting response terminal fields")
	default:
		return fail(502, "upstream_failed", "Basis Points returned a failed response")
	}
}

// NormalizeClientToolFailure marks an existing tool protocol failure as scoped
// to the current request. Only failure envelope fields are inspected; code or
// marker-looking text in normal output and tool arguments remains untouched.
func NormalizeClientToolFailure(payload map[string]any, event string) bool {
	if !ClassifyResponseTerminal(event, payload).Failed() {
		return false
	}
	changed := false
	for _, object := range []map[string]any{payload, objectValue(payload["response"])} {
		if object == nil {
			continue
		}
		failure := objectValue(object["error"])
		if failure == nil && object["error"] == nil && stringValue(object["code"]) == "invalid_tool_call" {
			failure = map[string]any{"code": "invalid_tool_call"}
			if message := stringValue(object["message"]); message != "" {
				failure["message"] = message
			}
			object["error"] = failure
			changed = true
		}
		if stringValue(failure["code"]) == "invalid_tool_call" && stringValue(failure["type"]) != "invalid_request_error" {
			failure["type"] = "invalid_request_error"
			changed = true
		}
	}
	return changed
}

// NormalizeResponseFailure produces a terminal recognized by Responses
// clients. Cancellation uses a failed event with an explicit cancellation
// code; incomplete remains incomplete. Request-scoped isolation always wins
// over a second diagnostic in the other envelope layer.
func NormalizeResponseFailure(payload map[string]any, terminal ResponseTerminal) string {
	response := objectValue(payload["response"])
	if response == nil {
		response = map[string]any{"output": []any{}}
		payload["response"] = response
	}
	failure := cloneObject(objectValue(response["error"]))
	if failure == nil {
		failure = make(map[string]any)
	}
	upstream := objectValue(payload["error"])
	if stringValue(upstream["code"]) == "bps_service_rejected" {
		failure = cloneObject(upstream)
	}
	for _, field := range []string{"code", "message", "type"} {
		if stringValue(failure[field]) == "" {
			if value := stringValue(upstream[field]); value != "" {
				failure[field] = value
			} else if value := stringValue(payload[field]); value != "" {
				// An SSE lifecycle type describes the envelope, not its error.
				if field == "type" && (strings.HasPrefix(value, "response.") || terminalKind(value) != TerminalNone) {
					continue
				}
				failure[field] = value
			}
		}
	}
	if terminal == TerminalIncomplete {
		response["status"] = "incomplete"
		if len(failure) > 0 {
			response["error"] = failure
		}
		return "response.incomplete"
	}
	response["status"] = "failed"
	if terminal == TerminalCancelled && stringValue(failure["code"]) != "bps_service_rejected" {
		if code := stringValue(failure["code"]); code != "" && code != "upstream_cancelled" {
			failure["upstream_code"] = code
		}
		failure["code"] = "upstream_cancelled"
	}
	if stringValue(failure["code"]) == "" {
		failure["code"] = "upstream_failed"
		if terminal == TerminalCancelled {
			failure["code"] = "upstream_cancelled"
		} else if terminal == TerminalInvalid {
			failure["code"] = "invalid_upstream_response"
		}
	}
	if stringValue(failure["message"]) == "" {
		failure["message"] = "Basis Points returned a failed response"
		if terminal == TerminalCancelled {
			failure["message"] = "Basis Points canceled the response before completion"
		}
	}
	if stringValue(failure["code"]) == "invalid_tool_call" {
		failure["type"] = "invalid_request_error"
	}
	response["error"] = failure
	return "response.failed"
}
