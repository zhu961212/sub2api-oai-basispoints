package transport

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// All fixtures are synthetic in-memory responses: no network, credentials,
// account selection, model generation, or request construction is measured.
type responseBenchmarkReader struct {
	data          []byte
	offset, chunk int
}

func (r *responseBenchmarkReader) Read(out []byte) (int, error) {
	if r.offset == len(r.data) {
		return 0, io.EOF
	}
	n := min(len(out), r.chunk, len(r.data)-r.offset)
	copy(out, r.data[r.offset:r.offset+n])
	r.offset += n
	return n, nil
}
func (*responseBenchmarkReader) Close() error { return nil }

type responseBenchmarkSink struct {
	streamStub
	bytes int
	ended bool
}

func (s *responseBenchmarkSink) Send(frame *pluginv1.ForwardResponse) error {
	s.bytes += len(frame.GetBodyChunk())
	s.ended = s.ended || frame.GetEnd() != nil
	return nil
}

func responseBenchmarkTextSSE(count, width int) []byte {
	var raw bytes.Buffer
	text := strings.Repeat("x", width)
	for range count {
		raw.WriteString(streamData(map[string]any{"type": "response.output_text.delta", "delta": text}))
	}
	raw.WriteString(streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_bench", "status": "completed", "output": []any{}, "usage": map[string]any{"input_tokens": 12, "output_tokens": count}}}))
	raw.WriteString("data: [DONE]\n\n")
	return raw.Bytes()
}

func BenchmarkBPSResponseIsolationSSE(b *testing.B) {
	for _, fixture := range []struct {
		name                string
		count, width, chunk int
	}{
		{"small_deltas_fragmented", 256, 64, 256},
		{"small_deltas_batched", 256, 64, 32 << 10},
		{"large_text_event", 1, 512 << 10, 4096},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			raw := responseBenchmarkTextSSE(fixture.count, fixture.width)
			out := make([]byte, 32<<10)
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &responseBenchmarkReader{data: raw, chunk: fixture.chunk}}
				if err := prepareBasisPointsResponse(resp, 2<<20); err != nil {
					b.Fatal(err)
				}
				n, err := io.CopyBuffer(io.Discard, resp.Body, out)
				_ = resp.Body.Close()
				if err != nil || n != int64(len(raw)) {
					b.Fatalf("invalid isolated stream: %d %v", n, err)
				}
			}
		})
	}
}

func BenchmarkBPSResponseIsolationJSON(b *testing.B) {
	for _, size := range []int{4096, 512 << 10} {
		name := "small"
		if size > 4096 {
			name = "large"
		}
		b.Run(name, func(b *testing.B) {
			raw := protocol.JSONBytes(map[string]any{"id": "resp_bench", "status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": strings.Repeat("x", size)}}}}})
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw))}
				if err := prepareBasisPointsResponse(resp, 2<<20); err != nil {
					b.Fatal(err)
				}
				body, err := readLimited(resp.Body, 2<<20)
				_ = resp.Body.Close()
				if err != nil || !bytes.Equal(body, raw) {
					b.Fatalf("invalid isolated JSON: %v", err)
				}
			}
		})
	}
}

func BenchmarkBPSResponseTransformedPipeline(b *testing.B) {
	raw := responseBenchmarkTextSSE(256, 64)
	source := map[string]any{"model": "gpt-6-astra", "stream": true}
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &responseBenchmarkReader{data: raw, chunk: 256}}
		if err := prepareBasisPointsResponse(resp, 2<<20); err != nil {
			b.Fatal(err)
		}
		sink := &responseBenchmarkSink{streamStub: streamStub{ctx: context.Background()}}
		if err := sendTransformedHTTPResponseStreamWithKeepalive(sink, resp, 2<<20, source, 0); err != nil {
			b.Fatal(err)
		}
		if !sink.ended || sink.bytes == 0 {
			b.Fatal("missing transformed response")
		}
	}
}
