package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func hostSessionScope(headers map[string]*pluginv1.HeaderValues) string {
	// Only conversation_id is guaranteed to retain the host's API-key
	// isolation. Account fingerprinting can replace session_id with one
	// account-wide UUID, and client aliases have no isolation guarantee.
	scope := ""
	for key, values := range headers {
		if !strings.EqualFold(key, "conversation_id") {
			continue
		}
		for _, value := range values.GetValues() {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if scope != "" && scope != value {
				return "" // Conflicting values must not choose a tenant randomly.
			}
			scope = value
		}
	}
	return scope
}

type sseRelayEvent struct {
	raw     []byte
	event   string
	data    string
	payload map[string]any
}

// The decoder emits complete records without waiting for the response to finish.
type sseRelayDecoder struct {
	buffer bytes.Buffer
	block  bytes.Buffer
	data   []string
	event  string
}

func (d *sseRelayDecoder) feed(chunk []byte, emit func(sseRelayEvent) error) error {
	for len(chunk) > 0 {
		// Scan only newly received bytes. Reading and rewriting an unfinished
		// line on each feed makes large SSE records quadratic in bytes copied.
		index := bytes.IndexByte(chunk, '\n')
		if index < 0 {
			_, _ = d.buffer.Write(chunk)
			return nil
		}
		line := chunk[:index+1]
		chunk = chunk[index+1:]
		if d.buffer.Len() > 0 {
			_, _ = d.buffer.Write(line)
			line = d.buffer.Bytes()
		}
		_, _ = d.block.Write(line)
		d.buffer.Reset()
		trimmed := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(trimmed) == 0 {
			if d.block.Len() > 0 {
				if err := emit(sseRelayEvent{raw: append([]byte(nil), d.block.Bytes()...), event: d.event, data: strings.Join(d.data, "\n")}); err != nil {
					return err
				}
			}
			d.block.Reset()
			d.data = nil
			d.event = ""
			continue
		}
		if bytes.HasPrefix(trimmed, []byte("event:")) {
			d.event = strings.TrimSpace(string(bytes.TrimPrefix(trimmed, []byte("event:"))))
		}
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			value := bytes.TrimPrefix(trimmed, []byte("data:"))
			d.data = append(d.data, strings.TrimPrefix(string(value), " "))
		}
	}
	return nil
}

func relayObject(value any) map[string]any { object, _ := value.(map[string]any); return object }
func isNativeRelayName(name string) bool {
	return name == "run_officejs" || name == "functions.run_officejs"
}
func relayToolItem(item map[string]any) bool {
	kind := protocol.StringValue(item["type"])
	return kind == "function_call" || kind == "custom_tool_call" || isNativeRelayName(protocol.StringValue(item["name"]))
}
func relayToolEvent(event sseRelayEvent) bool {
	switch event.event {
	case "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		return true
	case "response.output_item.added", "response.output_item.done":
		return relayToolItem(relayObject(event.payload["item"]))
	}
	return false
}

func relayOutputIndex(value any) (int, bool) {
	if value == nil {
		return 0, false
	}
	var number int
	_, err := fmt.Sscan(fmt.Sprint(value), &number)
	return number, err == nil && number >= 0 && fmt.Sprint(number) == fmt.Sprint(value)
}

func relayRecord(event string, payload map[string]any) []byte {
	return []byte("event: " + event + "\ndata: " + string(protocol.JSONBytes(payload)) + "\n\n")
}

func relayProtocolError(message string) error {
	return &protocol.APIError{Status: http.StatusBadGateway, Kind: "basispoints_protocol_error", Message: message}
}

// The terminal output is authoritative. Earlier records establish identity and
// position only; they must never resurrect a call the terminal response omitted.
func validateRelayTools(response map[string]any, pending []sseRelayEvent) error {
	output, _ := response["output"].([]any)
	for _, event := range pending {
		item := relayObject(event.payload["item"])
		id, callID := protocol.StringValue(event.payload["item_id"]), protocol.StringValue(event.payload["call_id"])
		if item != nil {
			id, callID = protocol.StringValue(item["id"]), protocol.StringValue(item["call_id"])
		}
		index, indexed := relayOutputIndex(event.payload["output_index"])
		if _, supplied := event.payload["output_index"]; supplied && !indexed {
			return relayProtocolError("Basis Points tool event has an invalid output index")
		}
		var match map[string]any
		if indexed {
			if index < len(output) {
				match = relayObject(output[index])
			}
		} else {
			for _, raw := range output {
				candidate := relayObject(raw)
				if (id != "" && id == protocol.StringValue(candidate["id"])) || (callID != "" && callID == protocol.StringValue(candidate["call_id"])) {
					if match != nil {
						return relayProtocolError("Basis Points terminal tool identity is ambiguous")
					}
					match = candidate
				}
			}
		}
		if !relayToolItem(match) || (id != "" && id != protocol.StringValue(match["id"])) || (callID != "" && callID != protocol.StringValue(match["call_id"])) {
			return relayProtocolError("Basis Points completed response omitted or relocated a tool item")
		}
		if name := protocol.StringValue(item["name"]); name != "" && name != protocol.StringValue(match["name"]) {
			return relayProtocolError("Basis Points terminal tool name differs from its earlier record")
		}
	}
	return nil
}

func structuredRelayRecord(event sseRelayEvent) bool {
	if strings.HasPrefix(event.event, "response.output_text.") || strings.HasPrefix(event.event, "response.refusal.") || strings.HasPrefix(event.event, "response.content_part.") {
		return true
	}
	return (event.event == "response.output_item.added" || event.event == "response.output_item.done") && protocol.StringValue(relayObject(event.payload["item"])["type"]) == "message"
}

func filterRelayResponse(response map[string]any, structured bool) {
	output, exists := response["output"].([]any)
	if !exists {
		return
	}
	filtered := make([]any, 0, len(output))
	for _, raw := range output {
		item := relayObject(raw)
		if !relayToolItem(item) && (!structured || protocol.StringValue(item["type"]) != "message") {
			filtered = append(filtered, raw)
		}
	}
	response["output"] = filtered
}

func emitRelayTool(item map[string]any, index int, emit func(string, map[string]any) error) error {
	field, prefix := "arguments", "response.function_call_arguments"
	if protocol.StringValue(item["type"]) == "custom_tool_call" {
		field, prefix = "input", "response.custom_tool_call_input"
	}
	added := make(map[string]any, len(item))
	for key, value := range item {
		added[key] = value
	}
	added[field], added["status"] = "", "in_progress"
	if err := emit("response.output_item.added", map[string]any{"output_index": index, "item": added}); err != nil {
		return err
	}
	if err := emit(prefix+".delta", map[string]any{"output_index": index, "item_id": item["id"], "call_id": item["call_id"], "delta": item[field]}); err != nil {
		return err
	}
	if err := emit(prefix+".done", map[string]any{"output_index": index, "item_id": item["id"], "call_id": item["call_id"], field: item[field]}); err != nil {
		return err
	}
	return emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
}

func emitStructuredRelayMessage(item map[string]any, index int, emit func(string, map[string]any) error) error {
	added := make(map[string]any, len(item))
	for key, value := range item {
		added[key] = value
	}
	added["content"], added["status"] = []any{}, "in_progress"
	if err := emit("response.output_item.added", map[string]any{"output_index": index, "item": added}); err != nil {
		return err
	}
	content, _ := item["content"].([]any)
	for contentIndex, raw := range content {
		part := relayObject(raw)
		field, prefix := "text", "response.output_text"
		if protocol.StringValue(part["type"]) == "refusal" {
			field, prefix = "refusal", "response.refusal"
		}
		emptyPart := make(map[string]any, len(part))
		for key, value := range part {
			emptyPart[key] = value
		}
		emptyPart[field] = ""
		payload := func() map[string]any {
			return map[string]any{"output_index": index, "content_index": contentIndex, "item_id": item["id"]}
		}
		addedPart := payload()
		addedPart["part"] = emptyPart
		if err := emit("response.content_part.added", addedPart); err != nil {
			return err
		}
		delta := payload()
		delta["delta"] = part[field]
		if err := emit(prefix+".delta", delta); err != nil {
			return err
		}
		done := payload()
		done[field] = part[field]
		if err := emit(prefix+".done", done); err != nil {
			return err
		}
		donePart := payload()
		donePart["part"] = part
		if err := emit("response.content_part.done", donePart); err != nil {
			return err
		}
	}
	return emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
}

// Buffer tool records only. Text/reasoning/keepalive records continue immediately;
// completed tool items are converted together so an invalid parallel call cannot
// cause half of a response to be replayed under the wrong protocol.
func sendTransformedHTTPResponseStream(stream pluginv1.TransportPlugin_ForwardServer, resp *http.Response, max int, source map[string]any) error {
	return sendTransformedHTTPResponseStreamWithKeepalive(stream, resp, max, source, 15*time.Second)
}

// Read upstream on demand so idle periods can emit SSE comments without a
// second gRPC sender. Never read ahead past a validated terminal response.
func sendTransformedHTTPResponseStreamWithKeepalive(stream pluginv1.TransportPlugin_ForwardServer, resp *http.Response, max int, source map[string]any, interval time.Duration) error {
	return sendTransformedHTTPResponseStreamWithRepair(stream, resp, max, source, interval, nil)
}

func sendTransformedHTTPResponseStreamWithRepair(stream pluginv1.TransportPlugin_ForwardServer, resp *http.Response, max int, source map[string]any, interval time.Duration, repair relayToolRepair) error {
	observers := basisPointsResponseStatusObservers(resp)
	isolateBasisPoints := consumeBasisPointsSSEInPlace(resp, max)
	ctx := stream.Context()
	type readResult struct {
		data []byte
		err  error
	}
	nextRead := make(chan struct{}, 1)
	reads := make(chan readResult, 1)
	stopReader := make(chan struct{})
	readerDone := make(chan struct{})
	buffer := acquireStreamReadBuffer()
	go func() {
		defer close(readerDone)
		buf := buffer[:]
		for {
			select {
			case <-stopReader:
				return
			case <-nextRead:
			}
			n, err := resp.Body.Read(buf)
			select {
			case <-stopReader:
				return
			case reads <- readResult{data: buf[:n], err: err}:
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		close(stopReader)
		// Closing the HTTP body unblocks Read on cancellation or Send failure.
		_ = resp.Body.Close()
		<-readerDone
		// The reader can finish before its final data+EOF result is consumed.
		// Recycle only here, after both reader and response processing finish.
		releaseStreamReadBuffer(buffer)
	}()
	var ticker *time.Ticker
	var heartbeat <-chan time.Time
	if interval > 0 {
		ticker = time.NewTicker(interval)
		heartbeat = ticker.C
		defer ticker.Stop()
	}
	headers := responseHeaders(resp.Header, -1)
	headers["Content-Type"] = &pluginv1.HeaderValues{Values: []string{"text/event-stream"}}
	headers["Cache-Control"] = &pluginv1.HeaderValues{Values: []string{"no-cache"}}
	headers["X-Accel-Buffering"] = &pluginv1.HeaderValues{Values: []string{"no"}}
	// Do not commit a successful response until there is a body to forward.
	// An error before Start can still be returned by the host as an HTTP error.
	start := &pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{StatusCode: int32(resp.StatusCode), Status: resp.Status, Protocol: resp.Proto, ProtocolMajor: int32(resp.ProtoMajor), ProtocolMinor: int32(resp.ProtoMinor), Headers: headers, ContentLength: -1}}}
	started := false
	var received, sent int64
	var sequence int64
	var pending []sseRelayEvent
	structured := protocol.HasStructuredOutput(source)
	terminal, done := false, false
	decoder := &sseRelayDecoder{}
	invalid := errors.New("stream ended before a terminal response")
	sendRaw := func(raw []byte) error {
		if len(raw) == 0 {
			return nil
		}
		if !started {
			if err := stream.Send(start); err != nil {
				return err
			}
			started = true
		}
		// SSE records can contain a full response or a large tool argument.
		// Bound RPC frames independently of event size, as the buffered HTTP
		// response path does. HTTP consumers reassemble the same SSE bytes.
		const chunkSize = 32 << 10
		for offset := 0; offset < len(raw); offset += chunkSize {
			end := min(offset+chunkSize, len(raw))
			chunk := append([]byte(nil), raw[offset:end]...)
			if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: chunk}}); err != nil {
				return err
			}
			sent += int64(len(chunk))
		}
		if ticker != nil {
			ticker.Reset(interval)
		}
		return nil
	}
	sendJSON := func(event string, payload map[string]any) error {
		// Buffered and synthesized tool records require downstream numbering.
		payload["type"] = event
		payload["sequence_number"] = sequence
		sequence++
		return sendRaw(relayRecord(event, payload))
	}
	emit := func(event sseRelayEvent) error {
		if strings.TrimSpace(event.data) == "[DONE]" {
			if !terminal {
				return invalid
			}
			if done {
				return nil
			}
			done = true
			return sendRaw([]byte("data: [DONE]\n\n"))
		}
		if done || terminal {
			return nil
		}
		// EOF flushing must not manufacture body bytes for an empty stream.
		if strings.TrimSpace(string(event.raw)) == "" {
			return nil
		}
		payload, err := protocol.RawObject([]byte(event.data))
		if err != nil {
			if protocol.ClassifyResponseTerminal(event.event, nil).Failed() {
				payload = map[string]any{}
			} else if strings.TrimSpace(event.data) == "" {
				return sendRaw(event.raw)
			} else {
				return relayProtocolError("Basis Points returned an invalid SSE event")
			}
		}
		event.payload = payload
		// Preserve the wire event until isolation and terminal classification have
		// inspected it. data.type must not mask a real event:error.
		if isolateBasisPoints {
			isolateBasisPointsFailureObject(payload, event.event, observers...)
		}
		state := protocol.ClassifyResponseTerminal(event.event, payload)
		if state.Failed() {
			pending = nil
			terminal = true
			if state == protocol.TerminalInvalid {
				return protocol.ResponseTerminalError(state, payload)
			}
			event.event = normalizeRelayFailure("response."+string(state), payload)
			filterRelayResponse(relayObject(payload["response"]), structured)
			return sendJSON(event.event, payload)
		}
		if kind := protocol.StringValue(payload["type"]); kind != "" {
			event.event = kind
		} else if event.event != "" {
			payload["type"] = event.event
		}
		if relayToolEvent(event) && !terminal {
			pending = append(pending, event)
			return nil
		}
		if structured && structuredRelayRecord(event) {
			// Reconstruct from the validated terminal response, rather than
			// replaying possibly inconsistent intermediate JSON fragments.
			return nil
		}
		switch event.event {
		case "response.completed", "response.done":
			response := relayObject(payload["response"])
			if response == nil {
				return invalid
			}
			// response.done is an upstream alias, not a Responses terminal.
			event.event = "response.completed"
			response["status"] = "completed"
			if err := validateRelayTools(response, pending); err != nil {
				return err
			}
			if err := protocol.ValidateStructuredResponse(response, source); err != nil {
				return err
			}
			_, translated, _, err := protocol.TransformResponseBody(protocol.JSONBytes(response), source)
			repairStart := -1
			if protocol.IsRepairableClientToolError(err) && repair != nil && protocol.ToolRepairEligible(source, response) {
				repairStart = len(response["output"].([]any)) - 1
				// The original tool records remain withheld. Keep the existing
				// response lifecycle and keepalives while requesting correction.
				_ = resp.Body.Close()
				repairCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				type repairResult struct {
					response map[string]any
					err      error
				}
				result := make(chan repairResult, 1)
				callback := repair
				repair = nil
				go func() { fixed, failure := callback(repairCtx, response); result <- repairResult{fixed, failure} }()
				waiting := true
				for waiting {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-heartbeat:
						if failure := sendRaw([]byte{':', ' ', 'k', 'e', 'e', 'p', 'a', 'l', 'i', 'v', 'e', 10, 10}); failure != nil {
							return failure
						}
					case fixed := <-result:
						if ctx.Err() != nil {
							return ctx.Err()
						}
						cancel()
						if fixed.err != nil {
							return fixed.err
						}
						response = fixed.response
						waiting = false
					}
				}
				_, translated, _, err = protocol.TransformResponseBody(protocol.JSONBytes(response), source)
			}
			if err != nil {
				return err
			}
			if translated == nil {
				return relayProtocolError("Basis Points terminal response could not be translated")
			}
			output, _ := translated["output"].([]any)
			for _, raw := range output {
				if isNativeRelayName(protocol.StringValue(relayObject(raw)["name"])) {
					return relayProtocolError("Basis Points native transport was not translated")
				}
			}
			for index, raw := range output {
				item := relayObject(raw)
				if relayToolItem(item) {
					if err := emitRelayTool(item, index, sendJSON); err != nil {
						return err
					}
				} else if (structured || repairStart >= 0 && index >= repairStart) && protocol.StringValue(item["type"]) == "message" {
					if err := emitStructuredRelayMessage(item, index, sendJSON); err != nil {
						return err
					}
				} else if repairStart >= 0 && index >= repairStart {
					// Repair reasoning was withheld with its own HTTP stream. Give
					// every appended item a lifecycle before advancing its index.
					if err := sendJSON("response.output_item.added", map[string]any{"output_index": index, "item": item}); err != nil {
						return err
					}
					if err := sendJSON("response.output_item.done", map[string]any{"output_index": index, "item": item}); err != nil {
						return err
					}
				}
			}
			pending = nil
			payload["response"] = translated
			protocol.RememberResponseContext(source, translated)
			terminal = true
			return sendJSON(event.event, payload)
		}
		if response := relayObject(payload["response"]); response != nil {
			filterRelayResponse(response, structured)
		}
		return sendJSON(event.event, payload)
	}
	finishStream := func() error {
		if !done {
			if err := sendRaw([]byte("data: [DONE]\n\n")); err != nil {
				return err
			}
			done = true
		}
		return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{BytesReceived: sent}}})
	}
	// Once body bytes are visible, all upstream failures must terminate inside
	// SSE. An Error frame at that point makes the host close the HTTP body with
	// an error, hiding the actual cause behind a generic decoding failure.
	fail := func(code, message string, diagnostics ...string) error {
		pending = nil
		if sent > 0 {
			failure := map[string]any{"code": code, "message": message}
			// Tool validation belongs to this response. Without a request-scoped
			// type, the host treats early response.failed as an account outage,
			// fails over, and replaces this diagnostic with a generic 502.
			if code == "bps_service_rejected" || code == "invalid_tool_call" {
				failure["type"] = "invalid_request_error"
			}
			if code == "invalid_tool_call" && len(diagnostics) > 0 {
				if reason := protocol.ClientToolDiagnosticReason(diagnostics[0]); reason != "" {
					failure["reason"] = reason
				}
			}
			if err := sendJSON("response.failed", map[string]any{
				"response": map[string]any{
					"status": "failed",
					"output": []any{},
					"error":  failure,
				},
			}); err != nil {
				return err
			}
			return finishStream()
		}
		return sendError(stream, code, message, true)
	}
	failStream := func(feedErr error) error {
		var api *protocol.APIError
		switch {
		case errors.As(feedErr, &api):
			return fail(api.Code(), api.Error(), api.DiagnosticReason())
		case errors.Is(feedErr, invalid):
			return fail("invalid_upstream_response", "Basis Points stream ended without a terminal response")
		default:
			// A downstream Send failure cannot be repaired with another event.
			return feedErr
		}
	}
	nextRead <- struct{}{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var result readResult
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-heartbeat:
			if err := sendRaw([]byte(": keepalive\n\n")); err != nil {
				return err
			}
			continue
		case result = <-reads:
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(result.data) > 0 {
			received += int64(len(result.data))
			if received > int64(max) {
				return fail("upstream_response_too_large", "upstream response exceeds configured limit")
			}
			if feedErr := decoder.feed(result.data, emit); feedErr != nil {
				return failStream(feedErr)
			}
			// The validated protocol terminal, not a clean network EOF, ends the
			// response. Read may return both these bytes and a transport error.
			if terminal {
				return finishStream()
			}
		}
		if errors.Is(result.err, io.EOF) {
			if feedErr := decoder.feed([]byte("\n\n"), emit); feedErr != nil {
				return failStream(feedErr)
			}
			if !terminal {
				return failStream(invalid)
			}
			return finishStream()
		}
		if result.err != nil {
			var api *protocol.APIError
			if errors.As(result.err, &api) {
				return fail(api.Code(), api.Error(), api.DiagnosticReason())
			}
			return fail("upstream_read", safeUpstreamReadError(result.err))
		}
		nextRead <- struct{}{}
	}
}
