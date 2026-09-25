package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

type concurrentForwardProbe struct {
	streamStub
	firstBody  chan struct{}
	sending    atomic.Int32
	overlapped atomic.Bool
}

func (s *concurrentForwardProbe) Send(frame *pluginv1.ForwardResponse) error {
	if s.sending.Add(1) != 1 {
		s.overlapped.Store(true)
	}
	defer s.sending.Add(-1)
	s.responses = append(s.responses, frame)
	if len(frame.GetBodyChunk()) > 0 {
		select {
		case s.firstBody <- struct{}{}:
		default:
		}
	}
	return nil
}

// One shared Transport handles independent streams concurrently. All requests
// use the same call_id, but distinct sessions/catalogs, including failures and
// cancellations that must not affect other requests. The barrier proves the
// upstream requests actually overlap instead of passing sequentially.
func TestForwardConcurrentRequestsRemainIsolated(t *testing.T) {
	const workers = 64
	for _, useProxy := range []bool{false, true} {
		t.Run(fmt.Sprintf("proxy=%v", useProxy), func(t *testing.T) {
			ctx, cancelAll := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancelAll()
			ready := make(chan struct{}, workers)
			release := make(chan struct{})
			var active, peak atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				index, err := strconv.Atoi(r.Header.Get("X-Test-Stream"))
				if err != nil {
					http.Error(w, "missing test stream", http.StatusBadRequest)
					return
				}
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				ready <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				case <-ctx.Done():
					return
				}
				owner := fmt.Sprintf("session-%03d", index)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.output_text.delta", "delta": owner}))
				w.(http.Flusher).Flush()
				if index%4 == 1 {
					return
				} // Premature upstream EOF.
				if index%4 == 2 {
					select {
					case <-r.Context().Done():
					case <-ctx.Done():
					}
					return
				}
				tool := fmt.Sprintf("tool_%03d", index)
				if index%4 == 3 {
					tool = "undeclared_tool"
				}
				native := relayNativeCall("shared_call", tool, map[string]any{"owner": owner})
				_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_" + owner, "status": "completed", "output": []any{native}}}))
			}))
			defer upstream.Close()
			transport := New()
			defer transport.Shutdown()
			applyConfig(t, transport, map[string]any{"responses_url": upstream.URL})
			type completed struct {
				index  int
				stream *concurrentForwardProbe
				err    error
			}
			results := make(chan completed, workers)
			for i := 0; i < workers; i++ {
				owner := fmt.Sprintf("session-%03d", i)
				body := protocol.JSONBytes(map[string]any{"model": "gpt-6-astra", "stream": true, "input": "inspect", "tools": []any{map[string]any{"type": "function", "name": fmt.Sprintf("tool_%03d", i), "parameters": map[string]any{"type": "object", "required": []any{"owner"}, "properties": map[string]any{"owner": map[string]any{"const": owner}}, "additionalProperties": false}}}})
				frames := requestFrames(t, upstream.URL, token(t, "local-test-account"), map[string]string{"session_id": fmt.Sprintf("%s-%v-%s", t.Name(), useProxy, owner), "X-Test-Stream": strconv.Itoa(i)}, body)
				if useProxy {
					frames[0].GetStart().ProxyUrl = upstream.URL
				}
				requestCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				probe := &concurrentForwardProbe{streamStub: streamStub{ctx: requestCtx, requests: frames}, firstBody: make(chan struct{}, 1)}
				if i%4 == 2 {
					go func() {
						select {
						case <-probe.firstBody:
							cancel()
						case <-requestCtx.Done():
						}
					}()
				}
				go func() { results <- completed{index: i, stream: probe, err: transport.Forward(probe)} }()
			}
			for i := 0; i < workers; i++ {
				select {
				case <-ready:
				case <-ctx.Done():
					t.Fatal("concurrent upstream requests did not all start")
				}
			}
			close(release)
			for i := 0; i < workers; i++ {
				var got completed
				select {
				case got = <-results:
				case <-ctx.Done():
					t.Fatal("concurrent streams did not finish")
				}
				if got.stream.overlapped.Load() {
					t.Fatalf("stream %d had concurrent Send calls", got.index)
				}
				if got.index%4 == 2 {
					if got.err != context.Canceled {
						t.Fatalf("canceled stream %d returned %v", got.index, got.err)
					}
					continue
				}
				if got.err != nil {
					t.Fatalf("stream %d transport error: %v", got.index, got.err)
				}
				var result forwardResult
				starts, ends := 0, 0
				for _, frame := range got.stream.responses {
					if start := frame.GetStart(); start != nil {
						starts++
						result.status = int(start.GetStatusCode())
					}
					result.body = append(result.body, frame.GetBodyChunk()...)
					if failure := frame.GetError(); failure != nil {
						t.Fatalf("stream %d used error frame: %v", got.index, failure)
					}
					if end := frame.GetEnd(); end != nil {
						ends++
						result.ended = true
						result.received = end.GetBytesReceived()
					}
				}
				if starts != 1 || ends != 1 || strings.Count(string(result.body), "data: [DONE]") != 1 {
					t.Fatalf("stream %d invalid lifecycle", got.index)
				}
				events := parsedStreamEvents(t, result)
				owner := fmt.Sprintf("session-%03d", got.index)
				if len(events) == 0 || events[0]["delta"] != owner {
					t.Fatalf("stream %d received another session output", got.index)
				}
				if got.index%4 != 0 {
					want := "invalid_upstream_response"
					if got.index%4 == 3 {
						want = "invalid_tool_call"
					}
					if streamFailureCode(result) != want {
						t.Fatalf("stream %d failure = %s", got.index, result.body)
					}
					assertNoToolExecutionOnStreamError(t, result)
					continue
				}
				last := events[len(events)-1]
				if last["type"] != "response.completed" {
					t.Fatalf("healthy stream %d failed: %s", got.index, result.body)
				}
				output, _ := relayObject(last["response"])["output"].([]any)
				if len(output) != 1 {
					t.Fatalf("stream %d lost its tool call", got.index)
				}
				call := relayObject(output[0])
				args, err := protocol.RawObject([]byte(protocol.StringValue(call["arguments"])))
				if err != nil || call["name"] != fmt.Sprintf("tool_%03d", got.index) || args["owner"] != owner || call["call_id"] != "shared_call" {
					t.Fatalf("stream %d tool identity leaked: %v", got.index, call)
				}
			}
			if peak.Load() != workers {
				t.Fatalf("peak concurrent upstreams = %d, want %d", peak.Load(), workers)
			}
			t.Logf("%d concurrent streams: 16 completed, 16 EOF, 16 canceled, 16 invalid tools; proxy=%v", workers, useProxy)
		})
	}
}
