package imagerelay

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testRelay(t *testing.T) *Relay {
	t.Helper()
	r, err := New("https://images.example/", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func dataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}
func imagePart(raw string) map[string]any {
	return map[string]any{"type": "input_image", "image_url": raw}
}
func request(parts ...map[string]any) map[string]any {
	content := make([]any, len(parts))
	for i, part := range parts {
		content[i] = part
	}
	return map[string]any{"input": []any{map[string]any{"role": "user", "content": content}}}
}
func get(r *Relay, method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}
func assertEmpty(t *testing.T, r *Relay) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) != 0 || len(r.images) != 0 || len(r.retired) != 0 || r.usedBytes != 0 || r.usedImages != 0 {
		t.Fatalf("relay retained image resources: entries=%d retired=%d bytes=%d files=%d", len(r.entries), len(r.retired), r.usedBytes, r.usedImages)
	}
	files, err := os.ReadDir(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name() != leaseName {
		t.Fatal("relay left staged files behind")
	}
}

func TestRewriteRoundTripDedupAndScope(t *testing.T) {
	r := testRelay(t)
	data := testPNG(t)
	part := imagePart(dataURL("image/png", data))
	changed, err := r.Rewrite(request(part), "account:1/key:1/thread:one")
	if err != nil || !changed {
		t.Fatalf("rewrite: %v %v", changed, err)
	}
	target := part["image_url"].(string)
	token := strings.TrimPrefix(target, "https://images.example"+Path)
	if len(token) != 43 || !validToken(token) {
		t.Fatal("invalid opaque image URL")
	}
	w := get(r, http.MethodGet, target)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("image bytes failed round trip")
	}
	if w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Content-Length") != strconv.Itoa(len(data)) || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("incorrect download headers")
	}
	r.mu.Lock()
	expires := r.entries[token].expires
	path := r.entries[token].path
	r.mu.Unlock()
	w = get(r, http.MethodHead, target)
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Fatal("HEAD must have metadata and no body")
	}
	r.mu.Lock()
	if !r.entries[token].expires.Equal(expires) {
		t.Error("downloads must not renew image TTL")
	}
	r.entries[token].expires = time.Now().Add(time.Minute)
	r.mu.Unlock()
	second := imagePart(dataURL("image/png", data))
	if _, err := r.Rewrite(request(second), "account:1/key:1/thread:one"); err != nil {
		t.Fatal(err)
	}
	if second["image_url"] != target || len(r.entries) != 1 || r.usedImages != 1 || r.usedBytes != int64(len(data)) {
		t.Fatal("same scope did not reuse image")
	}
	if time.Until(r.entries[token].expires) < 29*time.Minute {
		t.Fatal("rewrite did not renew TTL")
	}
	other := imagePart(dataURL("image/png", data))
	if _, err := r.Rewrite(request(other), "account:1/key:2/thread:one"); err != nil {
		t.Fatal(err)
	}
	if other["image_url"] == target || len(r.entries) != 2 {
		t.Fatal("image URL crossed scope boundary")
	}
	if runtime.GOOS != "windows" {
		for name, mode := range map[string]os.FileMode{r.root: 0700, r.dir: 0700, path: 0600} {
			info, err := os.Stat(name)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatal("image storage permissions are not private")
			}
		}
	}
}

func TestRewriteOnlyTypedImagesAndPreservesValues(t *testing.T) {
	r := testRelay(t)
	inline := dataURL("image/png", testPNG(t))
	parts := []map[string]any{imagePart(inline), imagePart(inline), imagePart(inline)}
	parts[0]["detail"] = "high"
	https := imagePart("https://cdn.example/a.png?sig=UNCHANGED%2F")
	arguments := map[string]any{"image": imagePart(inline), "number": json.Number("9007199254740993")}
	textPart := map[string]any{"type": "input_text", "text": inline}
	source := map[string]any{"number": json.Number("9007199254740993"), "input": []any{
		map[string]any{"type": "message", "role": "user", "content": []any{parts[0], https, textPart}},
		map[string]any{"type": "function_call_output", "output": []any{parts[1]}},
		map[string]any{"type": "custom_tool_call_output", "output": []any{parts[2]}},
		map[string]any{"type": "function_call", "arguments": arguments, "content": []any{imagePart(inline)}},
	}}
	if changed, err := r.Rewrite(source, "scope"); err != nil || !changed {
		t.Fatalf("rewrite: %v", err)
	}
	for _, part := range parts {
		if !strings.HasPrefix(part["image_url"].(string), "https://images.example"+Path) {
			t.Fatal("typed image not relayed")
		}
	}
	if https["image_url"] != "https://cdn.example/a.png?sig=UNCHANGED%2F" || textPart["text"] != inline || parts[0]["detail"] != "high" || source["number"] != json.Number("9007199254740993") || arguments["image"].(map[string]any)["image_url"] != inline {
		t.Fatal("unrelated input values changed")
	}
	if changed, err := r.Rewrite(source, "scope"); err != nil || changed {
		t.Fatal("already relayed request should remain unchanged")
	}
}

func TestSupportedImageFormats(t *testing.T) {
	r := testRelay(t)
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
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
		part := imagePart(dataURL(mime, data))
		if _, err := r.Rewrite(request(part), mime); err != nil {
			t.Fatalf("%s: %v", mime, err)
		}
		w := get(r, http.MethodGet, part["image_url"].(string))
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != mime || !bytes.Equal(w.Body.Bytes(), data) {
			t.Fatalf("format round trip failed: %s", mime)
		}
	}
}

func TestInvalidImageRollbackAndSafeErrors(t *testing.T) {
	r := testRelay(t)
	pngBytes := testPNG(t)
	largePixels := append([]byte(nil), pngBytes...)
	binary.BigEndian.PutUint32(largePixels[16:20], 100000)
	binary.BigEndian.PutUint32(largePixels[20:24], 100000)
	binary.BigEndian.PutUint32(largePixels[29:33], crc32.ChecksumIEEE(largePixels[12:29]))
	invalidImages := []string{
		"data:image/png,no-base64-marker-PRIVATE", "data:image/png;base64,",
		"data:image/png;base64,PRIVATE_INVALID", "data:image/png;base64,AB==",
		"data:image/png;base64;foo=PRIVATE,AAAA", "data:image/svg+xml;base64,PRIVATE",
		dataURL("text/html", []byte("PRIVATE")), dataURL("image/png", []byte("PRIVATE")),
		dataURL("image/jpeg", pngBytes), dataURL("image/png", largePixels),
		dataURL("image/png", pngBytes) + "\n",
	}
	for _, raw := range invalidImages {
		source := request(imagePart(dataURL("image/png", pngBytes)), imagePart(raw))
		before, _ := json.Marshal(source)
		changed, err := r.Rewrite(source, "PRIVATE_SCOPE")
		if changed || !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid image was accepted: changed=%v error=%v", changed, err)
		}
		for _, private := range []string{"PRIVATE", "data:", r.dir, raw} {
			if strings.Contains(err.Error(), private) {
				t.Fatal("error exposed image or path")
			}
		}
		after, _ := json.Marshal(source)
		if !bytes.Equal(before, after) {
			t.Fatal("failed request was partially rewritten")
		}
		assertEmpty(t, r)
	}
	part := imagePart(dataURL("image/png", pngBytes))
	part["file_id"] = "file_PRIVATE"
	if _, err := r.Rewrite(request(part), "scope"); !errors.Is(err, ErrInvalid) {
		t.Fatal("inline file_id conflict accepted")
	}
	assertEmpty(t, r)
}

func TestRequestAndStorageLimits(t *testing.T) {
	r := testRelay(t)
	pngBytes := testPNG(t)
	parts := make([]map[string]any, maxRequestImages+1)
	for i := range parts {
		parts[i] = imagePart(dataURL("image/png", pngBytes))
	}
	if _, err := r.Rewrite(request(parts...), "scope"); !errors.Is(err, ErrInvalid) {
		t.Fatal("image count limit not enforced")
	}
	assertEmpty(t, r)
	oversized := "data:image/png;base64," + strings.Repeat("A", base64.StdEncoding.EncodedLen(maxImageBytes)+1)
	if _, err := r.Rewrite(request(imagePart(oversized)), "scope"); !errors.Is(err, ErrInvalid) {
		t.Fatal("image size limit not enforced")
	}
	assertEmpty(t, r)
	// DecodeConfig validates the image header without allocating decoded pixels.
	// Extra encoded bytes exercise the aggregate upload bound, not pixel memory.
	padded := make([]byte, maxRequestBytes/2+1)
	copy(padded, pngBytes)
	raw := dataURL("image/png", padded)
	if _, err := r.Rewrite(request(imagePart(raw), imagePart(raw)), "scope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("aggregate image limit not enforced: %v", err)
	}
	assertEmpty(t, r)
	for _, fullByBytes := range []bool{false, true} {
		r.mu.Lock()
		if fullByBytes {
			r.usedBytes = maxStorageBytes
		} else {
			r.usedImages = maxStorageImages
		}
		r.mu.Unlock()
		part := imagePart(dataURL("image/png", pngBytes))
		if changed, err := r.Rewrite(request(part), "scope"); changed || !errors.Is(err, ErrFull) {
			t.Fatal("storage capacity not enforced")
		}
		r.mu.Lock()
		r.usedBytes, r.usedImages = 0, 0
		r.mu.Unlock()
		assertEmpty(t, r)
	}
}

func TestStorageFailureReleasesReservation(t *testing.T) {
	r := testRelay(t)
	original := r.dir
	r.dir = filepath.Join(original, "PRIVATE-missing")
	_, err := r.Rewrite(request(imagePart(dataURL("image/png", testPNG(t)))), "scope")
	r.dir = original
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), original) {
		t.Fatalf("unsafe storage error: %v", err)
	}
	assertEmpty(t, r)
}

func TestOriginValidationAndUpdates(t *testing.T) {
	for _, value := range []string{"", "http://PRIVATE.example", "https:///PRIVATE", "https://user:PRIVATE@example.com", "https://example.com/PRIVATE", "https://example.com?PRIVATE", "https://example.com?", "https://example.com#", " https://example.com"} {
		if err := ValidatePublicOrigin(value); !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatalf("invalid or unsafe origin validation: %v", err)
		}
	}
	r := testRelay(t)
	raw := dataURL("image/png", testPNG(t))
	before := imagePart(raw)
	if _, err := r.Rewrite(request(before), "scope"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetPublicOrigin("https://new.example"); err != nil {
		t.Fatal(err)
	}
	after := imagePart(raw)
	if _, err := r.Rewrite(request(after), "scope"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(after["image_url"].(string), "https://new.example"+Path) || get(r, http.MethodGet, before["image_url"].(string)).Code != http.StatusOK {
		t.Fatal("origin change invalidated in-flight image")
	}
	if !reflect.DeepEqual(before, imagePart(before["image_url"].(string))) {
		t.Fatal("unexpected source mutation")
	}
}

func TestConcurrentSameScopeReuse(t *testing.T) {
	r := testRelay(t)
	raw := dataURL("image/png", testPNG(t))
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			part := imagePart(raw)
			_, err := r.Rewrite(request(part), "scope")
			if err != nil {
				errs <- err
				return
			}
			if get(r, http.MethodGet, part["image_url"].(string)).Code != http.StatusOK {
				errs <- errors.New("download failed")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if len(r.entries) != 1 || r.usedImages != 1 {
		t.Fatal("concurrent images were not deduplicated")
	}
}
