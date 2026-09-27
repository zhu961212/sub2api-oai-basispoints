package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

const (
	autoDegradationNamespace   = "native_degradation_v1"
	autoDegradationIndexKey    = "account_index"
	autoDegradationRetry       = 5 * time.Minute
	autoDegradationMaxAccounts = 10000
	// The marker cannot collide with a caller's validDiagnosticID.
	autoDegradationDiagnosticID = "@automatic-native-check"
)

var errAutoDegradationStore = errors.New("automatic detection state is unavailable; account routing has not been changed")

// Only routing decisions and timestamps are stored; never tokens or model output.
type autoDegradationRecord struct {
	NativeTimezoneByIP bool      `json:"native_timezone_by_ip"`
	AccountID          int64     `json:"account_id"`
	SelectionBase      string    `json:"selection_base"`
	Model              string    `json:"model"`
	BPSEnabled         bool      `json:"bps_enabled"`
	Decided            bool      `json:"decided"`
	Status             string    `json:"status"`
	CheckedAt          time.Time `json:"checked_at"`
	NextCheckAt        time.Time `json:"next_check_at"`
	InFlight           bool      `json:"in_flight"`
	PendingStatus      string    `json:"pending_status"`
	Consecutive        int       `json:"consecutive"`
}

type autoDegradationState struct {
	mu                     sync.Mutex
	ioMu                   sync.Mutex
	workers                sync.WaitGroup
	generation             uint64
	revision               uint64
	bound, loaded, running bool
	records                map[int64]autoDegradationRecord
	firstDue               map[int64]time.Time
	cancel, probeCancel    context.CancelFunc
	wake                   chan struct{}
	lastRun, nextRun       time.Time
	storageError           string
}

func autoSelectionBase(c protocol.Config) string {
	ids, excluded := append([]int64{}, c.AccountIDs...), append([]int64{}, c.ExcludedAccountIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	sort.Slice(excluded, func(i, j int) bool { return excluded[i] < excluded[j] })
	raw, _ := json.Marshal(struct {
		Auto           bool
		ManualRevision int64
		IDs, Excluded  []int64
	}{c.AutoSelectNewAccounts, c.AutoDegradationManualRevision, ids, excluded})
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func autoRecordSelection(c protocol.Config, r autoDegradationRecord, base string) bool {
	if r.Decided && r.SelectionBase == base {
		return r.BPSEnabled
	}
	return c.HandlesAccount(r.AccountID)
}

// Call under t.mu. Rebinding fences both late reads and old request results.
func (t *Transport) bindAutoDegradation(host pluginv1.HostServiceClient) {
	s := &t.autoDegradation
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	if s.probeCancel != nil {
		s.probeCancel()
	}
	s.generation++
	s.revision++
	generation := s.generation
	s.bound, s.loaded, s.running = host != nil, host == nil, false
	s.records = make(map[int64]autoDegradationRecord)
	s.firstDue = make(map[int64]time.Time)
	s.storageError = ""
	s.lastRun, s.nextRun = time.Time{}, time.Time{}
	s.wake = make(chan struct{}, 1)
	wake := s.wake
	if host == nil {
		s.cancel = nil
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.workers.Add(1)
	s.mu.Unlock()
	go func() { defer s.workers.Done(); t.runAutoDegradation(ctx, host, generation, wake) }()
}

// Configuration changes cancel in-flight probes without discarding decisions.
func (t *Transport) configureAutoDegradation(old, next protocol.Config) {
	if old.AutoDegradationEnabled == next.AutoDegradationEnabled &&
		old.AutoDegradationIntervalMinutes == next.AutoDegradationIntervalMinutes &&
		old.NativeTimezoneByIP == next.NativeTimezoneByIP &&
		old.DegradationCheckModel == next.DegradationCheckModel && autoSelectionBase(old) == autoSelectionBase(next) {
		return
	}
	s := &t.autoDegradation
	s.mu.Lock()
	s.revision++
	if s.probeCancel != nil {
		s.probeCancel()
	}
	s.firstDue = make(map[int64]time.Time)
	s.nextRun = time.Time{}
	if s.wake != nil {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
}

func (t *Transport) automaticBPSSelection(c protocol.Config, id int64) (bool, error) {
	s := &t.autoDegradation
	s.mu.Lock()
	defer s.mu.Unlock()
	// Never briefly restore a stale manual route while durable decisions load.
	if s.bound && !s.loaded {
		return false, errAutoDegradationStore
	}
	record, exists := s.records[id]
	if !exists {
		return c.HandlesAccount(id), nil
	}
	return autoRecordSelection(c, record, autoSelectionBase(c)), nil
}

func nextAutoDegradationRecord(c protocol.Config, old autoDegradationRecord, result degradationAccountResult, now time.Time) autoDegradationRecord {
	base := autoSelectionBase(c)
	current := autoRecordSelection(c, old, base)
	record := old
	record.AccountID, record.SelectionBase, record.Model = result.AccountID, base, c.DegradationCheckModel
	record.NativeTimezoneByIP = c.NativeTimezoneByIP
	record.BPSEnabled = current
	record.Decided = old.Decided && old.SelectionBase == base
	record.Status, record.CheckedAt = result.Status, now
	record.InFlight = false
	record.NextCheckAt = now.Add(time.Duration(c.AutoDegradationIntervalMinutes) * time.Minute)
	if result.Status != "ok" && result.Status != "degraded" {
		record.PendingStatus, record.Consecutive = "", 0
		if result.Status != "skipped" {
			record.NextCheckAt = now.Add(autoDegradationRetry)
		}
		return record
	}
	desired := result.Status == "degraded"
	if record.Decided && desired == current {
		record.PendingStatus, record.Consecutive = "", 2
		return record
	}
	record.PendingStatus, record.Consecutive = result.Status, 1
	if old.SelectionBase == base && old.Model == c.DegradationCheckModel &&
		old.NativeTimezoneByIP == c.NativeTimezoneByIP && old.PendingStatus == result.Status &&
		!old.CheckedAt.IsZero() && now.Sub(old.CheckedAt) <= 2*autoDegradationRetry {
		record.Consecutive = min(old.Consecutive+1, 2)
	}
	if record.Consecutive >= 2 {
		record.Decided, record.BPSEnabled, record.PendingStatus = true, desired, ""
	} else {
		record.NextCheckAt = now.Add(autoDegradationRetry)
	}
	return record
}

func autoRecordKey(id int64) string { return "account-" + strconv.FormatInt(id, 10) }

func loadAutoDegradationRecords(ctx context.Context, host pluginv1.HostServiceClient) (map[int64]autoDegradationRecord, error) {
	response, err := host.KVGet(ctx, &pluginv1.KVGetRequest{Namespace: autoDegradationNamespace, Key: autoDegradationIndexKey})
	if err != nil || response == nil {
		return nil, errAutoDegradationStore
	}
	records := make(map[int64]autoDegradationRecord)
	if !response.Found {
		return records, nil
	}
	var ids []int64
	if json.Unmarshal(response.Value, &ids) != nil || len(ids) > autoDegradationMaxAccounts {
		return nil, errAutoDegradationStore
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return nil, errAutoDegradationStore
		}
		seen[id] = true
	}
	jobs := make(chan int64)
	var mu sync.Mutex
	var wg sync.WaitGroup
	failed := false
	for range min(8, len(ids)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				response, err := host.KVGet(ctx, &pluginv1.KVGetRequest{Namespace: autoDegradationNamespace, Key: autoRecordKey(id)})
				var r autoDegradationRecord
				valid := err == nil && response != nil && response.Found && json.Unmarshal(response.Value, &r) == nil &&
					r.AccountID == id && len(r.SelectionBase) == 64 && r.Consecutive >= 0 && r.Consecutive <= 2
				if valid {
					_, err = hex.DecodeString(r.SelectionBase)
					valid = err == nil
				}
				mu.Lock()
				if !valid {
					failed = true
				} else {
					records[id] = r
				}
				mu.Unlock()
			}
		}()
	}
dispatch:
	for _, id := range ids {
		select {
		case jobs <- id:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()
	if failed || ctx.Err() != nil {
		return nil, errAutoDegradationStore
	}
	return records, nil
}

// Persistence precedes publication. The short read lock orders a completed
// write with disabling automation, manual saves, rebinding, and shutdown.
func (t *Transport) persistAutoDegradationRecord(ctx context.Context, host pluginv1.HostServiceClient, generation, revision uint64, record autoDegradationRecord) error {
	s := &t.autoDegradation
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	t.mu.RLock()
	defer t.mu.RUnlock()
	s.mu.Lock()
	valid := !t.closed && t.cfg.AutoDegradationEnabled && s.generation == generation && s.revision == revision && s.loaded && ctx.Err() == nil
	_, existed := s.records[record.AccountID]
	ids := make([]int64, 0, len(s.records)+1)
	if !existed {
		for id := range s.records {
			ids = append(ids, id)
		}
		ids = append(ids, record.AccountID)
	}
	s.mu.Unlock()
	if !valid {
		return context.Canceled
	}
	if len(ids) > autoDegradationMaxAccounts {
		return errAutoDegradationStore
	}
	writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, _ := json.Marshal(record)
	if _, err := host.KVSet(writeCtx, &pluginv1.KVSetRequest{Namespace: autoDegradationNamespace, Key: autoRecordKey(record.AccountID), Value: raw}); err != nil {
		return errAutoDegradationStore
	}
	if !existed {
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		raw, _ = json.Marshal(ids)
		if _, err := host.KVSet(writeCtx, &pluginv1.KVSetRequest{Namespace: autoDegradationNamespace, Key: autoDegradationIndexKey, Value: raw}); err != nil {
			return errAutoDegradationStore
		}
	}
	s.mu.Lock()
	s.records[record.AccountID] = record
	s.mu.Unlock()
	return nil
}

func waitAutoDegradation(ctx context.Context, wake <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return true
	case <-timer.C:
		return true
	}
}

func (t *Transport) setAutoDegradationError(generation uint64, message string) {
	s := &t.autoDegradation
	s.mu.Lock()
	if s.generation == generation {
		s.storageError = message
	}
	s.mu.Unlock()
}

func (t *Transport) runAutoDegradation(ctx context.Context, host pluginv1.HostServiceClient, generation uint64, wake <-chan struct{}) {
	s := &t.autoDegradation
	for ctx.Err() == nil {
		s.ioMu.Lock()
		loadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		records, err := loadAutoDegradationRecords(loadCtx, host)
		cancel()
		s.mu.Lock()
		current := s.generation == generation && ctx.Err() == nil
		if current && err == nil {
			s.records, s.loaded, s.storageError = records, true, ""
		}
		s.mu.Unlock()
		s.ioMu.Unlock()
		if !current {
			return
		}
		if err == nil {
			break
		}
		t.setAutoDegradationError(generation, errAutoDegradationStore.Error())
		if !waitAutoDegradation(ctx, wake, time.Minute) {
			return
		}
	}
	for ctx.Err() == nil {
		t.mu.RLock()
		cfg, closed := t.cfg.Clone(), t.closed
		s.mu.Lock()
		revision := s.revision
		s.mu.Unlock()
		t.mu.RUnlock()
		if closed {
			return
		}
		if !cfg.AutoDegradationEnabled {
			if !waitAutoDegradation(ctx, wake, 24*time.Hour) {
				return
			}
			continue
		}
		listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		directory, err := host.ListAccounts(listCtx, &pluginv1.ListAccountsRequest{Platform: "openai", AccountType: "oauth"})
		cancel()
		if err != nil || directory == nil {
			t.setAutoDegradationError(generation, "automatic detection could not enumerate accounts; routing is unchanged")
			if !waitAutoDegradation(ctx, wake, time.Minute) {
				return
			}
			continue
		}
		now := time.Now()
		type scheduled struct {
			id  int64
			due time.Time
		}
		queue := []scheduled{}
		seen := make(map[int64]bool)
		eligible := make(map[int64]bool)
		for _, account := range directory.GetAccounts() {
			if account != nil && account.GetId() > 0 {
				id := account.GetId()
				if !seen[id] {
					eligible[id] = account.GetSchedulable()
				} else {
					eligible[id] = eligible[id] && account.GetSchedulable()
				}
				seen[id] = true
			}
		}
		if len(directory.GetAccounts()) == 0 {
			for _, id := range directory.GetAccountIds() {
				if id > 0 {
					eligible[id] = true
				}
			}
		}
		base := autoSelectionBase(cfg)
		s.mu.Lock()
		for id, available := range eligible {
			if !available {
				continue
			}
			r, exists := s.records[id]
			due := r.NextCheckAt
			if !exists || due.IsZero() || r.Model != cfg.DegradationCheckModel ||
				r.NativeTimezoneByIP != cfg.NativeTimezoneByIP || r.SelectionBase != base {
				due = s.firstDue[id]
				if due.IsZero() {
					due = now.Add(time.Duration(5+id%26) * time.Second)
					s.firstDue[id] = due
				}
			}
			// A shorter configured interval must not retain an old, distant deadline.
			if exists && !r.InFlight && r.PendingStatus == "" && !r.CheckedAt.IsZero() {
				regular := r.CheckedAt.Add(time.Duration(cfg.AutoDegradationIntervalMinutes) * time.Minute)
				if regular.Before(due) {
					due = regular
				}
			}
			queue = append(queue, scheduled{id, due})
		}
		sort.Slice(queue, func(i, j int) bool {
			if queue[i].due.Equal(queue[j].due) {
				return queue[i].id < queue[j].id
			}
			return queue[i].due.Before(queue[j].due)
		})
		s.nextRun = time.Time{}
		if len(queue) > 0 {
			s.nextRun = queue[0].due
		}
		s.mu.Unlock()
		ids := []int64{}
		for _, item := range queue {
			if item.due.After(now) || len(ids) >= degradationCheckParallel {
				break
			}
			ids = append(ids, item.id)
		}
		if len(ids) == 0 {
			delay := time.Minute
			if len(queue) > 0 {
				delay = min(delay, max(time.Until(queue[0].due), time.Second))
			}
			if !waitAutoDegradation(ctx, wake, delay) {
				return
			}
			continue
		}
		if !t.reserveAutomaticDiagnostic() {
			if !waitAutoDegradation(ctx, wake, 15*time.Second) {
				return
			}
			continue
		}
		t.runAutomaticBatch(ctx, host, generation, revision, cfg, ids)
		t.releaseAutomaticDiagnostic()
		delay := time.Second
		s.mu.Lock()
		if s.storageError != "" {
			delay = time.Minute
		}
		s.mu.Unlock()
		if !waitAutoDegradation(ctx, wake, delay) {
			return
		}
	}
}

func (t *Transport) reserveAutomaticDiagnostic() bool {
	s := t.diagnosticJobs
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now())
	if s.closed || s.active != "" || s.scopedActive > 0 {
		return false
	}
	s.active = autoDegradationDiagnosticID
	return true
}
func (t *Transport) releaseAutomaticDiagnostic() {
	s := t.diagnosticJobs
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == autoDegradationDiagnosticID {
		s.active = ""
	}
}

// Retain compatibility with concurrent scoped manual tests, while excluding
// automatic batches from all manual probe windows.
func (t *Transport) beginScopedDiagnostic() bool {
	s := t.diagnosticJobs
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.active == autoDegradationDiagnosticID {
		return false
	}
	s.scopedActive++
	return true
}

func (t *Transport) endScopedDiagnostic() {
	s := t.diagnosticJobs
	s.mu.Lock()
	s.scopedActive--
	s.mu.Unlock()
}

func (t *Transport) runAutomaticBatch(parent context.Context, host pluginv1.HostServiceClient, generation, revision uint64, cfg protocol.Config, ids []int64) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	s := &t.autoDegradation
	s.mu.Lock()
	if s.generation != generation || s.revision != revision {
		s.mu.Unlock()
		return
	}
	s.running, s.probeCancel, s.lastRun = true, cancel, time.Now()
	before := make(map[int64]autoDegradationRecord, len(ids))
	for _, id := range ids {
		r := s.records[id]
		r.AccountID = id
		before[id] = r
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.generation == generation {
			s.running = false
			s.probeCancel = nil
		}
		s.mu.Unlock()
	}()
	started := []int64{}
	for _, id := range ids {
		lease := before[id]
		if lease.SelectionBase != autoSelectionBase(cfg) {
			lease.BPSEnabled, lease.Decided = cfg.HandlesAccount(id), false
		}
		if lease.SelectionBase != autoSelectionBase(cfg) || lease.Model != cfg.DegradationCheckModel ||
			lease.NativeTimezoneByIP != cfg.NativeTimezoneByIP {
			lease.PendingStatus, lease.Consecutive = "", 0
		}
		lease.SelectionBase, lease.Model, lease.InFlight = autoSelectionBase(cfg), cfg.DegradationCheckModel, true
		lease.NativeTimezoneByIP = cfg.NativeTimezoneByIP
		lease.NextCheckAt = time.Now().Add(time.Duration(cfg.AutoDegradationIntervalMinutes) * time.Minute)
		if err := t.persistAutoDegradationRecord(ctx, host, generation, revision, lease); err != nil {
			if ctx.Err() == nil {
				t.setAutoDegradationError(generation, errAutoDegradationStore.Error())
			}
			return
		}
		started = append(started, id)
	}
	if len(started) == 0 {
		return
	}
	cfg.DegradationCheck, cfg.DegradationCheckAccountID, cfg.DegradationCheckAccountIDs = true, 0, started
	check, _ := t.runDegradationCheckWithBudget(ctx, cfg, degradationCheckBudget)
	for _, result := range check.Results {
		old, exists := before[result.AccountID]
		if !exists || ctx.Err() != nil {
			continue
		}
		next := nextAutoDegradationRecord(cfg, old, result, time.Now())
		if err := t.persistAutoDegradationRecord(ctx, host, generation, revision, next); err != nil {
			if !errors.Is(err, context.Canceled) {
				t.setAutoDegradationError(generation, errAutoDegradationStore.Error())
			}
			return
		}
	}
	if ctx.Err() == nil {
		t.setAutoDegradationError(generation, "")
	}
}

func (t *Transport) autoDegradationStatusJSON(status string, cfg protocol.Config, accounts []accountSummary) string {
	s := &t.autoDegradation
	base := autoSelectionBase(cfg)
	s.mu.Lock()
	rows := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		r := s.records[account.ID]
		r.AccountID = account.ID
		state := r.Status
		if state == "" {
			state = "unknown"
		}
		rows = append(rows, map[string]any{"account_id": account.ID, "status": state, "bps_enabled": autoRecordSelection(cfg, r, base), "managed": r.Decided && r.SelectionBase == base, "checked_at": autoStatusTime(r.CheckedAt), "next_check_at": autoStatusTime(r.NextCheckAt), "pending_status": r.PendingStatus, "consecutive": r.Consecutive})
	}
	snapshot := map[string]any{"enabled": cfg.AutoDegradationEnabled, "running": s.running, "ready": !s.bound || s.loaded, "interval_minutes": cfg.AutoDegradationIntervalMinutes, "last_run_at": autoStatusTime(s.lastRun), "next_run_at": autoStatusTime(s.nextRun), "error": s.storageError, "accounts": rows}
	s.mu.Unlock()
	for _, row := range rows {
		if t.isBPSAccountDisabled(row["account_id"].(int64), cfg) {
			row["bps_enabled"] = false
		}
	}
	var object map[string]any
	if json.Unmarshal([]byte(status), &object) != nil {
		return status
	}
	object["auto_degradation"] = snapshot
	raw, err := json.Marshal(object)
	if err != nil {
		return status
	}
	return string(raw)
}

func autoStatusTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}
