package attachments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func imageHistory(raw string, count int, kind string) (map[string]any, []map[string]any) {
	parts := make([]map[string]any, count)
	items := make([]any, count)
	for index := range parts {
		parts[index] = imagePart(raw)
		parts[index]["detail"] = nil
		if index == 0 {
			items[index] = message(parts[index])["input"].([]any)[0]
		} else {
			item := toolImageInput(kind, parts[index])
			item["call_id"] = fmt.Sprintf("call_history_%d", index)
			items[index] = item
		}
	}
	return map[string]any{"input": items}, parts
}

func TestGrowingImageHistoryReportsCumulativeLimits(t *testing.T) {
	for _, kind := range []string{"function_call_output", "custom_tool_call_output"} {
		for _, limit := range []string{"count", "bytes"} {
			t.Run(kind+"/"+limit, func(t *testing.T) {
				data := testPNG(t)
				lastAccepted := maxRequestImages
				want := fmt.Sprintf("received at least %d", maxRequestImages+1)
				if limit == "bytes" {
					padded := make([]byte, 11<<20)
					copy(padded, data)
					data = padded
					lastAccepted = 2
					want = "counted 33.00 MiB across 3 images"
				}
				raw := dataURL(data, "image/png")
				var uploads atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					uploads.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": "file-history-limit"})
				}))
				defer server.Close()
				uploader := New()
				for count := 1; count <= lastAccepted+1; count++ {
					// Each request replays the original user image and all prior
					// screenshots, as an expanded conversation does.
					source, parts := imageHistory(raw, count, kind)
					changed, err := uploader.Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), source, "history-scope")
					if count <= lastAccepted {
						if !changed || err != nil || uploads.Load() != 1 {
							t.Fatalf("round=%d changed=%t uploads=%d err=%v", count, changed, uploads.Load(), err)
						}
						if parts[0]["file_id"] != "file-history-limit" || parts[0]["image_url"] != nil {
							t.Fatal("replayed user image did not reuse its attachment")
						}
						for _, part := range parts[1:] {
							if part["image_url"] != raw || part["file_id"] != nil || part["detail"] != "auto" {
								t.Fatal("historical screenshot lost its inline reference or detail")
							}
						}
						continue
					}
					var typed *Error
					if changed || !errors.As(err, &typed) || typed.StatusCode() != http.StatusBadRequest || typed.Code() != "invalid_image" {
						t.Fatalf("history limit was not an explicit client error: changed=%t err=%v", changed, err)
					}
					for _, fragment := range []string{want, "Conversation history", "tool screenshots", "repeated images", "start a new conversation", fmt.Sprintf("path=input[%d].output[0]", count-1)} {
						if !strings.Contains(err.Error(), fragment) {
							t.Errorf("history diagnostic missing %q: %v", fragment, err)
						}
					}
					if strings.Contains(err.Error(), "data:image") || strings.Contains(err.Error(), "file-history") || strings.Contains(err.Error(), "PRIVATE") || uploads.Load() != 1 {
						t.Fatal("rejected history leaked private data or uploaded again")
					}
					for _, part := range parts {
						if part["image_url"] != raw || part["file_id"] != nil || part["detail"] != nil {
							t.Fatal("rejected history partially modified its images")
						}
					}
				}
			})
		}
	}
}
