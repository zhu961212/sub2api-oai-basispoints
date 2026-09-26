package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// Reading after the supplied bytes simulates a gateway that keeps its stream
// open until the request deadline. The completed event must prevent that read.
type degradationTailReader struct {
	data     []byte
	step     int
	err      error
	tailRead bool
	closed   bool
}

func (r *degradationTailReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		r.tailRead = true
		return 0, r.err
	}
	if r.step > 0 && len(p) > r.step {
		p = p[:r.step]
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (r *degradationTailReader) Close() error { r.closed = true; return nil }

func TestDegradationCheckStopsAtCompletedEventWithoutWaitingForEOF(t *testing.T) {
	for _, eventType := range []string{"response.completed", "response.done"} {
		for _, step := range []int{0, 1, 7} {
			t.Run(eventType+string(rune('0'+step)), func(t *testing.T) {
				data := streamData(map[string]any{"type": eventType, "response": map[string]any{"status": "completed", "output_text": "苹果17"}})
				body := &degradationTailReader{data: []byte(data), step: step, err: context.DeadlineExceeded}
				client := &http.Client{Transport: degradationRoundTripper(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
				})}
				tr := New()
				defer tr.Shutdown()
				host := &degradationTestHost{fakeHost: &fakeHost{token: token(t, "test-account")}}
				verdict, answer, err := tr.checkDegradationAccount(context.Background(), protocol.DefaultConfig(), host, client, 274, protocol.DefaultModelID)
				if err != nil || verdict != "ok" || answer != "苹果17" {
					t.Fatalf("completed answer lost: verdict=%s answer=%q err=%v", verdict, answer, err)
				}
				if body.tailRead || !body.closed {
					t.Fatalf("completed response read past terminal or leaked body: tail=%v closed=%v", body.tailRead, body.closed)
				}
			})
		}
	}
}

func TestDegradationHTTPErrorDoesNotWaitForBody(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		body := &degradationTailReader{err: errors.New("private upstream token and proxy")}
		client := &http.Client{Transport: degradationRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
		})}
		tr := New()
		host := &degradationTestHost{fakeHost: &fakeHost{token: token(t, "test-account")}}
		verdict, answer, err := tr.checkDegradationAccount(context.Background(), protocol.DefaultConfig(), host, client, 274, protocol.DefaultModelID)
		tr.Shutdown()
		if verdict != "error" || answer != "" || err == nil || !strings.Contains(err.Error(), "HTTP") || strings.Contains(err.Error(), "private") {
			t.Fatalf("HTTP error obscured: verdict=%s answer=%q err=%v", verdict, answer, err)
		}
		if body.tailRead || !body.closed {
			t.Fatal("HTTP failure body was read or was not closed")
		}
	}
}

func TestDegradationReaderRejectsIncompleteResponses(t *testing.T) {
	for name, data := range map[string]string{
		"partial text":             streamData(map[string]any{"type": "response.output_text.delta", "delta": "苹果16"}),
		"done without terminal":    "data: [DONE]" + string([]byte{10, 10}),
		"failed":                   streamData(map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "output_text": "苹果16"}}),
		"incomplete":               streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "incomplete", "output_text": "苹果16"}}),
		"missing response":         streamData(map[string]any{"type": "response.completed"}),
		"completed envelope error": streamData(map[string]any{"type": "response.completed", "error": map[string]any{"code": "unknown_failure"}, "response": map[string]any{"status": "completed", "output_text": "苹果17"}}),
		"unfinished envelope":      streamData(map[string]any{"type": "response.completed", "status": "incomplete", "response": map[string]any{"status": "completed", "output_text": "苹果16"}}),
	} {
		t.Run(name, func(t *testing.T) {
			body, kind, err := readDegradationResponse(context.Background(), strings.NewReader(data), "text/event-stream", 4096)
			if err == nil {
				_, err = degradationAnswer(body, kind)
			}
			if err == nil {
				t.Fatal("unfinished answer could be classified as degraded")
			}
		})
	}
}

func TestDegradationReaderSupportsJSONAndTerminalEOF(t *testing.T) {
	response := map[string]any{"status": "completed", "output_text": "苹果17"}
	stream := streamData(map[string]any{"type": "response.completed", "response": response})
	for _, entry := range []struct{ data, kind string }{
		{string(protocol.JSONBytes(response)), "application/json"},
		{strings.TrimRight(stream, string([]byte{10, 13})), "text/event-stream"},
		{stream, "application/octet-stream"},
	} {
		body, kind, err := readDegradationResponse(context.Background(), strings.NewReader(entry.data), entry.kind, 4096)
		if err != nil {
			t.Fatal(err)
		}
		answer, err := degradationAnswer(body, kind)
		if err != nil || answer != "苹果17" {
			t.Fatalf("answer=%q err=%v", answer, err)
		}
	}
}

func TestDegradationReaderRejectsConflictingFailureEvents(t *testing.T) {
	completed := streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output_text": "苹果17"}})
	for name, data := range map[string]string{
		"event overrides completed payload":     "event: error\n" + completed,
		"invalid error payload":                 "event: error\ndata: upstream broke\n\n" + completed,
		"error envelope before completed":       streamData(map[string]any{"error": map[string]any{"code": "unknown_failure"}}) + completed,
		"failed operation before completed":     streamData(map[string]any{"success": false}) + completed,
		"failed HTTP envelope before completed": streamData(map[string]any{"status_code": 500}) + completed,
	} {
		t.Run(name, func(t *testing.T) {
			reader := &degradationTailReader{data: []byte(data), step: 7, err: context.DeadlineExceeded}
			_, _, err := readDegradationResponse(context.Background(), reader, "text/event-stream", 4096)
			if err == nil || reader.tailRead {
				t.Fatalf("failure was accepted or waited for EOF: err=%v tail=%v", err, reader.tailRead)
			}
		})
	}
}

func TestDegradationReaderStopsAtCanceledTerminal(t *testing.T) {
	for _, kind := range []string{"response.cancelled", "response.canceled"} {
		reader := &degradationTailReader{data: []byte(streamData(map[string]any{"type": kind})), err: context.DeadlineExceeded}
		_, _, err := readDegradationResponse(context.Background(), reader, "text/event-stream", 4096)
		if err == nil || reader.tailRead || !strings.Contains(err.Error(), "did not complete") {
			t.Fatalf("canceled terminal waited for connection close: tail=%v err=%v", reader.tailRead, err)
		}
	}
}

func TestDegradationReaderKeepsLimitAndSanitizesFailures(t *testing.T) {
	_, _, err := readDegradationResponse(context.Background(), strings.NewReader(strings.Repeat("x", 100)), "application/json", 50)
	if err == nil || !strings.Contains(err.Error(), "configured limit") {
		t.Fatalf("limit lost: %v", err)
	}
	for _, entry := range []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "timed out"},
		{context.Canceled, "canceled"},
		{io.ErrUnexpectedEOF, "closed before"},
		{errors.New("private token and proxy"), "connection failed"},
	} {
		reader := &degradationTailReader{data: []byte("data: "), err: entry.err}
		_, _, err := readDegradationResponse(context.Background(), reader, "text/event-stream", 4096)
		if err == nil || !strings.Contains(err.Error(), entry.want) || strings.Contains(err.Error(), "private") {
			t.Fatalf("wrong or unsafe diagnostic: %v", err)
		}
	}
}
