package transport

import pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"

// A Basis Points quota response does not describe the host's Codex quota. Use
// the existing transport error contract so the host cannot apply account-wide
// 429 state. The safe diagnostic preserves the upstream status for the caller.
// RequestSent prevents replay against every account in the host's pool.
func sendBasisPointsRateLimit(stream pluginv1.TransportPlugin_ForwardServer) error {
	return sendError(stream, "PLUGIN_RATE_LIMITED", "Basis Points returned HTTP 429; retry later.", true)
}
