package transport

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// A native reference does not need upload or request rewriting, but failures
// still must not reflect its capability identifier into client diagnostics.
func TestNativeDocumentFailureDiagnosticsAcrossConversionModes(t *testing.T) {
	const fileID = "file-private_document_A1_2"
	for _, kind := range []string{"message", "function_call_output", "custom_tool_call_output"} {
		for _, mode := range []string{"http_error", "failed_json", "failed_sse", "captured_tool_failure"} {
			for _, rewrite := range []bool{false, true} {
				for _, transform := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/rewrite=%t/transform=%t", kind, mode, rewrite, transform), func(t *testing.T) {
						var uploads, requests atomic.Int32
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.URL.Path == "/attachments" {
								uploads.Add(1)
								w.WriteHeader(http.StatusInternalServerError)
								return
							}
							requests.Add(1)
							failure := map[string]any{"code": "document_parse_error", "message": "cannot read " + fileID}
							response := map[string]any{"id": "resp_document_failure", "status": "failed", "output": []any{}, "error": failure}
							if mode == "http_error" || mode == "captured_tool_failure" {
								if mode == "captured_tool_failure" {
									failure["code"] = "invalid_tool_call"
								}
								w.Header().Set("Content-Type", "application/json")
								w.WriteHeader(http.StatusUnprocessableEntity)
								_, _ = w.Write(protocol.JSONBytes(map[string]any{"error": failure}))
							} else if mode == "failed_sse" {
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = fmt.Fprintf(w, "event: response.failed\ndata: %s\n\ndata: [DONE]\n\n", protocol.JSONBytes(map[string]any{"type": "response.failed", "response": response}))
							} else {
								w.Header().Set("Content-Type", "application/json")
								_, _ = w.Write(protocol.JSONBytes(response))
							}
						}))
						defer upstream.Close()
						tr := New()
						defer tr.Shutdown()
						applyConfig(t, tr, map[string]any{"responses_url": upstream.URL + "/responses", "rewrite_tools": rewrite, "transform_responses": transform})
						source := documentForwardSource(map[string]any{"type": "input_file", "file_id": fileID}, kind, mode == "failed_sse" || mode == "captured_tool_failure")
						result := runForward(t, tr, requestFrames(t, upstream.URL, token(t, "document-failure-account"), nil, protocol.JSONBytes(source)))
						wantStatus := http.StatusOK
						if mode == "http_error" {
							wantStatus = http.StatusUnprocessableEntity
						}
						if result.errFrame != nil || result.status != wantStatus || !result.ended || uploads.Load() != 0 || requests.Load() != 1 {
							t.Fatalf("native document failure routing changed: status=%d err=%v ended=%t uploads=%d requests=%d body=%s", result.status, result.errFrame, result.ended, uploads.Load(), requests.Load(), result.body)
						}
						body := string(result.body)
						if strings.Contains(body, fileID) || strings.Contains(body, "response.completed") {
							t.Fatalf("document failure leaked a file ID or became successful: %s", body)
						}
						if mode == "captured_tool_failure" {
							if !strings.Contains(body, "invalid_tool_call") || !strings.Contains(body, "failed") {
								t.Fatalf("captured document failure lost its safe error: %s", body)
							}
						} else if !strings.Contains(body, "document_parse_error") || !strings.Contains(body, "file-[redacted]") {
							t.Fatalf("document failure lost its safe diagnostic: %s", body)
						}
					})
				}
			}
		}
	}
}
