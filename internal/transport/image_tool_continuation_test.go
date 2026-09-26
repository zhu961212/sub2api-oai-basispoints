package transport

import (
	"bytes"
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

// This local fixture models the official frontend wire shape, not a live schema.
func imageToolWireError(source map[string]any, imageURL string) string {
	items, _ := source["input"].([]any)
	for _, raw := range items {
		item := relayObject(raw)
		if item["type"] != "function_call_output" && item["type"] != "custom_tool_call_output" {
			continue
		}
		parts, _ := item["output"].([]any)
		for _, rawPart := range parts {
			part := relayObject(rawPart)
			if part["type"] != "input_image" {
				continue
			}
			if part["file_id"] != nil || part["image_url"] != imageURL {
				return "tool screenshot must retain its original inline image_url"
			}
			if part["detail"] == nil {
				return "tool screenshot detail must not be null or missing"
			}
		}
	}
	return ""
}

func imageToolWriteResponse(w http.ResponseWriter, response map[string]any, streaming bool) {
	if streaming {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.created", "response": map[string]any{"id": response["id"], "status": "in_progress", "output": []any{}}}))
		_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.completed", "response": response}))
		_, _ = io.WriteString(w, "data: [DONE]"+string([]byte{10, 10}))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func imageToolClientResponse(t *testing.T, result forwardResult, streaming bool) map[string]any {
	t.Helper()
	if result.errFrame != nil || result.status != http.StatusOK || !result.ended || result.received != int64(len(result.body)) {
		t.Fatalf("incomplete client response: status=%d error=%v ended=%t body=%s", result.status, result.errFrame, result.ended, result.body)
	}
	if !streaming {
		response, err := protocol.RawObject(result.body)
		if err != nil || response["status"] != "completed" {
			t.Fatalf("invalid JSON response: error=%v body=%s", err, result.body)
		}
		return response
	}
	var response map[string]any
	created, completed := 0, 0
	for _, event := range parsedStreamEvents(t, result) {
		switch event["type"] {
		case "response.created":
			created++
		case "response.completed":
			completed++
			response = relayObject(event["response"])
		case "response.failed", "error":
			t.Fatalf("client stream failed: %s", result.body)
		}
	}
	if created != 1 || completed != 1 || strings.Count(string(result.body), "data: [DONE]") != 1 || response["status"] != "completed" {
		t.Fatalf("invalid SSE lifecycle: %s", result.body)
	}
	return response
}

func imageToolCatalog(custom bool) ([]any, string, any) {
	if custom {
		return []any{map[string]any{"type": "custom", "name": "capture", "description": "Capture the edited document as an image."}}, "capture", "capture current document"
	}
	return []any{map[string]any{"type": "function", "name": "capture", "parameters": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}}, "capture", map[string]any{}
}

func TestImageToolContinuationTwoRounds(t *testing.T) {
	for _, custom := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			for _, nullDetail := range []bool{false, true} {
				t.Run(fmt.Sprintf("custom=%t/stream=%t/null_detail=%t", custom, streaming, nullDetail), func(t *testing.T) {
					imageBytes, imageURL := relayTestImage(t)
					const account, fileID, callID = "acct-image-continuation", "file-native-continuation", "call_image_capture"
					accessToken := token(t, account)
					tools, toolName, toolArgs := imageToolCatalog(custom)
					nativeCall := relayNativeCall(callID, toolName, toolArgs)
					var uploads, conversations atomic.Int32
					captured := make(chan map[string]any, 2)
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Header.Get("Authorization") != "Bearer "+accessToken || r.Header.Get("ChatGPT-Account-ID") != account {
							t.Error("attachment or conversation changed scheduled account")
							w.WriteHeader(http.StatusUnauthorized)
							return
						}
						if r.URL.Path == "/basispoints/api/attachments" {
							uploads.Add(1)
							relayTestUpload(t, w, r, imageBytes, fileID)
							return
						}
						raw, _ := io.ReadAll(r.Body)
						source, err := protocol.RawObject(raw)
						if err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						if message := imageToolWireError(source, imageURL); message != "" {
							http.Error(w, message, http.StatusUnprocessableEntity)
							return
						}
						captured <- source
						if conversations.Add(1) == 1 {
							imageToolWriteResponse(w, map[string]any{"id": "resp_image_first", "status": "completed", "output": []any{nativeCall}}, streaming)
						} else {
							response, _ := protocol.RawObject(relayTestCompletedResponse())
							imageToolWriteResponse(w, response, streaming)
						}
					}))
					defer upstream.Close()
					transport := New()
					defer transport.Shutdown()
					host := &fakeHost{tokenFor: map[int64]string{7: accessToken}}
					transport.host = host
					applyConfig(t, transport, map[string]any{"responses_url": upstream.URL + "/basispoints/api/responses"})
					user := map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Edit the document to match this image, then capture it."}, map[string]any{"type": "input_image", "image_url": imageURL}}}
					source := map[string]any{"model": "gpt-6-astra", "stream": streaming, "tools": tools, "input": []any{user}}
					headers := map[string]string{"conversation_id": t.Name()}
					first := runForward(t, transport, requestFrames(t, "https://unused.invalid/responses", "", headers, protocol.JSONBytes(source)))
					firstResponse := imageToolClientResponse(t, first, streaming)
					output, _ := firstResponse["output"].([]any)
					if len(output) != 1 {
						t.Fatalf("first response lost tool call: %#v", firstResponse)
					}
					clientCall := relayObject(output[0])
					callType, outputType := "function_call", "function_call_output"
					if custom {
						callType, outputType = "custom_tool_call", "custom_tool_call_output"
					}
					if clientCall["type"] != callType || clientCall["name"] != toolName || clientCall["call_id"] != callID {
						t.Fatalf("first response is not an executable client tool: %#v", clientCall)
					}
					screenshot := map[string]any{"type": "input_image", "image_url": imageURL}
					if nullDetail {
						screenshot["detail"] = nil
					}
					toolOutput := map[string]any{"type": outputType, "call_id": clientCall["call_id"], "output": []any{map[string]any{"type": "input_text", "text": "Edited document screenshot"}, screenshot}}
					source["input"] = []any{user, clientCall, toolOutput}
					second := runForward(t, transport, requestFrames(t, "https://unused.invalid/responses", "", headers, protocol.JSONBytes(source)))
					imageToolClientResponse(t, second, streaming)
					if !bytes.Contains(second.body, []byte("image received")) || uploads.Load() != 1 || conversations.Load() != 2 {
						t.Fatalf("continuation or attachment cache failed: uploads=%d conversations=%d body=%s", uploads.Load(), conversations.Load(), second.body)
					}
					if len(host.resolvedAccountIDs) != 2 || host.resolvedAccountIDs[0] != 7 || host.resolvedAccountIDs[1] != 7 {
						t.Fatalf("continuation changed account: %v", host.resolvedAccountIDs)
					}
					<-captured
					wire := <-captured
					calls, results, messageImages, toolImages := 0, 0, 0, 0
					for _, raw := range wire["input"].([]any) {
						item := relayObject(raw)
						if item["type"] == "function_call" && item["call_id"] == callID {
							calls++
							if item["name"] != "run_officejs" || item["arguments"] != nativeCall["arguments"] {
								t.Fatalf("continuation did not replay the original native call: %#v", item)
							}
						}
						if item["type"] == "function_call_output" && item["call_id"] == callID {
							results++
							for _, part := range item["output"].([]any) {
								image := relayObject(part)
								if image["type"] == "input_image" {
									toolImages++
									if image["image_url"] != imageURL || image["file_id"] != nil || image["detail"] != "auto" {
										t.Fatalf("invalid screenshot wire shape: %#v", image)
									}
								}
							}
						}
						if item["role"] == "user" {
							for _, part := range item["content"].([]any) {
								image := relayObject(part)
								if image["type"] == "input_image" {
									messageImages++
									if image["file_id"] != fileID || image["image_url"] != nil {
										t.Fatalf("message upload was not preserved beside the identical screenshot: %#v", image)
									}
								}
							}
						}
					}
					if calls != 1 || results != 1 || messageImages != 1 || toolImages != 1 {
						t.Fatalf("continuation lost call pairing or images: calls=%d results=%d message=%d tool=%d", calls, results, messageImages, toolImages)
					}
				})
			}
		}
	}
}

func TestImageToolOnlyScreenshotDoesNotUpload(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprintf("custom=%t", custom), func(t *testing.T) {
			_, imageURL := relayTestImage(t)
			var uploads, conversations atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/attachments" {
					uploads.Add(1)
					http.Error(w, "tool screenshots must not upload", http.StatusUnprocessableEntity)
					return
				}
				conversations.Add(1)
				raw, _ := io.ReadAll(r.Body)
				source, err := protocol.RawObject(raw)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if message := imageToolWireError(source, imageURL); message != "" {
					http.Error(w, message, http.StatusUnprocessableEntity)
					return
				}
				response, _ := protocol.RawObject(relayTestCompletedResponse())
				imageToolWriteResponse(w, response, false)
			}))
			defer upstream.Close()
			transport := New()
			defer transport.Shutdown()
			applyConfig(t, transport, map[string]any{"responses_url": upstream.URL + "/responses"})
			tools, toolName, toolArgs := imageToolCatalog(custom)
			call := map[string]any{"type": "function_call", "name": toolName, "call_id": "call_tool_only", "arguments": string(protocol.JSONBytes(toolArgs))}
			outputType := "function_call_output"
			if custom {
				call = map[string]any{"type": "custom_tool_call", "name": toolName, "call_id": "call_tool_only", "input": toolArgs}
				outputType = "custom_tool_call_output"
			}
			source := map[string]any{"model": "gpt-6-astra", "tools": tools, "input": []any{call, map[string]any{"type": outputType, "call_id": "call_tool_only", "output": []any{map[string]any{"type": "input_image", "image_url": imageURL, "detail": nil}}}}}
			result := runForward(t, transport, requestFrames(t, upstream.URL, token(t, "acct-tool-only"), nil, protocol.JSONBytes(source)))
			imageToolClientResponse(t, result, false)
			if uploads.Load() != 0 || conversations.Load() != 1 {
				t.Fatalf("tool-only screenshot used attachments: uploads=%d conversations=%d", uploads.Load(), conversations.Load())
			}
		})
	}
}

func TestImageToolStrictFixtureRejectsIncompatibleWireForms(t *testing.T) {
	_, imageURL := relayTestImage(t)
	for _, part := range []map[string]any{
		{"type": "input_image", "file_id": "file-uploaded-tool", "detail": "auto"},
		{"type": "input_image", "image_url": imageURL, "detail": nil},
	} {
		source := map[string]any{"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_fixture", "output": []any{part}}}}
		if imageToolWireError(source, imageURL) == "" {
			t.Fatalf("strict fixture accepted incompatible image: %#v", part)
		}
	}
}
