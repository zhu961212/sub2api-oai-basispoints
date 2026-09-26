package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestReadLimitedClassifiesReadAndSizeFailures(t *testing.T) {
	for _, tt := range []struct {
		name    string
		reader  io.Reader
		max     int
		code    string
		message string
	}{
		{"timeout", &relayStepReader{steps: []relayReadStep{{data: "partial", err: fmt.Errorf("private-token: %w", context.DeadlineExceeded)}}}, 1024, "upstream_read", safeUpstreamReadError(context.DeadlineExceeded)},
		{"truncated", &relayStepReader{steps: []relayReadStep{{data: "partial", err: fmt.Errorf("private-token: %w", io.ErrUnexpectedEOF)}}}, 1024, "upstream_read", safeUpstreamReadError(io.ErrUnexpectedEOF)},
		{"network", &relayStepReader{steps: []relayReadStep{{err: errors.New("https://user:private-token@example.invalid")}}}, 1024, "upstream_read", "upstream connection failed before response completed"},
		{"oversize", strings.NewReader("12345"), 4, "upstream_response_too_large", "upstream response exceeds configured limit"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data, err := readLimited(tt.reader, tt.max)
			var api *protocol.APIError
			if !errors.As(err, &api) || api.StatusCode() != http.StatusBadGateway || api.Code() != tt.code || api.Error() != tt.message {
				t.Fatalf("incorrect typed failure: %T %v", err, err)
			}
			if data != nil || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("failed read returned partial bytes or private diagnostic: data=%q error=%v", data, err)
			}
		})
	}
	data, err := readLimited(strings.NewReader("1234"), 4)
	if err != nil || string(data) != "1234" {
		t.Fatalf("exact size limit rejected: data=%q error=%v", data, err)
	}
}

func TestImageUpstreamErrorReadFailureClassification(t *testing.T) {
	for _, tt := range []struct {
		name   string
		reader io.Reader
		max    int
		code   string
	}{
		{"timeout", &relayStepReader{steps: []relayReadStep{{err: context.DeadlineExceeded}}}, 1024, "upstream_read"},
		{"oversize", strings.NewReader("12345"), 4, "upstream_response_too_large"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stream := &streamStub{ctx: context.Background()}
			response := &http.Response{StatusCode: http.StatusUnprocessableEntity, Header: make(http.Header), Body: io.NopCloser(tt.reader)}
			if err := sendImageUpstreamError(stream, response, tt.max); err != nil {
				t.Fatal(err)
			}
			result := imageResponseResult(stream)
			if result.errFrame == nil || result.errFrame.GetCode() != tt.code || !result.errFrame.GetRequestSent() || result.status != 0 || result.ended || len(result.body) != 0 {
				t.Fatalf("incorrect image read failure framing: %#v", result)
			}
			if tt.name == "timeout" && result.errFrame.GetMessage() != safeUpstreamReadError(context.DeadlineExceeded) {
				t.Fatalf("timeout diagnostic lost: %q", result.errFrame.GetMessage())
			}
		})
	}
}

func TestImageUpstreamHTTP422PreservesSafeMetadata(t *testing.T) {
	const dataURL = "data:image/png;base64,c2NyZWVuc2hvdA=="
	const relayURL = "https://relay.example/api/bps-images/private-relay-secret"
	for _, tt := range []struct {
		name         string
		body         map[string]any
		wantType     string
		wantCode     string
		wantParam    string
		messageParts []string
	}{
		{
			name: "nested_error",
			body: map[string]any{"error": map[string]any{
				"message": "Cannot load file-private-image-ref from " + relayURL + "; " + dataURL,
				"type":    "invalid_request_error", "code": "invalid_image", "param": "input[0].content[1].image_url",
				"input": map[string]any{"private-input-marker": dataURL}, "trace": "private-trace-marker",
			}},
			wantType: "invalid_request_error", wantCode: "invalid_image", wantParam: "input[0].content[1].image_url",
			messageParts: []string{"Cannot load file-[redacted]", "/api/bps-images/[redacted]", "data:image/[redacted]"},
		},
		{
			name: "redacted_metadata",
			body: map[string]any{"error": map[string]any{
				"message": "invalid image", "type": "file-private-type", "code": "file-private-code", "param": relayURL,
				"input": "private-input-marker",
			}},
			wantType: "file-[redacted]", wantCode: "file-[redacted]", wantParam: "https://relay.example/api/bps-images/[redacted]",
			messageParts: []string{"invalid image"},
		},
		{
			name: "pydantic_detail",
			body: map[string]any{"detail": []any{map[string]any{
				"type": "value_error", "loc": []any{"body", "input", 2, "content", 0, "image_url"},
				"msg":   "Invalid image file-private-image-ref " + dataURL + " from " + relayURL,
				"input": map[string]any{"private-input-marker": dataURL}, "ctx": map[string]any{"error": "private-trace-marker"},
			}}},
			wantType: "upstream_error", wantCode: "upstream_error",
			messageParts: []string{"value_error", "image_url", "Invalid image file-[redacted]", "data:image/[redacted]", "/api/bps-images/[redacted]"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stream := &streamStub{ctx: context.Background()}
			response := &http.Response{StatusCode: http.StatusUnprocessableEntity, Status: "422 Unprocessable Entity", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader(string(protocol.JSONBytes(tt.body))))}
			if err := sendImageUpstreamError(stream, response, 1<<20); err != nil {
				t.Fatal(err)
			}
			result := imageResponseResult(stream)
			if result.errFrame != nil || result.status != http.StatusUnprocessableEntity || !result.ended || result.received != int64(len(result.body)) {
				t.Fatalf("invalid HTTP error framing: %#v", result)
			}
			payload, err := protocol.RawObject(result.body)
			if err != nil {
				t.Fatal(err)
			}
			failure := relayObject(payload["error"])
			if failure["type"] != tt.wantType || failure["code"] != tt.wantCode || protocol.StringValue(failure["param"]) != tt.wantParam {
				t.Fatalf("safe validation identifiers were lost: %v", failure)
			}
			message := protocol.StringValue(failure["message"])
			if !strings.HasPrefix(message, "Basis Points returned HTTP 422: ") {
				t.Fatalf("HTTP status missing from message: %q", message)
			}
			for _, part := range tt.messageParts {
				if !strings.Contains(message, part) {
					t.Fatalf("validation message omitted %q: %q", part, message)
				}
			}
			for _, secret := range []string{"private-input-marker", "private-trace-marker", "private-image-ref", "private-relay-secret", "private-type", "private-code", "c2NyZWVuc2hvdA=="} {
				if strings.Contains(string(result.body), secret) {
					t.Fatalf("validation response leaked %q: %s", secret, result.body)
				}
			}
			for key := range failure {
				if key != "message" && key != "type" && key != "code" && key != "param" {
					t.Fatalf("unexpected echoed error field %q", key)
				}
			}
		})
	}
}
