package imagerelay_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/imagerelay"
)

// Fixture creation and base64 encoding happen before ResetTimer.
// A fixed-seed opaque RGB PNG is valid and approximately 3 MiB.
func benchmarkPNG(b *testing.B) ([]byte, string) {
	b.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 1024, 1024))
	state := uint32(0x9e3779b9)
	for i := 0; i < len(img.Pix); i += 4 {
		for c := 0; c < 3; c++ {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			img.Pix[i+c] = byte(state)
		}
		img.Pix[i+3] = 255
	}
	var buf bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	if err := encoder.Encode(&buf, img); err != nil {
		b.Fatal(err)
	}
	data := buf.Bytes()
	return data, "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
}

func benchmarkRequest(raw string, count int) (map[string]any, []map[string]any) {
	parts := make([]map[string]any, count)
	content := make([]any, count)
	for i := range parts {
		parts[i] = map[string]any{"type": "input_image", "image_url": raw}
		content[i] = parts[i]
	}
	return map[string]any{"input": []any{map[string]any{"role": "user", "content": content}}}, parts
}

func benchmarkRelay(b *testing.B, root string) *imagerelay.Relay {
	b.Helper()
	r, err := imagerelay.New("https://images.example", root)
	if err != nil {
		b.Fatal(err)
	}
	return r
}

func benchmarkColdRewrite(b *testing.B, copies int) {
	data, raw := benchmarkPNG(b)
	root := b.TempDir()
	r := benchmarkRelay(b, root)
	b.Cleanup(func() {
		if err := r.Close(); err != nil {
			b.Error(err)
		}
	})
	source, parts := benchmarkRequest(raw, copies)
	b.ReportAllocs()
	b.SetBytes(int64(len(data) * copies))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Bounded batches stay below storage quotas. Session creation/cleanup
		// is excluded and does not create a directory for every operation.
		if i > 0 && i%32 == 0 {
			b.StopTimer()
			if err := r.Close(); err != nil {
				b.Fatal(err)
			}
			r = benchmarkRelay(b, root)
			b.StartTimer()
		}
		for _, part := range parts {
			part["image_url"] = raw
		}
		if changed, err := r.Rewrite(source, "cold-"+strconv.Itoa(i)); err != nil || !changed {
			b.Fatalf("rewrite: changed=%v err=%v", changed, err)
		}
	}
}

func BenchmarkRewriteFirst3MiB(b *testing.B)       { benchmarkColdRewrite(b, 1) }
func BenchmarkRewriteDuplicate4x3MiB(b *testing.B) { benchmarkColdRewrite(b, 4) }

func BenchmarkRewriteSameScope3MiB(b *testing.B) {
	data, raw := benchmarkPNG(b)
	r := benchmarkRelay(b, b.TempDir())
	b.Cleanup(func() {
		if err := r.Close(); err != nil {
			b.Error(err)
		}
	})
	source, parts := benchmarkRequest(raw, 1)
	if _, err := r.Rewrite(source, "same-scope"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parts[0]["image_url"] = raw
		if changed, err := r.Rewrite(source, "same-scope"); err != nil || !changed {
			b.Fatalf("rewrite: changed=%v err=%v", changed, err)
		}
	}
}

// The sink measures file serving without a growing in-memory response copy.
type benchmarkResponse struct {
	header        http.Header
	status, bytes int
}

func (w *benchmarkResponse) Header() http.Header         { return w.header }
func (w *benchmarkResponse) WriteHeader(status int)      { w.status = status }
func (w *benchmarkResponse) Write(p []byte) (int, error) { w.bytes += len(p); return len(p), nil }

func BenchmarkDownload3MiB(b *testing.B) {
	data, raw := benchmarkPNG(b)
	r := benchmarkRelay(b, b.TempDir())
	b.Cleanup(func() {
		if err := r.Close(); err != nil {
			b.Error(err)
		}
	})
	source, parts := benchmarkRequest(raw, 1)
	if _, err := r.Rewrite(source, "download"); err != nil {
		b.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, parts[0]["image_url"].(string), nil)
	w := &benchmarkResponse{header: make(http.Header)}
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.status, w.bytes = 0, 0
		r.ServeHTTP(w, req)
		if w.status != http.StatusOK || w.bytes != len(data) {
			b.Fatalf("download: status=%d bytes=%d", w.status, w.bytes)
		}
	}
}
