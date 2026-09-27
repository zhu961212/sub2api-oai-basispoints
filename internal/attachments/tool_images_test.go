package attachments

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func toolImageInput(kind string, parts ...map[string]any) map[string]any {
	output := make([]any, len(parts))
	for index, part := range parts {
		output[index] = part
	}
	return map[string]any{"type": kind, "call_id": "call_screenshot", "output": output}
}

func TestToolOutputImagesStayInlineWithoutAttachmentTransport(t *testing.T) {
	raw := dataURL(testPNG(t), "image/png")
	for _, kind := range []string{"function_call_output", "custom_tool_call_output"} {
		t.Run(kind, func(t *testing.T) {
			missing, null, high := imagePart(raw), imagePart(raw), imagePart(raw)
			delete(missing, "detail")
			null["detail"] = nil
			text := map[string]any{"type": "input_text", "text": "original screenshot"}
			source := map[string]any{"input": []any{toolImageInput(kind, missing, text, null, high)}}
			// No uploader, HTTP client, or attachment endpoint is needed for tool
			// screenshots; the unchanged data URL is the upstream wire format.
			changed, err := (*Uploader)(nil).Rewrite(context.Background(), nil, "INVALID", nil, source, "")
			if !changed || err != nil {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			for index, part := range []map[string]any{missing, null, high} {
				want := "auto"
				if index == 2 {
					want = "high"
				}
				if part["image_url"] != raw || part["file_id"] != nil || part["detail"] != want {
					t.Fatalf("screenshot %d changed reference or detail", index)
				}
			}
			if text["text"] != "original screenshot" {
				t.Fatal("neighboring text changed")
			}
			// Already-normalized screenshots still report image processing so
			// callers serialize and retain the image-safe response handling.
			if changed, err := (*Uploader)(nil).Rewrite(context.Background(), nil, "INVALID", nil, source, ""); !changed || err != nil {
				t.Fatalf("normalized screenshot: changed=%v err=%v", changed, err)
			}
		})
	}
}

func TestSharedMessageAndToolImageUploadsRegardlessOfOrder(t *testing.T) {
	raw := dataURL(testPNG(t), "image/png")
	for _, kind := range []string{"function_call_output", "custom_tool_call_output"} {
		for _, first := range []string{"message", "tool"} {
			t.Run(kind+"/"+first, func(t *testing.T) {
				var uploads atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					uploads.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": "file-shared-message"})
				}))
				defer server.Close()
				u := New()
				for attempt := 0; attempt < 2; attempt++ {
					messageImage, repeatedMessage, toolImage := imagePart(raw), imagePart(raw), imagePart(raw)
					messageItem := message(messageImage, repeatedMessage)["input"].([]any)[0]
					toolItem := toolImageInput(kind, toolImage)
					items := []any{messageItem, toolItem}
					if first == "tool" {
						items[0], items[1] = items[1], items[0]
					}
					source := map[string]any{"input": items}
					changed, err := u.Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), source, "mixed-scope")
					if !changed || err != nil || uploads.Load() != 1 {
						t.Fatalf("attempt=%d changed=%v uploads=%d err=%v", attempt, changed, uploads.Load(), err)
					}
					for _, part := range []map[string]any{messageImage, repeatedMessage} {
						if part["file_id"] != "file-shared-message" || part["image_url"] != nil {
							t.Fatal("message did not use the native attachment")
						}
					}
					if toolImage["image_url"] != raw || toolImage["file_id"] != nil {
						t.Fatal("tool screenshot was uploaded or modified")
					}
				}
			})
		}
	}
}

func TestToolImageValidationBeforeMessageUploadAndCommit(t *testing.T) {
	data := testPNG(t)
	raw := dataURL(data, "image/png")
	huge := append([]byte(nil), data...)
	binary.BigEndian.PutUint32(huge[16:20], maxPixels+1)
	binary.BigEndian.PutUint32(huge[29:33], crc32.ChecksumIEEE(huge[12:29]))
	cases := map[string]string{
		"invalid_base64":  "data:image/png;base64,PRIVATE_BAD_%%%%",
		"wrong_mime":      dataURL(data, "image/jpeg"),
		"non_image":       dataURL([]byte("PRIVATE_NOT_IMAGE"), "image/png"),
		"too_many_pixels": dataURL(huge, "image/png"),
	}
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	for _, kind := range []string{"function_call_output", "custom_tool_call_output"} {
		for name, bad := range cases {
			t.Run(kind+"/"+name, func(t *testing.T) {
				first, screenshot := imagePart(raw), imagePart(bad)
				first["detail"], screenshot["detail"] = nil, nil
				source := message(first)
				source["input"] = append(source["input"].([]any), toolImageInput(kind, screenshot))
				before, _ := json.Marshal(source)
				changed, err := New().Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), source, "validation-scope")
				var typed *Error
				if changed || !errors.As(err, &typed) || typed.StatusCode() != http.StatusBadRequest || !strings.Contains(err.Error(), "path=input[1].output[0]") || strings.Contains(err.Error(), "PRIVATE") {
					t.Fatalf("unexpected validation result: changed=%v err=%v", changed, err)
				}
				after, _ := json.Marshal(source)
				if uploads.Load() != 0 || string(before) != string(after) {
					t.Fatal("invalid tool screenshot caused upload or request mutation")
				}
			})
		}
	}
}

func TestToolScreenshotsShareRequestResourceLimits(t *testing.T) {
	for _, kind := range []string{"function_call_output", "custom_tool_call_output"} {
		t.Run(kind+"/count", func(t *testing.T) {
			raw := dataURL(testPNG(t), "image/png")
			parts := make([]map[string]any, maxRequestImages+1)
			for index := range parts {
				parts[index] = imagePart(raw)
				parts[index]["detail"] = nil
			}
			source := map[string]any{"input": []any{toolImageInput(kind, parts...)}}
			changed, err := New().Rewrite(context.Background(), nil, "INVALID", nil, source, "")
			if changed || err == nil || !strings.Contains(err.Error(), "128 inline") || parts[0]["detail"] != nil {
				t.Fatalf("screenshot count budget: changed=%v err=%v", changed, err)
			}
		})
		t.Run(kind+"/bytes", func(t *testing.T) {
			data := make([]byte, 17<<20)
			copy(data, testPNG(t))
			raw := dataURL(data, "image/png")
			first, second := imagePart(raw), imagePart(raw)
			first["detail"], second["detail"] = nil, nil
			source := map[string]any{"input": []any{toolImageInput(kind, first, second)}}
			changed, err := New().Rewrite(context.Background(), nil, "INVALID", nil, source, "")
			if changed || err == nil || !strings.Contains(err.Error(), "32 MiB") || first["detail"] != nil || second["detail"] != nil {
				t.Fatalf("screenshot byte budget: changed=%v err=%v", changed, err)
			}
		})
	}
}

func TestCanceledToolScreenshotDoesNotModifySource(t *testing.T) {
	part := imagePart(dataURL(testPNG(t), "image/png"))
	part["detail"] = nil
	source := map[string]any{"input": []any{toolImageInput("function_call_output", part)}}
	before, _ := json.Marshal(source)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	changed, err := (*Uploader)(nil).Rewrite(ctx, nil, "INVALID", nil, source, "")
	after, _ := json.Marshal(source)
	if changed || !errors.Is(err, context.Canceled) || string(before) != string(after) {
		t.Fatalf("canceled screenshot: changed=%v err=%v", changed, err)
	}
}

func TestMixedMessageAndToolImagesShareRequestBudgets(t *testing.T) {
	for _, kind := range []string{"function_call_output", "custom_tool_call_output"} {
		for _, limit := range []string{"count", "bytes"} {
			t.Run(kind+"/"+limit, func(t *testing.T) {
				var uploads atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					uploads.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				}))
				defer server.Close()
				data := testPNG(t)
				count, want := maxRequestImages, "128 inline"
				if limit == "bytes" {
					padded := make([]byte, 17<<20)
					copy(padded, data)
					data, count, want = padded, 1, "32 MiB"
				}
				raw := dataURL(data, "image/png")
				messageImage := imagePart(raw)
				messageImage["detail"] = nil
				parts := make([]map[string]any, count)
				for index := range parts {
					parts[index] = imagePart(raw)
					parts[index]["detail"] = nil
				}
				source := message(messageImage)
				source["input"] = append(source["input"].([]any), toolImageInput(kind, parts...))
				changed, err := New().Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), source, "mixed-budget")
				if changed || err == nil || !strings.Contains(err.Error(), want) || uploads.Load() != 0 {
					t.Fatalf("shared %s budget: changed=%v uploads=%d err=%v", limit, changed, uploads.Load(), err)
				}
				for _, part := range append(parts, messageImage) {
					if part["image_url"] != raw || part["detail"] != nil || part["file_id"] != nil {
						t.Fatal("budget rejection modified an image")
					}
				}
			})
		}
	}
}
