package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// Retain all sent frames until concurrent requests finish. Scratch buffers must
// not escape into RPC frames or be recycled before a final read with io.EOF is
// processed by the receiver.
func TestConcurrentStreamsPreserveRetainedResponseFrames(t *testing.T) {
	for _, transformed := range []bool{false, true} {
		t.Run(fmt.Sprintf("transformed=%t", transformed), func(t *testing.T) {
			const count = 256
			stubs := make([]*streamStub, count)
			originals := make([]string, count)
			owners := make([]string, count)
			errors := make([]error, count)
			start := make(chan struct{})
			var group sync.WaitGroup
			for worker := range 64 {
				group.Add(1)
				go func() {
					defer group.Done()
					<-start
					for round := range 4 {
						i := worker*4 + round
						owners[i] = strings.Repeat(fmt.Sprintf("owner_%03d;", i), 128)
						originals[i] = streamData(map[string]any{"type": "response.output_text.delta", "delta": owners[i]}) + streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp_%d", i), "status": "completed", "output": []any{}}}) + "data: [DONE]\n\n"
						stubs[i] = &streamStub{ctx: context.Background()}
						response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(&relayStepReader{steps: []relayReadStep{{data: originals[i], err: io.EOF}}})}
						if transformed {
							errors[i] = sendTransformedHTTPResponseStreamWithKeepalive(stubs[i], response, 1<<20, map[string]any{"stream": true}, 0)
						} else {
							errors[i] = sendHTTPResponseStream(stubs[i], response, 1<<20)
							_ = response.Body.Close()
						}
					}
				}()
			}
			close(start)
			group.Wait()
			for i, stub := range stubs {
				if errors[i] != nil {
					t.Fatalf("response %d: %v", i, errors[i])
				}
				var body []byte
				ended := false
				for _, frame := range stub.responses {
					if failure := frame.GetError(); failure != nil {
						t.Fatalf("response %d failed: %v", i, failure)
					}
					body = append(body, frame.GetBodyChunk()...)
					ended = ended || frame.GetEnd() != nil
				}
				if !ended || !bytes.Contains(body, protocol.JSONBytes(owners[i])) || bytes.Count(body, []byte("data: [DONE]")) != 1 || bytes.Contains(body, []byte("response.failed")) {
					t.Fatalf("response %d lost its complete owner-isolated stream", i)
				}
				if !transformed && string(body) != originals[i] {
					t.Fatalf("response %d passthrough bytes changed", i)
				}
			}
		})
	}
}

// Synthetic in-memory streams; excludes HTTP/gRPC/network and model generation.
func BenchmarkHTTPResponseStreamParallel(b *testing.B) {
	for _, size := range []int{1024, 32 << 10} {
		b.Run(fmt.Sprintf("bytes=%d", size), func(b *testing.B) {
			raw := bytes.Repeat([]byte("x"), size)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					response := &http.Response{StatusCode: http.StatusOK, ContentLength: int64(size), Body: io.NopCloser(bytes.NewReader(raw))}
					sink := &responseBenchmarkSink{streamStub: streamStub{ctx: context.Background()}}
					if err := sendHTTPResponseStream(sink, response, 1<<20); err != nil {
						b.Error(err)
						return
					}
					_ = response.Body.Close()
					if sink.bytes != size || !sink.ended {
						b.Error("response was truncated")
						return
					}
				}
			})
		})
	}
}
