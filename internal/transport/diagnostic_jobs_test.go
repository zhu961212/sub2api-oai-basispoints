package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"google.golang.org/grpc"
)

type diagnosticTestHost struct {
	*degradationTestHost
	mu          sync.Mutex
	values      map[string][]byte
	failGet     bool
	failState   string
	beforeWrite func(string)
}

func (h *diagnosticTestHost) KVGet(ctx context.Context, req *pluginv1.KVGetRequest, _ ...grpc.CallOption) (*pluginv1.KVGetResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failGet {
		return nil, errors.New("private get error")
	}
	raw, ok := h.values[req.Namespace+"/"+req.Key]
	return &pluginv1.KVGetResponse{Found: ok, Value: append([]byte(nil), raw...)}, ctx.Err()
}
func (h *diagnosticTestHost) KVSet(ctx context.Context, req *pluginv1.KVSetRequest, _ ...grpc.CallOption) (*pluginv1.KVSetResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if req.Namespace == diagnosticJobNamespace {
		var job diagnosticJob
		if json.Unmarshal(req.Value, &job) != nil || req.TtlSeconds <= 0 {
			return nil, errors.New("invalid task record")
		}
		if h.beforeWrite != nil {
			h.beforeWrite(job.State)
		}
		if h.failState == job.State {
			return nil, errors.New("private set error")
		}
	}
	h.values[req.Namespace+"/"+req.Key] = append([]byte(nil), req.Value...)
	return &pluginv1.KVSetResponse{}, nil
}

func newDiagnosticTransport(t *testing.T) (*Transport, *diagnosticTestHost, *atomic.Int32) {
	t.Helper()
	tr := New()
	t.Cleanup(tr.Shutdown)
	host := &diagnosticTestHost{degradationTestHost: &degradationTestHost{fakeHost: &fakeHost{
		accounts: []*pluginv1.AccountInfo{{Id: 7, Schedulable: true}, {Id: 9, Schedulable: true}},
		tokenFor: map[int64]string{7: token(t, "acct-7"), 9: token(t, "acct-9")},
	}}, values: make(map[string][]byte)}
	tr.host = host
	calls := new(atomic.Int32)
	tr.client = &http.Client{Transport: degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output_text":"iPhone 17"}`))}, nil
	})}
	return tr, host, calls
}

func diagnosticPrepare(tr *Transport, id string, ids ...int64) map[string]any {
	cfg := protocol.DefaultConfig()
	cfg.DegradationCheck = true
	if len(ids) == 1 {
		cfg.DegradationCheckAccountID = ids[0]
	} else {
		cfg.DegradationCheckAccountIDs = ids
	}
	return map[string]any{"diagnostic_task": map[string]any{"protocol": diagnosticJobProtocol, "action": "prepare", "owner_id": tr.diagnosticJobs.ownerID, "task_id": id, "config": cfg}}
}

func diagnosticCommit(tr *Transport, id string) map[string]any {
	tr.diagnosticJobs.mu.Lock()
	receipt := tr.diagnosticJobs.jobs[id].Receipt
	tr.diagnosticJobs.mu.Unlock()
	return map[string]any{"diagnostic_task": map[string]any{"protocol": diagnosticJobProtocol, "action": "commit", "owner_id": tr.diagnosticJobs.ownerID, "task_id": id, "receipt": receipt}}
}

func diagnosticSave(t *testing.T, tr *Transport, value any) []byte {
	t.Helper()
	validation, err := tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: protocol.JSONBytes(value)})
	if err != nil || !validation.Valid {
		t.Fatalf("validate: %v %v", validation, err)
	}
	applied, err := tr.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: validation.NormalizedConfigJson})
	if err != nil || !applied.Applied {
		t.Fatalf("apply: %v %v", applied, err)
	}
	return validation.NormalizedConfigJson
}

func diagnosticJobCopy(tr *Transport, id string) diagnosticJob {
	tr.diagnosticJobs.mu.Lock()
	defer tr.diagnosticJobs.mu.Unlock()
	return *tr.diagnosticJobs.jobs[id]
}

func waitDiagnosticJob(t *testing.T, tr *Transport, id string) diagnosticJob {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job := diagnosticJobCopy(tr, id)
		if job.State != "prepared" && job.State != "queued" && job.State != "running" {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("diagnostic worker did not finish")
	return diagnosticJob{}
}

func TestDiagnosticJobPrepareCommitAndPassiveStatus(t *testing.T) {
	tr, _, calls := newDiagnosticTransport(t)
	applyConfig(t, tr, map[string]any{"account_ids": []int64{42}, "enabled_models": []string{protocol.DefaultModelID}})
	original := tr.cfg.Clone()
	prepared := diagnosticSave(t, tr, diagnosticPrepare(tr, "one", 7))
	for range 3 {
		health, err := tr.Health(context.Background(), &pluginv1.HealthRequest{})
		if err != nil || !strings.Contains(health.StatusJson, "prepared") {
			t.Fatalf("status: %v %v", health, err)
		}
	}
	if calls.Load() != 0 || !reflect.DeepEqual(original, tr.cfg) {
		t.Fatal("prepare/status changed routing or sent a request")
	}
	job := diagnosticJobCopy(tr, "one")
	if job.State != "prepared" || job.Receipt == "" {
		t.Fatalf("bad preparation: %+v", job)
	}
	saved := diagnosticSave(t, tr, diagnosticCommit(tr, "one"))
	job = waitDiagnosticJob(t, tr, "one")
	if job.State != "completed" || !job.Result.Success || calls.Load() != 1 {
		t.Fatalf("bad result: %+v calls=%d", job, calls.Load())
	}
	check := degradationScopedResult(t, job.Result)
	if check.RequestID != "one" || !reflect.DeepEqual(check.TargetAccountIDs, []int64{7}) {
		t.Fatalf("wrong target: %+v", check)
	}
	for _, raw := range [][]byte{prepared, saved, saved} {
		applied, _ := tr.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: raw})
		if !applied.Applied {
			t.Fatal(applied)
		}
	}
	if calls.Load() != 1 || !reflect.DeepEqual(original, tr.cfg) {
		t.Fatal("replay changed routing or resent diagnostic")
	}
}

func TestDiagnosticJobSavedEnvelopesAllowOrdinaryConnectivityTest(t *testing.T) {
	var httpRequests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		httpRequests.Add(1)
	}))
	defer endpoint.Close()
	tr, host, probes := newDiagnosticTransport(t)
	applyConfig(t, tr, map[string]any{"responses_url": endpoint.URL, "account_ids": []int64{42}})
	production := tr.cfg.Clone()
	prepared := diagnosticSave(t, tr, diagnosticPrepare(tr, "ordinary-test", 7))
	assertConnectivity := func(runtime *Transport, raw []byte) {
		t.Helper()
		before := runtime.cfg.Clone()
		result, err := runtime.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: raw})
		if err != nil || !result.Success {
			t.Fatalf("ordinary test rejected saved envelope: %v %v", result, err)
		}
		if !reflect.DeepEqual(runtime.cfg, before) || httpRequests.Load() != 0 {
			t.Fatal("ordinary connectivity test changed production config or sent an HTTP request")
		}
	}
	assertConnectivity(tr, prepared)
	if probes.Load() != 0 || diagnosticJobCopy(tr, "ordinary-test").State != "prepared" {
		t.Fatal("ordinary test committed the pending diagnostic")
	}
	saved := diagnosticSave(t, tr, diagnosticCommit(tr, "ordinary-test"))
	waitDiagnosticJob(t, tr, "ordinary-test")
	assertConnectivity(tr, saved)

	// The host normalizes and applies old config after a restart, but its Test
	// RPC still carries the original saved envelope from the previous boot.
	fresh := New()
	defer fresh.Shutdown()
	fresh.host = host
	fresh.client = tr.client
	diagnosticSave(t, fresh, json.RawMessage(saved))
	assertConnectivity(fresh, prepared)
	assertConnectivity(fresh, saved)
	if probes.Load() != 1 || !reflect.DeepEqual(fresh.cfg, production) || len(fresh.diagnosticJobs.jobs) != 0 {
		t.Fatal("ordinary test replayed a task or lost production config after restart")
	}
}

func TestDiagnosticJobOrdinaryTestRejectsCommandsAndForgedEnvelopes(t *testing.T) {
	tr, _, probes := newDiagnosticTransport(t)
	prepare := diagnosticPrepare(tr, "reject-test", 7)
	saved := diagnosticSave(t, tr, prepare)
	commit := diagnosticCommit(tr, "reject-test")
	var forged map[string]any
	if err := json.Unmarshal(saved, &forged); err != nil {
		t.Fatal(err)
	}
	forged["account_ids"] = []int64{999}
	for _, value := range []any{prepare, commit, forged} {
		result, err := tr.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: protocol.JSONBytes(value)})
		if err != nil || result.Success {
			t.Fatalf("ordinary test accepted an untrusted command: %v %v", result, err)
		}
	}
	if probes.Load() != 0 || diagnosticJobCopy(tr, "reject-test").State != "prepared" {
		t.Fatal("ordinary test executed a diagnostic command")
	}
}

func TestDiagnosticJobSnapshotIsolationBusyAndBulk(t *testing.T) {
	tr, host, calls := newDiagnosticTransport(t)
	first := diagnosticPrepare(tr, "bulk", 7, 9, 999)
	diagnosticSave(t, tr, first)
	other, _ := tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: protocol.JSONBytes(diagnosticPrepare(tr, "other", 9))})
	if other.Valid {
		t.Fatal("concurrent page replaced the pending task")
	}
	changed := diagnosticPrepare(tr, "bulk", 9)
	validation, _ := tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: protocol.JSONBytes(changed)})
	applied, _ := tr.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: validation.NormalizedConfigJson})
	if applied.Applied {
		t.Fatal("same ID accepted a replacement snapshot")
	}
	applyConfig(t, tr, map[string]any{"account_ids": []int64{999}, "excluded_account_ids": []int64{7, 9}, "auto_select_new_accounts": true})
	diagnosticSave(t, tr, diagnosticCommit(tr, "bulk"))
	job := waitDiagnosticJob(t, tr, "bulk")
	check := degradationScopedResult(t, job.Result)
	if calls.Load() != 2 || len(check.Results) != 3 || check.Results[2].Status != "skipped" || !reflect.DeepEqual(check.TargetAccountIDs, []int64{7, 9, 999}) {
		t.Fatalf("snapshot changed: %+v calls=%d", check, calls.Load())
	}
	if len(host.resolvedAccountIDs) != 2 {
		t.Fatalf("wrong credentials: %v", host.resolvedAccountIDs)
	}
}

func TestDiagnosticJobKeepsPreparedRequestParametersAfterProductionSave(t *testing.T) {
	for _, tc := range []struct {
		name              string
		snapshotSeconds   int
		productionSeconds int
		wantDeadline      time.Duration
	}{
		{name: "snapshot-shorter", snapshotSeconds: 10, productionSeconds: 300, wantDeadline: 10 * time.Second},
		{name: "production-shorter", snapshotSeconds: 300, productionSeconds: 10, wantDeadline: degradationCheckTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, _, _ := newDiagnosticTransport(t)
			command := diagnosticPrepare(tr, "request-parameters", 7)
			payload := command["diagnostic_task"].(map[string]any)
			cfg := payload["config"].(protocol.Config)
			cfg.TimeoutSeconds = tc.snapshotSeconds
			cfg.ResponsesURL = "https://prepared.example.invalid/responses"
			cfg.EnabledModels = []string{"gpt-6-luna"}
			cfg.DegradationCheckModel = "gpt-5.4-mini"
			payload["config"] = cfg
			diagnosticSave(t, tr, command)
			applyConfig(t, tr, map[string]any{
				"timeout_seconds": tc.productionSeconds, "responses_url": "https://production.example.invalid/responses",
				"enabled_models": []string{protocol.DefaultModelID},
			})
			type observation struct {
				remaining time.Duration
				model     string
				url       string
			}
			requests := make(chan observation, 1)
			// Preserve the current production client's timeout while injecting a
			// transport that records the deadline and completes immediately.
			tr.client.Timeout = time.Duration(tc.productionSeconds) * time.Second
			tr.client.Transport = degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
				deadline, _ := r.Context().Deadline()
				var body struct {
					Model string
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					return nil, err
				}
				requests <- observation{remaining: time.Until(deadline), model: body.Model, url: r.URL.String()}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(protocol.JSONBytes(map[string]any{"output_text": "iPhone 17"}))))}, nil
			})
			diagnosticSave(t, tr, diagnosticCommit(tr, "request-parameters"))
			job := waitDiagnosticJob(t, tr, "request-parameters")
			if job.State != "completed" {
				t.Fatalf("diagnostic failed: %+v", job)
			}
			select {
			case actual := <-requests:
				if actual.remaining < tc.wantDeadline-2*time.Second || actual.remaining > tc.wantDeadline {
					t.Fatalf("request deadline changed with production config: got %v, want near %v", actual.remaining, tc.wantDeadline)
				}
				if actual.model != "gpt-6-astra" || actual.url != nativeDegradationResponsesURL {
					t.Fatalf("request did not use fixed native model/endpoint: %+v", actual)
				}
			default:
				t.Fatal("diagnostic did not reach the injected transport")
			}
			if tr.client.Timeout != time.Duration(tc.productionSeconds)*time.Second {
				t.Fatal("diagnostic changed the shared production client timeout")
			}
		})
	}
}

func TestDiagnosticJobKVFailuresFailClosed(t *testing.T) {
	for _, state := range []string{"get", "prepared", "queued", "running"} {
		t.Run(state, func(t *testing.T) {
			tr, host, calls := newDiagnosticTransport(t)
			if state == "get" {
				host.failGet = true
			} else if state == "prepared" {
				host.failState = state
			}
			validation, _ := tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: protocol.JSONBytes(diagnosticPrepare(tr, state, 7))})
			applied, _ := tr.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: validation.NormalizedConfigJson})
			if state == "get" || state == "prepared" {
				if applied.Applied {
					t.Fatal("storage failure accepted prepare")
				}
			} else {
				if !applied.Applied {
					t.Fatal(applied)
				}
				host.mu.Lock()
				host.failState = state
				host.mu.Unlock()
				validation, _ = tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: protocol.JSONBytes(diagnosticCommit(tr, state))})
				applied, _ = tr.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: validation.NormalizedConfigJson})
				if state == "queued" && applied.Applied {
					t.Fatal("queued write failure accepted commit")
				}
				if state == "running" {
					job := waitDiagnosticJob(t, tr, state)
					if job.State != "failed" {
						t.Fatal(job.State)
					}
				}
			}
			if calls.Load() != 0 || len(host.resolvedAccountIDs) != 0 {
				t.Fatal("storage failure still sent a charged request")
			}
		})
	}
}

func TestDiagnosticJobRestartDiscardsCommandRestoresProduction(t *testing.T) {
	tr, host, calls := newDiagnosticTransport(t)
	applyConfig(t, tr, map[string]any{"account_ids": []int64{42}, "enabled_models": []string{protocol.DefaultModelID}})
	original := tr.cfg.Clone()
	diagnosticSave(t, tr, diagnosticPrepare(tr, "restart", 7))
	saved := diagnosticSave(t, tr, diagnosticCommit(tr, "restart"))
	waitDiagnosticJob(t, tr, "restart")
	fresh := New()
	defer fresh.Shutdown()
	fresh.host = host
	fresh.client = tr.client
	if fresh.diagnosticJobs.ownerID == tr.diagnosticJobs.ownerID {
		t.Fatal("boot owner reused")
	}
	validation, _ := fresh.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: saved})
	if !validation.Valid || strings.Contains(string(validation.NormalizedConfigJson), "diagnostic_task") {
		t.Fatal(validation)
	}
	applied, _ := fresh.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: validation.NormalizedConfigJson})
	if !applied.Applied || !reflect.DeepEqual(fresh.cfg, original) || calls.Load() != 1 {
		t.Fatal("restart replayed command or lost config")
	}
	direct, _ := fresh.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: protocol.JSONBytes(diagnosticPrepare(tr, "stale", 7))})
	if direct.Valid {
		t.Fatal("stale page accepted")
	}
}

func TestDiagnosticJobShutdownInterruptsWithoutReplay(t *testing.T) {
	tr, _, calls := newDiagnosticTransport(t)
	started := make(chan struct{})
	tr.client = &http.Client{Transport: degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	diagnosticSave(t, tr, diagnosticPrepare(tr, "cancel", 7))
	diagnosticSave(t, tr, diagnosticCommit(tr, "cancel"))
	<-started
	tr.Shutdown()
	job := diagnosticJobCopy(tr, "cancel")
	if job.State != "unknown" || calls.Load() != 1 {
		t.Fatalf("bad cancellation: %+v", job)
	}
}

func TestDiagnosticJobCannotAcknowledgeUnsaved403Recovery(t *testing.T) {
	tr, _, calls := newDiagnosticTransport(t)
	tr.disableBPSAccount(context.Background(), 7)
	tr.bpsAccounts.mu.RLock()
	record := tr.bpsAccounts.records[7]
	tr.bpsAccounts.mu.RUnlock()
	command := diagnosticPrepare(tr, "blocked", 7)
	payload := command["diagnostic_task"].(map[string]any)
	cfg := payload["config"].(protocol.Config)
	cfg.BPSReenabledAccounts = map[string]string{"7": record.BlockID}
	payload["config"] = cfg
	diagnosticSave(t, tr, command)
	diagnosticSave(t, tr, diagnosticCommit(tr, "blocked"))
	job := waitDiagnosticJob(t, tr, "blocked")
	check := degradationScopedResult(t, job.Result)
	if calls.Load() != 1 || check.Results[0].Status != "ok" || !tr.isBPSAccountDisabled(7, tr.cfg) {
		t.Fatalf("native probe should run without acknowledging BPS recovery: %+v", check)
	}
}

func TestDiagnosticJobFreshIDAndExpiredReceipt(t *testing.T) {
	tr, _, calls := newDiagnosticTransport(t)
	diagnosticSave(t, tr, diagnosticPrepare(tr, "expire", 7))
	commit := diagnosticCommit(tr, "expire")
	tr.diagnosticJobs.mu.Lock()
	tr.diagnosticJobs.jobs["expire"].ExpiresAt = time.Now().Add(-time.Second)
	tr.diagnosticJobs.mu.Unlock()
	validation, _ := tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: protocol.JSONBytes(commit)})
	if validation.Valid || calls.Load() != 0 {
		t.Fatal("expired task executed")
	}
	diagnosticSave(t, tr, diagnosticPrepare(tr, "next", 9))
	for _, bad := range []string{"", strings.Repeat("x", 129), "bad id"} {
		cmd := diagnosticPrepare(tr, fmt.Sprintf("bad-%d", len(bad)), 7)
		cmd["diagnostic_task"].(map[string]any)["task_id"] = bad
		v, _ := tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: protocol.JSONBytes(cmd)})
		if v.Valid {
			t.Fatal("invalid task ID accepted")
		}
	}
}

func TestDiagnosticJobRollbackRestoresSignedProductionEnvelope(t *testing.T) {
	tr, _, calls := newDiagnosticTransport(t)
	applyConfig(t, tr, map[string]any{"account_ids": []int64{42}})
	production := tr.cfg.Clone()
	prepared := diagnosticSave(t, tr, diagnosticPrepare(tr, "old", 7))
	saved := diagnosticSave(t, tr, diagnosticCommit(tr, "old"))
	waitDiagnosticJob(t, tr, "old")
	// A later ordinary save mutates the runtime before the host DB fails.
	applyConfig(t, tr, map[string]any{"account_ids": []int64{999}})
	validation, _ := tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: saved})
	if !validation.Valid {
		t.Fatal(validation)
	}
	applied, _ := tr.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: validation.NormalizedConfigJson})
	if !applied.Applied || !reflect.DeepEqual(tr.cfg, production) {
		t.Fatalf("rollback lost production config: %v %+v", applied, tr.cfg)
	}
	diagnosticSave(t, tr, diagnosticPrepare(tr, "new", 9))
	// Rolling back a failed save may also replay an older completed prepare.
	for _, raw := range [][]byte{prepared, saved} {
		validation, _ = tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: raw})
		if !validation.Valid {
			t.Fatal(validation)
		}
		applied, _ = tr.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: validation.NormalizedConfigJson})
		if !applied.Applied {
			t.Fatal(applied)
		}
	}
	if calls.Load() != 1 || diagnosticJobCopy(tr, "new").State != "prepared" {
		t.Fatal("rollback resent or replaced a diagnostic")
	}
	var forged map[string]any
	_ = json.Unmarshal(saved, &forged)
	forged["account_ids"] = []int64{123}
	invalid, _ := tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: protocol.JSONBytes(forged)})
	if invalid.Valid {
		t.Fatal("forged production envelope was accepted")
	}
}

func TestDiagnosticJobConcurrentCommitsExecuteOnce(t *testing.T) {
	tr, _, calls := newDiagnosticTransport(t)
	diagnosticSave(t, tr, diagnosticPrepare(tr, "concurrent", 7))
	value := protocol.JSONBytes(diagnosticCommit(tr, "concurrent"))
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			validation, _ := tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: value})
			if !validation.Valid {
				t.Error(validation)
				return
			}
			applied, _ := tr.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: validation.NormalizedConfigJson})
			if !applied.Applied {
				t.Error(applied)
			}
		}()
	}
	wg.Wait()
	waitDiagnosticJob(t, tr, "concurrent")
	if calls.Load() != 1 {
		t.Fatalf("duplicate commits sent %d requests", calls.Load())
	}
}

func TestDiagnosticJobResultWriteFailureAndHistoryDoNotReplay(t *testing.T) {
	tr, host, calls := newDiagnosticTransport(t)
	diagnosticSave(t, tr, diagnosticPrepare(tr, "result", 7))
	host.mu.Lock()
	host.failState = "completed"
	host.mu.Unlock()
	saved := diagnosticSave(t, tr, diagnosticCommit(tr, "result"))
	job := waitDiagnosticJob(t, tr, "result")
	if job.State != "completed" || job.Error == "" {
		t.Fatal("result persistence failure disappeared")
	}
	host.mu.Lock()
	host.failState = ""
	host.mu.Unlock()
	diagnosticSave(t, tr, diagnosticCommit(tr, "result"))
	tr.diagnosticJobs.mu.Lock()
	tr.diagnosticJobs.jobs["result"].UpdatedAt = time.Now().Add(-diagnosticHistoryTTL - time.Second)
	tr.diagnosticJobs.mu.Unlock()
	diagnosticSave(t, tr, diagnosticPrepare(tr, "later", 9))
	validation, _ := tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: saved})
	applied, _ := tr.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: validation.NormalizedConfigJson})
	if !applied.Applied || calls.Load() != 1 {
		t.Fatal("pruned consumed task replayed or broke rollback")
	}
}

func TestDiagnosticJobUnknownStoredRecordFailsClosed(t *testing.T) {
	tr, host, calls := newDiagnosticTransport(t)
	host.values[diagnosticJobNamespace+"/"+diagnosticJobKey(tr.diagnosticJobs.ownerID, "corrupt")] = []byte(`{"state":"unknown-state"}`)
	validation, _ := tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: protocol.JSONBytes(diagnosticPrepare(tr, "corrupt", 7))})
	applied, _ := tr.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: validation.NormalizedConfigJson})
	if applied.Applied || calls.Load() != 0 || len(host.resolvedAccountIDs) != 0 {
		t.Fatal("unknown persisted record accepted")
	}
	health, _ := tr.Health(context.Background(), &pluginv1.HealthRequest{})
	if !strings.Contains(health.StatusJson, "unrecognized persisted state") {
		t.Fatal("storage error missing from passive status")
	}
}

func TestDiagnosticJobExpiredSignedPrepareCannotReviveOrBlockRollback(t *testing.T) {
	tr, _, calls := newDiagnosticTransport(t)
	saved := diagnosticSave(t, tr, diagnosticPrepare(tr, "oldprepare", 7))
	var envelope map[string]json.RawMessage
	_ = json.Unmarshal(saved, &envelope)
	var cmd diagnosticCommand
	_ = json.Unmarshal(envelope["diagnostic_task"], &cmd)
	delete(envelope, "diagnostic_task")
	base, err := protocol.ParseConfig(protocol.JSONBytes(envelope))
	if err != nil {
		t.Fatal(err)
	}
	cmd.IssuedAt = time.Now().Add(-diagnosticPreparedTTL - time.Second).UnixMilli()
	cmd.Seal = tr.diagnosticJobs.envelopeSeal(protocol.JSONBytes(base), cmd)
	envelope["diagnostic_task"] = protocol.JSONBytes(cmd)
	tr.diagnosticJobs.mu.Lock()
	delete(tr.diagnosticJobs.jobs, "oldprepare")
	tr.diagnosticJobs.active = ""
	tr.diagnosticJobs.mu.Unlock()
	diagnosticSave(t, tr, diagnosticPrepare(tr, "current", 9))
	validation, _ := tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: protocol.JSONBytes(envelope)})
	applied, _ := tr.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: validation.NormalizedConfigJson})
	if !applied.Applied || calls.Load() != 0 || diagnosticJobCopy(tr, "current").State != "prepared" {
		t.Fatal("old prepare broke current task or rollback")
	}
}

func TestDiagnosticJobNoHostAndCommitForgery(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	validation, _ := tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: protocol.JSONBytes(diagnosticPrepare(tr, "unavailable", 7))})
	applied, _ := tr.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: validation.NormalizedConfigJson})
	if applied.Applied {
		t.Fatal("diagnostic accepted without host storage")
	}
	live, _, calls := newDiagnosticTransport(t)
	diagnosticSave(t, live, diagnosticPrepare(live, "receipt", 7))
	commit := diagnosticCommit(live, "receipt")
	command := commit["diagnostic_task"].(map[string]any)
	command["receipt"] = "forged"
	validation, _ = live.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: protocol.JSONBytes(commit)})
	if validation.Valid || calls.Load() != 0 {
		t.Fatal("forged receipt accepted")
	}
}
