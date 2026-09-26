package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"google.golang.org/grpc"
)

type bpsKVTestHost struct {
	pluginv1.HostServiceClient
	mu                sync.Mutex
	values            map[string][]byte
	failSet, failList bool
	sets, gets, lists int
	afterGet          func(context.Context, string)
	beforeSet         func(context.Context, string)
}

func newBPSKVTestHost() *bpsKVTestHost { return &bpsKVTestHost{values: map[string][]byte{}} }

func (h *bpsKVTestHost) KVGet(ctx context.Context, req *pluginv1.KVGetRequest, _ ...grpc.CallOption) (*pluginv1.KVGetResponse, error) {
	h.mu.Lock()
	value, found := h.values[req.Key]
	value = append([]byte(nil), value...)
	h.gets++
	hook := h.afterGet
	h.mu.Unlock()
	if hook != nil {
		hook(ctx, req.Key)
	}
	return &pluginv1.KVGetResponse{Found: found, Value: value}, nil
}

func (h *bpsKVTestHost) KVSet(ctx context.Context, req *pluginv1.KVSetRequest, _ ...grpc.CallOption) (*pluginv1.KVSetResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	hook := h.beforeSet
	h.mu.Unlock()
	if hook != nil {
		hook(ctx, req.Key)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Namespace != bpsAccountNamespace || req.TtlSeconds != 0 {
		return nil, errors.New("unexpected storage scope or TTL")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sets++
	if h.failSet {
		return nil, errors.New("private-storage-diagnostic")
	}
	h.values[req.Key] = append([]byte(nil), req.Value...)
	return &pluginv1.KVSetResponse{}, nil
}

func (h *bpsKVTestHost) KVList(_ context.Context, req *pluginv1.KVListRequest, _ ...grpc.CallOption) (*pluginv1.KVListResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lists++
	if h.failList {
		return nil, errors.New("private-list-diagnostic")
	}
	keys := []string{}
	for key := range h.values {
		if strings.HasPrefix(key, req.KeyPrefix) {
			keys = append(keys, key)
		}
	}
	// Exact prefix leaves sort last and may be absent from the first page.
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	if len(keys) > int(req.Limit) {
		keys = keys[:req.Limit]
	}
	return &pluginv1.KVListResponse{Keys: keys}, nil
}

func (h *bpsKVTestHost) seed(id int64, token string) {
	raw, _ := json.Marshal(bpsAccountRecord{AccountID: id, BlockID: token, Reason: bpsAccountFailureReason, HTTPStatus: 403})
	h.mu.Lock()
	h.values[bpsAccountKey(id)] = raw
	h.mu.Unlock()
}

func (h *bpsKVTestHost) record(id int64) bpsAccountRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var result bpsAccountRecord
	_ = json.Unmarshal(h.values[bpsAccountKey(id)], &result)
	return result
}

func newBPSAccountTransport(t *testing.T) *Transport {
	t.Helper()
	tr := New()
	t.Cleanup(func() { tr.bindBPSAccountStore(nil); tr.Shutdown() })
	return tr
}

func waitBPSStore(t *testing.T, tr *Transport) {
	t.Helper()
	tr.bpsAccounts.mu.RLock()
	ready := tr.bpsAccounts.ready
	tr.bpsAccounts.mu.RUnlock()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("account store did not load")
	}
}

func waitBPSCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBPSAccountRestrictionPersistsAcrossRestartRollbackAndReenable(t *testing.T) {
	host, tr := newBPSKVTestHost(), newBPSAccountTransport(t)
	tr.bindBPSAccountStore(host)
	waitBPSStore(t, tr)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tr.disableBPSAccount(canceled, 7)
	cfg := protocol.DefaultConfig()
	record := host.record(7)
	if len(record.BlockID) != 32 || record.Reason != bpsAccountFailureReason || !tr.isBPSAccountDisabled(7, cfg) {
		t.Fatalf("restriction missing: %#v", record)
	}
	enabled := cfg
	enabled.BPSReenabledAccounts = map[string]string{"7": record.BlockID}
	if tr.isBPSAccountDisabled(7, enabled) || !tr.isBPSAccountDisabled(7, cfg) {
		t.Fatal("acknowledgment or rollback changed durable state")
	}
	fresh := newBPSAccountTransport(t)
	fresh.bindBPSAccountStore(host)
	waitBPSStore(t, fresh)
	if !fresh.isBPSAccountDisabled(7, cfg) || fresh.isBPSAccountDisabled(7, enabled) {
		t.Fatal("restart changed acknowledgment semantics")
	}
	fresh.disableBPSAccount(context.Background(), 7)
	if host.record(7).BlockID == record.BlockID || !fresh.isBPSAccountDisabled(7, enabled) {
		t.Fatal("new 403 did not invalidate the old acknowledgment")
	}
}

func TestBPSAccountRestrictionWithoutHostIsImmediateAndLaterPersisted(t *testing.T) {
	tr := newBPSAccountTransport(t)
	cfg := protocol.DefaultConfig()
	tr.disableBPSAccount(context.Background(), 9)
	if !tr.isBPSAccountDisabled(9, cfg) || tr.bpsAccountStoreError() != nil {
		t.Fatal("legacy host memory restriction failed")
	}
	status := tr.bpsAccountStatusJSON("{}", cfg)
	if !strings.Contains(status, "memory only") {
		t.Fatal("missing persistence limitation")
	}
	host := newBPSKVTestHost()
	tr.bindBPSAccountStore(host)
	waitBPSStore(t, tr)
	waitBPSCondition(t, func() bool { return host.record(9).BlockID != "" })
	if tr.bpsAccountStoreError() != nil {
		t.Fatal("loaded store remained unavailable")
	}
}

func TestBPSAccountStaleAcknowledgementCannotReleaseConcurrentRestriction(t *testing.T) {
	host, tr := newBPSKVTestHost(), newBPSAccountTransport(t)
	tr.bindBPSAccountStore(host)
	waitBPSStore(t, tr)
	tr.disableBPSAccount(context.Background(), 7)
	previous := host.record(7)
	applyConfig(t, tr, map[string]any{"bps_reenabled_accounts": map[string]string{"7": previous.BlockID}})

	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	host.mu.Lock()
	host.beforeSet = func(ctx context.Context, key string) {
		if key != bpsAccountKey(7) {
			return
		}
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	host.mu.Unlock()
	go func() { defer close(finished); tr.disableBPSAccount(context.Background(), 7) }()
	defer func() { releaseOnce.Do(func() { close(release) }); <-finished }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("new restriction did not enter storage")
	}

	// A UI save based on the old status snapshot can arrive while a new 403
	// is already marked in memory but not yet committed to host storage.
	applyConfig(t, tr, map[string]any{"bps_reenabled_accounts": map[string]string{"7": previous.BlockID}})
	tr.mu.RLock()
	staleConfig := tr.cfg.Clone()
	tr.mu.RUnlock()
	tr.bpsAccounts.mu.RLock()
	latest, pending := tr.bpsAccounts.records[7], tr.bpsAccounts.dirty[7]
	tr.bpsAccounts.mu.RUnlock()
	if !pending || latest.BlockID == previous.BlockID || !tr.isBPSAccountDisabled(7, staleConfig) || tr.isBPSAccountDisabled(8, staleConfig) {
		t.Fatal("stale acknowledgement released the new restriction or affected another account")
	}
	releaseOnce.Do(func() { close(release) })
	<-finished
	if host.record(7).BlockID != latest.BlockID {
		t.Fatal("acknowledgement replaced the newer durable restriction")
	}

	fresh := newBPSAccountTransport(t)
	applyConfig(t, fresh, map[string]any{"bps_reenabled_accounts": staleConfig.BPSReenabledAccounts})
	fresh.bindBPSAccountStore(host)
	waitBPSStore(t, fresh)
	if !fresh.isBPSAccountDisabled(7, staleConfig) {
		t.Fatal("restart accepted stale acknowledgement for the latest restriction")
	}
	applyConfig(t, fresh, map[string]any{"bps_reenabled_accounts": map[string]string{"7": latest.BlockID}})
	fresh.mu.RLock()
	currentConfig := fresh.cfg.Clone()
	fresh.mu.RUnlock()
	if fresh.isBPSAccountDisabled(7, currentConfig) || host.record(7).BlockID != latest.BlockID {
		t.Fatal("exact acknowledgement did not restore BPS without deleting its durable marker")
	}
}

func TestBPSAccountFailedWriteRemainsBlockedAndRetriesWithoutReload(t *testing.T) {
	host, tr := newBPSKVTestHost(), newBPSAccountTransport(t)
	host.failSet = true
	tr.bindBPSAccountStore(host)
	waitBPSStore(t, tr)
	tr.disableBPSAccount(context.Background(), 11)
	cfg := protocol.DefaultConfig()
	status := tr.bpsAccountStatusJSON("{}", cfg)
	if !tr.isBPSAccountDisabled(11, cfg) || !strings.Contains(status, "could not be persisted") || strings.Contains(status, "private-storage") {
		t.Fatalf("failure handling incorrect: %s", status)
	}
	host.mu.Lock()
	host.failSet = false
	initialLists := host.lists
	host.mu.Unlock()
	tr.bpsAccounts.mu.RLock()
	wake := tr.bpsAccounts.wake
	tr.bpsAccounts.mu.RUnlock()
	select {
	case wake <- struct{}{}:
	default:
	}
	waitBPSCondition(t, func() bool { return host.record(11).BlockID != "" })
	waitBPSCondition(t, func() bool { return !strings.Contains(tr.bpsAccountStatusJSON("{}", cfg), "persistence_error") })
	for range 100 {
		_ = tr.isBPSAccountDisabled(11, cfg)
		_ = tr.bpsAccountStatusJSON("{}", cfg)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.lists != initialLists {
		t.Fatal("retry/status performed a full store reload")
	}
}

func TestBPSAccountConcurrentMarksPersistCurrentToken(t *testing.T) {
	host, tr := newBPSKVTestHost(), newBPSAccountTransport(t)
	tr.bindBPSAccountStore(host)
	waitBPSStore(t, tr)
	var workers sync.WaitGroup
	for range 40 {
		workers.Add(1)
		go func() { defer workers.Done(); tr.disableBPSAccount(context.Background(), 7) }()
	}
	workers.Wait()
	tr.bpsAccounts.mu.RLock()
	record := tr.bpsAccounts.records[7]
	dirty := tr.bpsAccounts.dirty[7]
	tr.bpsAccounts.mu.RUnlock()
	if dirty || host.record(7).BlockID != record.BlockID {
		t.Fatal("concurrent stale write replaced latest restriction")
	}
}

func TestBPSAccountSlowLoadCannotOverwriteNewMark(t *testing.T) {
	host, tr := newBPSKVTestHost(), newBPSAccountTransport(t)
	old := strings.Repeat("a", 32)
	host.seed(7, old)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	host.afterGet = func(ctx context.Context, _ string) {
		once.Do(func() {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
	}
	tr.bindBPSAccountStore(host)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("load did not start")
	}
	tr.disableBPSAccount(context.Background(), 7)
	current := host.record(7)
	close(release)
	waitBPSStore(t, tr)
	tr.bpsAccounts.mu.RLock()
	loaded := tr.bpsAccounts.records[7]
	tr.bpsAccounts.mu.RUnlock()
	if loaded.BlockID != current.BlockID || loaded.BlockID == old {
		t.Fatal("slow load overwrote new restriction")
	}
}

func TestBPSAccountWriteDeadlineIncludesStripeWait(t *testing.T) {
	for _, mode := range []string{"canceled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			host, tr := newBPSKVTestHost(), newBPSAccountTransport(t)
			entered, release := make(chan struct{}), make(chan struct{})
			firstDone, secondDone := make(chan struct{}), make(chan struct{})
			var enterOnce, releaseOnce sync.Once
			host.beforeSet = func(ctx context.Context, key string) {
				if key == bpsAccountKey(7) {
					enterOnce.Do(func() { close(entered) })
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
			}
			tr.bindBPSAccountStore(host)
			waitBPSStore(t, tr)
			go func() { defer close(firstDone); tr.disableBPSAccount(context.Background(), 7) }()
			defer func() { releaseOnce.Do(func() { close(release) }); <-firstDone }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("first write did not enter storage")
			}
			// IDs 7 and 39 share one of the 32 serialization stripes.
			tr.bpsAccounts.mu.Lock()
			tr.bpsAccounts.records[39] = bpsAccountRecord{AccountID: 39, BlockID: strings.Repeat("b", 32), Reason: bpsAccountFailureReason, HTTPStatus: 403}
			tr.bpsAccounts.dirty[39] = true
			epoch := tr.bpsAccounts.epoch
			tr.bpsAccounts.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			go func() { defer close(secondDone); tr.persistBPSAccount(ctx, 39, epoch) }()
			if mode == "canceled" {
				cancel()
			}
			<-ctx.Done()
			select {
			case <-secondDone:
			case <-time.After(250 * time.Millisecond):
				t.Fatal("expired persistence context remained blocked behind another stripe write")
			}
			tr.bpsAccounts.mu.RLock()
			dirty := tr.bpsAccounts.dirty[39]
			tr.bpsAccounts.mu.RUnlock()
			if !dirty || host.record(39).BlockID != "" {
				t.Fatal("canceled queued write lost its pending restriction")
			}
			releaseOnce.Do(func() { close(release) })
			<-firstDone
			tr.flushBPSAccountWrites(context.Background(), epoch)
			if host.record(39).BlockID == "" {
				t.Fatal("queued restriction was not retried")
			}
		})
	}
}

func TestBPSAccountRebindDiscardsOldLoad(t *testing.T) {
	oldHost, newHost, tr := newBPSKVTestHost(), newBPSKVTestHost(), newBPSAccountTransport(t)
	oldHost.seed(7, strings.Repeat("a", 32))
	newHost.seed(8, strings.Repeat("b", 32))
	entered := make(chan struct{})
	var once sync.Once
	oldHost.afterGet = func(ctx context.Context, _ string) { once.Do(func() { close(entered); <-ctx.Done() }) }
	tr.bindBPSAccountStore(oldHost)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("old load did not start")
	}
	tr.bpsAccounts.mu.RLock()
	oldReady := tr.bpsAccounts.ready
	tr.bpsAccounts.mu.RUnlock()
	tr.bindBPSAccountStore(newHost)
	waitBPSStore(t, tr)
	select {
	case <-oldReady:
	case <-time.After(time.Second):
		t.Fatal("old worker did not stop")
	}
	cfg := protocol.DefaultConfig()
	if tr.isBPSAccountDisabled(7, cfg) || !tr.isBPSAccountDisabled(8, cfg) {
		t.Fatal("old binding polluted new state")
	}
}

func TestBPSAccountLoadCoversTruncatedListAndExactPrefixLeaf(t *testing.T) {
	host := newBPSKVTestHost()
	host.seed(1, fmt.Sprintf("%032x", 1))
	for id := int64(10000); id < 11250; id++ {
		host.seed(id, fmt.Sprintf("%032x", id))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	records, err := loadBPSAccountRecords(ctx, host)
	if err != nil || len(records) != 1251 || records[1].AccountID != 1 {
		t.Fatalf("truncated list lost restrictions: count=%d error=%v", len(records), err)
	}
}

func TestBPSAccountRebindKeepsLatestMarkDuringInFlightWrite(t *testing.T) {
	host, tr := newBPSKVTestHost(), newBPSAccountTransport(t)
	host.seed(7, strings.Repeat("a", 32))
	entered, release := make(chan struct{}), make(chan struct{})
	var hookMu sync.Mutex
	var releaseOnce sync.Once
	calls := 0
	host.beforeSet = func(ctx context.Context, _ string) {
		hookMu.Lock()
		calls++
		first := calls == 1
		hookMu.Unlock()
		if first {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	}
	tr.bindBPSAccountStore(host)
	waitBPSStore(t, tr)
	firstDone := make(chan struct{})
	go func() { defer close(firstDone); tr.disableBPSAccount(context.Background(), 7) }()
	defer func() { releaseOnce.Do(func() { close(release) }); <-firstDone }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first write did not enter storage")
	}
	tr.bpsAccounts.mu.RLock()
	firstBlock := tr.bpsAccounts.records[7].BlockID
	oldStripe := tr.bpsAccounts.writes[7]
	tr.bpsAccounts.mu.RUnlock()
	tr.bindBPSAccountStore(host)
	waitBPSStore(t, tr)
	tr.bpsAccounts.mu.RLock()
	loaded := tr.bpsAccounts.records[7].BlockID
	newStripe := tr.bpsAccounts.writes[7]
	tr.bpsAccounts.mu.RUnlock()
	if loaded != firstBlock || oldStripe != newStripe {
		t.Fatal("rebind discarded a pending block or its cross-binding serialization")
	}
	secondDone := make(chan struct{})
	go func() { defer close(secondDone); tr.disableBPSAccount(context.Background(), 7) }()
	waitBPSCondition(t, func() bool {
		tr.bpsAccounts.mu.RLock()
		defer tr.bpsAccounts.mu.RUnlock()
		return tr.bpsAccounts.records[7].BlockID != firstBlock
	})
	releaseOnce.Do(func() { close(release) })
	<-firstDone
	select {
	case <-secondDone:
	case <-time.After(3 * time.Second):
		t.Fatal("new binding did not persist its latest block")
	}
	tr.bpsAccounts.mu.RLock()
	latest, dirty := tr.bpsAccounts.records[7], tr.bpsAccounts.dirty[7]
	tr.bpsAccounts.mu.RUnlock()
	if dirty || latest.BlockID == firstBlock || host.record(7).BlockID != latest.BlockID {
		t.Fatal("old write completion or old load snapshot replaced the newest restriction")
	}
}

func TestBPSAccountExpiredWriteNeverCallsStorage(t *testing.T) {
	host, tr := newBPSKVTestHost(), newBPSAccountTransport(t)
	tr.bindBPSAccountStore(host)
	waitBPSStore(t, tr)
	tr.bpsAccounts.mu.Lock()
	tr.bpsAccounts.records[7] = bpsAccountRecord{AccountID: 7, BlockID: strings.Repeat("b", 32), Reason: bpsAccountFailureReason, HTTPStatus: 403}
	tr.bpsAccounts.dirty[7] = true
	epoch := tr.bpsAccounts.epoch
	tr.bpsAccounts.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 32 {
		tr.persistBPSAccount(ctx, 7, epoch)
	}
	host.mu.Lock()
	sets := host.sets
	host.mu.Unlock()
	if sets != 0 {
		t.Fatal("an expired context acquired a free stripe and called storage")
	}
	if !strings.Contains(tr.bpsAccountStatusJSON("{}", protocol.DefaultConfig()), "could not be persisted") {
		t.Fatal("pending write timeout was not reported")
	}
}

func TestBPSAccountLoadFailureAndMalformedValueFailClosed(t *testing.T) {
	for _, mode := range []string{"list failure", "invalid value"} {
		t.Run(mode, func(t *testing.T) {
			host, tr := newBPSKVTestHost(), newBPSAccountTransport(t)
			if mode == "list failure" {
				host.failList = true
			} else {
				host.values[bpsAccountKey(7)] = []byte("invalid-private-record")
			}
			tr.bindBPSAccountStore(host)
			waitBPSStore(t, tr)
			if tr.bpsAccountStoreError() == nil {
				t.Fatal("unloaded restrictions allowed routing")
			}
			status := tr.bpsAccountStatusJSON("{}", protocol.DefaultConfig())
			if !strings.Contains(status, "persistence_error") || strings.Contains(status, "private") {
				t.Fatalf("unsafe status: %s", status)
			}
		})
	}
}
