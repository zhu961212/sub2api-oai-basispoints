package transport

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func toolRepairContextFixture(t *testing.T, ctx context.Context, roundTrip retryPolicyRoundTripper) (relayToolRepair, map[string]any) {
	t.Helper()
	source := executorOnlyCatalogSource(t.Name())
	prepared, err := protocol.PrepareResponsesBody(source, protocol.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.invalid/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	original := map[string]any{
		"id": "resp_context", "status": "completed",
		"output": []any{relayNativeCall("call_context", "exec_command", map[string]any{"cmd": "pwd"})},
	}
	return newRelayToolRepair(request, &http.Client{Transport: roundTrip}, protocol.JSONBytes(prepared), source, 1<<20), original
}

func TestToolRepairRejectsCanceledOriginalRequest(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "canceled"
		if expired {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if expired {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			}
			cancel()
			calls := 0
			repair, original := toolRepairContextFixture(t, ctx, func(request *http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected correction attempt")
			})
			if _, err := repair(context.Background(), original); !errors.Is(err, want) || calls != 0 {
				t.Fatalf("canceled original request caused correction: calls=%d err=%v want=%v", calls, err, want)
			}
		})
	}
}

func TestToolRepairKeepsOriginalRequestDeadline(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	calls := 0
	repair, original := toolRepairContextFixture(t, ctx, func(request *http.Request) (*http.Response, error) {
		calls++
		if got, ok := request.Context().Deadline(); !ok || !got.Equal(deadline) {
			t.Errorf("correction reset original deadline: got %v, set %t; want %v", got, ok, deadline)
		}
		return nil, errors.New("stop after inspecting request context")
	})
	_, _ = repair(context.Background(), original)
	if calls != 1 {
		t.Fatalf("expected one correction attempt, got %d", calls)
	}
}
