package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func budgetTestResponse() *http.Response {
	body := protocol.JSONBytes(map[string]any{"output_text": "iPhone 17"})
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}
}

func assertDegradationDeadline(t *testing.T, request *http.Request, want time.Duration) {
	t.Helper()
	deadline, ok := request.Context().Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining > want || remaining < want-2*time.Second {
		t.Errorf("account deadline=%v bounded=%t, want near %v", remaining, ok, want)
	}
}

func budgetTestAccounts(t *testing.T, host *diagnosticTestHost, count int) []int64 {
	t.Helper()
	host.accounts = nil
	host.tokenFor = make(map[int64]string)
	ids := make([]int64, count)
	for i := range count {
		id := int64(i + 1)
		ids[i] = id
		host.accounts = append(host.accounts, &pluginv1.AccountInfo{Id: id, Schedulable: true})
		host.tokenFor[id] = token(t, fmt.Sprintf("acct-%d", id))
	}
	return ids
}

func TestBackgroundDegradationBudgetUsesTargetWavesAndCapsTotal(t *testing.T) {
	for _, entry := range []struct {
		name                  string
		seconds, count        int
		single                bool
		wantAccount, wantScan time.Duration
	}{
		{"default empty", 0, 0, false, 300 * time.Second, 305 * time.Second},
		{"negative default", -1, 1, false, 300 * time.Second, 305 * time.Second},
		{"minimum", 1, 1, false, 10 * time.Second, 15 * time.Second},
		{"one wave", 60, 8, false, 60 * time.Second, 65 * time.Second},
		{"two waves", 60, 9, false, 60 * time.Second, 125 * time.Second},
		{"single overrides list", 120, 17, true, 120 * time.Second, 125 * time.Second},
		{"generation cap", 300, 1000, false, 300 * time.Second, 1805 * time.Second},
		{"maximum account", 9999, 1, false, 1800 * time.Second, 1805 * time.Second},
	} {
		t.Run(entry.name, func(t *testing.T) {
			cfg := protocol.DefaultConfig()
			cfg.TimeoutSeconds = entry.seconds
			cfg.DegradationCheckAccountIDs = make([]int64, entry.count)
			if entry.single {
				cfg.DegradationCheckAccountID = 7
			}
			scan, account := backgroundDegradationBudgets(cfg)
			if scan != entry.wantScan || account != entry.wantAccount {
				t.Fatalf("budgets=(%v,%v), want (%v,%v)", scan, account, entry.wantScan, entry.wantAccount)
			}
		})
	}
}

func TestBackgroundDegradationHonorsConfiguredAccountDeadline(t *testing.T) {
	for _, seconds := range []int{60, 120} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			tr, _, _ := newDiagnosticTransport(t)
			cfg := protocol.DefaultConfig()
			cfg.TimeoutSeconds, cfg.DegradationCheckAccountID = seconds, 7
			tr.client.Transport = degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
				assertDegradationDeadline(t, r, time.Duration(seconds)*time.Second)
				return budgetTestResponse(), nil
			})
			result, err := tr.runBackgroundDegradationCheck(context.Background(), cfg)
			if err != nil || !result.Completed || len(result.Results) != 1 || result.Results[0].Status != "ok" {
				t.Fatalf("background result=%+v err=%v", result, err)
			}
		})
	}
}

func TestSynchronousDegradationKeepsLegacyDeadline(t *testing.T) {
	tr, _, _ := newDiagnosticTransport(t)
	cfg := protocol.DefaultConfig()
	cfg.TimeoutSeconds, cfg.DegradationCheckAccountID = 120, 7
	tr.client.Transport = degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
		assertDegradationDeadline(t, r, 20*time.Second)
		return budgetTestResponse(), nil
	})
	result, err := tr.runDegradationCheck(context.Background(), cfg)
	if err != nil || !result.Completed || degradationCheckBudget != 24*time.Second {
		t.Fatalf("legacy budget changed: completed=%t budget=%v err=%v", result.Completed, degradationCheckBudget, err)
	}
}

func TestBackgroundDegradationLaterWaveGetsFullAccountBudget(t *testing.T) {
	tr, host, _ := newDiagnosticTransport(t)
	cfg := protocol.DefaultConfig()
	cfg.TimeoutSeconds = 60
	cfg.DegradationCheckAccountIDs = budgetTestAccounts(t, host, 16)
	entered, release := make(chan struct{}, 16), make(chan struct{})
	var calls, active, peak atomic.Int32
	tr.client.Transport = degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); current > previous && !peak.CompareAndSwap(previous, current); previous = peak.Load() {
		}
		assertDegradationDeadline(t, r, 60*time.Second)
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return budgetTestResponse(), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var result degradationCheckResult
	var runErr error
	go func() { result, runErr = tr.runBackgroundDegradationCheck(ctx, cfg); close(done) }()
	for range degradationCheckParallel {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("first wave did not start")
		}
	}
	if active.Load() != degradationCheckParallel {
		t.Errorf("active=%d, want 8", active.Load())
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("later wave did not finish")
	}
	if runErr != nil || !result.Completed || calls.Load() != 16 || peak.Load() != 8 {
		t.Fatalf("wave result: completed=%t calls=%d peak=%d err=%v", result.Completed, calls.Load(), peak.Load(), runErr)
	}
	for _, row := range result.Results {
		if row.Status != "ok" {
			t.Fatalf("later wave lost answer: %+v", row)
		}
	}
}

func TestDegradationAccountTimeoutDoesNotCancelOtherResults(t *testing.T) {
	tr, _, _ := newDiagnosticTransport(t)
	cfg := protocol.DefaultConfig()
	cfg.DegradationCheckAccountIDs = []int64{7, 9}
	tr.client.Transport = degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("ChatGPT-Account-ID") == "acct-7" {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return budgetTestResponse(), nil
	})
	result, err := tr.runDegradationCheckWithBudgets(context.Background(), cfg, time.Second, 30*time.Millisecond)
	if err != nil || !result.Completed || len(result.DegradedAccountIDs) != 0 || result.Results[0].Status != "error" || result.Results[1].Status != "ok" {
		t.Fatalf("one account timeout contaminated scan: %+v err=%v", result, err)
	}
	if result.Results[0].Answer != "" || !strings.Contains(result.Results[0].Error, "timed out") {
		t.Fatalf("timeout was classified: %+v", result.Results[0])
	}
}

func TestBackgroundDegradationAcceptsCompletionAfterLegacyTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr, _, _ := newDiagnosticTransport(t)
		cfg := protocol.DefaultConfig()
		cfg.TimeoutSeconds, cfg.DegradationCheckAccountID = 60, 7
		tr.client.Transport = degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
			select {
			case <-time.After(25 * time.Second):
				return budgetTestResponse(), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		})
		result, err := tr.runBackgroundDegradationCheck(context.Background(), cfg)
		if err != nil || !result.Completed || result.Results[0].Status != "ok" {
			t.Fatalf("valid delayed completion was cut off: %+v err=%v", result, err)
		}
	})
}

type budgetPendingSSE struct {
	ctx     context.Context
	data    []byte
	waiting chan<- struct{}
}

func (r *budgetPendingSSE) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	r.waiting <- struct{}{}
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (*budgetPendingSSE) Close() error { return nil }

func TestBackgroundDegradationParentCancellationStopsPendingSSE(t *testing.T) {
	tr, _, _ := newDiagnosticTransport(t)
	cfg := protocol.DefaultConfig()
	cfg.DegradationCheckAccountIDs = []int64{7, 9}
	waiting := make(chan struct{}, 2)
	tr.client.Transport = degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
		partial := sparseDegradationItemDone(0, sparseDegradationMessage("msg_partial", "assistant", "completed", "iPhone 17"))
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: &budgetPendingSSE{ctx: r.Context(), data: []byte(partial), waiting: waiting}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var result degradationCheckResult
	var runErr error
	go func() { result, runErr = tr.runBackgroundDegradationCheck(ctx, cfg); close(done) }()
	for range 2 {
		select {
		case <-waiting:
		case <-time.After(2 * time.Second):
			t.Fatal("SSE reads did not begin")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not stop stream reads")
	}
	if runErr != context.Canceled || result.Completed || len(result.DegradedAccountIDs) != 0 {
		t.Fatalf("parent cancellation returned a completed verdict: %+v err=%v", result, runErr)
	}
	for _, row := range result.Results {
		if row.Status != "error" || row.Answer != "" {
			t.Fatalf("partial text was classified: %+v", row)
		}
	}
}

func TestAutomaticDegradationUsesBackgroundDeadline(t *testing.T) {
	tr, host, _ := newAutoDetectionTestTransport(t)
	cfg := tr.cfg.Clone()
	cfg.TimeoutSeconds = 120
	var calls atomic.Int32
	tr.client.Transport = degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		assertDegradationDeadline(t, r, 120*time.Second)
		return budgetTestResponse(), nil
	})
	tr.runAutomaticBatch(context.Background(), host, 1, 1, cfg, []int64{7, 9})
	if calls.Load() != 2 {
		t.Fatalf("automatic batch requests=%d, want 2", calls.Load())
	}
}

func TestDiagnosticJobPublishesSnapshotExecutionBudget(t *testing.T) {
	tr, _, _ := newDiagnosticTransport(t)
	command := diagnosticPrepare(tr, "budget-metadata", 7)
	payload := command["diagnostic_task"].(map[string]any)
	cfg := payload["config"].(protocol.Config)
	cfg.TimeoutSeconds = 60
	payload["config"] = cfg
	diagnosticSave(t, tr, command)
	assertMetadata := func(wantState string) {
		t.Helper()
		var status map[string]any
		if err := json.Unmarshal([]byte(tr.diagnosticStatusJSON("{}")), &status); err != nil {
			t.Fatal(err)
		}
		summary, ok := status["diagnostic_tasks"].(map[string]any)
		if !ok {
			t.Fatal("missing task metadata")
		}
		tasks, ok := summary["tasks"].([]any)
		if !ok || len(tasks) != 1 {
			t.Fatalf("unexpected task metadata: %+v", summary)
		}
		task := tasks[0].(map[string]any)
		if task["state"] != wantState || task["execution_timeout_ms"] != float64(65000) {
			t.Fatalf("snapshot budget metadata changed: %+v", task)
		}
	}
	assertMetadata("prepared")
	tr.mu.Lock()
	tr.cfg.TimeoutSeconds = 10
	tr.mu.Unlock()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	tr.client.Transport = degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
		assertDegradationDeadline(t, r, 60*time.Second)
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return budgetTestResponse(), nil
	})
	diagnosticSave(t, tr, diagnosticCommit(tr, "budget-metadata"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("background job did not start")
	}
	assertMetadata("running")
	close(release)
	job := waitDiagnosticJob(t, tr, "budget-metadata")
	if job.State != "completed" || !job.Result.Success {
		t.Fatalf("background job failed: %+v", job)
	}
	assertMetadata("completed")
}

func TestDiagnosticJobScanTimeoutPreservesPartialResults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr, host, _ := newDiagnosticTransport(t)
		ids := budgetTestAccounts(t, host, 17)
		command := diagnosticPrepare(tr, "budget-partial", ids...)
		payload := command["diagnostic_task"].(map[string]any)
		cfg := payload["config"].(protocol.Config)
		cfg.TimeoutSeconds = 1800
		payload["config"] = cfg
		tr.client.Transport = degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("ChatGPT-Account-ID") == "acct-1" {
				return budgetTestResponse(), nil
			}
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		diagnosticSave(t, tr, command)
		diagnosticSave(t, tr, diagnosticCommit(tr, "budget-partial"))
		synctest.Wait()
		time.Sleep(1805 * time.Second)
		synctest.Wait()
		job := diagnosticJobCopy(tr, "budget-partial")
		if job.State != "failed" || job.Result == nil || job.Result.Success || job.Error != "" {
			t.Fatalf("scan deadline discarded known partial results: %+v", job)
		}
		check := degradationScopedResult(t, job.Result)
		if check.Completed || len(check.Results) != 17 || check.Results[0].Status != "ok" || len(check.DegradedAccountIDs) != 0 {
			t.Fatalf("scan deadline lost completed account: %+v", check)
		}
		if tr.diagnosticJobs.ctx.Err() != nil {
			t.Fatal("scan timeout canceled parent job store")
		}
	})
}
