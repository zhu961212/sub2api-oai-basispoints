package transport

import (
	"net/http"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

const upstreamModelNotFoundMessage = "Upstream rejected the requested model (HTTP 404 model_not_found); the model does not exist or is not available for this request"

// The host turns HTTP 404 model_not_found into an account/model cooldown.
// Keep this failure request-scoped with a nonreplayable plugin error, before
// sending HTTP headers or a body that could change scheduling or quota state.
// Other statuses and unrecognized 404 responses retain the existing path.
func captureUpstreamModelNotFound(resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		return false
	}
	// Retain the marker used by the existing tool-failure response path.
	// Probing it again would replace the marker with an unmatched replay body.
	if _, captured := resp.Body.(*httpRequestFailureBody); captured {
		return false
	}
	return captureHTTPFailureWithin(resp, 0, canonicalHTTPModelNotFound)
}

func canonicalHTTPModelNotFound(payload map[string]any, event string) map[string]any {
	if protocol.ClassifyResponseTerminal(event, payload) != protocol.TerminalFailed {
		return nil
	}
	found := false
	var inspect func(map[string]any, int) bool
	inspect = func(object map[string]any, depth int) bool {
		if depth > 8 || basisPointsFailureStatus(object, "error") != 0 {
			return false
		}
		if code := protocol.StringValue(object["code"]); code != "" {
			if code != "model_not_found" {
				return false
			}
			found = true
		}
		// Only traverse error envelopes, never output, tool arguments or arbitrary
		// metadata. Gateways may wrap the upstream error as detail.error.error.
		for _, key := range []string{"error", "detail", "response"} {
			if nested := relayObject(object[key]); nested != nil {
				if !inspect(nested, depth+1) {
					return false
				}
			} else if key == "error" && object[key] != nil {
				return false
			}
		}
		return true
	}
	if !inspect(payload, 0) || !found {
		return nil
	}
	return map[string]any{
		"object": "response", "status": "failed", "output": []any{},
		"error": map[string]any{
			"code": "model_not_found", "type": "invalid_request_error", "message": upstreamModelNotFoundMessage,
		},
	}
}
