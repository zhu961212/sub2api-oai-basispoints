package transport

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func assertBasisPointsRateLimit(t *testing.T, result forwardResult) {
	t.Helper()
	if result.errFrame == nil || result.errFrame.GetCode() != "PLUGIN_RATE_LIMITED" || !result.errFrame.GetRequestSent() {
		t.Fatalf("BPS rate limit must be a nonreplayable plugin error: %+v", result)
	}
	message := result.errFrame.GetMessage()
	if !strings.Contains(message, "Basis Points") || !strings.Contains(message, "HTTP 429") || strings.Contains(message, "PRIVATE") {
		t.Fatalf("BPS rate limit diagnostic is missing or unsafe: %q", message)
	}
	if result.status != 0 || len(result.headers) != 0 || len(result.body) != 0 || result.ended || result.received != 0 {
		t.Fatalf("BPS quota response leaked into the host HTTP error path: %+v", result)
	}
}

func basisPointsQuotaBody() []byte {
	return protocol.JSONBytes(map[string]any{"error": map[string]any{
		"type": "usage_limit_reached", "code": "rate_limit_exceeded",
		"message":   "PRIVATE upstream quota diagnostic file-private-token",
		"plan_type": "team", "resets_at": 4102444800,
	}})
}

func setCodexQuotaHeaders(header http.Header) {
	header.Set("Content-Type", "application/json")
	header.Set("X-Codex-Primary-Used-Percent", "100")
	header.Set("X-Codex-Primary-Reset-After-Seconds", "3600")
	header.Set("Retry-After", "3600")
	header.Set("X-Request-Id", "PRIVATE upstream request id")
}

func TestBasisPoints429NeverBecomesHostAccountRateLimit(t *testing.T) {
	for _, quotaMetadata := range []bool{false, true} {
		for _, rewrite := range []bool{false, true} {
			for _, transform := range []bool{false, true} {
				for _, streaming := range []bool{false, true} {
					for _, imageMode := range []string{"none", "conversation", "attachment"} {
						t.Run(fmt.Sprintf("quota=%t/rewrite=%t/transform=%t/stream=%t/image=%s", quotaMetadata, rewrite, transform, streaming, imageMode), func(t *testing.T) {
							imageBytes, inlineImage := relayTestImage(t)
							var uploads, conversations, hostCalls atomic.Int32
							host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
								hostCalls.Add(1)
								w.WriteHeader(http.StatusTeapot)
							}))
							defer host.Close()
							upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								if r.URL.Path == "/basispoints/api/attachments" {
									uploads.Add(1)
									if imageMode != "attachment" {
										relayTestUpload(t, w, r, imageBytes, "file-rate-limit-test")
										return
									}
								} else {
									conversations.Add(1)
								}
								if quotaMetadata {
									setCodexQuotaHeaders(w.Header())
								}
								w.WriteHeader(http.StatusTooManyRequests)
								if quotaMetadata {
									_, _ = w.Write(basisPointsQuotaBody())
								} else {
									_, _ = io.WriteString(w, "PRIVATE unstructured quota error")
								}
							}))
							defer upstream.Close()
							transport := New()
							defer transport.Shutdown()
							applyConfig(t, transport, map[string]any{
								"responses_url": upstream.URL + "/basispoints/api/responses",
								"rewrite_tools": rewrite, "transform_responses": transform,
							})
							source := map[string]any{"model": "gpt-6-astra", "input": "hi", "stream": streaming}
							if imageMode != "none" {
								source, _ = protocol.RawObject(relayTestBody(inlineImage))
								source["stream"] = streaming
							}
							result := runForward(t, transport, requestFrames(t, host.URL, token(t, "rate-limit-account"), nil, protocol.JSONBytes(source)))
							assertBasisPointsRateLimit(t, result)
							wantUploads, wantConversations := int32(0), int32(1)
							if imageMode != "none" {
								wantUploads = 1
							}
							if imageMode == "attachment" {
								wantConversations = 0
							}
							if uploads.Load() != wantUploads || conversations.Load() != wantConversations || hostCalls.Load() != 0 {
								t.Fatalf("unexpected routing or retry: uploads=%d conversations=%d host=%d", uploads.Load(), conversations.Load(), hostCalls.Load())
							}
						})
					}
				}
			}
		}
	}
}

func TestRateLimitRoutingKeepsBasisPointsSeparateFromHostQuota(t *testing.T) {
	monitorPrompt := strings.Join([]string{"Calculate and respond with ONLY the number, nothing else.", "", "Q: 3 + 5 = ?", "A:"}, string(rune(10)))
	for _, test := range []struct {
		name, model string
		input       any
		accountIDs  []int64
		wantBasis   bool
	}{
		{"unknown_model", "gpt-5.4", "hi", nil, false},
		{"monitor_probe", "gpt-6-astra", monitorPrompt, nil, true},
		{"unselected_account", "gpt-6-astra", "hi", []int64{99}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var hostCalls, basisCalls atomic.Int32
			responseBody := basisPointsQuotaBody()
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hostCalls.Add(1)
				setCodexQuotaHeaders(w.Header())
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write(responseBody)
			}))
			defer host.Close()
			basis := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				basisCalls.Add(1)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer basis.Close()
			transport := New()
			defer transport.Shutdown()
			applyConfig(t, transport, map[string]any{"responses_url": basis.URL, "account_ids": test.accountIDs})
			body := protocol.JSONBytes(map[string]any{"model": test.model, "input": test.input})
			result := runForward(t, transport, requestFrames(t, host.URL, token(t, "passthrough-account"), nil, body))
			if test.wantBasis {
				assertBasisPointsRateLimit(t, result)
				if hostCalls.Load() != 0 || basisCalls.Load() != 1 {
					t.Fatalf("selected model must keep BPS routing: host=%d basis=%d", hostCalls.Load(), basisCalls.Load())
				}
				return
			}
			if result.errFrame != nil || result.status != http.StatusTooManyRequests || !result.ended || !bytes.Equal(result.body, responseBody) {
				t.Fatalf("real host quota response changed: %+v", result)
			}
			if headerValue(result.headers, "X-Codex-Primary-Reset-After-Seconds") != "3600" || headerValue(result.headers, "Retry-After") != "3600" {
				t.Fatalf("real host quota headers changed: %+v", result.headers)
			}
			if hostCalls.Load() != 1 || basisCalls.Load() != 0 {
				t.Fatalf("passthrough routing changed: host=%d basis=%d", hostCalls.Load(), basisCalls.Load())
			}
		})
	}
}

func TestBasisPointsHTTP200HostQuotaMarkersAreIsolated(t *testing.T) {
	// The host's isOpenAIWSRateLimitError also recognizes these codes and
	// natural-language diagnostics, even without an HTTP 429 status.
	for _, signal := range []struct {
		name, code, message string
	}{
		{"insufficient_quota", "insufficient_quota", "PRIVATE quota diagnostic"},
		{"usage_limit_variant", "usage_limit_exceeded", "PRIVATE quota diagnostic"},
		{"usage_message", "service_error", "PRIVATE: The usage limit has been reached"},
		{"rate_message_exceeded", "service_error", "PRIVATE: Rate limit has been exceeded"},
		{"rate_message_reached", "service_error", "PRIVATE: Rate limit reached"},
		{"rate_code", "rate_limit_exceeded", "PRIVATE quota diagnostic"},
	} {
		for _, wire := range []string{"json", "sse"} {
			for _, transform := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/transform=%t", signal.name, wire, transform), func(t *testing.T) {
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						calls.Add(1)
						setCodexQuotaHeaders(w.Header())
						failure := map[string]any{"id": "resp_quota", "status": "failed", "output": []any{}, "error": map[string]any{"type": "service_error", "code": signal.code, "message": signal.message}}
						if wire == "sse" {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.failed", "response": failure}))
							return
						}
						_, _ = w.Write(protocol.JSONBytes(failure))
					}))
					defer upstream.Close()
					tr := New()
					defer tr.Shutdown()
					applyConfig(t, tr, map[string]any{"responses_url": upstream.URL, "transform_responses": transform, "account_ids": []int64{7}})
					frames := requestFrames(t, "https://host.example.invalid/responses", token(t, "quota-markers"), nil, protocol.JSONBytes(map[string]any{"model": "gpt-6-astra", "input": "hi", "stream": wire == "sse"}))
					frames[0].GetStart().AccountId = 7
					result := runForward(t, tr, frames)
					if result.errFrame != nil || result.status != http.StatusOK || !result.ended || calls.Load() != 1 {
						t.Fatalf("wrapped quota response changed framing or replayed: calls=%d result=%+v", calls.Load(), result)
					}
					body := string(result.body)
					if !strings.Contains(body, "bps_service_rejected") || !strings.Contains(body, "invalid_request_error") || !strings.Contains(body, "HTTP 429") || strings.Contains(body, "PRIVATE") {
						t.Fatalf("host quota signal escaped isolation: %s", body)
					}
					if headerValue(result.headers, "X-Codex-Primary-Used-Percent") != "" || headerValue(result.headers, "Retry-After") != "" || tr.isBPSAccountDisabled(7, protocol.DefaultConfig()) {
						t.Fatal("quota signal exposed host limits or triggered a 403 restriction")
					}
				})
			}
		}
	}
}

func TestBasisPointsQuotaWordsInNormalOutputRemainUnchanged(t *testing.T) {
	const explanation = "insufficient_quota usage_limit_exceeded: The usage limit has been reached. Rate limit exceeded; HTTP 403 and 429 are error codes."
	for _, payload := range []map[string]any{
		{"type": "response.output_text.delta", "delta": explanation},
		{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": explanation}}}}},
		{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "function_call", "name": "explain_error", "arguments": string(protocol.JSONBytes(map[string]any{"message": explanation, "status_code": 403}))}}}},
	} {
		forbidden := 0
		observe := newBasisPointsStatusObserver(func() { forbidden++ })
		raw := protocol.JSONBytes(payload)
		isolated, changed := isolateBasisPointsFailureJSON(raw, protocol.StringValue(payload["type"]), observe)
		if changed || !bytes.Equal(isolated, raw) || forbidden != 0 {
			t.Fatalf("normal output containing quota words changed or disabled BPS: changed=%t forbidden=%d body=%s", changed, forbidden, isolated)
		}
	}
}
