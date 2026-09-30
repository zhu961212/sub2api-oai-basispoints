package attachments

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type diagnosticRoundTripper func(*http.Request) (*http.Response, error)

func (f diagnosticRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestAttachmentTransportDiagnosticDoesNotLeakOrRetry(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		cause        error
	}{
		{"deadline", "timeout", fmt.Errorf("PRIVATE deadline: %w", context.DeadlineExceeded)},
		{"dns timeout", "timeout", &net.DNSError{Err: "PRIVATE failure", Name: "PRIVATE host", IsTimeout: true}},
		{"dns before dial", "dns", &net.OpError{Op: "dial", Err: &net.DNSError{Err: "PRIVATE failure", Name: "PRIVATE host", Server: "PRIVATE server"}}},
		{"dial", "dial", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("PRIVATE refusal")}},
		{"tls certificate verification", "tls", &tls.CertificateVerificationError{Err: errors.New("PRIVATE certificate")}},
		{"tls record", "tls", tls.RecordHeaderError{Msg: "PRIVATE record"}},
		{"tls alert", "tls", tls.AlertError(42)},
		{"unknown authority", "tls", x509.UnknownAuthorityError{Cert: &x509.Certificate{}}},
		{"hostname", "tls", x509.HostnameError{Certificate: &x509.Certificate{}, Host: "PRIVATE host"}},
		{"invalid certificate", "tls", x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.Expired, Detail: "PRIVATE details"}},
		{"system roots", "tls", x509.SystemRootsError{Err: errors.New("PRIVATE roots")}},
		{"eof", "connection_closed", io.EOF},
		{"unexpected eof", "connection_closed", fmt.Errorf("PRIVATE read: %w", io.ErrUnexpectedEOF)},
		{"closed connection", "connection_closed", net.ErrClosed},
		{"generic ignores text", "generic", errors.New("PRIVATE timeout dns tls dial eof https://private.invalid?token=PRIVATE")},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				_ = r.Body.Close()
				return nil, &url.Error{Op: "Post", URL: "https://PRIVATE:PRIVATE@private.invalid/PRIVATE?token=PRIVATE", Err: test.cause}
			})}
			raw := dataURL(testPNG(t), "image/png")
			part := imagePart(raw)
			changed, err := New().Rewrite(context.Background(), client, "https://private.invalid/PRIVATE/responses", testHeaders(), message(part), "scope")
			var attachment *Error
			if changed || !errors.As(err, &attachment) || calls != 1 {
				t.Fatalf("changed=%v calls=%d error=%v", changed, calls, err)
			}
			if attachment.StatusCode() != 502 || attachment.Code() != "attachment_transport" || attachment.Error() != "Basis Points attachment upload transport failed" || attachment.DiagnosticReason() != test.reason {
				t.Fatalf("unexpected error contract: status=%d code=%s message=%s reason=%s", attachment.StatusCode(), attachment.Code(), attachment.Error(), attachment.DiagnosticReason())
			}
			if attachment.Unwrap() != nil || strings.Contains(fmt.Sprintf("%+v", err), "PRIVATE") || strings.Contains(attachment.DiagnosticReason(), "private") {
				t.Fatal("transport failure retained private upstream cause")
			}
			if part["image_url"] != raw || part["file_id"] != nil {
				t.Fatal("failed upload modified the input image")
			}
		})
	}
}

func TestAttachmentDiagnosticReasonOnlyAllowsStaticTransportValues(t *testing.T) {
	for _, err := range []*Error{nil, {code: "attachment_transport", reason: "PRIVATE"}, {code: "invalid_image", reason: "timeout"}, {code: "invalid_attachment_response", reason: "PRIVATE"}, {code: "invalid_attachment_response", reason: "generic"}} {
		if err.DiagnosticReason() != "" {
			t.Fatal("unknown or unrelated diagnostic escaped the allowlist")
		}
	}
}

type diagnosticResponseBody struct {
	payload string
	cause   error
	onRead  func()
	closed  bool
}

func (b *diagnosticResponseBody) Read(p []byte) (int, error) {
	if b.onRead != nil {
		b.onRead()
	}
	return copy(p, b.payload), b.cause
}

func (b *diagnosticResponseBody) Close() error {
	b.closed = true
	return nil
}

func TestAttachmentResponseReadDiagnosticPreservesFailureAndDoesNotRetry(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		cause        error
	}{
		{"deadline", "timeout", fmt.Errorf("PRIVATE timeout: %w", context.DeadlineExceeded)},
		{"network timeout", "timeout", &net.OpError{Op: "read", Err: &net.DNSError{Err: "PRIVATE", IsTimeout: true}}},
		{"tls alert", "tls", tls.AlertError(42)},
		{"truncated body", "connection_closed", fmt.Errorf("PRIVATE response: %w", io.ErrUnexpectedEOF)},
		{"closed body", "connection_closed", net.ErrClosed},
		{"unknown reader error", "", errors.New("PRIVATE timeout dns tls dial https://PRIVATE.invalid?token=PRIVATE")},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Even a complete JSON fragment does not prove success when its read
			// reports an error. Never publish the file ID or upload it again.
			body := &diagnosticResponseBody{payload: `{"openai_file_id":"file-PRIVATE"}`, cause: test.cause}
			calls := 0
			client := &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				_ = r.Body.Close()
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
			})}
			raw := dataURL(testPNG(t), "image/png")
			part := imagePart(raw)
			changed, err := New().Rewrite(context.Background(), client, "https://PRIVATE.invalid/attachments/responses", testHeaders(), message(part), "scope")
			var attachment *Error
			if changed || !errors.As(err, &attachment) || calls != 1 || !body.closed {
				t.Fatalf("changed=%v calls=%d closed=%v error=%v", changed, calls, body.closed, err)
			}
			if attachment.StatusCode() != 502 || attachment.Code() != "invalid_attachment_response" || attachment.Error() != "Basis Points attachment upload returned an invalid response" || attachment.DiagnosticReason() != test.reason {
				t.Fatalf("read error contract changed: status=%d code=%s reason=%s", attachment.StatusCode(), attachment.Code(), attachment.DiagnosticReason())
			}
			if attachment.Unwrap() != nil || strings.Contains(fmt.Sprintf("%+v", err), "PRIVATE") || part["image_url"] != raw || part["file_id"] != nil {
				t.Fatal("read error retained private data or published the unverified file")
			}
		})
	}
}

func TestAttachmentInvalidResponseDoesNotInventTransportDiagnostic(t *testing.T) {
	for _, raw := range []string{
		`{"openai_file_id":`, `{"openai_file_id":"PRIVATE"}`, `{"error":"PRIVATE timeout dns tls"}`,
		strings.Repeat("PRIVATE", maxResponseBytes/7+1),
	} {
		calls := 0
		client := &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			_ = r.Body.Close()
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}, nil
		})}
		_, err := New().Rewrite(context.Background(), client, "https://PRIVATE.invalid/responses", testHeaders(), message(imagePart(dataURL(testPNG(t), "image/png"))), "scope")
		var attachment *Error
		if !errors.As(err, &attachment) || attachment.StatusCode() != 502 || attachment.Code() != "invalid_attachment_response" || attachment.DiagnosticReason() != "" || calls != 1 {
			t.Fatalf("invalid response was mislabeled or retried: calls=%d error=%v", calls, err)
		}
	}
}

func TestAttachmentResponseReadDiagnosticPreservesCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			// Virtual time advances only when Read waits on cancellation; host
			// scheduling cannot expire the deadline during PNG preparation.
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				var stop context.CancelFunc
				if deadline {
					ctx, stop = context.WithTimeout(ctx, 10*time.Millisecond)
					defer stop()
				}
				defer cancel()
				body := &diagnosticResponseBody{cause: io.ErrUnexpectedEOF, onRead: func() {
					if deadline {
						<-ctx.Done()
					} else {
						cancel()
					}
				}}
				calls := 0
				client := &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					_ = r.Body.Close()
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
				})}
				_, err := New().Rewrite(ctx, client, "https://PRIVATE.invalid/responses", testHeaders(), message(imagePart(dataURL(testPNG(t), "image/png"))), "scope")
				var attachment *Error
				if !errors.As(err, &attachment) || !errors.Is(err, ctx.Err()) || attachment.StatusCode() != 499 || attachment.Code() != "attachment_canceled" || attachment.DiagnosticReason() != "" || calls != 1 || !body.closed {
					t.Fatalf("read cancellation changed: calls=%d closed=%v error=%v", calls, body.closed, err)
				}
			})
		})
	}
}

func TestAttachmentTransportDiagnosticPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	client := &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		_ = r.Body.Close()
		cancel()
		return nil, fmt.Errorf("PRIVATE transport cause: %w", ctx.Err())
	})}
	raw := dataURL(testPNG(t), "image/png")
	_, err := New().Rewrite(ctx, client, "https://private.invalid/responses", testHeaders(), message(imagePart(raw)), "scope")
	var attachment *Error
	if !errors.As(err, &attachment) || !errors.Is(err, context.Canceled) || attachment.StatusCode() != 499 || attachment.Code() != "attachment_canceled" || attachment.DiagnosticReason() != "" || calls != 1 {
		t.Fatalf("cancellation contract changed: calls=%d error=%v", calls, err)
	}
	deadline, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	_, err = New().Rewrite(deadline, client, "https://private.invalid/responses", testHeaders(), message(imagePart(raw)), "scope")
	if !errors.As(err, &attachment) || !errors.Is(err, context.DeadlineExceeded) || attachment.StatusCode() != 499 || attachment.Code() != "attachment_canceled" || attachment.DiagnosticReason() != "" || calls != 1 {
		t.Fatalf("deadline cancellation contract changed: calls=%d error=%v", calls, err)
	}
}
