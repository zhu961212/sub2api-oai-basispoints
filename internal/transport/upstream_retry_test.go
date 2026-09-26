package transport

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/config"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestForwardRetriesTemporaryHTTPFailures(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		for _, withImage := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%d/image=%t", status, withImage), func(t *testing.T) {
				imageBytes, imageURL := relayTestImage(t)
				accessToken := token(t, "retry-account")
				var attempts, uploads atomic.Int32
				var firstBody []byte
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/basispoints/api/attachments" {
						uploads.Add(1)
						relayTestUpload(t, w, r, imageBytes, "file-retry-image")
						return
					}
					body, _ := io.ReadAll(r.Body)
					attempt := attempts.Add(1)
					if attempt == 1 {
						firstBody = body
					} else if !bytes.Equal(body, firstBody) {
						t.Error("retry changed the prepared request body")
					}
					if r.Header.Get("Authorization") != "Bearer "+accessToken || r.Header.Get("ChatGPT-Account-ID") != "retry-account" {
						t.Error("retry changed account credentials")
					}
					if attempt < 3 {
						w.Header().Set("Retry-After", "0")
						http.Error(w, "temporary upstream outage", status)
						return
					}
					response, _ := protocol.RawObject(relayTestCompletedResponse())
					imageToolWriteResponse(w, response, true)
				}))
				defer upstream.Close()
				transport := New()
				defer transport.Shutdown()
				applyConfig(t, transport, map[string]any{"responses_url": upstream.URL + "/basispoints/api/responses"})
				parts := []any{map[string]any{"type": "input_text", "text": "Create an HTML document."}}
				if withImage {
					parts = append(parts, map[string]any{"type": "input_image", "image_url": imageURL})
				}
				request := protocol.JSONBytes(map[string]any{"model": config.DefaultModelID, "stream": true, "input": []any{map[string]any{"role": "user", "content": parts}}})
				result := runForward(t, transport, requestFrames(t, upstream.URL, accessToken, nil, request))
				imageToolClientResponse(t, result, true)
				if attempts.Load() != 3 {
					t.Fatalf("attempts = %d, want 3", attempts.Load())
				}
				expectedUploads := int32(0)
				if withImage {
					expectedUploads = 1
				}
				if uploads.Load() != expectedUploads {
					t.Fatalf("uploads = %d, want %d", uploads.Load(), expectedUploads)
				}
			})
		}
	}
}
