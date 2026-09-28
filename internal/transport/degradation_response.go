package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// A completed SSE record is sufficient; a gateway may keep the connection open
// after it. Waiting for EOF used to turn a valid answer into a read timeout.
func readDegradationResponse(ctx context.Context, body io.Reader, contentType string, max int) ([]byte, string, error) {
	var buffered bytes.Buffer
	var decoder sseRelayDecoder
	var completedOutput degradationCompletedOutput
	var terminal []byte
	finished := errors.New("degradation response finished")
	emit := func(event sseRelayEvent) error {
		if protocol.ClassifyResponseTerminal(event.event, nil).Failed() {
			return fmt.Errorf("upstream response did not complete")
		}
		data := strings.TrimSpace(event.data)
		if data == "" {
			return nil
		}
		if data == "[DONE]" {
			return fmt.Errorf("upstream stream ended without a completed response")
		}
		payload, err := protocol.RawObject([]byte(data))
		if err != nil {
			return nil
		}
		state := protocol.ClassifyResponseTerminal(event.event, payload)
		if state.Failed() {
			return fmt.Errorf("upstream response did not complete")
		}
		if err := completedOutput.consume(event.event, payload); err != nil {
			return err
		}
		if state == protocol.TerminalCompleted {
			if err := degradationResponseError(payload); err != nil {
				return err
			}
			response, ok := payload["response"].(map[string]any)
			if !ok {
				return fmt.Errorf("invalid upstream completed response")
			}
			completedOutput.fill(response)
			terminal = protocol.JSONBytes(response)
			return finished
		}
		return nil
	}
	limited := io.LimitReader(body, int64(max)+1)
	chunk := make([]byte, 32<<10)
	emptyReads := 0
	for {
		n, readErr := limited.Read(chunk)
		if n > 0 {
			emptyReads = 0
			if buffered.Len()+n > max {
				return nil, "", fmt.Errorf("upstream response exceeds configured limit")
			}
			_, _ = buffered.Write(chunk[:n])
			if err := decoder.feed(chunk[:n], emit); err != nil {
				if errors.Is(err, finished) {
					return terminal, "application/json", nil
				}
				return nil, "", err
			}
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return nil, "", degradationReadError(ctx, io.ErrNoProgress)
			}
		}
		if errors.Is(readErr, io.EOF) {
			// Some gateways close directly after the last data line. Flush only at
			// EOF, never at a network error: partial replies cannot be classified.
			if err := decoder.feed([]byte{10, 10}, emit); err != nil {
				if errors.Is(err, finished) {
					return terminal, "application/json", nil
				}
				return nil, "", err
			}
			return buffered.Bytes(), contentType, nil
		}
		if readErr != nil {
			return nil, "", degradationReadError(ctx, readErr)
		}
		if ctx.Err() != nil {
			return nil, "", degradationReadError(ctx, ctx.Err())
		}
	}
}

// Return fixed diagnostics only: network errors can contain proxy passwords,
// bearer tokens or URLs, and are never copied into the configuration page.
func degradationReadError(ctx context.Context, err error) error {
	var networkErr net.Error
	switch {
	case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, context.Canceled):
		return fmt.Errorf("degradation check canceled")
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded), errors.As(err, &networkErr) && networkErr.Timeout():
		return fmt.Errorf("degradation check timed out before upstream completed")
	case errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("upstream connection closed before response completed")
	default:
		return fmt.Errorf("upstream connection failed before response completed")
	}
}
