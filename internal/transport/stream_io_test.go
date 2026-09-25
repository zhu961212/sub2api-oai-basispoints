package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type relayReadStep struct {
	data string
	err  error
}

type relayStepReader struct {
	steps []relayReadStep
	reads int
}

func (r *relayStepReader) Read(p []byte) (int, error) {
	r.reads++
	if len(r.steps) == 0 {
		return 0, io.EOF
	}
	step := &r.steps[0]
	n := copy(p, step.data)
	step.data = step.data[n:]
	if len(step.data) > 0 {
		return n, nil
	}
	err := step.err
	r.steps = r.steps[1:]
	return n, err
}

func runRelayReader(t *testing.T, reader io.Reader, max int) forwardResult {
	t.Helper()
	stub := &streamStub{ctx: context.Background()}
	response := &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: make(http.Header), Body: io.NopCloser(reader)}
	if err := sendTransformedHTTPResponseStream(stub, response, max, map[string]any{"stream": true}); err != nil {
		t.Fatalf("relay returned a transport error: %v", err)
	}
	var result forwardResult
	for _, frame := range stub.responses {
		if start := frame.GetStart(); start != nil {
			result.status = int(start.GetStatusCode())
		}
		result.body = append(result.body, frame.GetBodyChunk()...)
		if failure := frame.GetError(); failure != nil {
			result.errFrame = failure
		}
		if end := frame.GetEnd(); end != nil {
			if result.ended {
				t.Fatal("duplicate End frame")
			}
			result.ended, result.received = true, end.GetBytesReceived()
		}
	}
	return result
}

func TestStreamReadFailuresAfterOutputUseFailedEvent(t *testing.T) {
	text := streamData(map[string]any{"type": "response.output_text.delta", "delta": "working"})
	tool := streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": relayNativeCall("call_pending", "get_weather", map[string]any{})})
	for _, tt := range []struct {
		name  string
		steps []relayReadStep
		max   int
		code  string
	}{
		{"eof", []relayReadStep{{data: text}, {data: tool, err: io.EOF}}, 1 << 20, "invalid_upstream_response"},
		{"unexpected_eof", []relayReadStep{{data: text}, {data: tool, err: io.ErrUnexpectedEOF}}, 1 << 20, "upstream_read"},
		{"timeout", []relayReadStep{{data: text}, {err: context.DeadlineExceeded}}, 1 << 20, "upstream_read"},
		{"read_error_with_data", []relayReadStep{{data: text, err: errors.New("private upstream diagnostic")}}, 1 << 20, "upstream_read"},
		{"size_limit", []relayReadStep{{data: text}, {data: tool}}, len(text), "upstream_response_too_large"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := runRelayReader(t, &relayStepReader{steps: tt.steps}, tt.max)
			if got := streamFailureCode(result); got != tt.code {
				t.Fatalf("failure code = %q, want %q", got, tt.code)
			}
			events := parsedStreamEvents(t, result)
			if len(events) != 2 || events[0]["delta"] != "working" || events[1]["type"] != "response.failed" {
				t.Fatalf("expected text followed by failure, got %s", result.body)
			}
			assertNoToolExecutionOnStreamError(t, result)
			if strings.Count(string(result.body), "data: [DONE]") != 1 || strings.Contains(string(result.body), "private upstream diagnostic") {
				t.Fatalf("invalid failure ending: %s", result.body)
			}
		})
	}
}

func TestStreamReadFailuresBeforeOutputKeepErrorFrame(t *testing.T) {
	for _, tt := range []struct {
		name string
		step relayReadStep
		max  int
		code string
	}{
		{"empty", relayReadStep{err: io.EOF}, 1024, "invalid_upstream_response"},
		{"read_error", relayReadStep{err: io.ErrUnexpectedEOF}, 1024, "upstream_read"},
		{"size_limit", relayReadStep{data: "too large"}, 1, "upstream_response_too_large"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := runRelayReader(t, &relayStepReader{steps: []relayReadStep{tt.step}}, tt.max)
			if result.errFrame == nil || result.errFrame.GetCode() != tt.code || !result.errFrame.GetRequestSent() || result.status != 0 || result.ended || len(result.body) != 0 {
				t.Fatalf("expected non-replayable error frame before output: %#v", result)
			}
		})
	}
}

func TestStreamTerminalDoesNotWaitForConnectionClose(t *testing.T) {
	for _, kind := range []string{"response.completed", "response.failed", "response.incomplete"} {
		for _, withDone := range []bool{false, true} {
			for _, readErr := range []error{nil, io.ErrUnexpectedEOF} {
				t.Run(kind, func(t *testing.T) {
					status := strings.TrimPrefix(kind, "response.")
					body := streamData(map[string]any{"type": kind, "response": map[string]any{"id": "resp_terminal", "status": status, "output": []any{}}})
					if withDone {
						body += "data: [DONE]" + string([]byte{10, 10})
					}
					reader := &relayStepReader{steps: []relayReadStep{{data: body, err: readErr}, {err: context.DeadlineExceeded}}}
					result := runRelayReader(t, reader, 1<<20)
					events := parsedStreamEvents(t, result)
					if reader.reads != 1 || len(events) != 1 || events[0]["type"] != kind || strings.Count(string(result.body), "data: [DONE]") != 1 {
						t.Fatalf("terminal response was not final: reads=%d body=%s", reader.reads, result.body)
					}
				})
			}
		}
	}
}
