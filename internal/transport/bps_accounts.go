package transport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

const (
	bpsAccountNamespace     = "bps_account_block_v1"
	bpsAccountKeyPrefix     = "account-"
	bpsAccountListLimit     = 1000
	bpsAccountMaxRecords    = 100000
	bpsAccountWriteTimeout  = 2 * time.Second
	bpsAccountLoadTimeout   = 15 * time.Second
	bpsAccountFailureReason = "http_403"
	bpsAccountWriteFailure  = "Basis Points account restriction could not be persisted to host storage; it remains blocked in memory"
)

var errBPSAccountStore = errors.New("Basis Points account restrictions are not ready in host storage")

type bpsAccountRecord struct {
	AccountID        int64     `json:"account_id"`
	BlockID          string    `json:"block_id"`
	Reason           string    `json:"reason"`
	HTTPStatus       int       `json:"http_status"`
	BlockedAt        time.Time `json:"blocked_at,omitempty"`
	CheckedAt        time.Time `json:"last_check_at,omitempty"`
	NextCheckAt      time.Time `json:"next_check_at,omitempty"`
	RecoveredBlockID string    `json:"recovered_block_id,omitempty"`
	RecoveredAt      time.Time `json:"recovered_at,omitempty"`
	CheckStatus      string    `json:"check_status,omitempty"`
}

// This state belongs to the transport rather than Config. ApplyConfig never
// clears a durable marker: a persisted config acknowledges one exact block ID.
type bpsAccountState struct {
	mu         sync.RWMutex
	writes     [32]chan struct{}
	host       pluginv1.HostServiceClient
	epoch      uint64
	revision   uint64
	records    map[int64]bpsAccountRecord
	revisions  map[int64]uint64
	dirty      map[int64]bool
	loaded     bool
	loadError  string
	writeError string
	cancel     context.CancelFunc
	ready      chan struct{}
	wake       chan struct{}
}

func (s *bpsAccountState) initLocked() {
	for index := range s.writes {
		if s.writes[index] == nil {
			// Keep the same stripe across bindings so an old in-flight write
			// cannot complete after a newer write to the same storage key.
			s.writes[index] = make(chan struct{}, 1)
		}
	}
	if s.records == nil {
		s.records = make(map[int64]bpsAccountRecord)
	}
	if s.revisions == nil {
		s.revisions = make(map[int64]uint64)
	}
	if s.dirty == nil {
		s.dirty = make(map[int64]bool)
	}
}

// Bind is nonblocking and performs no RPC while holding either transport lock.
// Pass nil on shutdown; each old worker is canceled and its results fenced out.
func (t *Transport) bindBPSAccountStore(host pluginv1.HostServiceClient) {
	s := &t.bpsAccounts
	s.mu.Lock()
	s.initLocked()
	if s.cancel != nil {
		s.cancel()
	}
	s.epoch++
	epoch := s.epoch
	s.host, s.loaded, s.loadError = host, host == nil, ""
	s.ready = make(chan struct{})
	s.wake = make(chan struct{}, 1)
	ready, wake := s.ready, s.wake
	if host == nil {
		s.cancel = nil
		close(ready)
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.mu.Unlock()
	go t.runBPSAccountStore(ctx, host, epoch, ready, wake)
}

func (t *Transport) runBPSAccountStore(ctx context.Context, host pluginv1.HostServiceClient, epoch uint64, ready chan struct{}, wake <-chan struct{}) {
	defer func() {
		if ready != nil {
			close(ready)
		}
	}()
	needLoad := true
	retryDelay := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		s := &t.bpsAccounts
		if needLoad {
			s.mu.RLock()
			revision := s.revision
			pending := make(map[int64]bool, len(s.dirty))
			for id := range s.dirty {
				pending[id] = true
			}
			s.mu.RUnlock()
			loadCtx, cancel := context.WithTimeout(ctx, bpsAccountLoadTimeout)
			records, err := loadBPSAccountRecords(loadCtx, host)
			cancel()
			s.mu.Lock()
			if s.epoch != epoch {
				s.mu.Unlock()
				return
			}
			if err != nil {
				s.loadError = "Basis Points account restrictions could not be loaded from host storage"
			} else {
				for id, record := range records {
					// A mark made during a slow load, or pending before it, wins
					// over that older snapshot even if its write already finished.
					if s.revisions[id] <= revision && !pending[id] && !s.dirty[id] {
						s.records[id] = record
					}
				}
				s.loaded, s.loadError = true, ""
				needLoad = false
			}
			s.mu.Unlock()
			if ready != nil {
				close(ready)
				ready = nil
			}
		}
		t.flushBPSAccountWrites(ctx, epoch)
		s.mu.RLock()
		dirty := len(s.dirty) > 0
		s.mu.RUnlock()
		if !needLoad && !dirty {
			retryDelay = time.Second
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			continue
		}
		timer := time.NewTimer(retryDelay)
		retryDelay = min(retryDelay*2, 30*time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (t *Transport) disableBPSAccount(ctx context.Context, id int64) {
	if id <= 0 {
		return
	}
	// Every explicit BPS 403 reaches this shared write boundary, including
	// attachment uploads, tool correction and on-demand account checks. Use
	// the current policy so disabling it also covers requests already in flight.
	// Hold the config read lock through the in-memory mark to order it with
	// ApplyConfig; persistence can finish independently after that point.
	t.mu.RLock()
	if !t.cfg.BPSAutoDisableOn403 {
		t.mu.RUnlock()
		return
	}
	var random [16]byte
	_, _ = rand.Read(random[:])
	now := time.Now()
	record := bpsAccountRecord{AccountID: id, BlockID: hex.EncodeToString(random[:]), Reason: bpsAccountFailureReason, HTTPStatus: 403, BlockedAt: now, NextCheckAt: now.Add(bpsRecoveryInterval), CheckStatus: "waiting"}
	s := &t.bpsAccounts
	s.mu.Lock()
	s.initLocked()
	s.revision++
	s.records[id], s.revisions[id], s.dirty[id] = record, s.revision, true
	epoch := s.epoch
	s.mu.Unlock()
	t.mu.RUnlock()
	t.wakeBPSRecovery()
	if ctx == nil {
		ctx = context.Background()
	}
	// Receiving 403 is definitive even when the caller immediately cancels.
	// The write remains bounded and cannot delay a normal successful request.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bpsAccountWriteTimeout)
	defer cancel()
	t.persistBPSAccount(writeCtx, id, epoch)
	s.mu.RLock()
	wake, dirty := s.wake, s.dirty[id]
	s.mu.RUnlock()
	if dirty && wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (t *Transport) persistBPSAccount(ctx context.Context, id int64, epoch uint64) {
	s := &t.bpsAccounts
	s.mu.Lock()
	s.initLocked()
	stripe := s.writes[uint64(id)%uint64(len(s.writes))]
	current := s.epoch
	s.mu.Unlock()
	if current != epoch {
		return
	}
	acquired := false
	select {
	case stripe <- struct{}{}:
		acquired = true
	case <-ctx.Done():
	}
	// A ready stripe and canceled context can both win a select. In either
	// case, an expired caller must leave the record pending without an RPC.
	if !acquired || ctx.Err() != nil {
		if acquired {
			<-stripe
		}
		s.mu.Lock()
		if s.epoch == epoch && s.dirty[id] {
			s.writeError = bpsAccountWriteFailure
		}
		s.mu.Unlock()
		return
	}
	defer func() { <-stripe }()
	s.mu.RLock()
	record, exists := s.records[id]
	host, current, dirty, revision := s.host, s.epoch, s.dirty[id], s.revisions[id]
	s.mu.RUnlock()
	if current != epoch || !exists || !dirty {
		return
	}
	if host == nil {
		s.mu.Lock()
		if s.epoch == epoch {
			s.writeError = "Host storage is unavailable; Basis Points account restrictions are retained in memory only"
		}
		s.mu.Unlock()
		return
	}
	raw, _ := json.Marshal(record)
	_, err := host.KVSet(ctx, &pluginv1.KVSetRequest{Namespace: bpsAccountNamespace, Key: bpsAccountKey(id), Value: raw, TtlSeconds: 0})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.epoch != epoch {
		return
	}
	if err != nil {
		s.writeError = bpsAccountWriteFailure
		return
	}
	if s.records[id].BlockID == record.BlockID && s.revisions[id] == revision {
		delete(s.dirty, id)
	}
	if len(s.dirty) == 0 {
		s.writeError = ""
	}
}

func (t *Transport) flushBPSAccountWrites(ctx context.Context, epoch uint64) {
	s := &t.bpsAccounts
	s.mu.RLock()
	ids := make([]int64, 0, len(s.dirty))
	for id := range s.dirty {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		writeCtx, cancel := context.WithTimeout(ctx, bpsAccountWriteTimeout)
		t.persistBPSAccount(writeCtx, id, epoch)
		cancel()
	}
}

func (t *Transport) isBPSAccountDisabled(id int64, cfg protocol.Config) bool {
	t.bpsAccounts.mu.RLock()
	record, exists := t.bpsAccounts.records[id]
	t.bpsAccounts.mu.RUnlock()
	return exists && bpsRecordBlocked(record, cfg)
}

func bpsRecordBlocked(record bpsAccountRecord, cfg protocol.Config) bool {
	return record.BlockID != record.RecoveredBlockID && record.BlockID != cfg.BPSReenabledAccounts[strconv.FormatInt(record.AccountID, 10)]
}

// The caller may reject or bypass BPS while a connected store has not loaded.
// Health remains healthy, and legacy hosts without HostService keep working.
func (t *Transport) bpsAccountStoreError() error {
	t.bpsAccounts.mu.RLock()
	defer t.bpsAccounts.mu.RUnlock()
	if t.bpsAccounts.host != nil && (!t.bpsAccounts.loaded || t.bpsAccounts.loadError != "") {
		return errBPSAccountStore
	}
	return nil
}

func (t *Transport) bpsAccountStatusJSON(baseJSON string, cfg protocol.Config) string {
	base := map[string]json.RawMessage{}
	if json.Unmarshal([]byte(baseJSON), &base) != nil || base == nil {
		base = map[string]json.RawMessage{}
	}
	s := &t.bpsAccounts
	s.mu.RLock()
	records := make([]bpsAccountRecord, 0, len(s.records))
	for _, record := range s.records {
		if bpsRecordBlocked(record, cfg) {
			records = append(records, record)
		}
	}
	persistenceError := s.writeError
	if s.loadError != "" {
		persistenceError = s.loadError
	} else if s.host != nil && !s.loaded {
		persistenceError = "Basis Points account restrictions are loading from host storage"
	}
	if s.host == nil && len(s.records) > 0 {
		persistenceError = "Host storage is unavailable; Basis Points account restrictions are retained in memory only"
	}
	s.mu.RUnlock()
	sort.Slice(records, func(i, j int) bool { return records[i].AccountID < records[j].AccountID })
	ids := make([]int64, 0, len(records))
	summaries := make([]map[string]any, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.AccountID)
		summaries = append(summaries, map[string]any{"account_id": record.AccountID, "block_id": record.BlockID, "reason": record.Reason, "http_status": record.HTTPStatus, "blocked_at": autoStatusTime(record.BlockedAt), "last_check_at": autoStatusTime(record.CheckedAt), "next_check_at": autoStatusTime(record.NextCheckAt), "check_status": record.CheckStatus})
	}
	base["bps_disabled_account_ids"], _ = json.Marshal(ids)
	base["bps_disabled_accounts"], _ = json.Marshal(summaries)
	base["bps_recovery"], _ = json.Marshal(t.bpsRecoveryStatus())
	if persistenceError != "" {
		base["bps_account_persistence_error"], _ = json.Marshal(persistenceError)
	} else {
		delete(base, "bps_account_persistence_error")
	}
	raw, _ := json.Marshal(base)
	return string(raw)
}

func bpsAccountKey(id int64) string { return bpsAccountKeyPrefix + strconv.FormatInt(id, 10) }

func bpsAccountKeyID(key string) (int64, bool) {
	if !strings.HasPrefix(key, bpsAccountKeyPrefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(key, bpsAccountKeyPrefix), 10, 64)
	return id, err == nil && id > 0 && key == bpsAccountKey(id)
}

func listBPSAccountKeys(ctx context.Context, host pluginv1.HostServiceClient) ([]string, error) {
	keys := make(map[string]bool)
	var visit func(string) error
	visit = func(prefix string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := host.KVList(ctx, &pluginv1.KVListRequest{Namespace: bpsAccountNamespace, KeyPrefix: prefix, Limit: bpsAccountListLimit})
		if err != nil || result == nil {
			return errBPSAccountStore
		}
		for _, key := range result.Keys {
			if _, valid := bpsAccountKeyID(key); !valid || !strings.HasPrefix(key, prefix) {
				return errBPSAccountStore
			}
		}
		if len(result.Keys) >= bpsAccountListLimit {
			// A full response is possibly truncated. Decimal prefix splitting
			// is complete even without a cursor, including the exact leaf.
			if len(prefix) >= len(bpsAccountKeyPrefix)+19 {
				return errBPSAccountStore
			}
			// SCAN order is arbitrary: the exact key may be omitted from the
			// truncated page. Probe it separately; missing KVGet is harmless.
			if _, valid := bpsAccountKeyID(prefix); valid {
				keys[prefix] = true
			}
			for digit := byte(48); digit <= byte(57); digit++ {
				if err := visit(prefix + string(digit)); err != nil {
					return err
				}
			}
		} else {
			for _, key := range result.Keys {
				keys[key] = true
			}
		}
		if len(keys) > bpsAccountMaxRecords {
			return errBPSAccountStore
		}
		return nil
	}
	if err := visit(bpsAccountKeyPrefix); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}

func loadBPSAccountRecords(ctx context.Context, host pluginv1.HostServiceClient) (map[int64]bpsAccountRecord, error) {
	keys, err := listBPSAccountKeys(ctx, host)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	records := make(map[int64]bpsAccountRecord, len(keys))
	jobs := make(chan string)
	var mu sync.Mutex
	var workers sync.WaitGroup
	var firstError error
	for range min(8, len(keys)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for key := range jobs {
				id, _ := bpsAccountKeyID(key)
				response, readError := host.KVGet(ctx, &pluginv1.KVGetRequest{Namespace: bpsAccountNamespace, Key: key})
				var record bpsAccountRecord
				if readError == nil && response == nil {
					readError = errBPSAccountStore
				}
				if readError == nil && response.Found {
					if json.Unmarshal(response.Value, &record) != nil || record.AccountID != id || record.Reason != bpsAccountFailureReason || record.HTTPStatus != 403 || len(record.BlockID) != 32 {
						readError = errBPSAccountStore
					} else if _, err := hex.DecodeString(record.BlockID); err != nil {
						readError = errBPSAccountStore
					} else if record.RecoveredBlockID != "" && (record.RecoveredBlockID != record.BlockID || record.RecoveredAt.IsZero()) {
						readError = errBPSAccountStore
					}
				}
				mu.Lock()
				if readError != nil {
					if firstError == nil {
						firstError = errBPSAccountStore
						cancel()
					}
				} else if response.Found {
					records[id] = record
				}
				mu.Unlock()
			}
		}()
	}
	for _, key := range keys {
		select {
		case jobs <- key:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	workers.Wait()
	if firstError != nil {
		return nil, firstError
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return records, nil
}
