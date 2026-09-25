package attachments

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnonymousImageTrafficDoesNotPolluteReusableCache(t *testing.T) {
	uploads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads++
		_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": fmt.Sprintf("file-test-%d", uploads)})
	}))
	defer server.Close()
	u := New()
	raw := dataURL(testPNG(t), "image/png")
	rewrite := func(scope string) {
		t.Helper()
		if _, err := u.Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), message(imagePart(raw), imagePart(strings.Clone(raw))), scope); err != nil {
			t.Fatal(err)
		}
	}
	rewrite("trusted")
	for i := 0; i < maxCacheEntries+1; i++ {
		rewrite("")
	}
	if len(u.cache) != 1 || len(u.pending) != 0 {
		t.Fatalf("anonymous cache pollution: cache=%d pending=%d", len(u.cache), len(u.pending))
	}
	rewrite("trusted")
	if uploads != maxCacheEntries+2 {
		t.Fatalf("trusted entry evicted or request duplicates reuploaded: %d", uploads)
	}
}

func TestWarmCacheCannotBypassBase64Validation(t *testing.T) {
	uploads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads++
		_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": "file-good"})
	}))
	defer server.Close()
	u := New()
	raw := dataURL(testPNG(t), "image/png")
	if _, err := u.Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), message(imagePart(raw)), "trusted"); err != nil {
		t.Fatal(err)
	}
	separator := strings.IndexByte(raw, ',') + 1
	for _, bad := range []string{" ", "\r", "\n", "\t", "=", "%"} {
		corrupt := raw[:separator+8] + bad + raw[separator+9:]
		source := message(imagePart(corrupt))
		if changed, err := u.Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), source, "trusted"); err == nil || changed {
			t.Fatalf("cached validation accepted malformed payload %q", bad)
		}
	}
	if uploads != 1 {
		t.Fatalf("malformed request reached upload: %d", uploads)
	}
}
