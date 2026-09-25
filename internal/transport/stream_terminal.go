package transport

import "github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"

// normalizeRelayFailure translates upstream terminal aliases to Responses
// events. A clean [DONE] does not help a client that never recognized the
// preceding terminal event, and every failure needs a readable cause.
func normalizeRelayFailure(event string, payload map[string]any) string {
	redactImageErrorValues(payload)
	response := relayObject(payload["response"])
	if response == nil {
		response = map[string]any{"output": []any{}}
		payload["response"] = response
	}
	if event == "response.incomplete" {
		response["status"] = "incomplete"
		return event
	}
	cancelled := event == "response.cancelled" || event == "response.canceled" ||
		protocol.StringValue(response["status"]) == "cancelled" || protocol.StringValue(response["status"]) == "canceled"
	response["status"] = "failed"
	failure := relayObject(response["error"])
	if failure == nil {
		failure = make(map[string]any)
	}
	upstream := relayObject(payload["error"])
	for _, field := range []string{"code", "message"} {
		if protocol.StringValue(failure[field]) == "" {
			if value := protocol.StringValue(upstream[field]); value != "" {
				failure[field] = value
			} else if value := protocol.StringValue(payload[field]); value != "" {
				failure[field] = value
			}
		}
	}
	if protocol.StringValue(failure["code"]) == "" {
		failure["code"] = "upstream_failed"
		if cancelled {
			failure["code"] = "upstream_cancelled"
		}
	}
	if protocol.StringValue(failure["message"]) == "" {
		failure["message"] = "Basis Points returned a failed response"
		if cancelled {
			failure["message"] = "Basis Points canceled the response before completion"
		}
	}
	response["error"] = failure
	return "response.failed"
}
