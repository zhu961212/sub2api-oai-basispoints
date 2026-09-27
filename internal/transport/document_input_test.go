package transport

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func documentForwardSource(part map[string]any, kind string, stream bool) map[string]any {
	parts := []any{map[string]any{"type": "input_text", "text": "read this document"}, part}
	item := map[string]any{"role": "user", "content": parts}
	if kind != "message" {
		item = map[string]any{"type": kind, "call_id": "call_document", "output": parts}
	}
	return map[string]any{"model": protocol.DefaultModelID, "stream": stream, "input": []any{item}}
}

func TestDocumentForwardMatrix(t *testing.T) {
	for _, kind := range []string{"message", "function_call_output", "custom_tool_call_output"} {
		for _, representation := range []string{"data_uri", "base64", "file_id"} {
			for _, rewrite := range []bool{false, true} {
				for _, transform := range []bool{false, true} {
					for _, streaming := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/rewrite=%t/transform=%t/stream=%t", kind, representation, rewrite, transform, streaming), func(t *testing.T) {
							data := []byte("%PDF-1.7 document-upload-fixture")
							file := map[string]any{"type": "input_file", "filename": "报告.pdf"}
							if representation == "file_id" {
								file["file_id"] = "file-document"
							} else {
								value := base64.StdEncoding.EncodeToString(data)
								if representation == "data_uri" {
									value = "data:application/pdf;base64," + value
								}
								file["file_data"] = value
							}
							var uploads, requests atomic.Int32
							accessToken := token(t, "document-account")
							upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								if r.Header.Get("Authorization") != "Bearer "+accessToken || r.Header.Get("ChatGPT-Account-ID") != "document-account" {
									t.Error("document request used a different account")
								}
								if r.URL.Path == "/attachments" {
									uploads.Add(1)
									if err := r.ParseMultipartForm(1 << 20); err != nil {
										t.Error(err)
										w.WriteHeader(400)
										return
									}
									defer r.MultipartForm.RemoveAll()
									reader, header, err := r.FormFile("file")
									if err != nil {
										t.Error(err)
										w.WriteHeader(400)
										return
									}
									defer reader.Close()
									got, err := io.ReadAll(reader)
									if err != nil || !bytes.Equal(got, data) || header.Filename != "报告.pdf" || header.Header.Get("Content-Type") != "application/pdf" || len(r.MultipartForm.Value) != 0 {
										t.Error("document bytes, filename or multipart shape changed")
									}
									_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": "file-document"})
									return
								}
								requests.Add(1)
								raw, _ := io.ReadAll(r.Body)
								seen, err := protocol.RawObject(raw)
								if err != nil {
									t.Error(err)
									w.WriteHeader(400)
									return
								}
								if seen["__bps_session_scope"] != nil || seen["__bps_context_cache_disabled"] != nil {
									t.Error("private context markers reached upstream")
								}
								found := 0
								for _, rawItem := range seen["input"].([]any) {
									item, _ := rawItem.(map[string]any)
									for _, field := range []string{"content", "output"} {
										parts, _ := item[field].([]any)
										for _, rawPart := range parts {
											part, _ := rawPart.(map[string]any)
											if part["type"] == "input_file" {
												found++
												if part["file_id"] != "file-document" || part["file_data"] != nil {
													t.Error("document was not converted to native file ID")
												}
											}
										}
									}
								}
								if found != 1 {
									t.Errorf("document count changed: %d", found)
								}
								response := map[string]any{"id": "resp_document", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "document received"}}}}}
								if streaming {
									w.Header().Set("Content-Type", "text/event-stream")
									_, _ = w.Write(protocol.SyntheticStream(response))
								} else {
									w.Header().Set("Content-Type", "application/json")
									_ = json.NewEncoder(w).Encode(response)
								}
							}))
							defer upstream.Close()
							transport := New()
							defer transport.Shutdown()
							applyConfig(t, transport, map[string]any{"responses_url": upstream.URL + "/responses", "rewrite_tools": rewrite, "transform_responses": transform})
							source := documentForwardSource(file, kind, streaming)
							result := runForward(t, transport, requestFrames(t, upstream.URL, accessToken, nil, protocol.JSONBytes(source)))
							wantUploads := int32(1)
							if representation == "file_id" {
								wantUploads = 0
							}
							if result.errFrame != nil || result.status != 200 || !result.ended || !strings.Contains(string(result.body), "document received") || uploads.Load() != wantUploads || requests.Load() != 1 {
								t.Fatalf("document forward failed: status=%d error=%v uploads=%d requests=%d body=%s", result.status, result.errFrame, uploads.Load(), requests.Load(), result.body)
							}
						})
					}
				}
			}
		}
	}
}

func TestDocumentInvalidInputNeverReachesUpstream(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		for _, transform := range []bool{false, true} {
			for name, fields := range map[string]map[string]any{
				"file URL":      {"file_url": "https://private.example/PRIVATE.pdf"},
				"base64":        {"file_data": "PRIVATE%%%%", "filename": "report.pdf"},
				"mixed sources": {"file_data": "PRIVATE", "file_id": "file-PRIVATE"},
				"signature":     {"file_data": base64.StdEncoding.EncodeToString([]byte("PRIVATE-not-pdf")), "filename": "report.pdf"},
			} {
				t.Run(fmt.Sprintf("%s/rewrite=%t/transform=%t", name, rewrite, transform), func(t *testing.T) {
					fields["type"] = "input_file"
					var hits atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(500) }))
					defer server.Close()
					transport := New()
					defer transport.Shutdown()
					applyConfig(t, transport, map[string]any{"responses_url": server.URL + "/responses", "rewrite_tools": rewrite, "transform_responses": transform})
					result := runForward(t, transport, requestFrames(t, server.URL, token(t, "test"), nil, protocol.JSONBytes(documentForwardSource(fields, "message", true))))
					if hits.Load() != 0 || result.status != 400 || result.errFrame != nil || !result.ended || strings.Contains(string(result.body), "PRIVATE") {
						t.Fatalf("invalid document boundary changed: status=%d error=%v hits=%d body=%s", result.status, result.errFrame, hits.Load(), result.body)
					}
				})
			}
		}
	}
}

func TestDocumentMixedInvalidFileDoesNotUploadImage(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		for _, transform := range []bool{false, true} {
			t.Run(fmt.Sprintf("rewrite=%t/transform=%t", rewrite, transform), func(t *testing.T) {
				_, imageURL := relayTestImage(t)
				var hits atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": "file-unexpected"})
				}))
				defer upstream.Close()
				transport := New()
				defer transport.Shutdown()
				applyConfig(t, transport, map[string]any{"responses_url": upstream.URL + "/responses", "rewrite_tools": rewrite, "transform_responses": transform})
				source := map[string]any{"model": protocol.DefaultModelID, "input": []any{map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "input_image", "image_url": imageURL},
					map[string]any{"type": "input_file", "filename": "invalid.pdf", "file_data": base64.StdEncoding.EncodeToString([]byte("PRIVATE-not-pdf"))},
				}}}}
				result := runForward(t, transport, requestFrames(t, upstream.URL, token(t, "account"), nil, protocol.JSONBytes(source)))
				if hits.Load() != 0 || result.status != 400 || result.errFrame != nil || strings.Contains(string(result.body), "PRIVATE") {
					t.Fatalf("invalid mixed batch uploaded bytes: hits=%d status=%d err=%v body=%s", hits.Load(), result.status, result.errFrame, result.body)
				}
			})
		}
	}
}
