package attachments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func detailTestInput(raw string) (map[string]any, []map[string]any, []string) {
	var items []any
	var images []map[string]any
	var expected []string
	for _, kind := range []string{"message", "function_call_output", "custom_tool_call_output"} {
		var parts []any
		for _, detail := range []any{nil, "auto", "low", "high"} {
			part := map[string]any{"type": "input_image", "image_url": raw, "detail": detail}
			parts = append(parts, part)
			images = append(images, part)
			want, _ := detail.(string)
			if want == "" {
				want = "auto"
			}
			expected = append(expected, want)
		}
		missing := map[string]any{"type": "input_image", "image_url": raw}
		parts = append(parts, missing)
		images = append(images, missing)
		expected = append(expected, "auto")
		item := map[string]any{"type": kind}
		if kind == "message" {
			item["role"], item["content"] = "user", parts
		} else {
			item["call_id"], item["output"] = "call_detail_"+kind, parts
		}
		items = append(items, item)
	}
	return map[string]any{"input": items}, images, expected
}

func TestImageDetailDefaultsForAllContainersAndCache(t *testing.T) {
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": "file-detail-test"})
	}))
	defer server.Close()
	u := New()
	raw := dataURL(testPNG(t), "image/png")
	for _, phase := range []string{"upload", "cache"} {
		t.Run(phase, func(t *testing.T) {
			source, parts, expected := detailTestInput(raw)
			changed, err := u.Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), source, "detail-scope")
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			for index, part := range parts {
				if part["detail"] != expected[index] {
					t.Errorf("image %d: got detail=%v, want %s", index, part["detail"], expected[index])
				}
				if index < 5 {
					if part["file_id"] != "file-detail-test" || part["image_url"] != nil {
						t.Errorf("message image %d was not uploaded", index)
					}
				} else if part["image_url"] != raw || part["file_id"] != nil {
					t.Errorf("tool screenshot %d was changed", index)
				}
			}
			if uploads.Load() != 1 {
				t.Fatalf("uploads=%d, want exactly one including cache reuse", uploads.Load())
			}
		})
	}
}

func TestImageDetailDefaultsCommittedOnlyAfterAllUploadsSucceed(t *testing.T) {
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if uploads.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": "file-first-detail"})
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer server.Close()
	data := testPNG(t)
	source, parts, _ := detailTestInput(dataURL(data, "image/png"))
	parts[4]["image_url"] = dataURL(append(append([]byte(nil), data...), 0), "image/png")
	before, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := New().Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), source, "detail-scope")
	var typed *Error
	if changed || !errors.As(err, &typed) || typed.StatusCode() != http.StatusUnprocessableEntity || uploads.Load() != 2 {
		t.Fatalf("changed=%v uploads=%d err=%v", changed, uploads.Load(), err)
	}
	after, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("failed batch changed image references or detail defaults")
	}
}
