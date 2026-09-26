package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestFailureFastPathKeepsEscapedAndMislabelledErrorsIsolated(t *testing.T) {
	for _, raw := range []string{
		"{\"\\u0065rror\":{\"code\":\"rate_limit_exceeded\"}}",
		"{\"type\":\"response.\\u0066ailed\",\"code\":\"invalid_api_key\"}",
		"{\"type\":\"error\",\"error\":{\"status_code\":403,\"message\":\"PRIVATE\"}}",
		"{\"type\":\" response.failed \",\"code\":\"rate_limit_exceeded\"}",
	} {
		safe, changed := isolateBasisPointsFailureJSON([]byte(raw), "response.output_text.delta")
		if !changed || !bytes.Contains(safe, []byte("bps_service_rejected")) {
			t.Fatalf("escaped or mislabeled failure bypassed isolation: %s", safe)
		}
		value, _ := protocol.RawObject(safe)
		if basisPointsFailureStatus(value, "response.output_text.delta") != 0 {
			t.Fatalf("account state remained: %s", safe)
		}
	}
	safeOutput := []byte("{\"type\":\"response.output_text.delta\",\"delta\":\"error rate_limit_exceeded file-private\"}")
	isolated, changed := isolateBasisPointsFailureJSON(safeOutput, "response.output_text.delta")
	redacted, redactedChanged := redactImageFailureJSON(safeOutput, "response.output_text.delta")
	if changed || redactedChanged || !bytes.Equal(isolated, safeOutput) || !bytes.Equal(redacted, safeOutput) {
		t.Fatal("ordinary output changed during failure filtering")
	}
}

func TestSinglePassResponseConsumptionPreservesUnreadStateAndLimit(t *testing.T) {
	raw := streamData(map[string]any{"type": "response.output_text.delta", "delta": "hello"})
	makeResponse := func(max int) *http.Response {
		return &http.Response{Body: &basisPointsResponseReader{body: io.NopCloser(strings.NewReader(raw)), max: max}}
	}
	response := makeResponse(len(raw))
	if consumeBasisPointsSSEInPlace(response, len(raw)+1) {
		t.Fatal("single pass widened the source limit")
	}
	if !consumeBasisPointsSSEInPlace(response, len(raw)) || consumeBasisPointsSSEInPlace(response, len(raw)) {
		t.Fatal("unread wrapper should transfer exactly once")
	}
	remaining, err := io.ReadAll(response.Body)
	if err != nil || string(remaining) != raw {
		t.Fatal("unread source changed")
	}
	response = makeResponse(len(raw))
	prefix := make([]byte, 7)
	n, err := response.Body.Read(prefix)
	if err != nil {
		t.Fatal(err)
	}
	if consumeBasisPointsSSEInPlace(response, len(raw)) {
		t.Fatal("partially consumed decoder lost ownership of buffered data")
	}
	remaining, err = io.ReadAll(response.Body)
	if err != nil || string(append(prefix[:n], remaining...)) != raw {
		t.Fatal("partial response bytes lost")
	}
}

func TestPreparedJSONBodyPreservesPartialReadsAndLimit(t *testing.T) {
	body := newBasisPointsBufferedBody([]byte("abcdef"))
	prefix := make([]byte, 2)
	if n, err := body.Read(prefix); n != 2 || err != nil {
		t.Fatal("initial read failed")
	}
	raw, err := body.readRemaining(4)
	if err != nil || string(raw) != "cdef" {
		t.Fatalf("remaining bytes: %q %v", raw, err)
	}
	raw, err = body.readRemaining(0)
	if err != nil || len(raw) != 0 {
		t.Fatal("consumed body did not stay at EOF")
	}
	if _, err = body.Read(prefix); !errors.Is(err, io.EOF) {
		t.Fatal("ordinary read after take did not return EOF")
	}
	body = newBasisPointsBufferedBody([]byte("abcdef"))
	if _, err = body.readRemaining(5); !errors.Is(err, errBasisPointsResponseLimit) {
		t.Fatal("prepared body escaped response cap")
	}
}

func TestSinglePassBPSStreamDoesNotWaitForUpstreamEOF(t *testing.T) {
	first := streamData(map[string]any{"type": "response.output_text.delta", "delta": "early"})
	terminal := streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_done", "status": "completed", "output": []any{}}})
	for _, image := range []bool{false, true} {
		response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(&relayStepReader{steps: []relayReadStep{{data: first}, {data: terminal, err: io.EOF}}})}
		if err := prepareBasisPointsResponse(response, 1<<20); err != nil {
			t.Fatal(err)
		}
		stream := &streamStub{ctx: context.Background()}
		var err error
		if image {
			err = sendImageSafeHTTPResponseStream(stream, response, 1<<20)
		} else {
			err = sendTransformedHTTPResponseStreamWithKeepalive(stream, response, 1<<20, map[string]any{"stream": true}, 0)
		}
		if err != nil {
			t.Fatal(err)
		}
		var chunks [][]byte
		for _, frame := range stream.responses {
			if len(frame.GetBodyChunk()) > 0 {
				chunks = append(chunks, frame.GetBodyChunk())
			}
		}
		if len(chunks) < 2 || !bytes.Contains(chunks[0], []byte("early")) {
			t.Fatal("first event was delayed until completion")
		}
	}
}
