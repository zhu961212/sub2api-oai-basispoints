package transport

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestPreparedSSEReadLimitedPreservesResponseLimitError(t *testing.T) {
	const limit = 128
	raw := streamData(map[string]any{"type": "response.output_text.delta", "delta": strings.Repeat("x", limit)})
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(raw)),
	}
	defer response.Body.Close()
	if err := prepareBasisPointsResponse(response, limit); err != nil {
		t.Fatal(err)
	}
	body, err := readLimited(response.Body, limit)
	var api *protocol.APIError
	if !errors.As(err, &api) || api.Code() != "upstream_response_too_large" || api.StatusCode() != http.StatusBadGateway || len(body) != 0 {
		t.Fatalf("SSE wrapper limit was obscured: bytes=%d err=%v", len(body), err)
	}
	if !errors.Is(err, errBasisPointsResponseLimit) {
		t.Fatal("the safe response-limit error must retain its identity")
	}
}

func TestForwardNonStreamingSSEPreservesResponseLimitError(t *testing.T) {
	const limit = 64 << 10
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamData(map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "resp_large", "status": "completed", "output_text": strings.Repeat("x", limit)},
		}))
	}))
	defer upstream.Close()
	tr := New()
	defer tr.Shutdown()
	applyConfig(t, tr, map[string]any{
		"responses_url": upstream.URL, "max_response_bytes": limit,
		"rewrite_tools": false, "transform_responses": true,
	})
	source := protocol.JSONBytes(map[string]any{"model": "gpt-6-astra", "input": "hi", "stream": false})
	result := runForward(t, tr, requestFrames(t, "https://host.example.invalid/backend-api/codex/responses", token(t, "account"), nil, source))
	if result.errFrame == nil || result.errFrame.GetCode() != "upstream_response_too_large" || !result.errFrame.GetRequestSent() {
		t.Fatalf("buffered SSE limit was not reported: %+v", result)
	}
	if result.status != 0 || len(result.body) != 0 || result.ended || calls.Load() != 1 {
		t.Fatalf("oversized nonstream response was forwarded or replayed: %+v calls=%d", result, calls.Load())
	}
}
