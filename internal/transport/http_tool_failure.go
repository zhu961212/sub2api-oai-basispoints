package transport

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// HTTP error responses take a different path through the host than failures
// inside a successful SSE transport. Capture only a proven request-scoped
// failure before retrying it or allowing that path to change account state.
// The limit is no larger than the minimum configurable response limit.
const httpToolFailureProbeLimit = 64 << 10

const httpToolFailureProbeTimeout = time.Second

type httpRequestFailureBody struct {
	io.ReadCloser
	response map[string]any
}

type httpToolFailureReplayBody struct {
	io.Reader
	io.Closer
}

type httpToolFailureReadError struct{ err error }

func (r httpToolFailureReadError) Read([]byte) (int, error) { return 0, r.err }

func captureBasisPointsHTTPToolFailure(resp *http.Response) bool {
	return captureBasisPointsHTTPToolFailureWithin(resp, 0)
}

func captureBasisPointsHTTPToolFailureWithin(resp *http.Response, timeout time.Duration) bool {
	return captureHTTPFailureWithin(resp, timeout, canonicalHTTPToolFailure)
}

func captureHTTPFailureWithin(resp *http.Response, timeout time.Duration, classify func(map[string]any, string) map[string]any) bool {
	if resp == nil || resp.Body == nil || resp.StatusCode < 400 || resp.StatusCode > 599 ||
		resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 429 {
		return false
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	sse := strings.Contains(contentType, "text/event-stream")
	if !sse && contentType != "" && !strings.Contains(contentType, "json") {
		return false
	}
	original := resp.Body
	// Only an already-retriable attempt may close a slow error body early.
	// Non-retriable and final attempts retain the request's normal deadline,
	// so an unknown slow body remains available in full to the existing path.
	stopProbe := func() {}
	if timeout > 0 {
		probeClosed := make(chan struct{})
		timer := time.AfterFunc(timeout, func() {
			_ = original.Close()
			close(probeClosed)
		})
		stopProbe = func() {
			if !timer.Stop() {
				<-probeClosed
			}
		}
	}
	var prefix bytes.Buffer
	var readErr error
	var response map[string]any
	defer func() {
		stopProbe()
		if response != nil {
			_ = original.Close()
			resp.Body = &httpRequestFailureBody{
				ReadCloser: io.NopCloser(bytes.NewReader(protocol.JSONBytes(response))),
				response:   response,
			}
			resp.Header = resp.Header.Clone()
			if resp.Header == nil {
				resp.Header = make(http.Header)
			}
			resp.Header.Set("Content-Type", "application/json")
			resp.Header.Del("Content-Length")
			resp.Header.Del("Content-Encoding")
			resp.ContentLength = -1
			return
		}
		tail := io.Reader(original)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			tail = httpToolFailureReadError{readErr}
		}
		resp.Body = &httpToolFailureReplayBody{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), tail), Closer: original}
	}()
	if !sse {
		raw, err := io.ReadAll(io.LimitReader(original, httpToolFailureProbeLimit+1))
		prefix.Write(raw)
		readErr = err
		if err != nil || len(raw) > httpToolFailureProbeLimit {
			return false
		}
		object, err := protocol.RawObject(raw)
		if err == nil {
			response = classify(object, "error")
		}
		return response != nil
	}
	stop := errors.New("HTTP failure probe complete")
	decoder := &sseRelayDecoder{}
	consume := func(event sseRelayEvent) error {
		if strings.TrimSpace(event.data) == "" && event.event == "" {
			return nil
		}
		object, err := protocol.RawObject([]byte(event.data))
		if err != nil {
			return stop
		}
		response = classify(object, event.event)
		if response != nil {
			return stop
		}
		// Only harmless lifecycle metadata can precede the failure. Output,
		// conflicting terminals or arbitrary records never become evidence.
		kind := event.event
		if kind == "" {
			kind = protocol.StringValue(object["type"])
		}
		if kind != "response.created" && kind != "response.in_progress" {
			return stop
		}
		if dataKind := protocol.StringValue(object["type"]); dataKind != "" && dataKind != kind {
			return stop
		}
		for _, key := range []string{"item", "delta", "arguments", "input", "output", "content"} {
			if object[key] != nil {
				return stop
			}
		}
		if protocol.ClassifyResponseTerminal(event.event, object) != protocol.TerminalNone {
			return stop
		}
		nested := relayObject(object["response"])
		if output, exists := nested["output"]; exists {
			items, ok := output.([]any)
			if !ok || len(items) != 0 {
				return stop
			}
		}
		return nil
	}
	buffer := make([]byte, 4096)
	for prefix.Len() < httpToolFailureProbeLimit {
		n, err := original.Read(buffer[:min(len(buffer), httpToolFailureProbeLimit-prefix.Len())])
		prefix.Write(buffer[:n])
		readErr = err
		decoded := decoder.feed(buffer[:n], consume)
		if decoded == nil && errors.Is(err, io.EOF) {
			decoded = decoder.feed([]byte{10, 10}, consume)
		}
		if decoded != nil || err != nil {
			return response != nil
		}
		if n == 0 {
			return false
		}
	}
	return false
}

func canonicalHTTPToolFailure(payload map[string]any, event string) map[string]any {
	if protocol.ClassifyResponseTerminal(event, payload) != protocol.TerminalFailed || basisPointsFailureStatus(payload, event) != 0 {
		return nil
	}
	found := false
	nested := relayObject(payload["response"])
	// The persistent 403 observer also inspects nested detail. Do not erase
	// that signal (or explicit auth/quota detail) while rebuilding a failure.
	if basisPointsExplicitForbiddenStatus(payload, event) {
		return nil
	}
	for _, object := range []map[string]any{payload, nested} {
		if detail := relayObject(object["detail"]); detail != nil && basisPointsFailureStatus(map[string]any{"error": detail}, "error") != 0 {
			return nil
		}
	}
	for _, object := range []map[string]any{payload, nested} {
		for _, candidate := range []map[string]any{object, relayObject(object["error"])} {
			if code := protocol.StringValue(candidate["code"]); code != "" {
				if code != "invalid_tool_call" {
					return nil
				}
				found = true
			}
		}
		if object["error"] != nil && relayObject(object["error"]) == nil {
			return nil
		}
	}
	if !found {
		return nil
	}
	protocol.NormalizeClientToolFailure(payload, event)
	diagnostic := protocol.ResponseTerminalError(protocol.TerminalFailed, payload)
	var apiError *protocol.APIError
	if !errors.As(diagnostic, &apiError) || apiError.Kind != "invalid_tool_call" {
		return nil
	}
	result := map[string]any{
		"object": "response", "status": "failed", "output": []any{},
		"error": map[string]any{"type": "invalid_request_error", "code": "invalid_tool_call", "message": apiError.Message},
	}
	if reason := apiError.DiagnosticReason(); reason != "" {
		relayObject(result["error"])["reason"] = reason
	}
	if nested == nil {
		nested = payload
	}
	for _, key := range []string{"id", "model", "created_at", "usage"} {
		if value, ok := nested[key]; ok {
			result[key] = value
		}
	}
	return result
}
