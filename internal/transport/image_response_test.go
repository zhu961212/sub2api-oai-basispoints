package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestImageRelaySuccessfulHTTPFailureRedaction(t *testing.T) {
	for _, transform := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("transform=%t/stream=%t", transform, streaming), func(t *testing.T) {
				imageBytes, image := relayTestImage(t)
				var forwardedID string
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/attachments" {
						relayTestUpload(t, w, r, imageBytes, "file-private-native-A1_2")
						return
					}
					body, _ := io.ReadAll(r.Body)
					source, _ := protocol.RawObject(body)
					urls := relayTestFileIDs(source)
					if len(urls) != 1 {
						t.Errorf("expected one forwarded image, got %d", len(urls))
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					forwardedID = urls[0]
					response := map[string]any{"id": "resp_failed", "status": "failed", "error": map[string]any{"code": "image_fetch_error", "message": "cannot fetch " + forwardedID + "; previous /api/bps-images/private-legacy-token"}, "output": []any{}}
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "event: response.failed\ndata: %s\n\ndata: [DONE]\n\n", protocol.JSONBytes(map[string]any{"type": "response.failed", "response": response}))
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(protocol.JSONBytes(response))
					}
				}))
				defer upstream.Close()
				transport := New()
				defer transport.Shutdown()
				cfg := map[string]any{"responses_url": upstream.URL + "/responses"}
				cfg["rewrite_tools"], cfg["transform_responses"] = false, transform
				applyConfig(t, transport, cfg)
				source, _ := protocol.RawObject(relayTestBody(image))
				source["stream"] = streaming
				result := runForward(t, transport, requestFrames(t, "https://unused.invalid/responses", token(t, "acct-image-failure"), nil, protocol.JSONBytes(source)))
				if result.errFrame != nil || result.status != http.StatusOK || !result.ended {
					t.Fatalf("status=%d error=%v ended=%t body=%s", result.status, result.errFrame, result.ended, result.body)
				}
				if forwardedID != "file-private-native-A1_2" || bytes.Contains(result.body, []byte(forwardedID)) || bytes.Contains(result.body, []byte("private-legacy-token")) || !bytes.Contains(result.body, []byte("file-[redacted]")) || !bytes.Contains(result.body, []byte("/api/bps-images/[redacted]")) {
					t.Fatalf("failure leaked image URL or lost diagnostic: %s", result.body)
				}
				if !bytes.Contains(result.body, []byte("failed")) || bytes.Contains(result.body, []byte("response.completed")) {
					t.Fatalf("failed response became success: %s", result.body)
				}
			})
		}
	}
}

func TestImageFailureRedactionPreservesOutput(t *testing.T) {
	url := "https://images.example.test/api/bps-images/secret-image-token"
	output := []any{map[string]any{"type": "function_call", "arguments": url}, map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": url}}}}
	raw := protocol.JSONBytes(map[string]any{"status": "failed", "error": map[string]any{"message": url}, "output": output, "usage": map[string]any{"tokens": 9007199254740993}})
	redacted, changed := redactImageFailureJSON(raw, "")
	if !changed || bytes.Count(redacted, []byte(url)) != 2 || !bytes.Contains(redacted, []byte("9007199254740993")) {
		t.Fatalf("normal output or integer changed: %s", redacted)
	}
	for _, raw := range [][]byte{
		[]byte(" { \"status\": \"completed\", \"output_text\": \"" + url + "\" } \n"),
		[]byte("{malformed}"),
	} {
		redacted, changed := redactImageFailureJSON(raw, "")
		if changed || !bytes.Equal(raw, redacted) {
			t.Fatalf("normal response bytes changed: %q", redacted)
		}
	}
}

func imageResponseResult(stub *streamStub) forwardResult {
	var result forwardResult
	for _, frame := range stub.responses {
		if start := frame.GetStart(); start != nil {
			result.status, result.headers = int(start.GetStatusCode()), start.GetHeaders()
		}
		result.body = append(result.body, frame.GetBodyChunk()...)
		if failure := frame.GetError(); failure != nil {
			result.errFrame = failure
		}
		if end := frame.GetEnd(); end != nil {
			result.ended, result.received = true, end.GetBytesReceived()
		}
	}
	return result
}

func imageStreamResponse(body io.ReadCloser) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}
}

func TestImageErrorStreamSplitRecordsAndTail(t *testing.T) {
	normal := "id: normal\r\n: keepalive\r\nevent: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"/api/bps-images/keep-this-output\"}\r\n\r\n"
	failure := "id: failure\r\nevent: response.failed\r\ndata: {\"type\":\"response.failed\",\r\ndata: \"response\":{\"status\":\"failed\",\"error\":{\"message\":\"/api/bps-images/remove-secret\"}}}\r\n\r\n"
	tail := "event: error\ndata: failed to load /api/bps-images/remove-tail"
	wire := normal + failure + tail
	var steps []relayReadStep
	for i := range wire {
		steps = append(steps, relayReadStep{data: wire[i : i+1]})
	}
	stub := &streamStub{ctx: context.Background()}
	if err := sendImageSafeHTTPResponseStream(stub, imageStreamResponse(io.NopCloser(&relayStepReader{steps: steps})), 1<<20); err != nil {
		t.Fatal(err)
	}
	result := imageResponseResult(stub)
	if result.errFrame != nil || !result.ended || result.received != int64(len(result.body)) {
		t.Fatalf("invalid result: %+v", result)
	}
	if !bytes.HasPrefix(result.body, []byte(normal)) || !bytes.Contains(result.body, []byte("id: failure\r\nevent: response.failed\r\n")) || bytes.Contains(result.body, []byte("remove-secret")) || bytes.Contains(result.body, []byte("remove-tail")) || bytes.Count(result.body, []byte("[redacted]")) != 2 {
		t.Fatalf("incorrect event redaction: %s", result.body)
	}
	if bytes.HasSuffix(result.body, []byte("\n")) || bytes.Contains(result.body, []byte("[DONE]")) {
		t.Fatalf("EOF invented a terminal delimiter: %q", result.body)
	}
}

type imageReadCloser struct {
	io.Reader
	closed bool
}

func (r *imageReadCloser) Close() error { r.closed = true; return nil }

func TestImageErrorStreamBoundsAndReadFailure(t *testing.T) {
	normal := "event: ping\ndata: {}\n\n"
	for _, tt := range []struct {
		name, wire string
		max        int
		readErr    error
		code       string
	}{
		{"total", normal + normal, len(normal) + 1, io.EOF, "upstream_response_too_large"},
		{"pending_event", "data: " + strings.Repeat("x", maxImageErrorEventBytes), 16 << 20, nil, "upstream_response_too_large"},
		{"complete_event", "data: " + strings.Repeat("x", maxImageErrorEventBytes) + "\n\n", 16 << 20, nil, "upstream_response_too_large"},
		{"read_failure", normal, 1 << 20, io.ErrUnexpectedEOF, "upstream_read"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := &streamStub{ctx: context.Background()}
			body := &imageReadCloser{Reader: &relayStepReader{steps: []relayReadStep{{data: tt.wire, err: tt.readErr}}}}
			if err := sendImageSafeHTTPResponseStream(stub, imageStreamResponse(body), tt.max); err != nil {
				t.Fatal(err)
			}
			result := imageResponseResult(stub)
			if result.ended || result.errFrame == nil || result.errFrame.GetCode() != tt.code || !body.closed {
				t.Fatalf("invalid bounded failure: %+v closed=%t", result, body.closed)
			}
		})
	}
}

type imageFailingStream struct {
	streamStub
	bodySend func()
	failure  error
}

func (s *imageFailingStream) Send(frame *pluginv1.ForwardResponse) error {
	if len(frame.GetBodyChunk()) > 0 {
		if s.bodySend != nil {
			s.bodySend()
		}
		if s.failure != nil {
			return s.failure
		}
	}
	return s.streamStub.Send(frame)
}

func TestImageErrorStreamSendsBeforeNextReadAndStopsOnSendFailure(t *testing.T) {
	reader := &relayStepReader{steps: []relayReadStep{{data: "data: {}\n\n"}, {data: "data: {}\n\n"}}}
	body := &imageReadCloser{Reader: reader}
	failure := errors.New("client stopped reading")
	stub := &imageFailingStream{streamStub: streamStub{ctx: context.Background()}, failure: failure, bodySend: func() {
		if reader.reads != 1 {
			t.Fatalf("buffered response before send: reads=%d", reader.reads)
		}
	}}
	if err := sendImageSafeHTTPResponseStream(stub, imageStreamResponse(body), 1<<20); !errors.Is(err, failure) {
		t.Fatalf("Send failure lost: %v", err)
	}
	if reader.reads != 1 || !body.closed || len(stub.responses) != 1 {
		t.Fatalf("continued after send failure: reads=%d closed=%t frames=%d", reader.reads, body.closed, len(stub.responses))
	}
}

type imageBlockingBody struct {
	closed chan struct{}
	once   sync.Once
}

func (r *imageBlockingBody) Read([]byte) (int, error) { <-r.closed; return 0, context.Canceled }
func (r *imageBlockingBody) Close() error             { r.once.Do(func() { close(r.closed) }); return nil }

func TestImageErrorStreamCancellationClosesUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &imageBlockingBody{closed: make(chan struct{})}
	stub := &streamStub{ctx: ctx}
	done := make(chan error, 1)
	go func() { done <- sendImageSafeHTTPResponseStream(stub, imageStreamResponse(body), 1<<20) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request kept reading upstream")
	}
	result := imageResponseResult(stub)
	if result.ended || result.errFrame == nil || result.errFrame.GetCode() != "upstream_read" {
		t.Fatalf("cancellation became successful End: %+v", result)
	}
}
