package transport

import (
	"fmt"
	"net/http"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
)

// A Basis Points quota response does not describe the host's Codex quota. Use
// the existing transport error contract so the host cannot apply account-wide
// 429 state. The safe diagnostic preserves the upstream status for the caller.
// RequestSent prevents replay against every account in the host's pool.
func sendBasisPointsRateLimit(stream pluginv1.TransportPlugin_ForwardServer) error {
	return sendError(stream, "PLUGIN_RATE_LIMITED", "Basis Points returned HTTP 429; retry later.", true)
}

// A prior real rejection supplied Retry-After. RequestSent deliberately keeps
// the host from replaying this locally paused request against other accounts;
// it is a replay barrier, not a claim that this new request reached BPS.
func sendBasisPointsBackoff(stream pluginv1.TransportPlugin_ForwardServer, seconds int64) error {
	return sendError(stream, "PLUGIN_RATE_LIMITED", fmt.Sprintf("A prior Basis Points request returned HTTP 429; this BPS request was not sent. Retry after %d seconds.", seconds), true)
}

// BPS access and quota decisions describe this service, not the host Codex
// account. A sent transport error keeps the host from disabling that account.
func sendBasisPointsAccountStatus(stream pluginv1.TransportPlugin_ForwardServer, status int) error {
	if status == http.StatusTooManyRequests {
		return sendBasisPointsRateLimit(stream)
	}
	return sendError(stream, "PLUGIN_UPSTREAM_REJECTED", basisPointsAccountStatusMessage(status), true)
}

func basisPointsAccountStatusMessage(status int) string {
	if status == http.StatusTooManyRequests {
		return "Basis Points returned HTTP 429; retry later."
	}
	return fmt.Sprintf("Basis Points returned HTTP %d; this request could not be completed.", status)
}
