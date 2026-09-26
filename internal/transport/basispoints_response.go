package transport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// Only BPS callers use this boundary. The host interprets Codex quota headers
// and HTTP-200 error payloads as account state, even when an HTTP 429 is absent.
// Ordinary output, tool arguments, session headers, and host passthrough stay
// untouched. Streaming input remains incremental and bounded.
func prepareBasisPointsResponse(resp *http.Response, max int, observers ...func(int)) error {
	reportBasisPointsStatus(resp.StatusCode, observers)
	headers := resp.Header.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	for key := range headers {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "x-codex-primary-") ||
			strings.HasPrefix(lower, "x-codex-secondary-") ||
			strings.HasPrefix(lower, "x-codex-credits-") ||
			strings.HasPrefix(lower, "x-ratelimit-") ||
			strings.HasPrefix(lower, "ratelimit-") || lower == "retry-after" {
			delete(headers, key)
		}
	}
	resp.Header = headers
	if strings.Contains(strings.ToLower(headers.Get("Content-Type")), "text/event-stream") {
		resp.Body = &basisPointsResponseReader{body: resp.Body, max: max, observers: observers}
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		return nil
	}
	raw, err := readLimited(resp.Body, max)
	if err != nil {
		return err
	}
	failureEvent := ""
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failureEvent = "error"
	}
	raw, _ = isolateBasisPointsFailureJSON(raw, failureEvent, observers...)
	_ = resp.Body.Close()
	resp.Body = newBasisPointsBufferedBody(raw)
	resp.ContentLength = int64(len(raw))
	resp.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	return nil
}

// Inspect failure diagnostics only, never output text or tool arguments.
func basisPointsFailureStatus(object map[string]any, event string) int {
	response := relayObject(object["response"])
	failure := imageFailureKind(event) || imageFailureKind(protocol.StringValue(object["type"])) ||
		imageFailureKind(protocol.StringValue(object["status"])) || object["error"] != nil
	if response != nil {
		failure = failure || imageFailureKind(protocol.StringValue(response["status"])) || response["error"] != nil
	}
	if !failure {
		return 0
	}
	fields := []map[string]any{object, relayObject(object["error"]), relayObject(object["detail"])}
	if response != nil {
		fields = append(fields, response, relayObject(response["error"]))
	}
	var diagnostics []string
	for _, field := range fields {
		for _, key := range []string{"status_code", "status"} {
			var status int
			switch value := field[key].(type) {
			case json.Number:
				status, _ = strconv.Atoi(string(value))
			case float64:
				status = int(value)
			case int:
				status = value
			case string:
				status, _ = strconv.Atoi(value)
			}
			if status == 401 || status == 403 || status == 429 {
				return status
			}
		}
		for _, key := range []string{"code", "type", "message"} {
			diagnostics = append(diagnostics, strings.ToLower(protocol.StringValue(field[key])))
		}
	}
	combined := strings.Join(diagnostics, " ")
	for _, marker := range []string{"rate_limit", "usage_limit_reached", "quota_exceeded"} {
		if strings.Contains(combined, marker) {
			return 429
		}
	}
	for _, marker := range []string{"authentication", "unauthorized", "invalid_api_key", "api_key_disabled", "invalid_token", "access_token_invalid", "token_revoked", "token_invalidated", "invalid_credentials", "credential_invalid"} {
		if strings.Contains(combined, marker) {
			return 401
		}
	}
	for _, marker := range []string{"permission", "forbidden", "access denied", "deactivated_workspace"} {
		if strings.Contains(combined, marker) {
			return 403
		}
	}
	for _, subject := range []string{"workspace", "account", "organization", "org"} {
		for _, state := range []string{"deactivated", "disabled", "suspended"} {
			if strings.Contains(combined, subject+"_"+state) || strings.Contains(combined, state+"_"+subject) {
				return 403
			}
		}
	}
	return 0
}

func isolateBasisPointsFailureJSON(raw []byte, event string, observers ...func(int)) ([]byte, bool) {
	if !mayContainResponseFailure(raw, event) {
		return raw, false
	}
	object, err := protocol.RawObject(raw)
	if err != nil {
		return raw, false
	}
	if !isolateBasisPointsFailureObject(object, event, observers...) {
		return raw, false
	}
	return protocol.JSONBytes(object), true
}

// A fast negative check only: any escaped string takes the full JSON path, so
// escaped keys and error kinds cannot bypass isolation. Marker-looking user
// output merely causes the normal parser to run; it is never rewritten here.
func mayContainResponseFailure(raw []byte, event string) bool {
	if imageFailureKind(event) || bytes.IndexByte(raw, 92) >= 0 {
		return true
	}
	for _, marker := range responseFailureMarkers {
		if bytes.Contains(raw, marker) {
			return true
		}
	}
	return false
}

var responseFailureMarkers = [][]byte{
	// Values are trimmed by protocol.StringValue, so search conservative
	// substrings rather than requiring quotes adjacent to the marker.
	[]byte("error"), []byte("failed"), []byte("incomplete"), []byte("cancelled"), []byte("canceled"),
}

func isolateBasisPointsFailureObject(object map[string]any, event string, observers ...func(int)) bool {
	status := basisPointsFailureStatus(object, event)
	if len(observers) > 0 && basisPointsExplicitForbiddenStatus(object, event) {
		reportBasisPointsStatus(http.StatusForbidden, observers)
	}
	if status == 0 {
		return false
	}
	safeError := func() map[string]any {
		// Host stream handling requires a request-scoped terminal type: generic
		// upstream_error still replays the request across its account pool.
		return map[string]any{"type": "invalid_request_error", "code": "bps_service_rejected", "message": basisPointsAccountStatusMessage(status)}
	}
	clean := func(target map[string]any) {
		for _, key := range []string{"code", "message", "detail", "details", "status_code", "plan_type", "resets_at", "resets_in_seconds"} {
			delete(target, key)
		}
		if value := protocol.StringValue(target["status"]); value != "failed" && value != "incomplete" && value != "cancelled" && value != "canceled" {
			delete(target, "status")
		}
		if kind := protocol.StringValue(target["type"]); kind != "" && !imageFailureKind(kind) && kind != "response.completed" && kind != "response.done" {
			delete(target, "type")
		}
	}
	clean(object)
	if response := relayObject(object["response"]); response != nil {
		clean(response)
		response["error"] = safeError()
		if object["error"] != nil {
			object["error"] = safeError()
		}
	} else {
		object["error"] = safeError()
	}
	return true
}

func isolateBasisPointsFailureEvent(event sseRelayEvent, observers ...func(int)) []byte {
	raw, changed := isolateBasisPointsFailureJSON([]byte(event.data), event.event, observers...)
	if !changed {
		return event.raw
	}
	var result bytes.Buffer
	written := false
	for _, line := range bytes.SplitAfter(event.raw, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			result.Write(line)
			continue
		}
		if written {
			continue
		}
		written = true
		result.WriteString("data: ")
		result.Write(raw)
		if bytes.HasSuffix(line, []byte("\r\n")) {
			result.WriteString("\r\n")
		} else if bytes.HasSuffix(line, []byte("\n")) {
			result.WriteByte(10)
		}
	}
	return result.Bytes()
}

type basisPointsResponseReader struct {
	body      io.ReadCloser
	max       int
	received  int
	decoder   sseRelayDecoder
	ready     bytes.Buffer
	readErr   error
	buffer    []byte
	observers []func(int)
}

// One observer belongs to one BPS request, including its attachment upload and
// tool correction. Ordinary responses and other status codes never take a lock.
func newBasisPointsStatusObserver(onForbidden func()) func(int) {
	var once sync.Once
	return func(status int) {
		if status == http.StatusForbidden && onForbidden != nil {
			once.Do(onForbidden)
		}
	}
}

func reportBasisPointsStatus(status int, observers []func(int)) {
	if status != http.StatusForbidden {
		return
	}
	for _, observer := range observers {
		if observer != nil {
			observer(status)
		}
	}
}

func basisPointsResponseStatusObservers(resp *http.Response) []func(int) {
	if body, ok := resp.Body.(*basisPointsResponseReader); ok {
		return body.observers
	}
	return nil
}

// Persistent BPS selection changes require an explicit error status. The
// broader account-signal classifier above remains only a host-isolation rule.
func basisPointsExplicitForbiddenStatus(object map[string]any, event string) bool {
	response := relayObject(object["response"])
	failure := imageFailureKind(event) || imageFailureKind(protocol.StringValue(object["type"])) ||
		imageFailureKind(protocol.StringValue(object["status"])) || object["error"] != nil
	if response != nil {
		failure = failure || imageFailureKind(protocol.StringValue(response["status"])) || response["error"] != nil
	}
	if !failure {
		return false
	}
	fields := []map[string]any{object, relayObject(object["error"]), relayObject(object["detail"]), response}
	if response != nil {
		fields = append(fields, relayObject(response["error"]), relayObject(response["detail"]))
	}
	for _, field := range fields {
		for _, key := range []string{"status_code", "status"} {
			switch value := field[key].(type) {
			case int:
				if value == http.StatusForbidden {
					return true
				}
			case float64:
				if value == http.StatusForbidden {
					return true
				}
			case json.Number:
				number, err := value.Float64()
				if err == nil && number == http.StatusForbidden {
					return true
				}
			case string:
				if value == "403" {
					return true
				}
			}
		}
	}
	return false
}

// Existing consumers that already decode SSE can enforce the same isolation
// on their parsed objects. Unwrap only a wholly unread filter; otherwise its
// decoder may own bytes that the underlying body can no longer provide.
func consumeBasisPointsSSEInPlace(resp *http.Response, max int) bool {
	body, ok := resp.Body.(*basisPointsResponseReader)
	if !ok || body.max < max || body.received != 0 || body.readErr != nil || body.ready.Len() != 0 || body.decoder.block.Len() != 0 || body.decoder.buffer.Len() != 0 {
		return false
	}
	resp.Body = body.body
	return true
}

// readLimited may take the remaining immutable bytes without a second full
// response copy. The body still obeys ordinary partial-read and EOF semantics.
type basisPointsBufferedBody struct {
	raw    []byte
	reader *bytes.Reader
}

func newBasisPointsBufferedBody(raw []byte) *basisPointsBufferedBody {
	return &basisPointsBufferedBody{raw: raw, reader: bytes.NewReader(raw)}
}
func (b *basisPointsBufferedBody) Read(out []byte) (int, error) { return b.reader.Read(out) }
func (*basisPointsBufferedBody) Close() error                   { return nil }
func (b *basisPointsBufferedBody) readRemaining(max int) ([]byte, error) {
	if b.reader.Len() > max {
		return nil, errBasisPointsResponseLimit
	}
	raw := b.raw[len(b.raw)-b.reader.Len():]
	_, _ = b.reader.Seek(0, io.SeekEnd)
	return raw, nil
}

var errBasisPointsResponseLimit = &protocol.APIError{Status: http.StatusBadGateway, Kind: "upstream_response_too_large", Message: "upstream response exceeds configured limit"}

func (r *basisPointsResponseReader) Close() error { return r.body.Close() }

func (r *basisPointsResponseReader) Read(out []byte) (int, error) {
	if len(out) == 0 {
		return 0, nil
	}
	if r.ready.Len() > 0 {
		return r.ready.Read(out)
	}
	emit := func(event sseRelayEvent) error {
		if len(event.raw) > r.max {
			return errBasisPointsResponseLimit
		}
		r.ready.Write(isolateBasisPointsFailureEvent(event, r.observers...))
		return nil
	}
	if r.buffer == nil {
		r.buffer = make([]byte, 32<<10)
	}
	for r.ready.Len() == 0 && r.readErr == nil {
		n, err := r.body.Read(r.buffer)
		r.received += n
		if r.received > r.max {
			r.readErr = errBasisPointsResponseLimit
			break
		}
		if feedErr := r.decoder.feed(r.buffer[:n], emit); feedErr != nil {
			r.readErr = feedErr
			break
		}
		if r.decoder.block.Len()+r.decoder.buffer.Len() > r.max {
			r.readErr = errBasisPointsResponseLimit
			break
		}
		if errors.Is(err, io.EOF) && r.decoder.block.Len()+r.decoder.buffer.Len() > 0 {
			raw := append(append([]byte(nil), r.decoder.block.Bytes()...), r.decoder.buffer.Bytes()...)
			line := bytes.TrimSuffix(r.decoder.buffer.Bytes(), []byte("\r"))
			if bytes.HasPrefix(line, []byte("event:")) {
				r.decoder.event = strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:"))))
			}
			if bytes.HasPrefix(line, []byte("data:")) {
				r.decoder.data = append(r.decoder.data, strings.TrimPrefix(string(bytes.TrimPrefix(line, []byte("data:"))), " "))
			}
			if feedErr := emit(sseRelayEvent{raw: raw, event: r.decoder.event, data: strings.Join(r.decoder.data, "\n")}); feedErr != nil {
				r.readErr = feedErr
				break
			}
		}
		r.readErr = err
	}
	if r.ready.Len() > 0 {
		return r.ready.Read(out)
	}
	return 0, r.readErr
}
