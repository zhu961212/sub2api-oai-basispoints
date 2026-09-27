package transport

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestUpstreamToolFailureRemainsRequestScopedAcrossFormats(t *testing.T) {
	const message = "Basis Points returned a malformed or ambiguous client tool relay envelope"
	for _, location := range []string{"nested", "outer", "direct"} {
		for _, transformed := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				for _, sse := range []bool{false, true} {
					for _, status := range []int{200, 502} {
						t.Run(fmt.Sprintf("%s/transform=%t/stream=%t/sse=%t/status=%d", location, transformed, stream, sse, status), func(t *testing.T) {
							failure := map[string]any{"code": "invalid_tool_call", "message": message}
							response := map[string]any{"id": "resp_existing_failure", "status": "failed", "output": []any{}}
							if status == http.StatusBadGateway {
								response["output"] = []any{relayNativeCall("call_with_failed_http", "functions.exec", "PRIVATE TOOL INPUT")}
							}
							payload := map[string]any{"type": "response.failed", "response": response}
							switch location {
							case "nested":
								response["error"] = failure
							case "outer":
								payload["error"] = failure
							case "direct":
								payload["code"], payload["message"] = failure["code"], failure["message"]
							}
							var attempts atomic.Int32
							upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								attempts.Add(1)
								// Test terminal handling independently of the existing
								// bounded retry policy for HTTP 5xx responses.
								w.Header().Set("X-Should-Retry", "false")
								_, _ = io.Copy(io.Discard, r.Body)
								if sse {
									w.Header().Set("Content-Type", "text/event-stream")
									w.WriteHeader(status)
									_, _ = io.WriteString(w, streamData(payload))
								} else {
									w.Header().Set("Content-Type", "application/json")
									w.WriteHeader(status)
									_, _ = w.Write(protocol.JSONBytes(payload))
								}
							}))
							defer upstream.Close()
							tr := New()
							defer tr.Shutdown()
							applyConfig(t, tr, map[string]any{"responses_url": upstream.URL, "transform_responses": transformed})
							source := executorOnlyCatalogSource(t.Name())
							source["stream"] = stream
							result := runForward(t, tr, requestFrames(t, "https://host.invalid/v1/responses", token(t, "acct-existing-failure"), nil, protocol.JSONBytes(source)))
							if attempts.Load() != 1 {
								t.Fatalf("failed upstream terminal was retried %d times", attempts.Load())
							}
							if status == http.StatusBadGateway {
								assertCanonicalHTTPToolFailure(t, result, stream, message)
							}
							if result.errFrame != nil {
								if result.errFrame.GetCode() != "invalid_tool_call" || result.errFrame.GetMessage() != message || !result.errFrame.GetRequestSent() {
									t.Fatalf("buffered failure lost safe diagnostic: %+v", result)
								}
								return
							}
							if !result.ended {
								t.Fatalf("failure stream did not end: %+v", result)
							}
							var failures []map[string]any
							if strings.Contains(string(result.body), "data:") {
								for _, event := range parsedStreamEvents(t, result) {
									failures = append(failures, relayObject(event["error"]), relayObject(relayObject(event["response"])["error"]))
								}
							} else {
								object, err := protocol.RawObject(result.body)
								if err != nil {
									t.Fatal(err)
								}
								failures = append(failures, relayObject(object["error"]), relayObject(relayObject(object["response"])["error"]))
							}
							found := false
							for _, diagnostic := range failures {
								if diagnostic == nil {
									continue
								}
								if diagnostic["code"] != "invalid_tool_call" || diagnostic["message"] != message || diagnostic["type"] != "invalid_request_error" {
									t.Fatalf("request scope or original error lost: %#v", diagnostic)
								}
								found = true
							}
							if !found {
								t.Fatalf("no canonical tool failure: %s", result.body)
							}
						})
					}
				}
			}
		}
	}
}
func assertCanonicalHTTPToolFailure(t *testing.T, result forwardResult, stream bool, message string) {
	t.Helper()
	if result.errFrame != nil || result.status != http.StatusOK || !result.ended {
		t.Fatalf("HTTP failure did not enter host request-scoped terminal path: status=%d rpc=%v ended=%t", result.status, result.errFrame, result.ended)
	}
	wantType := "application/json"
	if stream {
		wantType = "text/event-stream"
	}
	if !strings.Contains(strings.ToLower(headerValue(result.headers, "Content-Type")), wantType) {
		t.Fatalf("client format=%q, want %s", headerValue(result.headers, "Content-Type"), wantType)
	}
	if strings.Contains(string(result.body), "PRIVATE TOOL INPUT") || strings.Contains(string(result.body), "run_officejs") || strings.Contains(string(result.body), "response.completed") {
		t.Fatalf("failed HTTP response exposed a tool or completion: %s", result.body)
	}
	var response map[string]any
	if stream {
		terminals := 0
		for _, event := range parsedStreamEvents(t, result) {
			if event["type"] == "response.failed" {
				terminals++
				response = relayObject(event["response"])
			}
			if output, _ := relayObject(event["response"])["output"].([]any); len(output) != 0 {
				t.Fatal("failed SSE exposed output items")
			}
		}
		if terminals != 1 || strings.Count(string(result.body), "data: [DONE]") != 1 {
			t.Fatalf("failed SSE did not terminate exactly once: %s", result.body)
		}
		assertNoToolExecutionOnStreamError(t, result)
	} else {
		var err error
		response, err = protocol.RawObject(result.body)
		if err != nil {
			t.Fatal(err)
		}
	}
	if response["status"] != "failed" {
		t.Fatalf("missing canonical failed response: %#v", response)
	}
	output, ok := response["output"].([]any)
	if !ok || len(output) != 0 {
		t.Fatalf("failed response must contain empty output: %#v", response)
	}
	failure := relayObject(response["error"])
	if failure["code"] != "invalid_tool_call" || failure["type"] != "invalid_request_error" || failure["message"] != message {
		t.Fatalf("canonical tool diagnostic changed: %#v", failure)
	}
	if result.received != int64(len(result.body)) {
		t.Fatal("HTTP failure end byte count changed")
	}
}
