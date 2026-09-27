package transport

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// A heartbeat commits HTTP 200 even when all upstream output is still buffered.
// Failures must then end in SSE and cannot release pending tools.
func TestStreamKeepaliveFailuresAfterHeartbeatEndInsideSSE(t *testing.T) {
	for _, tt := range []struct{ name, code string }{
		{"eof", "invalid_upstream_response"},
		{"unexpected_eof", "upstream_read"},
		{"size_limit", "upstream_response_too_large"},
		{"invalid_tool", "invalid_tool_call"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			response, writer, body := keepalivePipeResponse(t)
			stream := newKeepaliveStreamProbe(ctx)
			tool := "get_weather"
			if tt.name == "invalid_tool" {
				tool = "unknown_tool"
			}
			native := relayNativeCall("call_unconfirmed_"+tt.name, tool, map[string]any{})
			pending := streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": native})
			max := 1 << 20
			if tt.name == "size_limit" {
				max = len(pending)
			}
			source := map[string]any{"stream": true, "tools": []any{map[string]any{"type": "function", "name": "get_weather", "parameters": map[string]any{}}}}
			finished := make(chan error, 1)
			go func() {
				finished <- sendTransformedHTTPResponseStreamWithKeepalive(stream, response, max, source, 20*time.Millisecond)
			}()
			writeKeepaliveUpstream(t, writer, pending)
			waitKeepaliveFrames(t, stream, finished, 1)
			for _, frame := range stream.snapshot() {
				if chunk := frame.GetBodyChunk(); len(chunk) > 0 && string(chunk) != ": keepalive\n\n" {
					t.Fatalf("unconfirmed tool escaped before upstream failure: %q", chunk)
				}
			}
			switch tt.name {
			case "eof":
				_ = writer.Close()
			case "unexpected_eof":
				_ = writer.CloseWithError(io.ErrUnexpectedEOF)
			case "size_limit":
				writeKeepaliveUpstream(t, writer, strings.Repeat("x", 128))
			case "invalid_tool":
				writeKeepaliveUpstream(t, writer, streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_invalid_keepalive", "status": "completed", "output": []any{native}}}))
			}
			if err := waitKeepaliveResult(t, finished); err != nil {
				t.Fatalf("committed failure escaped as transport error: %v", err)
			}
			var result forwardResult
			starts, ends := 0, 0
			frames := stream.snapshot()
			for _, frame := range frames {
				if start := frame.GetStart(); start != nil {
					starts++
					result.status = int(start.GetStatusCode())
				}
				result.body = append(result.body, frame.GetBodyChunk()...)
				if failure := frame.GetError(); failure != nil {
					t.Fatalf("heartbeat committed HTTP 200, but failure used gRPC Error: %v", failure)
				}
				if end := frame.GetEnd(); end != nil {
					ends++
					result.ended, result.received = true, end.GetBytesReceived()
				}
			}
			if starts != 1 || ends != 1 || frames[len(frames)-1].GetEnd() == nil || result.received != int64(len(result.body)) {
				t.Fatalf("invalid committed failure lifecycle: starts=%d ends=%d bytes=%d/%d", starts, ends, result.received, len(result.body))
			}
			if got := streamFailureCode(result); got != tt.code {
				t.Fatalf("failure code = %q, want %q: %s", got, tt.code, result.body)
			}
			events := parsedStreamEvents(t, result)
			if len(events) != 1 || events[0]["type"] != "response.failed" {
				t.Fatalf("expected one failure event after heartbeat: %s", result.body)
			}
			failure := relayObject(relayObject(events[0]["response"])["error"])
			if tt.code == "invalid_tool_call" {
				if failure["type"] != "invalid_request_error" {
					t.Fatal("tool failure after heartbeat may trigger host failover")
				}
			} else if failure["type"] == "invalid_request_error" {
				t.Fatal("transport failure was incorrectly made request-scoped")
			}
			if strings.Count(string(result.body), "data: [DONE]") != 1 || !strings.HasSuffix(string(result.body), "data: [DONE]\n\n") {
				t.Fatalf("failure did not terminate with exactly one DONE: %s", result.body)
			}
			assertNoToolExecutionOnStreamError(t, result)
			select {
			case <-body.closed:
			default:
				t.Fatal("failure did not close upstream body")
			}
			if stream.concurrent.Load() {
				t.Fatal("heartbeat and failure Send calls overlapped")
			}
		})
	}
}

type keepaliveReadStartedBody struct {
	*keepaliveTrackedBody
	started chan struct{}
	once    sync.Once
}

func (b *keepaliveReadStartedBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.keepaliveTrackedBody.Read(p)
}

func TestStreamKeepaliveCancellationBeforeHeartbeatClosesBlockedReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response, _, body := keepalivePipeResponse(t)
	started := make(chan struct{})
	response.Body = &keepaliveReadStartedBody{keepaliveTrackedBody: body, started: started}
	stream := newKeepaliveStreamProbe(ctx)
	finished := make(chan error, 1)
	go func() {
		finished <- sendTransformedHTTPResponseStreamWithKeepalive(stream, response, 1<<20, map[string]any{"stream": true}, time.Hour)
	}()
	select {
	case <-started:
	case err := <-finished:
		t.Fatalf("stream ended before upstream Read: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("upstream Read never started")
	}
	cancel()
	if err := waitKeepaliveResult(t, finished); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stream returned %v, want context.Canceled", err)
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("cancellation before heartbeat did not close blocked upstream reader")
	}
	if frames := stream.snapshot(); len(frames) != 0 {
		t.Fatalf("canceling before heartbeat must not commit a response: %v", frames)
	}
}
