package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

const maxImageErrorEventBytes = 8 << 20

var errImageResponseLimit = errors.New("upstream response exceeds configured limit")

func imageFailureKind(kind string) bool {
	switch kind {
	case "error", "response.error", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "failed", "incomplete", "cancelled", "canceled":
		return true
	}
	return false
}

// Limit redaction to failure diagnostics. Output text and tool arguments remain
// intact even when a failed response includes partial output.
func redactImageFailureFields(object map[string]any, failure bool) {
	failure = failure || imageFailureKind(protocol.StringValue(object["type"])) || imageFailureKind(protocol.StringValue(object["status"]))
	if value := object["error"]; value != nil {
		holder := map[string]any{"error": value}
		redactImageErrorValues(holder)
		object["error"] = holder["error"]
		failure = true
	}
	if failure {
		for _, key := range []string{"message", "detail", "details", "reason", "code", "incomplete_details"} {
			if value, ok := object[key]; ok {
				holder := map[string]any{key: value}
				redactImageErrorValues(holder)
				object[key] = holder[key]
			}
		}
	}
	if response := relayObject(object["response"]); response != nil {
		redactImageFailureFields(response, failure)
	}
}

func redactImageFailureJSON(raw []byte, event string) ([]byte, bool) {
	return redactImageFailureJSONWithIsolation(raw, event, false)
}

func redactImageFailureJSONWithIsolation(raw []byte, event string, isolateBasisPoints bool, observers ...func(int)) ([]byte, bool) {
	if !mayContainResponseFailure(raw, event) {
		return raw, false
	}
	if !json.Valid(raw) {
		return raw, false
	}
	object, err := protocol.RawObject(raw)
	if err != nil || object == nil {
		return raw, false
	}
	before := protocol.JSONBytes(object)
	if isolateBasisPoints {
		isolateBasisPointsFailureObject(object, event, observers...)
	}
	redactImageFailureFields(object, imageFailureKind(event))
	after := protocol.JSONBytes(object)
	if bytes.Equal(before, after) {
		return raw, false
	}
	return after, true
}

func redactImageFailureEvent(event sseRelayEvent) []byte {
	return redactImageFailureEventWithIsolation(event, false)
}

func redactImageFailureEventWithIsolation(event sseRelayEvent, isolateBasisPoints bool, observers ...func(int)) []byte {
	data, changed := redactImageFailureJSONWithIsolation([]byte(event.data), event.event, isolateBasisPoints, observers...)
	if !changed {
		if !imageFailureKind(event.event) || json.Valid([]byte(event.data)) {
			return event.raw
		}
		// Some gateways send a plain diagnostic instead of JSON. Restrict replacement
		// to data lines, preserving event IDs, comments, delimiters, and line endings.
		var result bytes.Buffer
		for _, line := range bytes.SplitAfter(event.raw, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				line = []byte(redactImageDiagnostic(string(line)))
			}
			result.Write(line)
		}
		return result.Bytes()
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
		result.Write(data)
		if bytes.HasSuffix(line, []byte("\r\n")) {
			result.WriteString("\r\n")
		} else if bytes.HasSuffix(line, []byte("\n")) {
			result.WriteByte('\n')
		}
	}
	return result.Bytes()
}

func sendImageSafeHTTPResponse(stream pluginv1.TransportPlugin_ForwardServer, resp *http.Response, max int) error {
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return sendImageSafeHTTPResponseStream(stream, resp, max)
	}
	body, err := readLimited(resp.Body, max)
	if err != nil {
		return sendError(stream, errorCode(err), safeError(err), true)
	}
	body, _ = redactImageFailureJSON(body, "")
	return sendHTTPResponse(stream, resp, body, resp.Header.Get("Content-Type"))
}

// Decode records incrementally so an error can be scrubbed before any part of
// its data is forwarded, without waiting for the upstream response to finish.
func sendImageSafeHTTPResponseStream(stream pluginv1.TransportPlugin_ForwardServer, resp *http.Response, max int) error {
	observers := basisPointsResponseStatusObservers(resp)
	isolateBasisPoints := consumeBasisPointsSSEInPlace(resp, max)
	defer resp.Body.Close()
	stopClose := context.AfterFunc(stream.Context(), func() { _ = resp.Body.Close() })
	defer stopClose()
	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{
		StatusCode: int32(resp.StatusCode), Status: resp.Status, Protocol: resp.Proto,
		ProtocolMajor: int32(resp.ProtoMajor), ProtocolMinor: int32(resp.ProtoMinor),
		Headers: responseHeaders(resp.Header, -1), ContentLength: -1,
	}}}); err != nil {
		return err
	}
	eventLimit := min(max, maxImageErrorEventBytes)
	var received, sent int64
	decoder := &sseRelayDecoder{}
	emit := func(event sseRelayEvent) error {
		if len(event.raw) > eventLimit {
			return errImageResponseLimit
		}
		body := redactImageFailureEventWithIsolation(event, isolateBasisPoints, observers...)
		if sent+int64(len(body)) > int64(max) {
			return errImageResponseLimit
		}
		for len(body) > 0 {
			n := min(len(body), 32<<10)
			chunk := append([]byte(nil), body[:n]...)
			if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: chunk}}); err != nil {
				return err
			}
			sent += int64(n)
			body = body[n:]
		}
		return nil
	}
	fail := func(err error) error {
		if errors.Is(err, errImageResponseLimit) {
			return sendError(stream, "upstream_response_too_large", errImageResponseLimit.Error(), true)
		}
		return err
	}
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			received += int64(n)
			if received > int64(max) {
				return fail(errImageResponseLimit)
			}
			if err := decoder.feed(buffer[:n], emit); err != nil {
				return fail(err)
			}
			if decoder.block.Len()+decoder.buffer.Len() > eventLimit {
				return fail(errImageResponseLimit)
			}
		}
		if errors.Is(readErr, io.EOF) {
			// Preserve an undelimited final record exactly; EOF must not invent a
			// terminal event or turn an upstream failure into a completed response.
			if decoder.block.Len()+decoder.buffer.Len() > 0 {
				raw := append(append([]byte(nil), decoder.block.Bytes()...), decoder.buffer.Bytes()...)
				line := bytes.TrimSuffix(decoder.buffer.Bytes(), []byte("\r"))
				if bytes.HasPrefix(line, []byte("event:")) {
					decoder.event = strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:"))))
				}
				if bytes.HasPrefix(line, []byte("data:")) {
					decoder.data = append(decoder.data, strings.TrimPrefix(string(bytes.TrimPrefix(line, []byte("data:"))), " "))
				}
				if err := emit(sseRelayEvent{raw: raw, event: decoder.event, data: strings.Join(decoder.data, "\n")}); err != nil {
					return fail(err)
				}
			}
			return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{BytesReceived: sent}}})
		}
		if readErr != nil {
			return sendError(stream, "upstream_read", "upstream response could not be read", true)
		}
	}
}
