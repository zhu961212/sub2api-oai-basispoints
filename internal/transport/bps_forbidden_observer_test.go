package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestBPSForbiddenObserverOnlyOnceConcurrently(t *testing.T) {
	var count atomic.Int32
	observe := newBasisPointsStatusObserver(func() { count.Add(1) })
	var workers sync.WaitGroup
	for range 64 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for _, status := range []int{0, 200, 401, 403, 429, 500, 403} {
				observe(status)
			}
		}()
	}
	workers.Wait()
	if count.Load() != 1 {
		t.Fatalf("403 notification count = %d, want one", count.Load())
	}
}

func TestBPSForbiddenResponseObserversSurviveEveryConsumer(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		for _, consumer := range []string{"json", "sse_raw", "sse_transformed", "sse_image", "sse_correction"} {
			t.Run(fmt.Sprintf("%s/%d", consumer, status), func(t *testing.T) {
				failure := map[string]any{"status": "failed", "output": []any{}, "error": map[string]any{"status_code": status, "message": "PRIVATE diagnostic"}}
				wire := string(protocol.JSONBytes(failure))
				contentType := "application/json"
				if consumer != "json" {
					wire = streamData(map[string]any{"type": "response.failed", "response": failure})
					contentType = "text/event-stream"
				}
				response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(wire))}
				var count atomic.Int32
				observe := newBasisPointsStatusObserver(func() { count.Add(1) })
				if err := prepareBasisPointsResponse(response, 1<<20, observe); err != nil {
					t.Fatal(err)
				}
				stream := &streamStub{ctx: context.Background()}
				switch consumer {
				case "json", "sse_raw":
					raw, err := io.ReadAll(response.Body)
					if err != nil || !strings.Contains(string(raw), "bps_service_rejected") {
						t.Fatalf("missing isolated failure: %s %v", raw, err)
					}
				case "sse_transformed":
					_ = sendTransformedHTTPResponseStreamWithKeepalive(stream, response, 1<<20, map[string]any{"stream": true}, 0)
				case "sse_image":
					if err := sendImageSafeHTTPResponseStream(stream, response, 1<<20); err != nil {
						t.Fatal(err)
					}
				case "sse_correction":
					if _, err := readToolRepairResponse(response, 1<<20); err == nil {
						t.Fatal("correction failure accepted")
					}
				}
				want := int32(0)
				if status == 403 {
					want = 1
				}
				if count.Load() != want {
					t.Fatalf("notification count = %d, want %d", count.Load(), want)
				}
			})
		}
	}
}

func TestBPSForbiddenObserverIgnoresNormalOutputAndOtherStatuses(t *testing.T) {
	var count atomic.Int32
	observe := newBasisPointsStatusObserver(func() { count.Add(1) })
	for _, status := range []int{200, 401, 429, 500} {
		observe(status)
	}
	for _, payload := range []map[string]any{
		{"type": "response.output_text.delta", "delta": "HTTP 403 forbidden permission workspace_suspended"},
		{"status": "completed", "output": []any{map[string]any{"type": "function_call", "arguments": "403 permission denied"}}},
		{"type": "response.failed", "error": map[string]any{"status_code": 401, "message": "unauthorized"}},
		{"type": "response.failed", "error": map[string]any{"status_code": 429, "message": "rate_limit"}},
		{"type": "response.failed", "error": map[string]any{"status_code": 400, "message": "not a permission issue"}},
		{"type": "response.failed", "error": map[string]any{"code": "workspace_suspended", "message": "forbidden"}},
		{"type": "response.failed", "error": map[string]any{"code": "403", "message": "HTTP 403"}},
		{"type": "response.output_text.delta", "status_code": 403, "delta": "ordinary response"},
		{"status": "completed", "output": []any{map[string]any{"type": "message", "status_code": 403}}},
	} {
		isolateBasisPointsFailureJSON(protocol.JSONBytes(payload), "", observe)
	}
	if count.Load() != 0 {
		t.Fatal("non403 response disabled BPS")
	}
	// Broad host-account isolation still recognizes diagnostic keywords, but
	// persistence requires an explicit status rather than an inferred one.
	raw, _ := isolateBasisPointsFailureJSON(protocol.JSONBytes(map[string]any{"type": "error", "error": map[string]any{"code": "workspace_suspended"}}), "", observe)
	if count.Load() != 0 || !strings.Contains(string(raw), "bps_service_rejected") {
		t.Fatalf("keyword-only isolation changed persistence: count=%d body=%s", count.Load(), raw)
	}
}

func TestBPSForbiddenObserverRequiresExplicitErrorStatus(t *testing.T) {
	for _, value := range []any{403, "403", json.Number("403.0")} {
		for _, key := range []string{"status", "status_code"} {
			t.Run(fmt.Sprintf("%s_%v", key, value), func(t *testing.T) {
				var count atomic.Int32
				observe := newBasisPointsStatusObserver(func() { count.Add(1) })
				payload := map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "error": map[string]any{key: value, "code": "any_error_code"}}}
				isolateBasisPointsFailureJSON(protocol.JSONBytes(payload), "", observe)
				if count.Load() != 1 {
					t.Fatalf("explicit403 did not disable once: %d", count.Load())
				}
			})
		}
	}
	for _, value := range []any{403.5, "403 forbidden", 400, "429"} {
		var count atomic.Int32
		observe := newBasisPointsStatusObserver(func() { count.Add(1) })
		payload := map[string]any{"type": "error", "error": map[string]any{"status_code": value, "message": "permission denied"}}
		isolateBasisPointsFailureJSON(protocol.JSONBytes(payload), "", observe)
		if count.Load() != 0 {
			t.Fatalf("ambiguous status %v disabled BPS", value)
		}
	}
}

func TestBPSForbiddenAttachmentAndCorrectionHTTPObservers(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			var count atomic.Int32
			observe := newBasisPointsStatusObserver(func() { count.Add(1) })
			err := sendImageRelayError(&streamStub{ctx: context.Background()}, &protocol.APIError{Status: status, Kind: "attachment_upload_error", Message: "safe fixture"}, observe)
			if err != nil {
				t.Fatal(err)
			}
			want := int32(0)
			if status == 403 {
				want = 1
			}
			if count.Load() != want {
				t.Fatalf("attachment notification count = %d, want %d", count.Load(), want)
			}
			count.Store(0)
			observe = newBasisPointsStatusObserver(func() { count.Add(1) })
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer upstream.Close()
			source := executorOnlyCatalogSource(t.Name())
			prepared, err := protocol.PrepareResponsesBody(source, protocol.DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, upstream.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			repair := newRelayToolRepair(req, upstream.Client(), protocol.JSONBytes(prepared), source, 1<<20, observe)
			original := map[string]any{"id": "resp_rejected", "status": "completed", "output": []any{relayNativeCall("call_rejected", "exec_command", map[string]any{"cmd": "pwd"})}}
			if _, err := repair(context.Background(), original); err == nil {
				t.Fatal("HTTP failure accepted")
			}
			if count.Load() != want {
				t.Fatalf("correction notification count = %d, want %d", count.Load(), want)
			}
		})
	}
}
