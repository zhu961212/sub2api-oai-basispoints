package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"google.golang.org/grpc"
)

// fakeHost records resolved IDs; serialize that test-only bookkeeping because
// account scans intentionally issue concurrent identity requests.
type degradationTestHost struct {
	*fakeHost
	mu sync.Mutex
}

func (h *degradationTestHost) ResolveOutboundIdentity(ctx context.Context, request *pluginv1.ResolveOutboundIdentityRequest, opts ...grpc.CallOption) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fakeHost.ResolveOutboundIdentity(ctx, request, opts...)
}

type degradationRoundTripper func(*http.Request) (*http.Response, error)

func (f degradationRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDegradationAnswerRejectsUnfinishedOrEmptyReplies(t *testing.T) {
	cases := map[string]map[string]any{
		"whitespace":     {"output_text": "  \t\n"},
		"incomplete":     {"status": "incomplete", "output_text": "苹果16"},
		"failed":         {"status": "failed", "output_text": "苹果16"},
		"error":          {"error": map[string]any{"code": "rate_limit_exceeded"}, "output_text": "苹果16"},
		"truncated chat": {"choices": []any{map[string]any{"finish_reason": "length", "message": map[string]any{"content": "苹果16"}}}},
		"filtered chat":  {"choices": []any{map[string]any{"finish_reason": "content_filter", "message": map[string]any{"content": "苹果16"}}}},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if answer, err := degradationAnswer(protocol.JSONBytes(body), "application/json"); err == nil || answer != "" {
				t.Fatalf("unclassifiable response accepted: answer=%q err=%v", answer, err)
			}
		})
	}
	for _, status := range []string{"completed", "incomplete", "failed"} {
		t.Run("SSE "+status, func(t *testing.T) {
			body := protocol.JSONBytes(map[string]any{"type": "response.completed", "response": map[string]any{"status": status, "output_text": "苹果16"}})
			stream := fmt.Sprintf("event: response.completed\ndata: %s\n\n", body)
			answer, err := degradationAnswer([]byte(stream), "text/event-stream")
			if status == "completed" {
				if err != nil || answer != "苹果16" {
					t.Fatalf("completed SSE rejected: answer=%q err=%v", answer, err)
				}
			} else if err == nil || answer != "" {
				t.Fatal("unfinished SSE was classified")
			}
		})
	}
}

func TestDegradationCheckNeverClassifiesHTTPFailures(t *testing.T) {
	host := &degradationTestHost{fakeHost: &fakeHost{token: token(t, "test-account")}}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			transport := New()
			defer transport.Shutdown()
			client := &http.Client{Transport: degradationRoundTripper(func(*http.Request) (*http.Response, error) {
				body := protocol.JSONBytes(map[string]any{"output_text": "苹果16"})
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})}
			verdict, answer, err := transport.checkDegradationAccount(context.Background(), protocol.DefaultConfig(), host, client, 1, protocol.DefaultModelID)
			if verdict != "error" || answer != "" || err == nil {
				t.Fatalf("HTTP failure classified: status=%s answer=%q err=%v", verdict, answer, err)
			}
			if blocked := transport.isBPSAccountDisabled(1, protocol.DefaultConfig()); blocked != (status == http.StatusForbidden) {
				t.Fatalf("HTTP %d disabled BPS=%t", status, blocked)
			}
		})
	}
}

func TestDegradationForbiddenSignalDisablesFutureChecks(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			transport := New()
			defer transport.Shutdown()
			host := &degradationTestHost{fakeHost: &fakeHost{token: token(t, "test-account")}}
			calls := 0
			client := &http.Client{Transport: degradationRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				object := map[string]any{"error": map[string]any{"status_code": 403, "code": "arbitrary_code"}}
				body, contentType := string(protocol.JSONBytes(object)), "application/json"
				if stream {
					body = "event: error\ndata: " + body + "\n\n"
					contentType = "text/event-stream"
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			verdict, _, err := transport.checkDegradationAccount(context.Background(), protocol.DefaultConfig(), host, client, 1, protocol.DefaultModelID)
			if verdict != "error" || err == nil || !transport.isBPSAccountDisabled(1, protocol.DefaultConfig()) {
				t.Fatalf("first forbidden response: verdict=%s err=%v", verdict, err)
			}
			verdict, _, err = transport.checkDegradationAccount(context.Background(), protocol.DefaultConfig(), host, client, 1, protocol.DefaultModelID)
			if verdict != "skipped" || err == nil || calls != 1 {
				t.Fatalf("disabled account was probed again: verdict=%s calls=%d err=%v", verdict, calls, err)
			}
		})
	}
}

func TestDegradationCheckBoundsAllBatchesAndConcurrency(t *testing.T) {
	if degradationCheckBudget >= 30*time.Second {
		t.Fatal("scan must leave time before the host's 30-second deadline")
	}
	host := &degradationTestHost{fakeHost: &fakeHost{token: token(t, "test-account")}}
	for accountID := int64(1); accountID <= 40; accountID++ {
		host.accounts = append(host.accounts, &pluginv1.AccountInfo{Id: accountID, Schedulable: true})
	}
	var active, peak atomic.Int32
	transport := New()
	defer transport.Shutdown()
	transport.host = host
	transport.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
		count := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); count > previous && !peak.CompareAndSwap(previous, count); previous = peak.Load() {
		}
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	started := time.Now()
	check, err := transport.runDegradationCheckWithBudget(context.Background(), protocol.DefaultConfig(), 40*time.Millisecond)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("queued batches exceeded total budget: %s", elapsed)
	}
	if err != nil || !check.Completed || len(check.Results) != 40 || len(check.DegradedAccountIDs) != 0 {
		t.Fatalf("budget results incomplete: completed=%t results=%d degraded=%v err=%v", check.Completed, len(check.Results), check.DegradedAccountIDs, err)
	}
	for _, result := range check.Results {
		if result.Status != "error" || result.Answer != "" || result.Error == "" {
			t.Fatalf("timeout was classified: %#v", result)
		}
	}
	if peak.Load() < 2 || peak.Load() > degradationCheckParallel || active.Load() != 0 {
		t.Fatalf("unexpected request concurrency: peak=%d active=%d", peak.Load(), active.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	check, err = transport.runDegradationCheckWithBudget(ctx, protocol.DefaultConfig(), time.Second)
	if err == nil || check.Completed || len(check.DegradedAccountIDs) != 0 {
		t.Fatal("parent cancellation was reported as completed")
	}
}
