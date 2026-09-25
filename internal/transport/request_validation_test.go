package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestForwardRequestValidationReturnsHTTPError(t *testing.T) {
	const privateText = "private-request-text-do-not-return"
	const privateImage = "file:///private-image-payload-do-not-return.png"
	cases := []struct {
		name   string
		fields map[string]any
	}{
		{"previous response", map[string]any{"previous_response_id": "private-response-id"}},
		{"forced tool", map[string]any{"tool_choice": map[string]any{"type": "function", "name": "private-function-name"}}},
		{"invalid image URL", map[string]any{"input": []any{map[string]any{
			"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": privateText},
				map[string]any{"type": "input_image", "image_url": privateImage},
			},
		}}}},
	}
	for _, rewrite := range []bool{false, true} {
		for _, transform := range []bool{false, true} {
			for _, test := range cases {
				t.Run(fmt.Sprintf("rewrite=%t/transform=%t/%s", rewrite, transform, test.name), func(t *testing.T) {
					source := map[string]any{"model": protocol.DefaultModelID, "input": privateText, "stream": true}
					for key, value := range test.fields {
						source[key] = value
					}
					body := protocol.JSONBytes(source)
					var hits atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						hits.Add(1)
						received, err := io.ReadAll(r.Body)
						if err != nil || !bytes.Equal(received, body) {
							t.Errorf("unmodified request changed: %v", err)
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(protocol.JSONBytes(map[string]any{"id": "resp_native", "status": "completed", "output": []any{}}))
					}))
					defer upstream.Close()
					transport := New()
					defer transport.Shutdown()
					applyConfig(t, transport, map[string]any{
						"responses_url": upstream.URL, "rewrite_tools": rewrite, "transform_responses": transform,
					})
					accessToken := token(t, "private-account-id")
					stub := &streamStub{ctx: context.Background(), requests: requestFrames(t, upstream.URL, accessToken, nil, body)}
					if err := transport.Forward(stub); err != nil {
						t.Fatal(err)
					}
					if !rewrite && !transform {
						if hits.Load() != 1 || len(stub.responses) != 3 || stub.responses[0].GetStart().GetStatusCode() != http.StatusOK || stub.responses[2].GetEnd() == nil {
							t.Fatalf("disabled validation changed passthrough: hits=%d frames=%v", hits.Load(), stub.responses)
						}
						return
					}
					if hits.Load() != 0 {
						t.Fatalf("invalid request reached upstream %d times", hits.Load())
					}
					response := assertRequestValidationHTTPError(t, stub.responses, http.StatusBadRequest, "unsupported_capability")
					for _, secret := range []string{privateText, privateImage, "private-response-id", "private-function-name", accessToken} {
						if strings.Contains(response, secret) {
							t.Fatal("validation error exposed private request content or credentials")
						}
					}
				})
			}
		}
	}
}

func TestForwardPreparedRequestValidationReturnsHTTPError(t *testing.T) {
	for _, transform := range []bool{false, true} {
		t.Run(fmt.Sprintf("transform=%t", transform), func(t *testing.T) {
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer upstream.Close()
			transport := New()
			defer transport.Shutdown()
			applyConfig(t, transport, map[string]any{
				"responses_url": upstream.URL, "rewrite_tools": true, "transform_responses": transform,
			})
			body := protocol.JSONBytes(map[string]any{
				"model": protocol.DefaultModelID, "input": "private-input-text",
				"text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "invalid name private-schema-name", "schema": map[string]any{}}},
			})
			stub := &streamStub{ctx: context.Background(), requests: requestFrames(t, upstream.URL, token(t, "account"), nil, body)}
			if err := transport.Forward(stub); err != nil {
				t.Fatal(err)
			}
			if hits.Load() != 0 {
				t.Fatal("invalid prepared request reached upstream")
			}
			response := assertRequestValidationHTTPError(t, stub.responses, http.StatusBadRequest, "unsupported_capability")
			if strings.Contains(response, "private-input-text") || strings.Contains(response, "private-schema-name") {
				t.Fatal("preparation error exposed private request content")
			}
		})
	}
}

func TestSendRequestValidationErrorPreservesProtocolStatus(t *testing.T) {
	_, malformed := protocol.RawObject([]byte("private-invalid-json-input"))
	for _, test := range []struct {
		name string
		err  error
		want int
		code string
	}{
		{"invalid JSON", malformed, http.StatusBadRequest, "invalid_request"},
		{"wrapped client error", fmt.Errorf("validation: %w", &protocol.APIError{Status: http.StatusUnprocessableEntity, Kind: "invalid_input", Message: "input is invalid"}), http.StatusUnprocessableEntity, "invalid_input"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := &streamStub{ctx: context.Background()}
			if err := sendRequestValidationError(stub, test.err); err != nil {
				t.Fatal(err)
			}
			response := assertRequestValidationHTTPError(t, stub.responses, test.want, test.code)
			if strings.Contains(response, "private-invalid-json-input") {
				t.Fatal("JSON validation exposed request body")
			}
		})
	}
}

func TestSendRequestValidationErrorKeepsNonClientErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{"upstream failure", &protocol.APIError{Status: http.StatusBadGateway, Kind: "invalid_upstream_response", Message: "upstream response is invalid"}, "invalid_upstream_response"},
		{"unclassified failure", errors.New("internal preparation failure"), "invalid_request"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := &streamStub{ctx: context.Background()}
			if err := sendRequestValidationError(stub, test.err); err != nil {
				t.Fatal(err)
			}
			if len(stub.responses) != 1 || stub.responses[0].GetError() == nil {
				t.Fatal("non-client failure became a normal HTTP validation response")
			}
			failure := stub.responses[0].GetError()
			if failure.GetCode() != test.code || failure.GetRequestSent() {
				t.Fatalf("failure boundary changed: %v", failure)
			}
		})
	}
}

func assertRequestValidationHTTPError(t *testing.T, frames []*pluginv1.ForwardResponse, status int, code string) string {
	t.Helper()
	if len(frames) != 3 || frames[0].GetStart() == nil || len(frames[1].GetBodyChunk()) == 0 || frames[2].GetEnd() == nil {
		t.Fatalf("expected Start/Body/End frames for validation error: %v", frames)
	}
	for _, frame := range frames {
		if frame.GetError() != nil {
			t.Fatalf("validation failure became transport error: %v", frame.GetError())
		}
	}
	start, body, end := frames[0].GetStart(), frames[1].GetBodyChunk(), frames[2].GetEnd()
	if start.GetStatusCode() != int32(status) || start.GetStatus() != fmt.Sprintf("%d %s", status, http.StatusText(status)) {
		t.Fatalf("validation status changed: %v", start)
	}
	if start.GetHeaders()["Content-Type"].GetValues()[0] != "application/json" {
		t.Fatalf("validation response is not JSON: %v", start.GetHeaders())
	}
	if start.GetContentLength() != int64(len(body)) || end.GetBytesReceived() != int64(len(body)) || start.GetHeaders()["Content-Length"].GetValues()[0] != strconv.Itoa(len(body)) {
		t.Fatal("validation response length does not match the emitted body")
	}
	var envelope map[string]map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("validation response is not an OpenAI JSON error: %v", err)
	}
	failure := envelope["error"]
	if len(envelope) != 1 || len(failure) != 4 || failure["type"] != "invalid_request_error" || failure["code"] != code || failure["param"] != nil || failure["message"] == "" {
		t.Fatalf("unexpected OpenAI error envelope: %s", body)
	}
	return string(body)
}
