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
func newRelayToolRepair(req *http.Request, client *http.Client, prepared []byte, source map[string]any, max int) relayToolRepair {
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
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, relayProtocolError("Basis Points tool correction returned an unsuccessful HTTP response")
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
		return response, nil
	}
	terminal := errors.New("repair terminal")
	decoder := &sseRelayDecoder{}
	var response map[string]any
	consume := func(event sseRelayEvent) error {
		if strings.TrimSpace(event.data) == "" {
			return nil
		}
		payload, err := protocol.RawObject([]byte(event.data))
		if err != nil {
			return relayProtocolError("Basis Points tool correction returned invalid SSE")
		}
		if kind := protocol.StringValue(payload["type"]); kind != "" {
			event.event = kind
		}
		switch event.event {
		case "response.completed", "response.done":
			response = relayObject(payload["response"])
			if response == nil {
				return relayProtocolError("Basis Points tool correction omitted its terminal response")
			}
			if protocol.StringValue(response["status"]) == "" {
				response["status"] = "completed"
			}
			return terminal
		case "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
			return relayProtocolError("Basis Points tool correction did not complete")
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
