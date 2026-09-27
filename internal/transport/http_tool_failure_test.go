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
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

const httpFailureTestMessage = "Basis Points returned a malformed or ambiguous client tool relay envelope"

type httpFailureTestBody struct {
	data                     []byte
	maxRead                  int
	readErr                  error
	errWithFinalData         bool
	reads, afterData, closes int
}

func (b *httpFailureTestBody) Read(p []byte) (int, error) {
	b.reads++
	if len(b.data) == 0 {
		b.afterData++
		if b.readErr != nil {
			return 0, b.readErr
		}
		return 0, io.EOF
	}
	n := min(len(p), len(b.data))
	if b.maxRead > 0 {
		n = min(n, b.maxRead)
	}
	copy(p, b.data[:n])
	b.data = b.data[n:]
	if len(b.data) == 0 && b.errWithFinalData {
		return n, b.readErr
	}
	return n, nil
}
func (b *httpFailureTestBody) Close() error { b.closes++; return nil }
func httpFailureTestPayload(message string) map[string]any {
	return map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_probe", "model": "m中文", "status": "failed", "output": []any{relayNativeCall("call_must_not_escape", "functions.exec", "PRIVATE TOOL INPUT")}, "error": map[string]any{"code": "invalid_tool_call", "message": message, "details": "PRIVATE DIAGNOSTIC"}}}
}
func httpFailureTestResponse(raw []byte, sse bool, body *httpFailureTestBody) *http.Response {
	body.data = append([]byte(nil), raw...)
	contentType := "application/json"
	if sse {
		contentType = "text/event-stream"
	}
	return &http.Response{StatusCode: 502, Header: http.Header{"Content-Type": {contentType}}, Body: body}
}

func TestHTTPToolFailureCompleteSSEDoesNotWaitForEOF(t *testing.T) {
	for _, withDataErr := range []bool{false, true} {
		t.Run(fmt.Sprintf("error_with_data=%t", withDataErr), func(t *testing.T) {
			sentinel := errors.New("read after terminal must not happen")
			raw := []byte(streamData(httpFailureTestPayload(httpFailureTestMessage)))
			body := &httpFailureTestBody{readErr: sentinel, errWithFinalData: withDataErr}
			resp := httpFailureTestResponse(raw, true, body)
			if !captureBasisPointsHTTPToolFailure(resp) {
				t.Fatal("complete SSE tool failure was not captured")
			}
			if body.reads != 1 || body.afterData != 0 || body.closes != 1 {
				t.Fatalf("read beyond terminal or failed to close: reads=%d tail=%d closes=%d", body.reads, body.afterData, body.closes)
			}
			captured, ok := resp.Body.(*basisPointsHTTPToolFailureBody)
			if !ok || captured.response["status"] != "failed" {
				t.Fatal("missing marked canonical failure")
			}
			raw, err := io.ReadAll(resp.Body)
			if err != nil || strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "run_officejs") {
				t.Fatalf("canonical body leaked or failed: %s %v", raw, err)
			}
			_ = resp.Body.Close()
			if body.closes != 1 {
				t.Fatal("captured wrapper closed original twice")
			}
		})
	}
}

func TestHTTPToolFailureFragmentedUnicodeAndMetadata(t *testing.T) {
	created := streamData(map[string]any{"type": "response.created", "response": map[string]any{"id": "前缀é", "status": "in_progress", "output": []any{}}})
	progress := streamData(map[string]any{"type": "response.in_progress", "response": map[string]any{"status": "in_progress", "output": []any{}}})
	raw := []byte(": keepalive\n\n" + created + progress + streamData(httpFailureTestPayload(httpFailureTestMessage)))
	for _, size := range []int{1, 2, 7, 4096} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			body := &httpFailureTestBody{maxRead: size, readErr: errors.New("no EOF needed")}
			resp := httpFailureTestResponse(raw, true, body)
			if !captureBasisPointsHTTPToolFailure(resp) {
				t.Fatal("fragmented Unicode/metadata prevented capture")
			}
			marked := resp.Body.(*basisPointsHTTPToolFailureBody)
			if marked.response["model"] != "m中文" || body.afterData != 0 || body.closes != 1 {
				t.Fatal("fragmented metadata changed bytes or read past terminal")
			}
		})
	}
}

func TestHTTPToolFailureRestoresUnmatchedBytesAndErrors(t *testing.T) {
	ordinary := []byte(`{"error":{"code":"server_error","message":"ordinary"}}`)
	output := []byte(streamData(map[string]any{"type": "response.output_text.delta", "delta": "invalid_tool_call"}) + streamData(httpFailureTestPayload(httpFailureTestMessage)))
	for _, test := range []struct {
		name      string
		raw       []byte
		sse       bool
		wantError bool
	}{
		{"json_unmatched", ordinary, false, false}, {"sse_output_before_failure", output, true, false},
		{"json_over_limit", []byte(strings.Repeat(" ", httpToolFailureProbeLimit+8) + string(protocol.JSONBytes(httpFailureTestPayload(httpFailureTestMessage)))), false, false},
		{"sse_over_limit", []byte(":" + strings.Repeat("x", httpToolFailureProbeLimit+8) + "\n\n" + streamData(httpFailureTestPayload(httpFailureTestMessage))), true, false},
		{"json_read_error", ordinary, false, true}, {"sse_read_error", output, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sentinel := errors.New("original read failure")
			body := &httpFailureTestBody{}
			if test.wantError {
				body.readErr = sentinel
				body.errWithFinalData = true
			}
			resp := httpFailureTestResponse(test.raw, test.sse, body)
			contentType := resp.Header.Get("Content-Type")
			if captureBasisPointsHTTPToolFailure(resp) {
				t.Fatal("unmatched or incomplete evidence captured")
			}
			if body.closes != 0 || resp.StatusCode != 502 || resp.Header.Get("Content-Type") != contentType {
				t.Fatal("probe changed unrecognized response ownership or headers")
			}
			got, err := io.ReadAll(resp.Body)
			if !bytes.Equal(got, test.raw) || test.wantError != errors.Is(err, sentinel) {
				t.Fatalf("replay lost bytes/error: len=%d want=%d err=%v", len(got), len(test.raw), err)
			}
			if !test.wantError && err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if body.closes != 1 {
				t.Fatal("replay did not close original body exactly once")
			}
		})
	}
}

func TestHTTPToolFailureAccountAndConflictBoundaries(t *testing.T) {
	for _, status := range []int{200, 302, 401, 403, 429} {
		body := &httpFailureTestBody{}
		resp := httpFailureTestResponse(protocol.JSONBytes(httpFailureTestPayload(httpFailureTestMessage)), false, body)
		resp.StatusCode = status
		if captureBasisPointsHTTPToolFailure(resp) || body.reads != 0 || body.closes != 0 || resp.Body != body {
			t.Fatalf("HTTP %d was inspected or captured", status)
		}
	}
	for _, variation := range []string{"quota", "auth", "explicit_forbidden", "detail_401", "detail_403", "detail_429", "conflicting_outer_code", "conflicting_nested_code", "string_error", "output_only"} {
		t.Run(variation, func(t *testing.T) {
			payload := httpFailureTestPayload(httpFailureTestMessage)
			nested := relayObject(payload["response"])
			failure := relayObject(nested["error"])
			switch variation {
			case "quota":
				failure["message"] = "rate limit exceeded"
			case "auth":
				failure["message"] = "invalid_api_key"
			case "explicit_forbidden":
				failure["status_code"] = 403
			case "detail_401":
				nested["detail"] = map[string]any{"status_code": 401}
			case "detail_403":
				nested["detail"] = map[string]any{"status_code": 403}
			case "detail_429":
				nested["detail"] = map[string]any{"status_code": 429}
			case "conflicting_outer_code":
				payload["error"] = map[string]any{"code": "server_error"}
			case "conflicting_nested_code":
				nested["code"] = "server_error"
			case "string_error":
				payload["error"] = "invalid_tool_call"
			case "output_only":
				delete(nested, "error")
				nested["status"] = "completed"
				payload["type"] = "response.completed"
				nested["output"] = []any{map[string]any{"error": map[string]any{"code": "invalid_tool_call"}, "text": "invalid_tool_call"}}
			}
			for _, sse := range []bool{false, true} {
				raw := protocol.JSONBytes(payload)
				if sse {
					raw = []byte(streamData(payload))
				}
				body := &httpFailureTestBody{}
				resp := httpFailureTestResponse(raw, sse, body)
				if captureBasisPointsHTTPToolFailure(resp) {
					t.Fatal("account/conflicting/output code bypassed strict evidence")
				}
				got, err := io.ReadAll(resp.Body)
				if err != nil || !bytes.Equal(got, raw) {
					t.Fatal("rejection damaged replay body")
				}
				_ = resp.Body.Close()
			}
		})
	}
}

func TestHTTPToolFailureCanonicalDiagnosticWhitelist(t *testing.T) {
	unknown := "Basis Points returned an unknown client tool absent from the active catalog"
	for _, message := range []string{httpFailureTestMessage, unknown, "PRIVATE source", httpFailureTestMessage + " PRIVATE source"} {
		payload := httpFailureTestPayload(message)
		result := canonicalHTTPToolFailure(payload, "response.failed")
		if result == nil {
			t.Fatal("known tool failure not canonicalized")
		}
		want := message
		if strings.Contains(message, "PRIVATE") {
			want = "Basis Points returned an invalid client tool call"
		}
		failure := relayObject(result["error"])
		output, ok := result["output"].([]any)
		if failure["message"] != want || failure["code"] != "invalid_tool_call" || failure["type"] != "invalid_request_error" || !ok || len(output) != 0 || strings.Contains(string(protocol.JSONBytes(result)), "PRIVATE") {
			t.Fatalf("unsafe canonical failure: %#v", result)
		}
	}
}

func TestHTTPToolFailureRejectsConflictingLifecyclePrefix(t *testing.T) {
	for _, kind := range []string{"response.output_item.added", "response.output_text.delta"} {
		prefix := "event: response.created\n" + streamData(map[string]any{"type": kind, "item": relayNativeCall("early_tool", "functions.exec", "PRIVATE TOOL INPUT"), "response": map[string]any{"status": "in_progress", "output": []any{}}})
		raw := []byte(prefix + streamData(httpFailureTestPayload(httpFailureTestMessage)))
		body := &httpFailureTestBody{}
		resp := httpFailureTestResponse(raw, true, body)
		if captureBasisPointsHTTPToolFailure(resp) {
			t.Fatalf("conflicting %s lifecycle prefix was treated as harmless", kind)
		}
		got, err := io.ReadAll(resp.Body)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatal("rejected lifecycle prefix lost bytes")
		}
		_ = resp.Body.Close()
	}
}

type httpFailureBlockingBody struct {
	prefix              *bytes.Reader
	ctx                 context.Context
	blocked, closed     chan struct{}
	readOnce, closeOnce sync.Once
	afterData, closes   atomic.Int32
}

func (b *httpFailureBlockingBody) Read(p []byte) (int, error) {
	if b.prefix.Len() != 0 {
		return b.prefix.Read(p)
	}
	b.afterData.Add(1)
	b.readOnce.Do(func() { close(b.blocked) })
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.closed:
		return 0, io.ErrClosedPipe
	}
}
func (b *httpFailureBlockingBody) Close() error {
	b.closeOnce.Do(func() { b.closes.Add(1); close(b.closed) })
	return nil
}

func TestHTTPToolFailureCompleteSSELeavesBlockingTailUnread(t *testing.T) {
	raw := []byte(streamData(httpFailureTestPayload(httpFailureTestMessage)))
	body := &httpFailureBlockingBody{prefix: bytes.NewReader(raw), ctx: context.Background(), blocked: make(chan struct{}), closed: make(chan struct{})}
	defer body.Close()
	resp := &http.Response{StatusCode: 502, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}
	done := make(chan bool, 1)
	start := time.Now()
	go func() { done <- captureBasisPointsHTTPToolFailure(resp) }()
	select {
	case captured := <-done:
		if !captured {
			t.Fatal("complete terminal was not captured")
		}
	case <-time.After(500 * time.Millisecond):
		_ = body.Close()
		t.Fatal("complete terminal waited for a blocking tail or probe timeout")
	}
	if body.afterData.Load() != 0 || body.closes.Load() != 1 {
		t.Fatal("complete terminal read blocked tail or failed to close")
	}
	t.Logf("complete terminal returned in %s", time.Since(start))
}

func TestHTTPToolFailureProbeHonorsRequestCancellation(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%t", sse), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			prefix := []byte(`{"error":`)
			contentType := "application/json"
			if sse {
				prefix = []byte(streamData(map[string]any{"type": "response.created", "response": map[string]any{"status": "in_progress", "output": []any{}}}))
				contentType = "text/event-stream"
			}
			body := &httpFailureBlockingBody{prefix: bytes.NewReader(prefix), ctx: ctx, blocked: make(chan struct{}), closed: make(chan struct{})}
			defer body.Close()
			resp := &http.Response{StatusCode: 502, Header: http.Header{"Content-Type": {contentType}}, Body: body}
			done := make(chan bool, 1)
			go func() { done <- captureBasisPointsHTTPToolFailure(resp) }()
			select {
			case <-body.blocked:
			case <-time.After(500 * time.Millisecond):
				t.Fatal("probe did not reach cancellable read")
			}
			cancel()
			select {
			case captured := <-done:
				if captured {
					t.Fatal("incomplete response became failure evidence")
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("probe ignored request cancellation")
			}
			got, err := io.ReadAll(resp.Body)
			if !bytes.Equal(got, prefix) || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation replay lost prefix/error: %q %v", got, err)
			}
			_ = resp.Body.Close()
			if body.closes.Load() != 1 {
				t.Fatal("canceled replay did not close original")
			}
		})
	}
}

func TestHTTPToolFailureSlowUnmatchedFinalBodiesRemainComplete(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		allowRetry bool
		wantCalls  int32
	}{
		{"non_retryable_status", 400, true, 1},
		{"retry_explicitly_disabled", 502, false, 1},
		{"final_attempt", 502, true, basisPointsHTTPAttempts},
	} {
		for _, sse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/sse=%t", test.name, sse), func(t *testing.T) {
				t.Parallel()
				ordinary := map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "output": []any{}, "error": map[string]any{"code": "server_error", "message": "ordinary delayed failure"}}}
				raw := protocol.JSONBytes(ordinary)
				prefix := raw[:len(raw)/2]
				tail := raw[len(prefix):]
				contentType := "application/json"
				if sse {
					prefix = []byte(streamData(map[string]any{"type": "response.created", "response": map[string]any{"status": "in_progress", "output": []any{}}}))
					tail = []byte(streamData(ordinary))
					raw = append(append([]byte(nil), prefix...), tail...)
					contentType = "text/event-stream"
				}
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					call := calls.Add(1)
					w.Header().Set("Content-Type", contentType)
					if !test.allowRetry {
						w.Header().Set("X-Should-Retry", "false")
					}
					w.WriteHeader(test.status)
					if call < test.wantCalls {
						_, _ = w.Write(raw)
						return
					}
					_, _ = w.Write(prefix)
					w.(http.Flusher).Flush()
					timer := time.NewTimer(httpToolFailureProbeTimeout + 250*time.Millisecond)
					defer timer.Stop()
					select {
					case <-r.Context().Done():
						return
					case <-timer.C:
						_, _ = w.Write(tail)
					}
				}))
				defer server.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader([]byte("{}")))
				if err != nil {
					t.Fatal(err)
				}
				response, err := doBasisPointsRequest(server.Client(), request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				got, err := io.ReadAll(response.Body)
				if err != nil || !bytes.Equal(got, raw) || response.StatusCode != test.status || calls.Load() != test.wantCalls {
					t.Fatalf("slow unmatched body changed: status=%d calls=%d bytes=%d/%d err=%v", response.StatusCode, calls.Load(), len(got), len(raw), err)
				}
			})
		}
	}
}

func TestHTTPToolFailureRetryableProbeTimeoutClosesAndReplays(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%t", sse), func(t *testing.T) {
			prefix := []byte("{")
			contentType := "application/json"
			if sse {
				prefix = []byte(streamData(map[string]any{"type": "response.created", "response": map[string]any{"status": "in_progress", "output": []any{}}}))
				contentType = "text/event-stream"
			}
			body := &httpFailureBlockingBody{prefix: bytes.NewReader(prefix), ctx: context.Background(), blocked: make(chan struct{}), closed: make(chan struct{})}
			defer body.Close()
			response := &http.Response{StatusCode: 502, Header: http.Header{"Content-Type": {contentType}}, Body: body}
			done := make(chan bool, 1)
			go func() { done <- captureBasisPointsHTTPToolFailureWithin(response, 25*time.Millisecond) }()
			select {
			case captured := <-done:
				if captured {
					t.Fatal("timed out incomplete body was captured")
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("retryable probe timeout did not unblock read")
			}
			got, err := io.ReadAll(response.Body)
			if !bytes.Equal(got, prefix) || !errors.Is(err, io.ErrClosedPipe) || body.closes.Load() != 1 {
				t.Fatalf("timed probe lost prefix/error or close: bytes=%d/%d closes=%d err=%v", len(got), len(prefix), body.closes.Load(), err)
			}
		})
	}
}

func TestHTTPToolFailureRetryableSlowProbeAllowsNextAttempt(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%t", sse), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			firstClosed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) != 1 {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, "{}")
					return
				}
				prefix := []byte("{")
				contentType := "application/json"
				if sse {
					prefix = []byte(streamData(map[string]any{"type": "response.created", "response": map[string]any{"status": "in_progress", "output": []any{}}}))
					contentType = "text/event-stream"
				}
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write(prefix)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(firstClosed)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader([]byte("{}")))
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			response, err := doBasisPointsRequest(server.Client(), request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			got, err := io.ReadAll(response.Body)
			if err != nil || string(got) != "{}" || response.StatusCode != 200 || calls.Load() != 2 {
				t.Fatalf("retry did not reach healthy second response: status=%d calls=%d err=%v", response.StatusCode, calls.Load(), err)
			}
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Fatalf("retry waited beyond probe and backoff allowance: %s", elapsed)
			}
			select {
			case <-firstClosed:
			case <-time.After(500 * time.Millisecond):
				t.Fatal("retryable timed out response was not closed")
			}
		})
	}
}
