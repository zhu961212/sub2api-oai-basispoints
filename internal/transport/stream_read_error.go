package transport

import (
	"context"
	"errors"
	"io"
	"net"
)

// Network error strings may contain credentials or request URLs. Classify the
// cause, but expose only fixed diagnostics to the client.
func safeUpstreamReadError(err error) string {
	var networkErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &networkErr) && networkErr.Timeout():
		return "upstream response timed out before completion; check timeout_seconds and increase it for long-running requests"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "upstream connection closed before response completed"
	default:
		return "upstream connection failed before response completed"
	}
}
