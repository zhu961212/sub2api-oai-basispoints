package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/attachments"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

type attachmentDiagnosticSpoof struct{}

func (attachmentDiagnosticSpoof) Error() string            { return "safe fixture" }
func (attachmentDiagnosticSpoof) StatusCode() int          { return 502 }
func (attachmentDiagnosticSpoof) Code() string             { return "attachment_transport" }
func (attachmentDiagnosticSpoof) DiagnosticReason() string { return "PRIVATE" }

type attachmentDiagnosticReadError struct{}

func (attachmentDiagnosticReadError) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestImageRelayForwardsOnlyStaticAttachmentReason(t *testing.T) {
	_, imageURL := relayTestImage(t)
	source, err := protocol.RawObject(relayTestBody(imageURL))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &http.Client{Transport: retryPolicyRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		_ = request.Body.Close()
		return nil, &net.DNSError{Name: "PRIVATE host", Server: "PRIVATE server", Err: "PRIVATE cause"}
	})}
	_, uploadErr := attachments.New().Rewrite(context.Background(), client, "https://PRIVATE.invalid/PRIVATE/responses", nil, source, "scope")
	var attachment *attachments.Error
	if !errors.As(uploadErr, &attachment) || calls != 1 {
		t.Fatalf("upload fixture failed: calls=%d error=%v", calls, uploadErr)
	}
	readCalls := 0
	readClient := &http.Client{Transport: retryPolicyRoundTripper(func(request *http.Request) (*http.Response, error) {
		readCalls++
		_ = request.Body.Close()
		body := io.MultiReader(strings.NewReader(`{"openai_file_id":"file-PRIVATE"}`), attachmentDiagnosticReadError{})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(body)}, nil
	})}
	_, readErr := attachments.New().Rewrite(context.Background(), readClient, "https://PRIVATE.invalid/PRIVATE/responses", nil, source, "scope")
	if !errors.As(readErr, &attachment) || readCalls != 1 {
		t.Fatalf("read fixture failed: calls=%d error=%v", readCalls, readErr)
	}
	for _, test := range []struct {
		name string
		err  error
		want map[string]any
	}{
		{"attachment", uploadErr, map[string]any{"message": "Basis Points attachment upload transport failed", "type": "server_error", "code": "attachment_transport", "param": nil, "reason": "dns"}},
		{"attachment response read", readErr, map[string]any{"message": "Basis Points attachment upload returned an invalid response", "type": "server_error", "code": "invalid_attachment_response", "param": nil, "reason": "connection_closed"}},
		{"unrelated error", &protocol.APIError{Status: 502, Kind: "attachment_transport", Message: "safe fixture"}, map[string]any{"message": "safe fixture", "type": "server_error", "code": "attachment_transport", "param": nil}},
		{"spoofed reason", attachmentDiagnosticSpoof{}, map[string]any{"message": "safe fixture", "type": "server_error", "code": "attachment_transport", "param": nil}},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := &streamStub{ctx: context.Background()}
			if err := sendImageRelayError(stream, test.err); err != nil {
				t.Fatal(err)
			}
			if len(stream.responses) != 3 || stream.responses[0].GetStart().GetStatusCode() != 502 || stream.responses[0].GetStart().GetStatus() != "502 Bad Gateway" || stream.responses[2].GetEnd() == nil {
				t.Fatal("attachment HTTP forwarding lifecycle changed")
			}
			body := stream.responses[1].GetBodyChunk()
			var got map[string]map[string]any
			if err := json.Unmarshal(body, &got); err != nil || len(got) != 1 || !reflect.DeepEqual(got["error"], test.want) {
				t.Fatalf("unexpected forwarding JSON: %s error=%v", body, err)
			}
			if strings.Contains(string(body), "PRIVATE") || stream.responses[0].GetStart().GetHeaders()["Retry-After"] != nil {
				t.Fatal("diagnostic leaked private data or introduced a retry policy")
			}
		})
	}
}
