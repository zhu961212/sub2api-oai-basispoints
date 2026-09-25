package attachments

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"testing"
)

type benchmarkUploadTransport struct{}

func (benchmarkUploadTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	_, err := io.Copy(io.Discard, request.Body)
	_ = request.Body.Close()
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{\"openai_file_id\":\"file-benchmark\"}"))}, nil
}

func BenchmarkAttachmentRewrite(b *testing.B) {
	pixels := image.NewNRGBA(image.Rect(0, 0, 1024, 1024))
	random := rand.New(rand.NewSource(17))
	_, _ = random.Read(pixels.Pix)
	for i := 3; i < len(pixels.Pix); i += 4 {
		pixels.Pix[i] = 255
	}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, pixels); err != nil {
		b.Fatal(err)
	}
	raw := dataURL(pngBytes.Bytes(), "image/png")
	client := &http.Client{Transport: benchmarkUploadTransport{}}
	headers := testHeaders()
	ctx := context.Background()
	endpoint := "https://local-benchmark.invalid/responses"
	for _, scenario := range []struct {
		name   string
		copies int
		warm   bool
	}{
		{"first", 1, false}, {"warm", 1, true}, {"four_duplicates", 4, false},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			cached := New()
			if scenario.warm {
				if _, err := cached.Rewrite(ctx, client, endpoint, headers, message(imagePart(raw)), "bench"); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.SetBytes(int64(pngBytes.Len()))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				uploader := cached
				if !scenario.warm {
					uploader = New()
				}
				parts := make([]map[string]any, scenario.copies)
				for j := range parts {
					parts[j] = imagePart(raw)
				}
				if _, err := uploader.Rewrite(ctx, client, endpoint, headers, message(parts...), "bench"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
