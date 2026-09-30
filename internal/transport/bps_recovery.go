package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"sync"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

const (
	bpsRecoveryInterval = 6 * time.Hour
	bpsRecoveryTimeout  = 30 * time.Second
)

// Only fixed status codes and timestamps reach storage or the configuration
// page. Credentials and the synthetic inference output are never retained.
type bpsRecoveryState struct {
	mu                   sync.Mutex
	workers              sync.WaitGroup
	generation, revision uint64
	cancel, probeCancel  context.CancelFunc
	wake                 chan struct{}
	running              bool
	lastRun, nextRun     time.Time
}

// Called under t.mu, like the other background workers.
func (t *Transport) bindBPSRecovery(host pluginv1.HostServiceClient) {
	s := &t.bpsRecovery
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
	s.running = false
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
	go func() { defer s.workers.Done(); t.runBPSRecovery(ctx, host, generation, wake) }()
}

// Every routing/authentication/configuration change fences pending probes.
func (t *Transport) configureBPSRecovery(old, next protocol.Config) {
	if reflect.DeepEqual(old, next) {
		return
	}
	s := &t.bpsRecovery
	s.mu.Lock()
	s.revision++
	if s.probeCancel != nil {
		s.probeCancel()
	}
	s.nextRun = time.Time{}
	s.running, s.probeCancel = false, nil
	s.mu.Unlock()
	t.wakeBPSRecovery()
}

func (t *Transport) wakeBPSRecovery() {
	s := &t.bpsRecovery
	s.mu.Lock()
	wake := s.wake
	s.mu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (t *Transport) bpsRecoveryStatus() map[string]any {
	s := &t.bpsRecovery
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"interval_hours": 6, "preserves_selection": true, "running": s.running, "last_run_at": autoStatusTime(s.lastRun), "next_run_at": autoStatusTime(s.nextRun)}
}

func bpsRecoveryModel(c protocol.Config) string {
	for _, model := range protocol.AvailableModels() {
		if c.HandlesModel(model) {
			return model
		}
	}
	return ""
}

func (t *Transport) runBPSRecovery(ctx context.Context, host pluginv1.HostServiceClient, generation uint64, wake <-chan struct{}) {
	for ctx.Err() == nil {
		delay := t.runBPSRecoveryPass(ctx, host, generation, time.Now())
		if !waitAutoDegradation(ctx, wake, delay) {
			return
		}
	}
}

// The persisted next_check_at is also the lease: a restart during a probe does
// not issue it again before the six-hour deadline. An old record is migrated
// with a future deadline before any request is allowed.
func (t *Transport) runBPSRecoveryPass(parent context.Context, host pluginv1.HostServiceClient, generation uint64, now time.Time) time.Duration {
	t.mu.RLock()
	cfg, base, closed := t.cfg.Clone(), t.client, t.closed
	s := &t.bpsRecovery
	s.mu.Lock()
	revision, valid := s.revision, s.generation == generation
	s.mu.Unlock()
	t.mu.RUnlock()
	if closed || !valid || parent.Err() != nil {
		return time.Minute
	}
	if !cfg.BPSAutoDisableOn403 || bpsRecoveryModel(cfg) == "" {
		return time.Hour
	}
	a := &t.bpsAccounts
	a.mu.RLock()
	epoch, ready := a.epoch, a.loaded && a.loadError == "" && a.host != nil
	records := make([]bpsAccountRecord, 0, len(a.records))
	for _, r := range a.records {
		if bpsRecordBlocked(r, cfg) {
			records = append(records, r)
		}
	}
	a.mu.RUnlock()
	if !ready {
		return time.Second
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].NextCheckAt.Equal(records[j].NextCheckAt) {
			return records[i].AccountID < records[j].AccountID
		}
		return records[i].NextCheckAt.Before(records[j].NextCheckAt)
	})
	delay := time.Hour
	nextRun := time.Time{}
	due := []bpsAccountRecord{}
	for _, record := range records {
		if record.NextCheckAt.IsZero() {
			record.BlockedAt, record.NextCheckAt, record.CheckStatus = now, now.Add(bpsRecoveryInterval), "waiting"
			if err := t.persistBPSRecoveryRecord(parent, host, generation, revision, epoch, record); err != nil {
				return time.Minute
			}
		}
		if nextRun.IsZero() || record.NextCheckAt.Before(nextRun) {
			nextRun = record.NextCheckAt
		}
		if !record.NextCheckAt.After(now) {
			due = append(due, record)
		}
	}
	s.mu.Lock()
	if s.generation == generation && s.revision == revision {
		s.nextRun = nextRun
	}
	s.mu.Unlock()
	if len(due) == 0 {
		if !nextRun.IsZero() {
			delay = min(delay, max(nextRun.Sub(now), time.Second))
		}
		return delay
	}
	listCtx, cancelList := context.WithTimeout(parent, 5*time.Second)
	directory, err := host.ListAccounts(listCtx, &pluginv1.ListAccountsRequest{Platform: "openai", AccountType: "oauth"})
	cancelList()
	if err != nil || directory == nil {
		return time.Minute
	}
	eligible := map[int64]bool{}
	for _, account := range directory.GetAccounts() {
		if account == nil || account.GetId() <= 0 {
			continue
		}
		id := account.GetId()
		available, exists := eligible[id]
		eligible[id] = account.GetSchedulable() && (!exists || available)
	}
	if len(directory.GetAccounts()) == 0 {
		for _, id := range directory.GetAccountIds() {
			if id > 0 {
				eligible[id] = true
			}
		}
	}
	for _, record := range due {
		if parent.Err() != nil {
			return time.Minute
		}
		useBPS, selectionErr := t.automaticBPSSelection(cfg, record.AccountID)
		if selectionErr != nil {
			return time.Minute
		}
		lease := record
		leaseStarted := time.Now()
		if now.After(leaseStarted) {
			leaseStarted = now
		}
		lease.NextCheckAt, lease.CheckStatus = leaseStarted.Add(bpsRecoveryInterval), "checking"
		allowed := eligible[record.AccountID] && useBPS && base != nil
		if !allowed {
			lease.CheckStatus = "skipped"
		}
		if err := t.persistBPSRecoveryRecord(parent, host, generation, revision, epoch, lease); err != nil {
			return time.Minute
		}
		if !allowed {
			continue
		}
		ctx, cancel := context.WithTimeout(parent, bpsRecoveryTimeout)
		s.mu.Lock()
		if s.generation != generation || s.revision != revision {
			s.mu.Unlock()
			cancel()
			return time.Minute
		}
		s.running, s.probeCancel, s.lastRun = true, cancel, time.Now()
		s.mu.Unlock()
		status := t.probeBPSRecovery(ctx, cfg, host, base, record.AccountID)
		finished := time.Now()
		canceled := ctx.Err() != nil
		cancel()
		s.mu.Lock()
		if s.generation == generation && s.revision == revision {
			s.running, s.probeCancel = false, nil
		}
		s.mu.Unlock()
		if canceled && parent.Err() != nil {
			return time.Minute
		}
		lease.CheckedAt, lease.CheckStatus = finished, status
		lease.NextCheckAt = finished.Add(bpsRecoveryInterval)
		if status == "recovered" && !canceled {
			lease.RecoveredBlockID, lease.RecoveredAt = record.BlockID, finished
		}
		if err := t.persistBPSRecoveryRecord(parent, host, generation, revision, epoch, lease); err != nil {
			return time.Minute
		}
	}
	return time.Second
}

// Storage commits are serialized with ordinary restriction writes. The exact
// block ID is checked again after I/O, so a concurrent new 403 always wins.
// A bounded config read lock prevents publishing recovery across a config save.
func (t *Transport) persistBPSRecoveryRecord(ctx context.Context, host pluginv1.HostServiceClient, generation, revision, epoch uint64, record bpsAccountRecord) error {
	a := &t.bpsAccounts
	a.mu.Lock()
	a.initLocked()
	stripe := a.writes[uint64(record.AccountID)%uint64(len(a.writes))]
	a.mu.Unlock()
	writeCtx, cancel := context.WithTimeout(ctx, bpsAccountWriteTimeout)
	defer cancel()
	select {
	case stripe <- struct{}{}:
	case <-writeCtx.Done():
		return writeCtx.Err()
	}
	defer func() { <-stripe }()
	t.mu.RLock()
	defer t.mu.RUnlock()
	s := &t.bpsRecovery
	s.mu.Lock()
	valid := s.generation == generation && s.revision == revision
	s.mu.Unlock()
	a.mu.RLock()
	current, exists := a.records[record.AccountID]
	valid = valid && a.epoch == epoch && a.loaded && a.loadError == "" && exists && current.BlockID == record.BlockID && bpsRecordBlocked(current, t.cfg)
	a.mu.RUnlock()
	if !valid || t.closed || !t.cfg.BPSAutoDisableOn403 || writeCtx.Err() != nil {
		return context.Canceled
	}
	raw, _ := json.Marshal(record)
	_, err := host.KVSet(writeCtx, &pluginv1.KVSetRequest{Namespace: bpsAccountNamespace, Key: bpsAccountKey(record.AccountID), Value: raw})
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.epoch != epoch || a.records[record.AccountID].BlockID != record.BlockID {
		return context.Canceled
	}
	if err != nil {
		a.writeError = bpsAccountWriteFailure
		return errBPSAccountStore
	}
	a.revision++
	a.records[record.AccountID], a.revisions[record.AccountID] = record, a.revision
	delete(a.dirty, record.AccountID)
	if len(a.dirty) == 0 {
		a.writeError = ""
	}
	return nil
}

// One real, minimal inference using the restricted account and its own proxy.
// Do not use the access endpoint: it can report allowed while inference is 403.
// Do not use doBasisPointsRequest: automatic checks must never replay requests.
func (t *Transport) probeBPSRecovery(ctx context.Context, cfg protocol.Config, host pluginv1.HostServiceClient, base *http.Client, id int64) string {
	model := bpsRecoveryModel(cfg)
	if model == "" || ctx.Err() != nil {
		return "skipped"
	}
	identity, err := host.ResolveOutboundIdentity(ctx, &pluginv1.ResolveOutboundIdentityRequest{AccountId: id})
	if err != nil || identity == nil || !identity.GetFound() || identity.GetToken() == "" ||
		(identity.GetAccountId() != 0 && identity.GetAccountId() != id) ||
		(identity.GetPlatform() != "" && identity.GetPlatform() != "openai") ||
		(identity.GetAccountType() != "" && identity.GetAccountType() != "oauth") {
		return "identity_unavailable"
	}
	headers := make(http.Header)
	applyOutboundIdentity(headers, identity, false)
	forwarded := make(map[string]*pluginv1.HeaderValues, len(headers))
	for key, values := range headers {
		forwarded[key] = &pluginv1.HeaderValues{Values: append([]string(nil), values...)}
	}
	requestHeaders, proxyURL, err := prepareHeaders(ctx, &pluginv1.ForwardRequestStart{AccountId: id, Headers: forwarded, ProxyUrl: identity.GetProxyUrl()}, nil, cfg.AuthMode)
	if err != nil {
		return "identity_unavailable"
	}
	client, err := t.clientForProxy(base, proxyURL)
	if err != nil {
		return "transport_error"
	}
	client = withoutBPSRedirects(client)
	client.Timeout = min(bpsRecoveryTimeout, time.Duration(cfg.TimeoutSeconds)*time.Second)
	body, err := protocol.PrepareResponsesBody(map[string]any{
		"model": model, "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Reply only OK."}}}},
		"stream": true, "store": false, "reasoning": map[string]any{"effort": "low"},
	}, cfg)
	if err != nil {
		return "invalid_request"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.ResponsesURL, bytes.NewReader(protocol.JSONBytes(body)))
	if err != nil {
		return "invalid_request"
	}
	req.Header = requestHeaders
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return "transport_error"
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return "forbidden"
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Sprintf("http_%d", resp.StatusCode)
	}
	// Reuse the bounded terminal reader but retain explicit 403 observations
	// from failed HTTP-200 JSON/SSE before the failure diagnostics are isolated.
	forbidden := false
	if err := prepareBasisPointsResponse(resp, cfg.MaxResponseBytes, func(status int) { forbidden = forbidden || status == http.StatusForbidden }); err != nil {
		return "invalid_response"
	}
	raw, contentType, err := readDegradationResponse(ctx, resp.Body, resp.Header.Get("Content-Type"), cfg.MaxResponseBytes)
	if forbidden {
		return "forbidden"
	}
	if err != nil || ctx.Err() != nil {
		return "invalid_response"
	}
	object, err := protocol.RawObject(raw)
	if err != nil || protocol.ClassifyResponseTerminal("", object) != protocol.TerminalCompleted {
		return "invalid_response"
	}
	if _, err := degradationAnswer(raw, contentType); err != nil {
		return "invalid_response"
	}
	return "recovered"
}
