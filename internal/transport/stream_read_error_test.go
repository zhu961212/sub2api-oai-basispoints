package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSafeUpstreamReadError(t *testing.T) {
	timeout := "upstream response timed out before completion; check timeout_seconds and increase it for long-running requests"
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"deadline", context.DeadlineExceeded, timeout},
		{"wrapped_deadline", fmt.Errorf("private-token: %w", context.DeadlineExceeded), timeout},
		{"network_timeout", &url.Error{Op: "Get", URL: "https://user:private-token@example.invalid", Err: &net.DNSError{Err: "private-token", Name: "example.invalid", IsTimeout: true}}, timeout},
		{"truncated", fmt.Errorf("private-token: %w", io.ErrUnexpectedEOF), "upstream connection closed before response completed"},
		{"network_error", errors.New("https://user:private-token@example.invalid"), "upstream connection failed before response completed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeUpstreamReadError(tt.err); got != tt.want {
				t.Fatalf("diagnostic = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStreamReadTimeoutBeforeVisibleOutput(t *testing.T) {
	pending := streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": relayNativeCall("call_timeout", "get_weather", map[string]any{})})
	for _, tt := range []struct {
		name string
		data string
	}{
		{"empty", ""},
		{"unfinished_record", "data: {"},
		{"pending_tool", pending},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader := &relayStepReader{steps: []relayReadStep{{data: tt.data, err: fmt.Errorf("private-token: %w", context.DeadlineExceeded)}}}
			result := runRelayReader(t, reader, 1<<20)
			if result.errFrame == nil || result.errFrame.GetCode() != "upstream_read" || !result.errFrame.GetRequestSent() {
				t.Fatalf("timeout must remain a non-replayable error: %#v", result)
			}
			if result.errFrame.GetMessage() != safeUpstreamReadError(context.DeadlineExceeded) {
				t.Fatalf("unexpected timeout message: %q", result.errFrame.GetMessage())
			}
			if result.status != 0 || result.ended || len(result.body) != 0 || reader.reads != 1 {
				t.Fatalf("timeout unexpectedly committed or continued the response: %#v, reads=%d", result, reader.reads)
			}
			assertNoToolExecutionOnStreamError(t, result)
		})
	}
}

func TestStreamReadTimeoutAfterVisibleOutput(t *testing.T) {
	text := streamData(map[string]any{"type": "response.output_text.delta", "delta": "working"})
	pending := streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": relayNativeCall("call_timeout", "get_weather", map[string]any{})})
	pending += streamData(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "call_timeout", "delta": "unfinished tool argument"})
	for _, tt := range []struct {
		name       string
		prefix     string
		data       string
		wantEvents int
	}{
		{"text", text, "", 2},
		{"text_and_pending_tool", text, pending, 2},
		{"heartbeat_and_pending_tool", ": keepalive\n\n", pending, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader := &relayStepReader{steps: []relayReadStep{{data: tt.prefix}, {data: tt.data, err: context.DeadlineExceeded}}}
			result := runRelayReader(t, reader, 1<<20)
			events := parsedStreamEvents(t, result)
			if len(events) != tt.wantEvents || events[len(events)-1]["type"] != "response.failed" {
				t.Fatalf("expected partial output followed by failure: %s", result.body)
			}
			failure := relayObject(relayObject(events[len(events)-1]["response"])["error"])
			if failure["code"] != "upstream_read" || failure["message"] != safeUpstreamReadError(context.DeadlineExceeded) {
				t.Fatalf("missing safe timeout diagnostic: %v", failure)
			}
			if strings.Count(string(result.body), "data: [DONE]") != 1 || !strings.HasSuffix(string(result.body), "data: [DONE]\n\n") || reader.reads != 2 {
				t.Fatalf("timeout did not end exactly once: reads=%d body=%s", reader.reads, result.body)
			}
			assertNoToolExecutionOnStreamError(t, result)
		})
	}
}

type cancelingStreamErrorReader struct {
	relayStepReader
	cancel context.CancelFunc
}

func (r *cancelingStreamErrorReader) Read(p []byte) (int, error) {
	n, err := r.relayStepReader.Read(p)
	if err != nil {
		r.cancel()
	}
	return n, err
}

func TestStreamReadCancellationDoesNotEmitTimeoutFailure(t *testing.T) {
	text := streamData(map[string]any{"type": "response.output_text.delta", "delta": "working"})
	for _, prefix := range []string{"", text} {
		ctx, cancel := context.WithCancel(context.Background())
		reader := &cancelingStreamErrorReader{relayStepReader: relayStepReader{steps: []relayReadStep{{data: prefix}, {err: context.DeadlineExceeded}}}, cancel: cancel}
		stream := &streamStub{ctx: ctx}
		response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(reader)}
		err := sendTransformedHTTPResponseStream(stream, response, 1<<20, map[string]any{"stream": true})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation returned %v", err)
		}
		var body strings.Builder
		for _, frame := range stream.responses {
			if frame.GetError() != nil || frame.GetEnd() != nil {
				t.Fatalf("cancellation emitted a failure or completion frame: %v", frame)
			}
			body.Write(frame.GetBodyChunk())
		}
		if strings.Contains(body.String(), "response.failed") || strings.Contains(body.String(), "[DONE]") {
			t.Fatalf("cancellation manufactured a terminal event: %s", body.String())
		}
	}
}

func TestStreamCompletedResponseWinsOverSameReadTimeout(t *testing.T) {
	body := streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_timeout_tail", "status": "completed", "output": []any{}}})
	reader := &relayStepReader{steps: []relayReadStep{{data: body, err: context.DeadlineExceeded}}}
	result := runRelayReader(t, reader, 1<<20)
	events := parsedStreamEvents(t, result)
	if len(events) != 1 || events[0]["type"] != "response.completed" || reader.reads != 1 || strings.Count(string(result.body), "data: [DONE]") != 1 {
		t.Fatalf("valid completion was overridden by read timeout: %s", result.body)
	}
}
