package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
)

type keepaliveStreamProbe struct {
	streamStub
	mu            sync.Mutex
	frames        []*pluginv1.ForwardResponse
	notifications chan *pluginv1.ForwardResponse
	active        atomic.Int32
	concurrent    atomic.Bool
	failed        atomic.Bool
	afterFailure  atomic.Bool
	sendDelay     time.Duration
	fail          func(*pluginv1.ForwardResponse) bool
	sendErr       error
}

func newKeepaliveStreamProbe(ctx context.Context) *keepaliveStreamProbe {
	return &keepaliveStreamProbe{streamStub: streamStub{ctx: ctx}, notifications: make(chan *pluginv1.ForwardResponse, 128)}
}

func (s *keepaliveStreamProbe) Send(frame *pluginv1.ForwardResponse) error {
	if s.active.Add(1) != 1 {
		s.concurrent.Store(true)
	}
	defer s.active.Add(-1)
	if s.failed.Load() {
		s.afterFailure.Store(true)
	}
	// Slow sends expose a heartbeat writer that violates gRPC's single-sender rule.
	delay := s.sendDelay
	if delay == 0 {
		delay = 2 * time.Millisecond
	}
	time.Sleep(delay)
	if s.fail != nil && s.fail(frame) {
		s.failed.Store(true)
		return s.sendErr
	}
	s.mu.Lock()
	s.frames = append(s.frames, frame)
	s.mu.Unlock()
	select {
	case s.notifications <- frame:
	default:
	}
	return nil
}

func (s *keepaliveStreamProbe) snapshot() []*pluginv1.ForwardResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pluginv1.ForwardResponse(nil), s.frames...)
}

type keepaliveTrackedBody struct {
	io.ReadCloser
	once   sync.Once
	closed chan struct{}
}

func (b *keepaliveTrackedBody) Close() error {
	var err error
	b.once.Do(func() { err = b.ReadCloser.Close(); close(b.closed) })
	return err
}

func keepalivePipeResponse(t *testing.T) (*http.Response, *io.PipeWriter, *keepaliveTrackedBody) {
	t.Helper()
	reader, writer := io.Pipe()
	body := &keepaliveTrackedBody{ReadCloser: reader, closed: make(chan struct{})}
	t.Cleanup(func() { _ = writer.Close(); _ = body.Close() })
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: make(http.Header), Body: body, ContentLength: -1}, writer, body
}

func waitKeepaliveResult(t *testing.T, finished <-chan error) error {
	t.Helper()
	select {
	case err := <-finished:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not finish while the upstream reader remained blocked")
		return nil
	}
}

func waitKeepaliveFrames(t *testing.T, stream *keepaliveStreamProbe, finished <-chan error, count int) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for got := 0; got < count; {
		select {
		case frame := <-stream.notifications:
			if bytes.Equal(frame.GetBodyChunk(), []byte(": keepalive\n\n")) {
				got++
			}
		case err := <-finished:
			t.Fatalf("stream ended before idle heartbeat %d: %v", got+1, err)
		case <-timer.C:
			t.Fatalf("only received %d of %d idle heartbeats", got, count)
		}
	}
}

func writeKeepaliveUpstream(t *testing.T, writer *io.PipeWriter, value string) {
	t.Helper()
	written := make(chan error, 1)
	go func() { _, err := io.WriteString(writer, value); written <- err }()
	if err := waitKeepaliveResult(t, written); err != nil {
		t.Fatalf("write upstream record: %v", err)
	}
}

func TestStreamKeepaliveWhileUpstreamSilentOrToolBuffered(t *testing.T) {
	for _, name := range []string{"silent", "buffered_tool", "after_text"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			response, writer, _ := keepalivePipeResponse(t)
			stream := newKeepaliveStreamProbe(ctx)
			stream.sendDelay = 25 * time.Millisecond
			source := map[string]any{"stream": true, "tools": []any{map[string]any{"type": "function", "name": "get_weather", "parameters": map[string]any{}}}}
			finished := make(chan error, 1)
			go func() {
				finished <- sendTransformedHTTPResponseStreamWithKeepalive(stream, response, 1<<20, source, 20*time.Millisecond)
			}()
			output := []any{}
			if name == "buffered_tool" {
				native := relayNativeCall("call_keepalive", "get_weather", map[string]any{})
				output = append(output, native)
				writeKeepaliveUpstream(t, writer, streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": native}))
			} else if name == "after_text" {
				writeKeepaliveUpstream(t, writer, streamData(map[string]any{"type": "response.output_text.delta", "delta": "working"}))
			}
			waitKeepaliveFrames(t, stream, finished, 2)
			if name != "after_text" {
				for _, frame := range stream.snapshot() {
					if chunk := frame.GetBodyChunk(); len(chunk) > 0 && !bytes.Equal(chunk, []byte(": keepalive\n\n")) {
						t.Fatalf("unvalidated tool data became visible before terminal: %q", chunk)
					}
				}
			}
			terminal := streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_keepalive", "status": "completed", "output": output}})
			writeKeepaliveUpstream(t, writer, terminal+"data: [DONE]\n\ndata: [DONE]\n\n")
			if err := waitKeepaliveResult(t, finished); err != nil {
				t.Fatal(err)
			}
			var body bytes.Buffer
			var endBytes int64
			starts, ends := 0, 0
			for _, frame := range stream.snapshot() {
				if frame.GetStart() != nil {
					starts++
				}
				_, _ = body.Write(frame.GetBodyChunk())
				if end := frame.GetEnd(); end != nil {
					ends++
					endBytes = end.GetBytesReceived()
				}
				if failure := frame.GetError(); failure != nil {
					t.Fatalf("unexpected transport failure: %v", failure)
				}
			}
			if starts != 1 || ends != 1 || strings.Count(body.String(), "data: [DONE]") != 1 || endBytes != int64(body.Len()) {
				t.Fatalf("invalid stream lifecycle: starts=%d ends=%d bytes=%d/%d body=%s", starts, ends, endBytes, body.Len(), body.String())
			}
			events := parsedStreamEvents(t, forwardResult{body: body.Bytes(), ended: ends == 1, received: endBytes})
			completed := 0
			for _, event := range events {
				if event["type"] == "response.completed" {
					completed++
				}
			}
			if completed != 1 {
				t.Fatalf("completed events = %d, want one", completed)
			}
			if stream.concurrent.Load() {
				t.Fatal("heartbeat and event Send calls overlapped")
			}
		})
	}
}

func TestStreamKeepaliveCancellationClosesBlockedReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response, _, body := keepalivePipeResponse(t)
	stream := newKeepaliveStreamProbe(ctx)
	finished := make(chan error, 1)
	go func() {
		finished <- sendTransformedHTTPResponseStreamWithKeepalive(stream, response, 1<<20, map[string]any{"stream": true}, 20*time.Millisecond)
	}()
	waitKeepaliveFrames(t, stream, finished, 1)
	cancel()
	if err := waitKeepaliveResult(t, finished); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stream returned %v, want context.Canceled", err)
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("cancellation did not close the blocked upstream reader")
	}
	if stream.concurrent.Load() {
		t.Fatal("concurrent downstream Send calls")
	}
}

func TestStreamKeepaliveSendFailureClosesBlockedReader(t *testing.T) {
	for _, name := range []string{"start", "heartbeat"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			response, _, body := keepalivePipeResponse(t)
			stream := newKeepaliveStreamProbe(ctx)
			stream.sendErr = errors.New("downstream closed")
			stream.fail = func(frame *pluginv1.ForwardResponse) bool {
				if name == "start" {
					return frame.GetStart() != nil
				}
				return len(frame.GetBodyChunk()) > 0
			}
			finished := make(chan error, 1)
			go func() {
				finished <- sendTransformedHTTPResponseStreamWithKeepalive(stream, response, 1<<20, map[string]any{"stream": true}, 20*time.Millisecond)
			}()
			if err := waitKeepaliveResult(t, finished); !errors.Is(err, stream.sendErr) {
				t.Fatalf("Send error = %v, want %v", err, stream.sendErr)
			}
			select {
			case <-body.closed:
			default:
				t.Fatal("Send failure did not close the blocked upstream reader")
			}
			if stream.afterFailure.Load() {
				t.Fatal("stream attempted another Send after downstream failure")
			}
			if stream.concurrent.Load() {
				t.Fatal("concurrent downstream Send calls")
			}
		})
	}
}
