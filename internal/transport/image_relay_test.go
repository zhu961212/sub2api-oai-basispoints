package transport

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func relayTestImage(t *testing.T) ([]byte, string) {
	t.Helper()
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 2, 1))); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes(), "data:image/png;base64," + base64.StdEncoding.EncodeToString(buffer.Bytes())
}

func relayTestBody(imageURL string) []byte {
	return protocol.JSONBytes(map[string]any{"model": "gpt-6-astra", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": imageURL}}}}})
}

func relayTestFileIDs(source map[string]any) []string {
	var found []string
	input, _ := source["input"].([]any)
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		for _, field := range []string{"content", "output"} {
			parts, _ := item[field].([]any)
			for _, rawPart := range parts {
				part, _ := rawPart.(map[string]any)
				if part["type"] == "input_image" {
					value, _ := part["file_id"].(string)
					found = append(found, value)
				}
			}
		}
	}
	return found
}

func relayTestUpload(t *testing.T, w http.ResponseWriter, r *http.Request, expected []byte, fileID string) {
	t.Helper()
	if r.Method != http.MethodPost {
		t.Errorf("attachment method=%s", r.Method)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		t.Errorf("invalid multipart attachment: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, header, err := r.FormFile("file")
	if err != nil {
		t.Errorf("missing attachment file: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil || !bytes.Equal(data, expected) || header.Header.Get("Content-Type") != "image/png" || len(r.MultipartForm.File["file"]) != 1 || len(r.MultipartForm.File) != 1 || len(r.MultipartForm.Value) != 0 {
		t.Error("attachment changed original PNG bytes, MIME, or file-only multipart shape")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": fileID})
}

func relayTestCompletedResponse() []byte {
	return protocol.JSONBytes(map[string]any{"id": "resp_image", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "image received"}}}}})
}

func TestImageRelayForwardMatrix(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol"} {
		t.Run(model, func(t *testing.T) { testImageRelayForwardMatrix(t, model) })
	}
}

func testImageRelayForwardMatrix(t *testing.T, model string) {
	t.Helper()
	for _, rewrite := range []bool{false, true} {
		for _, transform := range []bool{false, true} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("rewrite=%t/transform=%t/stream=%t", rewrite, transform, streaming), func(t *testing.T) {
					imageBytes, inlineImage := relayTestImage(t)
					var requestBody []byte
					var requestHeaders http.Header
					var requestPath string
					var attachmentHeaders http.Header
					var uploads atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/basispoints/api/attachments" {
							uploads.Add(1)
							attachmentHeaders = r.Header.Clone()
							relayTestUpload(t, w, r, imageBytes, "file-native-matrix")
							return
						}
						requestBody, _ = io.ReadAll(r.Body)
						requestHeaders, requestPath = r.Header.Clone(), r.URL.Path
						response := relayTestCompletedResponse()
						if streaming {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\ndata: [DONE]\n\n", protocol.JSONBytes(map[string]any{"type": "response.completed", "response": json.RawMessage(response)}))
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write(response)
						}
					}))
					defer upstream.Close()
					var proxyCalls atomic.Int32
					accountProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						proxyCalls.Add(1)
						outbound := r.Clone(r.Context())
						outbound.RequestURI = ""
						response, err := http.DefaultTransport.RoundTrip(outbound)
						if err != nil {
							http.Error(w, "test proxy failed", http.StatusBadGateway)
							return
						}
						defer response.Body.Close()
						for key, values := range response.Header {
							w.Header()[key] = values
						}
						w.WriteHeader(response.StatusCode)
						_, _ = io.Copy(w, response.Body)
					}))
					defer accountProxy.Close()
					transport := New()
					defer transport.Shutdown()
					accessToken := token(t, "acct-scheduled")
					host := &fakeHost{tokenFor: map[int64]string{7: accessToken}, proxyURLFor: map[int64]string{7: accountProxy.URL}}
					transport.host = host
					cfg := map[string]any{"responses_url": upstream.URL + "/basispoints/api/responses"}
					cfg["rewrite_tools"], cfg["transform_responses"] = rewrite, transform
					applyConfig(t, transport, cfg)
					arguments := string(protocol.JSONBytes(map[string]any{"id": json.Number("9007199254740993"), "image_url": inlineImage}))
					imagePart := func() map[string]any {
						return map[string]any{"type": "input_image", "image_url": inlineImage, "detail": "high"}
					}
					source := map[string]any{"model": model, "stream": streaming, "input": []any{
						map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "before " + inlineImage}, imagePart(), map[string]any{"type": "input_text", "text": "after", "large_integer": json.Number("9007199254740993")}}},
						map[string]any{"type": "function_call_output", "call_id": "call_function", "output": []any{map[string]any{"type": "input_text", "text": "function image"}, imagePart()}},
						map[string]any{"type": "custom_tool_call_output", "call_id": "call_custom", "output": []any{imagePart(), map[string]any{"type": "input_text", "text": "custom image"}}},
						map[string]any{"type": "function_call", "call_id": "call_arguments", "name": "inspect", "arguments": arguments},
					}}
					result := runForward(t, transport, requestFrames(t, "https://unused.invalid/responses", "", map[string]string{"X-Codex-Session-Id": t.Name()}, protocol.JSONBytes(source)))
					if result.errFrame != nil || result.status != http.StatusOK || !result.ended || !bytes.Contains(result.body, []byte("image received")) {
						t.Fatalf("forward failed: status=%d error=%v body=%s", result.status, result.errFrame, result.body)
					}
					if proxyCalls.Load() != 2 || uploads.Load() != 1 || len(host.resolvedAccountIDs) != 1 || host.resolvedAccountIDs[0] != 7 {
						t.Fatalf("account/proxy changed: calls=%d ids=%v", proxyCalls.Load(), host.resolvedAccountIDs)
					}
					if requestPath != "/basispoints/api/responses" || requestHeaders.Get("Authorization") != "Bearer "+accessToken || requestHeaders.Get("ChatGPT-Account-ID") != "acct-scheduled" {
						t.Fatal("BPS route or scheduled account changed")
					}
					if attachmentHeaders.Get("Authorization") != requestHeaders.Get("Authorization") || attachmentHeaders.Get("ChatGPT-Account-ID") != requestHeaders.Get("ChatGPT-Account-ID") {
						t.Fatal("attachment upload did not use the same scheduled account")
					}
					seen, err := protocol.RawObject(requestBody)
					if err != nil {
						t.Fatal(err)
					}
					if seen["model"] != model || seen["__bps_session_scope"] != nil {
						t.Fatal("model or local scope changed on wire")
					}
					ids := relayTestFileIDs(seen)
					if len(ids) != 3 {
						t.Fatalf("image count=%d; request=%s", len(ids), requestBody)
					}
					if ids[0] != "file-native-matrix" || ids[1] != "" || ids[2] != "" {
						t.Fatalf("message/tool image wire forms changed: file IDs=%v", ids)
					}
					var user, call map[string]any
					for _, raw := range seen["input"].([]any) {
						item := raw.(map[string]any)
						if item["role"] == "user" {
							user = item
						}
						if item["call_id"] == "call_arguments" {
							call = item
						}
						if item["call_id"] == "call_function" || item["call_id"] == "call_custom" {
							for _, rawPart := range item["output"].([]any) {
								part := rawPart.(map[string]any)
								if part["type"] == "input_image" && (part["image_url"] != inlineImage || part["file_id"] != nil || part["detail"] != "high") {
									t.Fatalf("tool screenshot changed on wire: %#v", part)
								}
							}
						}
					}
					parts := user["content"].([]any)
					if len(parts) != 3 || parts[0].(map[string]any)["text"] != "before "+inlineImage || parts[2].(map[string]any)["text"] != "after" || parts[1].(map[string]any)["detail"] != "high" {
						t.Fatal("neighboring text, ordering, or image detail changed")
					}
					if parts[2].(map[string]any)["large_integer"] != json.Number("9007199254740993") {
						t.Fatal("large integer lost precision")
					}
					if rewrite {
						outer, err := protocol.RawObject([]byte(call["arguments"].(string)))
						if err != nil {
							t.Fatal(err)
						}
						inner, err := protocol.RawObject([]byte(outer["code"].(string)))
						if err != nil {
							t.Fatal(err)
						}
						args := inner["args"].(map[string]any)
						if args["image_url"] != inlineImage || args["id"] != json.Number("9007199254740993") {
							t.Fatal("tool arguments rewritten as image content or lost precision")
						}
					} else if call["arguments"] != arguments {
						t.Fatal("tool arguments changed with rewriting disabled")
					}
				})
			}
		}
	}
}

func TestImageRelayRejectsInvalidImagesBeforeUpstream(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
	}{
		{"invalid_base64", "data:image/png;base64,PRIVATE_BAD_%%%"}, {"non_image", "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("PRIVATE_NOT_IMAGE"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
			defer upstream.Close()
			transport := New()
			defer transport.Shutdown()
			cfg := map[string]any{"responses_url": upstream.URL + "/responses"}
			applyConfig(t, transport, cfg)
			result := runForward(t, transport, requestFrames(t, upstream.URL, token(t, "acct-test"), nil, relayTestBody(test.data)))
			if result.status != http.StatusBadRequest || result.errFrame != nil || calls.Load() != 0 {
				t.Fatalf("invalid image reached upstream: status=%d calls=%d body=%s", result.status, calls.Load(), result.body)
			}
			if strings.Contains(string(result.body), "PRIVATE") || strings.Contains(string(result.body), test.data) {
				t.Fatalf("image data leaked: %s", result.body)
			}
		})
	}
}

func TestImageRelayLeavesPassthroughAccountsAndModelsUntouched(t *testing.T) {
	for _, test := range []struct {
		name, model string
		ids         []int64
	}{{"unselected_account", "gpt-6-astra", []int64{99}}, {"other_model", "gpt-5.4", nil}} {
		t.Run(test.name, func(t *testing.T) {
			var received []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte("transparent"))
			}))
			defer upstream.Close()
			transport := New()
			defer transport.Shutdown()
			cfg := map[string]any{"responses_url": "http://must-not-call.invalid/responses"}
			cfg["account_ids"] = test.ids
			applyConfig(t, transport, cfg)
			body := bytes.Replace(relayTestBody("data:image/png;base64,PRIVATE_INVALID"), []byte("gpt-6-astra"), []byte(test.model), 1)
			result := runForward(t, transport, requestFrames(t, upstream.URL, token(t, "passthrough"), nil, body))
			if result.status != http.StatusAccepted || result.errFrame != nil || !bytes.Equal(received, body) || string(result.body) != "transparent" {
				t.Fatalf("passthrough changed: result=%+v body=%s", result, received)
			}
		})
	}
}

func TestImageRelayUploadFailureStopsConversation(t *testing.T) {
	_, inlineImage := relayTestImage(t)
	for _, tt := range []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"authentication", http.StatusUnauthorized, "PRIVATE_AUTH_BODY file-private-token", http.StatusUnauthorized},
		{"rate_limit", http.StatusTooManyRequests, "PRIVATE_LIMIT_BODY", http.StatusTooManyRequests},
		{"upstream_failure", http.StatusServiceUnavailable, "PRIVATE_UPSTREAM_BODY", http.StatusServiceUnavailable},
		{"missing_file_id", http.StatusOK, string(protocol.JSONBytes(map[string]any{"secret": "PRIVATE_RESPONSE"})), http.StatusBadGateway},
		{"malformed_file_id", http.StatusOK, string(protocol.JSONBytes(map[string]any{"openai_file_id": "https://private.example.test/token"})), http.StatusBadGateway},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var uploads, conversations atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/basispoints/api/attachments" {
					uploads.Add(1)
					w.WriteHeader(tt.status)
					_, _ = io.WriteString(w, tt.body)
					return
				}
				conversations.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			transport := New()
			defer transport.Shutdown()
			applyConfig(t, transport, map[string]any{"responses_url": upstream.URL + "/basispoints/api/responses", "rewrite_tools": false, "transform_responses": false})
			result := runForward(t, transport, requestFrames(t, upstream.URL, token(t, "acct-upload-failure"), nil, relayTestBody(inlineImage)))
			if tt.status == http.StatusTooManyRequests {
				assertBasisPointsRateLimit(t, result)
				if uploads.Load() != 1 || conversations.Load() != 0 {
					t.Fatalf("upload rate limit was retried: uploads=%d conversations=%d", uploads.Load(), conversations.Load())
				}
				return
			}
			if result.errFrame != nil || result.status != tt.want || !result.ended || uploads.Load() != 1 || conversations.Load() != 0 {
				t.Fatalf("upload failure was not surfaced safely: result=%+v uploads=%d conversations=%d", result, uploads.Load(), conversations.Load())
			}
			if bytes.Contains(result.body, []byte("PRIVATE")) || bytes.Contains(result.body, []byte("file-private-token")) || bytes.Contains(result.body, []byte("private.example.test")) || bytes.Contains(result.body, []byte(inlineImage)) {
				t.Fatalf("attachment failure exposed private content: %s", result.body)
			}
		})
	}
}

func TestImageRelayExistingReferencesNeedNoUpload(t *testing.T) {
	var uploads atomic.Int32
	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/attachments" {
			uploads.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(relayTestCompletedResponse())
	}))
	defer upstream.Close()
	transport := New()
	defer transport.Shutdown()
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL + "/responses", "rewrite_tools": false, "transform_responses": false})
	parts := []any{map[string]any{"type": "input_image", "file_id": "file-existing", "detail": "low"},
		map[string]any{"type": "input_image", "image_url": "https://images.example.test/already-hosted.png", "detail": "high"}}
	body := protocol.JSONBytes(map[string]any{"model": "gpt-6-astra", "input": []any{map[string]any{"role": "user", "content": parts}}})
	result := runForward(t, transport, requestFrames(t, upstream.URL, token(t, "acct-existing"), nil, body))
	if result.errFrame != nil || result.status != http.StatusOK || uploads.Load() != 0 || !bytes.Equal(received, body) {
		t.Fatalf("existing image references changed or uploaded: result=%+v uploads=%d body=%s", result, uploads.Load(), received)
	}
}

func TestAutomaticAttachmentsSurviveConfigChanges(t *testing.T) {
	imageBytes, inlineImage := relayTestImage(t)
	var uploads atomic.Int32
	var capturedID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/attachments" {
			uploads.Add(1)
			relayTestUpload(t, w, r, imageBytes, "file-native-lifecycle")
			return
		}
		body, _ := io.ReadAll(r.Body)
		source, _ := protocol.RawObject(body)
		ids := relayTestFileIDs(source)
		if len(ids) > 0 {
			capturedID = ids[0]
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(relayTestCompletedResponse())
	}))
	defer upstream.Close()
	transport := New()
	defer transport.Shutdown()
	cfg := map[string]any{"responses_url": upstream.URL + "/responses"}
	forward := func() {
		t.Helper()
		result := runForward(t, transport, requestFrames(t, upstream.URL, token(t, "acct-lifecycle"), map[string]string{"conversation_id": "host-isolated-key-lifecycle-session"}, relayTestBody(inlineImage)))
		if result.errFrame != nil || result.status != http.StatusOK || capturedID != "file-native-lifecycle" {
			t.Fatalf("automatic image forward failed: %+v %s", result, result.body)
		}
	}
	applyConfig(t, transport, cfg)
	forward()
	applyConfig(t, transport, cfg)
	forward()
	if uploads.Load() != 1 {
		t.Fatalf("unchanged configuration lost attachment deduplication: uploads=%d", uploads.Load())
	}
	cfg["enabled_models"] = protocol.AvailableModels()
	applyConfig(t, transport, cfg)
	forward()
	cfg["enabled_models"] = []string{"gpt-6-astra"}
	applyConfig(t, transport, cfg)
	forward()
	if uploads.Load() != 1 {
		t.Fatalf("model selection changes lost attachment deduplication: uploads=%d", uploads.Load())
	}
}
