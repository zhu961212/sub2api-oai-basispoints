package transport

import (
	"context"
	"encoding/json"
	"errors"
	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"google.golang.org/grpc"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type autoDetectionTestHost struct {
	*diagnosticTestHost
	failAutoSet atomic.Bool
}

func (h *autoDetectionTestHost) KVSet(ctx context.Context, r *pluginv1.KVSetRequest, options ...grpc.CallOption) (*pluginv1.KVSetResponse, error) {
	if r.Namespace == autoDegradationNamespace && h.failAutoSet.Load() {
		return nil, errors.New("private-storage-error")
	}
	return h.diagnosticTestHost.KVSet(ctx, r, options...)
}
func newAutoDetectionTestTransport(t *testing.T) (*Transport, *autoDetectionTestHost, *atomic.Int32) {
	t.Helper()
	tr, host, calls := newDiagnosticTransport(t)
	h := &autoDetectionTestHost{diagnosticTestHost: host}
	tr.host = h
	tr.cfg.AutoDegradationEnabled = true
	tr.cfg.AutoSelectNewAccounts = true
	tr.cfg.ExcludedAccountIDs = []int64{7, 9}
	tr.autoDegradation.records = make(map[int64]autoDegradationRecord)
	tr.autoDegradation.firstDue = make(map[int64]time.Time)
	tr.autoDegradation.loaded, tr.autoDegradation.bound = true, true
	tr.autoDegradation.generation, tr.autoDegradation.revision = 1, 1
	tr.autoDegradation.wake = make(chan struct{}, 1)
	return tr, h, calls
}

func TestAutoDegradationConfirmsBothDirectionsAndPreservesErrors(t *testing.T) {
	cfg := protocol.DefaultConfig()
	cfg.AutoSelectNewAccounts = true
	cfg.ExcludedAccountIDs = []int64{7}
	now := time.Now()
	record := autoDegradationRecord{AccountID: 7}
	step := func(status string) {
		record = nextAutoDegradationRecord(cfg, record, degradationAccountResult{AccountID: 7, Status: status}, now)
		now = now.Add(autoDegradationRetry)
	}
	step("degraded")
	if record.Decided || record.BPSEnabled || record.Consecutive != 1 {
		t.Fatalf("first answer switched route: %+v", record)
	}
	step("error")
	if record.BPSEnabled || record.Consecutive != 0 {
		t.Fatalf("error switched route or confirmed: %+v", record)
	}
	step("degraded")
	step("degraded")
	if !record.Decided || !record.BPSEnabled || record.PendingStatus != "" {
		t.Fatalf("confirmed degradation not selected: %+v", record)
	}
	step("ok")
	if !record.BPSEnabled || record.Consecutive != 1 {
		t.Fatalf("single recovery changed routing: %+v", record)
	}
	step("skipped")
	if !record.BPSEnabled || record.Consecutive != 0 {
		t.Fatalf("skipped changed routing: %+v", record)
	}
	step("ok")
	step("ok")
	if record.BPSEnabled || !record.Decided {
		t.Fatalf("confirmed recovery not applied: %+v", record)
	}
	if record.NextCheckAt.Sub(record.CheckedAt) != 30*time.Minute {
		t.Fatal("stable account does not use regular interval")
	}
}

func TestAutoDegradationModelAndStaleConfirmationsDoNotCombine(t *testing.T) {
	cfg := protocol.DefaultConfig()
	cfg.AccountIDs = []int64{9}
	now := time.Now()
	first := nextAutoDegradationRecord(cfg, autoDegradationRecord{AccountID: 7}, degradationAccountResult{AccountID: 7, Status: "degraded"}, now)
	stale := nextAutoDegradationRecord(cfg, first, degradationAccountResult{AccountID: 7, Status: "degraded"}, now.Add(time.Hour))
	if stale.Decided || stale.Consecutive != 1 {
		t.Fatal("stale answer confirmed switching")
	}
	cfg.DegradationCheckModel = "another-native-model"
	changed := nextAutoDegradationRecord(cfg, first, degradationAccountResult{AccountID: 7, Status: "degraded"}, now.Add(autoDegradationRetry))
	if changed.Decided || changed.Consecutive != 1 {
		t.Fatal("answers from different models were combined")
	}
}

func TestAutoDegradationManualSaveCanRestoreOriginalSelection(t *testing.T) {
	tr, _, _ := newAutoDetectionTestTransport(t)
	tr.cfg.AutoDegradationEnabled = false
	tr.autoDegradation.records[7] = autoDegradationRecord{AccountID: 7, SelectionBase: autoSelectionBase(tr.cfg), Decided: true, BPSEnabled: true}
	before, _ := tr.automaticBPSSelection(tr.cfg, 7)
	if !before {
		t.Fatal("disabled automation did not preserve effective selection")
	}
	// This uncheck exactly matches the original excluded IDs; explicit edit
	// intent must still invalidate the old automatic decision.
	manual := tr.cfg.Clone()
	manual.AutoDegradationManualRevision++
	after, err := tr.automaticBPSSelection(manual, 7)
	if err != nil || after {
		t.Fatal("manual return to the original baseline was ignored")
	}
}

func TestAutoDegradationBatchPersistsThenRestoresAfterRestart(t *testing.T) {
	tr, host, calls := newAutoDetectionTestTransport(t)
	tr.client = &http.Client{Transport: degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if strings.Contains(r.URL.String(), "bps") {
			t.Error("automatic probe reached BPS")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output_text":"iPhone 16"}`))}, nil
	})}
	for range 2 {
		tr.runAutomaticBatch(context.Background(), host, 1, 1, tr.cfg, []int64{7})
	}
	route, err := tr.automaticBPSSelection(tr.cfg, 7)
	if err != nil || !route || calls.Load() != 2 {
		t.Fatalf("automatic selection=%v err=%v calls=%d", route, err, calls.Load())
	}
	records, err := loadAutoDegradationRecords(context.Background(), host)
	if err != nil || !records[7].Decided || !records[7].BPSEnabled {
		t.Fatalf("durable state missing: %+v %v", records, err)
	}
	stopped := tr.cfg.Clone()
	stopped.AutoDegradationEnabled = false
	restarted := New()
	t.Cleanup(restarted.Shutdown)
	restarted.cfg = stopped
	restarted.autoDegradation.records = records
	route, err = restarted.automaticBPSSelection(stopped, 7)
	if err != nil || !route {
		t.Fatal("disabling/restart discarded last route")
	}
	// A subsequent explicit manual selection replaces the saved automatic base.
	manual := stopped.Clone()
	manual.ExcludedAccountIDs = []int64{7}
	manual.AccountIDs = []int64{9}
	route, err = restarted.automaticBPSSelection(manual, 7)
	if err != nil || route {
		t.Fatal("stale automatic result overrode a manual save")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	for key, raw := range host.values {
		if strings.HasPrefix(key, autoDegradationNamespace+"/") && (strings.Contains(string(raw), "Bearer") || strings.Contains(string(raw), "iPhone")) {
			t.Fatal("credentials or answer leaked into state")
		}
	}
}

func TestAutoDegradationStorageFailureDoesNotProbeOrSwitch(t *testing.T) {
	tr, host, calls := newAutoDetectionTestTransport(t)
	host.failAutoSet.Store(true)
	tr.runAutomaticBatch(context.Background(), host, 1, 1, tr.cfg, []int64{7})
	route, err := tr.automaticBPSSelection(tr.cfg, 7)
	if calls.Load() != 0 || route || err != nil {
		t.Fatal("storage failure issued a probe or changed routing")
	}
	if strings.Contains(tr.autoDegradation.storageError, "private") || tr.autoDegradation.storageError == "" {
		t.Fatal("storage diagnostic not safely surfaced")
	}
}

func TestAutoDegradationDisableCancelsInflightAndFencesResult(t *testing.T) {
	tr, host, calls := newAutoDetectionTestTransport(t)
	started := make(chan struct{})
	tr.client = &http.Client{Transport: degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	done := make(chan struct{})
	go func() { defer close(done); tr.runAutomaticBatch(context.Background(), host, 1, 1, tr.cfg, []int64{7}) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe never started")
	}
	cfg := tr.cfg.Clone()
	cfg.AutoDegradationEnabled = false
	result, err := tr.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: protocol.JSONBytes(cfg)})
	if err != nil || !result.Applied {
		t.Fatalf("disable failed: %v %+v", err, result)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("disable did not cancel probe")
	}
	if tr.autoDegradation.records[7].Decided {
		t.Fatal("late result changed route after disabling")
	}
	tr.runAutomaticBatch(context.Background(), host, 1, 1, cfg, []int64{7})
	if calls.Load() != 1 {
		t.Fatal("stale revision sent an additional probe")
	}
}

func TestAutoDegradationSerializesWithManualJobs(t *testing.T) {
	tr, _, _ := newAutoDetectionTestTransport(t)
	if !tr.reserveAutomaticDiagnostic() {
		t.Fatal("automatic worker cannot reserve idle slot")
	}
	cfg := diagnosticPrepare(tr, "manual-during-automatic", 7)
	validation, _ := tr.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: protocol.JSONBytes(cfg)})
	if validation.Valid {
		applied, _ := tr.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: validation.NormalizedConfigJson})
		if applied.Applied {
			t.Fatal("manual task overlapped automatic batch")
		}
	}
	tr.releaseAutomaticDiagnostic()
	diagnosticSave(t, tr, diagnosticPrepare(tr, "manual-reservation", 7))
	if tr.reserveAutomaticDiagnostic() {
		t.Fatal("automatic task overlapped prepared manual task")
	}
}

func TestAutoDegradationEffectiveRoutesAnd403Protection(t *testing.T) {
	captured := make(chan accountSelectionCapture, 4)
	native := accountSelectionUpstream(t, "native", captured)
	bps := accountSelectionUpstream(t, "bps", captured)
	tr := New()
	t.Cleanup(tr.Shutdown)
	applyConfig(t, tr, map[string]any{"responses_url": bps.URL, "account_ids": []int64{9}, "rewrite_tools": false, "transform_responses": false})
	base := autoSelectionBase(tr.cfg)
	tr.autoDegradation.records = map[int64]autoDegradationRecord{
		7: {AccountID: 7, SelectionBase: base, Decided: true, BPSEnabled: true},
		9: {AccountID: 9, SelectionBase: base, Decided: true, BPSEnabled: false},
	}
	assertAccountSelectionForward(t, tr, native.URL, captured, 7, protocol.DefaultModelID, "bps")
	assertAccountSelectionForward(t, tr, native.URL, captured, 9, protocol.DefaultModelID, "native")
	tr.bpsAccounts.records = map[int64]bpsAccountRecord{7: {AccountID: 7, BlockID: strings.Repeat("a", 32), Reason: bpsAccountFailureReason, HTTPStatus: 403}}
	assertAccountSelectionForward(t, tr, native.URL, captured, 7, protocol.DefaultModelID, "native")
	status := tr.autoDegradationStatusJSON("{}", tr.cfg, []accountSummary{{ID: 7}, {ID: 9}})
	var parsed struct {
		Auto struct {
			Accounts []struct {
				BPSEnabled bool `json:"bps_enabled"`
			}
		} `json:"auto_degradation"`
	}
	if json.Unmarshal([]byte(status), &parsed) != nil || len(parsed.Auto.Accounts) != 2 || parsed.Auto.Accounts[0].BPSEnabled || parsed.Auto.Accounts[1].BPSEnabled {
		t.Fatalf("UI and actual routing disagree: %s", status)
	}
}

func TestAutoDegradationWorkerStaysIdleWhenDisabled(t *testing.T) {
	tr, host, calls := newAutoDetectionTestTransport(t)
	tr.mu.Lock()
	tr.cfg.AutoDegradationEnabled = false
	tr.bindAutoDegradation(host)
	tr.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for {
		tr.autoDegradation.mu.Lock()
		loaded := tr.autoDegradation.loaded
		tr.autoDegradation.mu.Unlock()
		if loaded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not load state")
		}
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 0 {
		t.Fatal("disabled worker sent a probe")
	}
	tr.Shutdown()
	if calls.Load() != 0 {
		t.Fatal("shutdown sent a probe")
	}
}

func TestAutoDegradationExpiredManualReservationDoesNotBlock(t *testing.T) {
	tr, _, _ := newAutoDetectionTestTransport(t)
	diagnosticSave(t, tr, diagnosticPrepare(tr, "abandoned-manual", 7))
	tr.diagnosticJobs.mu.Lock()
	tr.diagnosticJobs.jobs["abandoned-manual"].ExpiresAt = time.Now().Add(-time.Second)
	tr.diagnosticJobs.mu.Unlock()
	if !tr.reserveAutomaticDiagnostic() {
		t.Fatal("expired preparation permanently blocks automation")
	}
	defer tr.releaseAutomaticDiagnostic()
	if validDiagnosticID(autoDegradationDiagnosticID) {
		t.Fatal("caller can claim internal reservation identifier")
	}
}

func TestAutoDegradationScopedManualAdmission(t *testing.T) {
	tr, _, calls := newAutoDetectionTestTransport(t)
	if !tr.reserveAutomaticDiagnostic() {
		t.Fatal("cannot reserve automatic slot")
	}
	cfg := tr.cfg.Clone()
	cfg.DegradationCheck = true
	cfg.DegradationCheckAccountID = 7
	result := tr.testScopedDegradation(context.Background(), cfg, "scoped-while-auto")
	if result.Success || calls.Load() != 0 {
		t.Fatal("scoped manual probe overlapped automatic scan")
	}
	tr.releaseAutomaticDiagnostic()
	if !tr.beginScopedDiagnostic() || !tr.beginScopedDiagnostic() {
		t.Fatal("concurrent scoped manual compatibility lost")
	}
	if tr.reserveAutomaticDiagnostic() {
		t.Fatal("automatic task overlaps scoped tasks")
	}
	tr.endScopedDiagnostic()
	if tr.reserveAutomaticDiagnostic() {
		t.Fatal("one remaining scoped task must block automation")
	}
	tr.endScopedDiagnostic()
	if !tr.reserveAutomaticDiagnostic() {
		t.Fatal("finished scoped tests kept automatic slot blocked")
	}
	tr.releaseAutomaticDiagnostic()
}

func TestAutoDegradationInterruptedNewModelDropsOldConfirmation(t *testing.T) {
	tr, host, _ := newAutoDetectionTestTransport(t)
	first := nextAutoDegradationRecord(tr.cfg, autoDegradationRecord{AccountID: 7}, degradationAccountResult{AccountID: 7, Status: "degraded"}, time.Now())
	tr.autoDegradation.records[7] = first
	tr.cfg.DegradationCheckModel = "different-native-model"
	ctx, cancel := context.WithCancel(context.Background())
	tr.client = &http.Client{Transport: degradationRoundTripper(func(r *http.Request) (*http.Response, error) { cancel(); return nil, context.Canceled })}
	tr.runAutomaticBatch(ctx, host, 1, 1, tr.cfg, []int64{7})
	lease := tr.autoDegradation.records[7]
	if !lease.InFlight || lease.PendingStatus != "" || lease.Consecutive != 0 {
		t.Fatalf("new model lease carried old confirmation: %+v", lease)
	}
	record := nextAutoDegradationRecord(tr.cfg, lease, degradationAccountResult{AccountID: 7, Status: "degraded"}, time.Now().Add(autoDegradationRetry))
	if record.Decided || record.Consecutive != 1 {
		t.Fatal("first completed result for new model confirmed stale result")
	}
}

func TestAutoDegradationBackgroundWorkerSwitchesAndRecovers(t *testing.T) {
	tr, host, calls := newAutoDetectionTestTransport(t)
	host.fakeHost.accounts = []*pluginv1.AccountInfo{{Id: 7, Schedulable: true}}
	var answer atomic.Value
	answer.Store("iPhone 16")
	tr.client = &http.Client{Transport: degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		raw := protocol.JSONBytes(map[string]any{"output_text": answer.Load().(string)})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})}
	tr.mu.Lock()
	tr.bindAutoDegradation(host)
	tr.mu.Unlock()
	waitFor := func(condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for !condition() {
			if time.Now().After(deadline) {
				t.Fatalf("worker did not settle; calls=%d", calls.Load())
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFor(func() bool {
		tr.autoDegradation.mu.Lock()
		defer tr.autoDegradation.mu.Unlock()
		return tr.autoDegradation.loaded
	})
	forceDue := func() {
		tr.autoDegradation.mu.Lock()
		r, exists := tr.autoDegradation.records[7]
		if exists {
			r.NextCheckAt = time.Now().Add(-time.Second)
			tr.autoDegradation.records[7] = r
		} else {
			tr.autoDegradation.firstDue[7] = time.Now().Add(-time.Second)
		}
		wake := tr.autoDegradation.wake
		tr.autoDegradation.mu.Unlock()
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	for n := int32(1); n <= 4; n++ {
		if n == 3 {
			answer.Store("iPhone 17")
		}
		forceDue()
		waitFor(func() bool {
			tr.autoDegradation.mu.Lock()
			defer tr.autoDegradation.mu.Unlock()
			return calls.Load() >= n && !tr.autoDegradation.running && !tr.autoDegradation.records[7].InFlight
		})
		route, err := tr.automaticBPSSelection(tr.cfg, 7)
		want := n == 2 || n == 3
		if err != nil || route != want {
			t.Fatalf("after scheduled check %d: BPS=%v want=%v error=%v", n, route, want, err)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("worker duplicated completed probes: %d", calls.Load())
	}
}
