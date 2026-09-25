package attachments

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRequestAndImageByteLimitsBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	data := make([]byte, 17<<20)
	copy(data, testPNG(t))
	raw := dataURL(data, "image/png")
	first, second := imagePart(raw), imagePart(raw)
	changed, err := New().Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), message(first, second), "scope")
	if changed || err == nil || !strings.Contains(err.Error(), "32 MiB") || calls.Load() != 0 || first["image_url"] == nil {
		t.Fatalf("combined byte limit missing: %v", err)
	}
	_, err = parseImage("data:image/png;base64," + strings.Repeat("A", base64.StdEncoding.EncodedLen(maxImageBytes)+4))
	if err == nil || !strings.Contains(err.Error(), "20 MiB") {
		t.Fatalf("single image limit missing: %v", err)
	}
}

func TestMalformedUploadResponseDoesNotLeakBody(t *testing.T) {
	tests := []any{nil, "file-with space", "https://PRIVATE/file", "PRIVATE_TOKEN", float64(123), strings.Repeat("file-", 100)}
	for _, id := range tests {
		t.Run(fmt.Sprintf("%T-%d", id, len(fmt.Sprint(id))), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": id, "secret": "PRIVATE_TOKEN"})
			}))
			defer server.Close()
			part := imagePart(dataURL(testPNG(t), "image/png"))
			changed, err := New().Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), message(part), "scope")
			var typed *Error
			if changed || !errors.As(err, &typed) || typed.StatusCode() != 502 || typed.Code() != "invalid_attachment_response" || strings.Contains(err.Error(), "PRIVATE") || part["image_url"] == nil {
				t.Fatalf("invalid upload response accepted or leaked: %v", err)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("A", maxResponseBytes+1))
	}))
	defer server.Close()
	_, err := New().Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), message(imagePart(dataURL(testPNG(t), "image/png"))), "scope")
	if err == nil {
		t.Fatal("unbounded upstream response accepted")
	}
}

func TestAttachmentURLRemainsInConfiguredDirectory(t *testing.T) {
	for input, want := range map[string]string{
		"https://bps.openai.com/basispoints/api/responses": "https://bps.openai.com/basispoints/api/attachments",
		"https://custom.example:8443/api/responses/":       "https://custom.example:8443/api/attachments",
		"http://127.0.0.1:1234/responses":                  "http://127.0.0.1:1234/attachments",
	} {
		got, err := attachmentURL(input)
		if err != nil || got != want {
			t.Errorf("endpoint %q: %q %v", input, got, err)
		}
	}
	for _, input := range []string{"https://user:PRIVATE@upstream.test/responses", "file:///PRIVATE", "https://upstream.test/responses?token=PRIVATE", "https://upstream.test/responses#PRIVATE", " https://upstream.test/responses", "https:///responses"} {
		_, err := attachmentURL(input)
		if err == nil || strings.Contains(err.Error(), "PRIVATE") {
			t.Errorf("unsafe endpoint accepted or leaked: %v", err)
		}
	}
}

func TestCacheBoundedAndUploadCapacityBounded(t *testing.T) {
	u := New()
	for i := 0; i < maxCacheEntries+20; i++ {
		var key [32]byte
		binary.BigEndian.PutUint32(key[:4], uint32(i))
		if _, err := u.getOrUpload(context.Background(), key, true, func() (string, error) { return "file-test", nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(u.cache) != maxCacheEntries || len(u.pending) != 0 {
		t.Fatalf("cache or pending metadata grew unbounded: %d %d", len(u.cache), len(u.pending))
	}
	for i := 0; i < maxUploads; i++ {
		u.uploads <- struct{}{}
	}
	_, err := u.Rewrite(context.Background(), http.DefaultClient, "https://uncontacted.invalid/responses", testHeaders(), message(imagePart(dataURL(testPNG(t), "image/png"))), "scope")
	var typed *Error
	if !errors.As(err, &typed) || typed.StatusCode() != 503 || typed.Code() != "attachment_busy" {
		t.Fatalf("upload capacity did not fail closed: %v", err)
	}
	if len(u.pending) != 0 {
		t.Fatal("rejected upload remained pending")
	}
}

func TestCredentialHeaderCasingAndDuplicateValuesCannotAliasCache(t *testing.T) {
	u := New()
	img, err := parseImage(dataURL(testPNG(t), "image/png"))
	if err != nil {
		t.Fatal(err)
	}
	key := func(headers http.Header) [32]byte {
		t.Helper()
		key, err := u.imageKey(context.Background(), "https://bps.test/attachments", "scope", headers, img)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	first := http.Header{"authorization": {"Bearer ONE"}, "chatgpt-account-id": {"account"}}
	second := http.Header{"authorization": {"Bearer TWO"}, "chatgpt-account-id": {"account"}}
	if key(first) == key(second) {
		t.Fatal("lowercase credentials aliased cache")
	}
	second = first.Clone()
	second["Authorization"] = []string{"Bearer TWO"}
	if key(first) == key(second) {
		t.Fatal("duplicate case credentials aliased cache")
	}
	second = first.Clone()
	second["authorization"] = append(second["authorization"], "Bearer TWO")
	if key(first) == key(second) {
		t.Fatal("multiple credential values aliased cache")
	}
}
