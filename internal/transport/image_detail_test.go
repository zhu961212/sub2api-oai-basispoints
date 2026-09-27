package transport

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestOriginalImageDetailForwardMatrix(t *testing.T) {
	for _, kind := range []string{"message", "function_call_output", "custom_tool_call_output"} {
		for _, reference := range []string{"inline", "https", "file_id"} {
			for _, rewrite := range []bool{false, true} {
				for _, transform := range []bool{false, true} {
					for _, streaming := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/rewrite=%t/transform=%t/stream=%t", kind, reference, rewrite, transform, streaming), func(t *testing.T) {
							imageBytes, inlineURL := relayTestImage(t)
							const uploadedID = "file-native-original-detail"
							image := map[string]any{"type": "input_image", "detail": "original"}
							switch reference {
							case "inline":
								image["image_url"] = inlineURL
							case "https":
								image["image_url"] = "https://images.example/Original.PNG?token=Exact%2FBytes"
							case "file_id":
								image["file_id"] = "file-existing-original-detail"
							}
							field := "output"
							item := map[string]any{"type": kind, "call_id": "call_original_detail"}
							if kind == "message" {
								field = "content"
								item["role"] = "user"
								delete(item, "call_id")
							}
							item[field] = []any{map[string]any{"type": "input_text", "text": "before original"}, image, map[string]any{"type": "input_text", "text": "after original"}}
							source := map[string]any{"model": protocol.DefaultModelID, "stream": streaming, "input": []any{item}}
							before := protocol.JSONBytes(source)
							var uploads, responses atomic.Int32
							captured := make(chan map[string]any, 1)
							upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								if r.URL.Path == "/basispoints/api/attachments" {
									uploads.Add(1)
									relayTestUpload(t, w, r, imageBytes, uploadedID)
									return
								}
								responses.Add(1)
								raw, _ := io.ReadAll(r.Body)
								wire, err := protocol.RawObject(raw)
								if err != nil {
									t.Error(err)
									w.WriteHeader(http.StatusBadRequest)
									return
								}
								captured <- wire
								if protocol.HasOriginalImageDetail(wire) {
									http.Error(w, "original detail is not supported", http.StatusUnprocessableEntity)
									return
								}
								response, _ := protocol.RawObject(relayTestCompletedResponse())
								imageToolWriteResponse(w, response, streaming)
							}))
							defer upstream.Close()
							tr := New()
							defer tr.Shutdown()
							applyConfig(t, tr, map[string]any{"responses_url": upstream.URL + "/basispoints/api/responses", "rewrite_tools": rewrite, "transform_responses": transform})
							result := runForward(t, tr, requestFrames(t, upstream.URL, token(t, "acct-original-detail"), nil, before))
							imageToolClientResponse(t, result, streaming)
							wantUploads := int32(0)
							if kind == "message" && reference == "inline" {
								wantUploads = 1
							}
							if uploads.Load() != wantUploads || responses.Load() != 1 {
								t.Fatalf("unexpected upload or response count: uploads=%d responses=%d", uploads.Load(), responses.Load())
							}
							wire := <-captured
							var wireItem map[string]any
							for _, raw := range wire["input"].([]any) {
								candidate := relayObject(raw)
								if kind == "message" && candidate["role"] == "user" || kind != "message" && candidate["call_id"] == "call_original_detail" {
									wireItem = candidate
								}
							}
							parts, _ := wireItem[field].([]any)
							if len(parts) != 3 || relayObject(parts[0])["text"] != "before original" || relayObject(parts[2])["text"] != "after original" {
								t.Fatalf("normalization lost ordering, neighboring text, or typed content: %#v", wireItem)
							}
							gotImage := relayObject(parts[1])
							if gotImage["detail"] != "high" || gotImage["type"] != "input_image" {
								t.Fatalf("original detail was not normalized to high: %#v", gotImage)
							}
							if wantUploads == 1 {
								if gotImage["file_id"] != uploadedID || gotImage["image_url"] != nil {
									t.Fatal("user inline image did not use its uploaded file ID")
								}
							} else if gotImage["image_url"] != image["image_url"] || gotImage["file_id"] != image["file_id"] {
								t.Fatal("normalization changed image bytes, URL, or existing file ID")
							}
							if !bytes.Equal(protocol.JSONBytes(source), before) {
								t.Fatal("forwarding changed the client request fixture")
							}
						})
					}
				}
			}
		}
	}
}
