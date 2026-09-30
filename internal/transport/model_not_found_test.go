package transport

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestModelNotFoundProbeDiagnosticBoundaries(t *testing.T) {
	failure := map[string]any{"code": "model_not_found", "type": "invalid_request_error", "message": "The requested model is unavailable"}
	for _, test := range []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{"reported_gateway_payload", modelNotFoundTestPayload(), true},
		{"direct_error", map[string]any{"error": failure}, true},
		{"nested_error", map[string]any{"error": map[string]any{"error": failure}}, true},
		{"detail_only", map[string]any{"detail": map[string]any{"error": map[string]any{"error": failure}}}, true},
		{"response_failed", map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "error": failure}}, true},
		{"unrelated_404", map[string]any{"error": map[string]any{"code": "route_not_found", "message": "Not found"}}, false},
		{"message_only", map[string]any{"error": map[string]any{"message": "model_not_found was mentioned in this error"}}, false},
		{"output_only", map[string]any{"status": "completed", "output": []any{map[string]any{"error": failure}}}, false},
		{"arguments_only", map[string]any{"type": "error", "arguments": map[string]any{"error": failure}}, false},
		{"unrelated_detail", map[string]any{"error": failure, "detail": map[string]any{"code": "server_error"}}, false},
		{"nested_quota", map[string]any{"error": failure, "detail": map[string]any{"error": map[string]any{"code": "rate_limit_exceeded"}}}, false},
		{"nested_forbidden", map[string]any{"error": failure, "detail": map[string]any{"status_code": 403}}, false},
		{"nested_auth", map[string]any{"error": failure, "response": map[string]any{"error": map[string]any{"code": "invalid_api_key"}}}, false},
		{"string_error", map[string]any{"error": "model_not_found"}, false},
	} {
		for _, sse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/sse=%t", test.name, sse), func(t *testing.T) {
				raw := protocol.JSONBytes(test.payload)
				if sse {
					raw = relayRecord("error", test.payload)
				}
				body := &httpFailureTestBody{maxRead: 3}
				resp := httpFailureTestResponse(raw, sse, body)
				resp.StatusCode = http.StatusNotFound
				resp.Header.Set("X-Codex-Primary-Used-Percent", "42")
				if got := captureUpstreamModelNotFound(resp); got != test.want {
					t.Fatalf("capture = %t, want %t", got, test.want)
				}
				if !test.want {
					got, err := io.ReadAll(resp.Body)
					if err != nil || !bytes.Equal(got, raw) || body.closes != 0 || resp.Header.Get("X-Codex-Primary-Used-Percent") != "42" {
						t.Fatalf("unmatched response was changed: body=%s error=%v closes=%d", got, err, body.closes)
					}
				}
				_ = resp.Body.Close()
				if body.closes != 1 {
					t.Fatalf("upstream body must close once, got %d", body.closes)
				}
			})
		}
	}
}

func TestModelNotFoundProbePreservesOtherStatuses(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 429, 500, 502, 503} {
		body := &httpFailureTestBody{}
		resp := httpFailureTestResponse(protocol.JSONBytes(modelNotFoundTestPayload()), false, body)
		resp.StatusCode = status
		if captureUpstreamModelNotFound(resp) || body.reads != 0 || body.closes != 0 || resp.Body != body {
			t.Fatalf("HTTP %d was inspected or captured", status)
		}
	}
}

func TestModelNotFoundProbeRestoresIncompleteEvidence(t *testing.T) {
	failure := protocol.JSONBytes(modelNotFoundTestPayload())
	for _, test := range []struct {
		name           string
		raw            []byte
		sse, readError bool
	}{
		{"invalid_json", []byte("not valid JSON model_not_found"), false, false},
		{"json_over_limit", append(bytes.Repeat([]byte(" "), httpToolFailureProbeLimit+1), failure...), false, false},
		{"json_read_error", failure, false, true},
		{"sse_output_first", append(relayRecord("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": "visible"}), relayRecord("error", modelNotFoundTestPayload())...), true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			sentinel := errors.New("original read failure")
			body := &httpFailureTestBody{}
			if test.readError {
				body.readErr, body.errWithFinalData = sentinel, true
			}
			resp := httpFailureTestResponse(test.raw, test.sse, body)
			resp.StatusCode = http.StatusNotFound
			if captureUpstreamModelNotFound(resp) {
				t.Fatal("incomplete evidence captured")
			}
			got, err := io.ReadAll(resp.Body)
			if !bytes.Equal(got, test.raw) || test.readError != errors.Is(err, sentinel) {
				t.Fatalf("replay changed bytes/error: size=%d want=%d error=%v", len(got), len(test.raw), err)
			}
			_ = resp.Body.Close()
			if body.closes != 1 {
				t.Fatalf("replay body must close once, got %d", body.closes)
			}
		})
	}
}

func TestModelNotFoundCompleteSSEDoesNotWaitForEOF(t *testing.T) {
	body := &httpFailureTestBody{readErr: errors.New("must not read after terminal")}
	resp := httpFailureTestResponse(relayRecord("error", modelNotFoundTestPayload()), true, body)
	resp.StatusCode = http.StatusNotFound
	if !captureUpstreamModelNotFound(resp) || body.afterData != 0 || body.closes != 1 {
		t.Fatalf("complete SSE failure was not captured immediately: reads=%d tail=%d closes=%d", body.reads, body.afterData, body.closes)
	}
}

func TestModelNotFoundProbeKeepsCapturedToolFailure(t *testing.T) {
	body := &httpFailureTestBody{}
	resp := httpFailureTestResponse(protocol.JSONBytes(httpFailureTestPayload(httpFailureTestMessage)), false, body)
	resp.StatusCode = http.StatusNotFound
	if !captureBasisPointsHTTPToolFailure(resp) {
		t.Fatal("tool failure was not captured")
	}
	original := resp.Body
	if captureUpstreamModelNotFound(resp) || resp.Body != original {
		t.Fatal("model probe consumed the existing tool-failure marker")
	}
}

func modelNotFoundTestPayload() map[string]any {
	failure := map[string]any{
		"code": "model_not_found", "type": "invalid_request_error", "param": nil,
		"message": "The model 'gpt-6-astra-degrade2-luna-1p-codexswic-ev3' does not exist or you do not have access to it.",
	}
	return map[string]any{
		"error": failure,
		"detail": map[string]any{
			"error":           map[string]any{"error": failure},
			"upstream_status": 404, "upstream_request_id": "PRIVATE upstream request id",
		},
	}
}

func TestModelNotFoundDoesNotBecomeHostAccountCooldown(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			for _, sse := range []bool{false, true} {
				t.Run(fmt.Sprintf("passthrough=%t/stream=%t/sse=%t", passthrough, streaming, sse), func(t *testing.T) {
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						if calls.Add(1) > 1 {
							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write(protocol.JSONBytes(map[string]any{"id": "resp_next", "status": "completed", "output": []any{}}))
							return
						}
						setCodexQuotaHeaders(w.Header())
						payload := modelNotFoundTestPayload()
						body := protocol.JSONBytes(payload)
						if sse {
							w.Header().Set("Content-Type", "text/event-stream")
							payload["type"] = "error"
							body = relayRecord("error", payload)
						}
						w.WriteHeader(http.StatusNotFound)
						_, _ = w.Write(body)
					}))
					defer upstream.Close()
					transport := New()
					defer transport.Shutdown()
					applyConfig(t, transport, map[string]any{"responses_url": upstream.URL, "rewrite_tools": false, "transform_responses": false})
					model := "gpt-6-astra"
					if passthrough {
						model = "gpt-6-astra-degrade2-luna-1p-codexswic-ev3"
					}
					body := protocol.JSONBytes(map[string]any{"model": model, "input": "hi", "stream": streaming})
					frames := requestFrames(t, upstream.URL, token(t, "model-not-found-account"), nil, body)
					result := runForward(t, transport, frames)
					if result.errFrame == nil || result.errFrame.GetCode() != "PLUGIN_MODEL_NOT_FOUND" || !result.errFrame.GetRequestSent() {
						t.Fatalf("model rejection must be a nonreplayable request error: %+v", result)
					}
					if result.status != 0 || len(result.headers) != 0 || len(result.body) != 0 || result.ended || result.received != 0 {
						t.Fatalf("model rejection reached host HTTP/account handling: %+v", result)
					}
					message := result.errFrame.GetMessage()
					if !strings.Contains(message, "HTTP 404") || !strings.Contains(message, "model_not_found") || strings.Contains(message, "PRIVATE") {
						t.Fatalf("missing or unsafe model rejection diagnostic: %q", message)
					}
					if calls.Load() != 1 {
						t.Fatalf("model rejection was retried: %d", calls.Load())
					}
					next := runForward(t, transport, requestFrames(t, upstream.URL, token(t, "model-not-found-account"), nil, body))
					if next.errFrame != nil || next.status != http.StatusOK || calls.Load() != 2 {
						t.Fatalf("model rejection disabled later requests: result=%+v calls=%d", next, calls.Load())
					}
				})
			}
		}
	}
}
