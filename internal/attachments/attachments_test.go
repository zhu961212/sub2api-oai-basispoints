package attachments

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func dataURL(data []byte, mime string) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}
func imagePart(data string) map[string]any {
	return map[string]any{"type": "input_image", "image_url": data, "detail": "high"}
}
func message(parts ...map[string]any) map[string]any {
	content := make([]any, len(parts))
	for i, part := range parts {
		content[i] = part
	}
	return map[string]any{"input": []any{map[string]any{"role": "user", "content": content}}}
}
func testHeaders() http.Header {
	h := make(http.Header)
	h.Set("Authorization", "Bearer PRIVATE_TEST_TOKEN")
	h.Set("ChatGPT-Account-ID", "PRIVATE_TEST_ACCOUNT")
	h.Set("X-OpenAI-Account-ID", "PRIVATE_TEST_ACCOUNT")
	h.Set("X-Basispoints-Auth-Mode", "chatgpt")
	return h
}

func TestNativeUploadPreservesBytesAndKeepsToolImagesInline(t *testing.T) {
	data := testPNG(t)
	raw := dataURL(data, "image/png")
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/basispoints/api/attachments" {
			t.Errorf("unexpected upload method/path: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != testHeaders().Get("Authorization") || r.Header.Get("ChatGPT-Account-ID") != testHeaders().Get("ChatGPT-Account-ID") {
			t.Error("account identity changed")
		}
		if r.ContentLength <= int64(len(data)) || len(r.TransferEncoding) != 0 {
			t.Error("multipart length was not provided")
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		part, err := reader.NextPart()
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		got, err := io.ReadAll(part)
		if err != nil || !bytes.Equal(got, data) || part.FormName() != "file" || part.Header.Get("Content-Type") != "image/png" {
			t.Error("image multipart bytes or metadata changed")
		}
		if _, err = reader.NextPart(); err != io.EOF {
			t.Error("unexpected multipart purpose or other field")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": "file-native-test"})
	}))
	defer server.Close()
	parts := []map[string]any{imagePart(raw), imagePart(raw), imagePart(raw)}
	parts[0]["detail"] = "original"
	delete(parts[2], "detail")
	arguments := map[string]any{"type": "function_call", "arguments": raw}
	source := map[string]any{"id": json.Number("9007199254740993"), "input": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": raw}, parts[0]}},
		map[string]any{"type": "function_call_output", "output": []any{parts[1]}},
		map[string]any{"type": "custom_tool_call_output", "output": []any{parts[2]}}, arguments,
	}}
	u := New()
	headers := testHeaders()
	before := headers.Clone()
	changed, err := u.Rewrite(context.Background(), server.Client(), server.URL+"/basispoints/api/responses", headers, source, "session")
	if err != nil || !changed || uploads.Load() != 1 {
		t.Fatalf("changed=%v uploads=%d err=%v", changed, uploads.Load(), err)
	}
	if parts[0]["file_id"] != "file-native-test" || parts[0]["image_url"] != nil {
		t.Error("message attachment reference missing")
	}
	for _, part := range parts[1:] {
		if part["image_url"] != raw || part["file_id"] != nil {
			t.Error("tool screenshot did not retain its original data URL")
		}
	}
	if parts[0]["detail"] != "original" || parts[1]["detail"] != "high" || parts[2]["detail"] != "auto" {
		t.Error("detail changed")
	}
	if source["id"] != json.Number("9007199254740993") || arguments["arguments"] != raw || !reflect.DeepEqual(headers, before) {
		t.Error("unrelated data or caller headers changed")
	}
	if _, err := u.Rewrite(context.Background(), server.Client(), server.URL+"/basispoints/api/responses", headers, message(imagePart(raw)), "session"); err != nil || uploads.Load() != 1 {
		t.Fatalf("replay did not reuse attachment: %v", err)
	}
}

func TestNoInlineImageIsImmediateNoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := message(imagePart("https://example.invalid/image.png"))
	changed, err := (*Uploader)(nil).Rewrite(ctx, nil, "INVALID", nil, source, "")
	if changed || err != nil {
		t.Fatalf("no-image request requires attachment setup: %v", err)
	}
}

func TestValidationCompletesBeforeUploadAndCommit(t *testing.T) {
	data := testPNG(t)
	raw := dataURL(data, "image/png")
	huge := append([]byte(nil), data...)
	binary.BigEndian.PutUint32(huge[16:20], maxPixels+1)
	binary.BigEndian.PutUint32(huge[29:33], crc32.ChecksumIEEE(huge[12:29]))
	tests := map[string]string{
		"invalid_base64":    "data:image/png;base64,PRIVATE_BAD_%%%%",
		"wrong_mime":        dataURL(data, "image/jpeg"),
		"non_image":         dataURL([]byte("PRIVATE_NOT_IMAGE"), "image/png"),
		"too_many_pixels":   dataURL(huge, "image/png"),
		"interior_padding":  "data:image/png;base64,AAAA====AAAA",
		"missing_separator": "data:image/png;base64AAAA",
		"not_base64":        "data:image/png,PRIVATE",
		"wrong_format":      "data:image/svg+xml;base64,AAAA",
		"whitespace":        "data:image/png;base64,AA AA===",
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	for name, bad := range tests {
		t.Run(name, func(t *testing.T) {
			first, second := imagePart(raw), imagePart(bad)
			changed, err := New().Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), message(first, second), "scope")
			var typed *Error
			if changed || !errors.As(err, &typed) || typed.StatusCode() != 400 || !strings.Contains(err.Error(), "path=input[0].content[1]") || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("unexpected validation result: %v", err)
			}
			if first["image_url"] != raw || first["file_id"] != nil || calls.Load() != 0 {
				t.Error("invalid request uploaded or partially rewrote image")
			}
		})
	}
	parts := make([]map[string]any, 21)
	for i := range parts {
		parts[i] = imagePart(raw)
	}
	if changed, err := New().Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), message(parts...), "scope"); changed || err == nil || !strings.Contains(err.Error(), "20 inline") {
		t.Fatalf("image count limit missing: %v", err)
	}
	if calls.Load() != 0 {
		t.Error("count rejection happened after uploading")
	}
}

func TestNativeUploadAcceptsSupportedRasterFormats(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	var jpg, gifBytes bytes.Buffer
	if err := jpeg.Encode(&jpg, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifBytes, img, nil); err != nil {
		t.Fatal(err)
	}
	webp, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	for mime, data := range map[string][]byte{"image/png": testPNG(t), "image/jpeg": jpg.Bytes(), "image/gif": gifBytes.Bytes(), "image/webp": webp} {
		t.Run(mime, func(t *testing.T) {
			parsed, err := parseImage(dataURL(data, mime))
			if err != nil {
				t.Fatal(err)
			}
			if err := validateImage(context.Background(), parsed); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUploadFailureIsAtomicAndSafe(t *testing.T) {
	data := testPNG(t)
	secondData := append(append([]byte(nil), data...), 0)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": "file-first"})
			return
		}
		w.WriteHeader(401)
		_, _ = io.WriteString(w, "PRIVATE_TEST_TOKEN PRIVATE_TEST_ACCOUNT PRIVATE_IMAGE_PAYLOAD")
	}))
	defer server.Close()
	first, second := imagePart(dataURL(data, "image/png")), imagePart(dataURL(secondData, "image/png"))
	changed, err := New().Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), message(first, second), "scope")
	var typed *Error
	if changed || !errors.As(err, &typed) || typed.StatusCode() != 401 || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("unsafe upload error: %v", err)
	}
	if first["image_url"] == nil || second["image_url"] == nil || first["file_id"] != nil {
		t.Error("failed upload partially mutated caller request")
	}
}

func TestAuthenticatedUploadNeverFollowsRedirect(t *testing.T) {
	var reached atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		t.Error("redirect target received request")
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/attachments", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := server.Client()
	_, err := New().Rewrite(context.Background(), client, server.URL+"/responses", testHeaders(), message(imagePart(dataURL(testPNG(t), "image/png"))), "scope")
	if err == nil || reached.Load() != 0 || client.CheckRedirect != nil {
		t.Fatalf("redirect policy was not isolated: %v", err)
	}
}

func TestCacheScopeAuthEndpointAndTTLIsolation(t *testing.T) {
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": fmt.Sprintf("file-%d", uploads.Add(1))})
	}))
	defer server.Close()
	u := New()
	now := time.Now()
	u.now = func() time.Time { return now }
	raw := dataURL(testPNG(t), "image/png")
	headers := testHeaders()
	rewrite := func(scope, path string, h http.Header) {
		t.Helper()
		if _, err := u.Rewrite(context.Background(), server.Client(), server.URL+path, h, message(imagePart(raw)), scope); err != nil {
			t.Fatal(err)
		}
	}
	rewrite("one", "/api/responses", headers)
	rewrite("one", "/api/responses", headers)
	if uploads.Load() != 1 {
		t.Fatal("same account replay did not reuse")
	}
	rewrite("two", "/api/responses", headers)
	changed := headers.Clone()
	changed.Set("Authorization", "Bearer OTHER")
	rewrite("one", "/api/responses", changed)
	changed = headers.Clone()
	changed.Set("ChatGPT-Account-ID", "OTHER")
	rewrite("one", "/api/responses", changed)
	rewrite("one", "/alternate/responses", headers)
	rewrite("", "/api/responses", headers)
	rewrite("", "/api/responses", headers)
	if uploads.Load() != 7 {
		t.Fatalf("scope/auth/endpoint isolation failed: %d", uploads.Load())
	}
	now = now.Add(cacheTTL)
	rewrite("one", "/api/responses", headers)
	if uploads.Load() != 8 {
		t.Fatal("expired file ID reused")
	}
}

func TestConcurrentReuseWaiterCanCancel(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-finish
		_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": "file-shared"})
	}))
	defer server.Close()
	u := New()
	raw := dataURL(testPNG(t), "image/png")
	done := make(chan error, 1)
	go func() {
		_, err := u.Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), message(imagePart(raw)), "scope")
		done <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := u.Rewrite(ctx, server.Client(), server.URL+"/responses", testHeaders(), message(imagePart(raw)), "scope")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting upload did not respect cancellation: %v", err)
	}
	var group sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := u.Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), message(imagePart(raw)), "scope")
			errs <- err
		}()
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("duplicate concurrent uploads: %d", calls.Load())
	}
}
