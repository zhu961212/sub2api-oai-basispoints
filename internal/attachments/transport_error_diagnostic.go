package attachments

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
)

func transportFailure(err error) error {
	// Do not retain err: net/http errors can embed the endpoint, credentials,
	// proxy address, certificate names, or other private upstream data.
	return &Error{
		status: 502, code: "attachment_transport",
		message: "Basis Points attachment upload transport failed",
		reason:  attachmentTransportReason(err),
	}
}

func responseReadFailure(err error) error {
	// Keep the existing invalid-response contract. Only typed transport causes
	// add a diagnostic; malformed JSON, size limits, and arbitrary reader errors
	// must not be presented as a known network failure.
	reason := attachmentTransportReason(err)
	if reason == "generic" {
		reason = ""
	}
	return &Error{
		status: 502, code: "invalid_attachment_response",
		message: "Basis Points attachment upload returned an invalid response",
		reason:  reason,
	}
}

func attachmentTransportReason(err error) string {
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout() {
		return "timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var verification *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var certificate x509.CertificateInvalidError
	var roots x509.SystemRootsError
	if errors.As(err, &verification) || errors.As(err, &record) || errors.As(err, &alert) ||
		errors.As(err, &authority) || errors.As(err, &hostname) ||
		errors.As(err, &certificate) || errors.As(err, &roots) {
		return "tls"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return "connection_closed"
	}
	var operation *net.OpError
	if errors.As(err, &operation) && operation.Op == "dial" {
		return "dial"
	}
	return "generic"
}
