package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

type relayToolRepair func(context.Context, map[string]any) (map[string]any, error)

// One repair belongs to the same Forward operation and uses its original
// client, URL, credentials, proxy and prepared model/history. No image rewrite,
// account selection or client tool execution happens in this callback.
func newRelayToolRepair(req *http.Request, client *http.Client, prepared []byte, source map[string]any, max int, observers ...func(int)) relayToolRepair {
	attempted := false
	return func(ctx context.Context, original map[string]any) (map[string]any, error) {
		if attempted || !protocol.ToolRepairEligible(source, original) {
			return nil, relayProtocolError("Basis Points tool correction is not permitted for this response")
		}
		attempted = true
		body, err := protocol.RawObject(prepared)
		if err != nil {
			return nil, relayProtocolError("Basis Points tool correction could not preserve the original request")
		}
		body, ok := protocol.PrepareToolRepairBody(body, source, original)
		if !ok {
			return nil, relayProtocolError("Basis Points tool correction could not preserve the original request")
		}
		raw := protocol.JSONBytes(body)
		retry := req.Clone(ctx)
		retry.Body = io.NopCloser(bytes.NewReader(raw))
		retry.ContentLength = int64(len(raw))
		retry.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
		resp, err := client.Do(retry)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, relayProtocolError("Basis Points tool correction request failed")
		}
		defer resp.Body.Close()
		reportBasisPointsStatus(resp.StatusCode, observers)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
				return nil, basisPointsServiceError(basisPointsAccountStatusMessage(resp.StatusCode))
			}
			return nil, relayProtocolError("Basis Points tool correction returned an unsuccessful HTTP response")
		}
		if err := prepareBasisPointsResponse(resp, max, observers...); err != nil {
			return nil, err
		}
		repaired, err := readToolRepairResponse(resp, max)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, err
		}
		return protocol.MergeToolRepairResponse(source, original, repaired)
	}
}

// Stop at a terminal SSE record, not EOF: the upstream may leave its stream
// open indefinitely. The request context owns cancellation of a blocked read.
func readToolRepairResponse(resp *http.Response, max int) (map[string]any, error) {
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		raw, err := readLimited(resp.Body, max)
		if err != nil {
			return nil, relayProtocolError("Basis Points tool correction response exceeds the configured limit")
		}
		response, err := protocol.RawObject(raw)
		if err != nil {
			return nil, relayProtocolError("Basis Points tool correction returned invalid JSON")
		}
		if terminal := protocol.ClassifyResponseTerminal("", response); terminal.Failed() {
			return nil, protocol.ResponseTerminalError(terminal, response)
		}
		return response, nil
	}
	observers := basisPointsResponseStatusObservers(resp)
	isolateBasisPoints := consumeBasisPointsSSEInPlace(resp, max)
	terminal := errors.New("repair terminal")
	decoder := &sseRelayDecoder{}
	var response map[string]any
	consume := func(event sseRelayEvent) error {
		if strings.TrimSpace(event.data) == "" {
			if state := protocol.ClassifyResponseTerminal(event.event, nil); state.Failed() {
				return protocol.ResponseTerminalError(state, nil)
			}
			return nil
		}
		payload, err := protocol.RawObject([]byte(event.data))
		if err != nil {
			return relayProtocolError("Basis Points tool correction returned invalid SSE")
		}
		if isolateBasisPoints {
			isolateBasisPointsFailureObject(payload, event.event, observers...)
		}
		state := protocol.ClassifyResponseTerminal(event.event, payload)
		if state.Failed() {
			return protocol.ResponseTerminalError(state, payload)
		}
		if state == protocol.TerminalCompleted {
			response = relayObject(payload["response"])
			if response == nil {
				return relayProtocolError("Basis Points tool correction omitted its terminal response")
			}
			if protocol.StringValue(response["status"]) == "" {
				response["status"] = "completed"
			}
			return terminal
		}
		return nil
	}
	buf := make([]byte, 32<<10)
	received := 0
	for {
		n, readErr := resp.Body.Read(buf)
		received += n
		if received > max {
			return nil, relayProtocolError("Basis Points tool correction response exceeds the configured limit")
		}
		err := decoder.feed(buf[:n], consume)
		if err == nil && errors.Is(readErr, io.EOF) {
			err = decoder.feed([]byte{10, 10}, consume)
		}
		if errors.Is(err, terminal) {
			return response, nil
		}
		if err != nil {
			return nil, err
		}
		if readErr != nil {
			return nil, relayProtocolError("Basis Points tool correction ended before a completed response")
		}
	}
}

func isolatedBasisPointsFailure(payload map[string]any) error {
	response := relayObject(payload["response"])
	for _, failure := range []map[string]any{relayObject(payload["error"]), relayObject(response["error"])} {
		if protocol.StringValue(failure["code"]) == "bps_service_rejected" {
			return basisPointsServiceError(protocol.StringValue(failure["message"]))
		}
	}
	return nil
}

func basisPointsServiceError(message string) error {
	return &protocol.APIError{Status: http.StatusBadGateway, Kind: "bps_service_rejected", Message: message}
}
