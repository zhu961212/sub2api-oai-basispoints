package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"google.golang.org/grpc"
)

type bpsRecoveryTestHost struct {
	*bpsKVTestHost
	identity *pluginv1.ResolveOutboundIdentityResponse
	accounts []*pluginv1.AccountInfo
	lookups  atomic.Int32
}

func (h *bpsRecoveryTestHost) ListAccounts(context.Context, *pluginv1.ListAccountsRequest, ...grpc.CallOption) (*pluginv1.ListAccountsResponse, error) {
	return &pluginv1.ListAccountsResponse{Accounts: h.accounts}, nil
}
func (h *bpsRecoveryTestHost) ResolveOutboundIdentity(_ context.Context, req *pluginv1.ResolveOutboundIdentityRequest, _ ...grpc.CallOption) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	h.lookups.Add(1)
	if req.AccountId != 7 {
		return nil, errors.New("unexpected account")
	}
	return h.identity, nil
}

func newBPSRecoveryTest(t *testing.T, handler http.HandlerFunc) (*Transport, *bpsRecoveryTestHost, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	host := &bpsRecoveryTestHost{bpsKVTestHost: newBPSKVTestHost(), identity: &pluginv1.ResolveOutboundIdentityResponse{Found: true, AccountId: 7, Platform: "openai", AccountType: "oauth", Token: token(t, "recovery-test-account")}, accounts: []*pluginv1.AccountInfo{{Id: 7, Schedulable: true}}}
	tr := newBPSAccountTransport(t)
	tr.cfg.ResponsesURL = server.URL + "/backend-api/basispoints/responses"
	tr.cfg.EnabledModels = []string{"gpt-5.6-sol"}
	tr.host, tr.client = host, server.Client()
	tr.bindBPSAccountStore(host)
	waitBPSStore(t, tr)
	tr.disableBPSAccount(context.Background(), 7)
	return tr, host, server
}

func dueBPSRecovery(tr *Transport, host *bpsRecoveryTestHost, now time.Time) bpsAccountRecord {
	tr.bpsAccounts.mu.Lock()
	record := tr.bpsAccounts.records[7]
	record.NextCheckAt = now.Add(-time.Second)
	tr.bpsAccounts.records[7] = record
	tr.bpsAccounts.mu.Unlock()
	raw, _ := json.Marshal(record)
	host.mu.Lock()
	host.values[bpsAccountKey(7)] = raw
	host.mu.Unlock()
	return record
}

func runBPSRecoveryTestPass(tr *Transport, host *bpsRecoveryTestHost, now time.Time) {
	tr.bpsRecovery.mu.Lock()
	generation := tr.bpsRecovery.generation
	tr.bpsRecovery.mu.Unlock()
	tr.runBPSRecoveryPass(context.Background(), host, generation, now)
}

func TestBPSRecoveryPersistsLeaseBeforeRealInferenceAndRestoresAcrossRestart(t *testing.T) {
	var tr *Transport
	var host *bpsRecoveryTestHost
	var requests atomic.Int32
	now := time.Now()
	tr, host, _ = newBPSRecoveryTest(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		lease := host.record(7)
		if lease.CheckStatus != "checking" || lease.NextCheckAt.Before(now.Add(bpsRecoveryInterval)) || lease.NextCheckAt.After(time.Now().Add(bpsRecoveryInterval)) {
			t.Error("inference started before durable six-hour lease")
		}
		if r.URL.Path != "/backend-api/basispoints/responses" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+host.identity.Token || r.Header.Get("ChatGPT-Account-ID") != "recovery-test-account" {
			t.Error("probe did not use selected account identity/inference endpoint")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != "gpt-5.6-sol" || body["stream"] != true {
			t.Error("probe did not use configured model")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"test","status":"completed","output_text":"OK"}`)
	})
	original := host.record(7)
	if original.NextCheckAt.Sub(original.BlockedAt) != bpsRecoveryInterval {
		t.Fatal("new 403 did not schedule six-hour recovery")
	}
	runBPSRecoveryTestPass(tr, host, now)
	if requests.Load() != 0 {
		t.Fatal("probe fired before scheduled deadline")
	}
	original = dueBPSRecovery(tr, host, now)
	runBPSRecoveryTestPass(tr, host, now)
	if requests.Load() != 1 || tr.isBPSAccountDisabled(7, tr.cfg) {
		t.Fatal("successful inference did not recover the blocked account")
	}
	recovered := host.record(7)
	if recovered.RecoveredBlockID != original.BlockID || recovered.RecoveredAt.IsZero() || recovered.CheckedAt.IsZero() {
		t.Fatal("recovery was not durably tied to exact block")
	}
	fresh := newBPSAccountTransport(t)
	fresh.bindBPSAccountStore(host)
	waitBPSStore(t, fresh)
	if fresh.isBPSAccountDisabled(7, fresh.cfg) {
		t.Fatal("restart lost successful recovery")
	}
	tr.disableBPSAccount(context.Background(), 7)
	if !tr.isBPSAccountDisabled(7, tr.cfg) || host.record(7).BlockID == original.BlockID {
		t.Fatal("later 403 was cleared by earlier recovery")
	}
}

func TestBPSRecoveryRejectsUnprovenResponsesWithoutRetries(t *testing.T) {
	for _, test := range []struct {
		name                    string
		status                  int
		body, contentType, want string
	}{
		{"forbidden", 403, "", "application/json", "forbidden"},
		{"unauthorized", 401, "", "application/json", "http_401"},
		{"rate limit", 429, "", "application/json", "http_429"},
		{"server error", 503, "", "application/json", "http_503"},
		{"redirect", 307, "", "application/json", "http_307"},
		{"malformed", 200, "not-json", "application/json", "invalid_response"},
		{"unfinished", 200, `{"status":"in_progress","output_text":"OK"}`, "application/json", "invalid_response"},
		{"missing completion", 200, `{"output_text":"OK"}`, "application/json", "invalid_response"},
		{"incomplete", 200, `{"status":"incomplete","output_text":"OK"}`, "application/json", "invalid_response"},
		{"embedded forbidden", 200, `{"status":"failed","error":{"status_code":403,"message":"private-token"}}`, "application/json", "forbidden"},
		{"SSE embedded forbidden", 200, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"status_code\":403}}}\n\n", "text/event-stream", "forbidden"},
		{"SSE truncated", 200, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n", "text/event-stream", "invalid_response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			tr, host, _ := newBPSRecoveryTest(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", test.contentType)
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			})
			now := time.Now()
			original := dueBPSRecovery(tr, host, now)
			runBPSRecoveryTestPass(tr, host, now)
			record := host.record(7)
			if record.NextCheckAt.Sub(record.CheckedAt) != bpsRecoveryInterval {
				t.Fatal("retry deadline is not six hours after this account's completed check")
			}
			if !tr.isBPSAccountDisabled(7, tr.cfg) || record.BlockID != original.BlockID || record.CheckStatus != test.want || requests.Load() != 1 {
				t.Fatalf("unproven recovery: status=%q calls=%d disabled=%v", record.CheckStatus, requests.Load(), tr.isBPSAccountDisabled(7, tr.cfg))
			}
			runBPSRecoveryTestPass(tr, host, now.Add(5*time.Hour))
			if requests.Load() != 1 {
				t.Fatal("failed probe was retried before six hours")
			}
			status := tr.bpsAccountStatusJSON("{}", tr.cfg)
			if strings.Contains(status, "private-token") || strings.Contains(status, host.identity.Token) {
				t.Fatal("status exposed credential or upstream diagnostic")
			}
		})
	}
}

func TestBPSRecoverySuccessfulSSE(t *testing.T) {
	tr, host, _ := newBPSRecoveryTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output_text\":\"OK\"}}\n\n")
	})
	now := time.Now()
	dueBPSRecovery(tr, host, now)
	runBPSRecoveryTestPass(tr, host, now)
	if tr.isBPSAccountDisabled(7, tr.cfg) {
		t.Fatalf("completed stream did not recover: %+v", host.record(7))
	}
}

func TestBPSRecoveryRequiresDurableLeaseAndCurrentScope(t *testing.T) {
	for _, kind := range []string{"storage", "models", "account", "unschedulable", "policy", "manual_restore"} {
		t.Run(kind, func(t *testing.T) {
			var requests atomic.Int32
			tr, host, _ := newBPSRecoveryTest(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })
			now := time.Now()
			record := dueBPSRecovery(tr, host, now)
			switch kind {
			case "storage":
				host.mu.Lock()
				host.failSet = true
				host.mu.Unlock()
			case "models":
				tr.cfg.EnabledModels = []string{}
			case "account":
				tr.cfg.AccountIDs = []int64{9}
			case "unschedulable":
				host.accounts[0].Schedulable = false
			case "policy":
				tr.cfg.BPSAutoDisableOn403 = false
			case "manual_restore":
				tr.cfg.BPSReenabledAccounts = map[string]string{"7": record.BlockID}
			}
			runBPSRecoveryTestPass(tr, host, now)
			if requests.Load() != 0 || host.lookups.Load() != 0 {
				t.Fatal("probe bypassed durability or scope gate")
			}
		})
	}
}

func TestBPSRecoveryConcurrentNew403Wins(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	tr, host, _ := newBPSRecoveryTest(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		fmt.Fprint(w, `{"status":"completed","output_text":"OK"}`)
	})
	now := time.Now()
	old := dueBPSRecovery(tr, host, now)
	done := make(chan struct{})
	go func() { runBPSRecoveryTestPass(tr, host, now); close(done) }()
	<-entered
	tr.disableBPSAccount(context.Background(), 7)
	close(release)
	<-done
	current := host.record(7)
	if !tr.isBPSAccountDisabled(7, tr.cfg) || current.BlockID == old.BlockID || current.RecoveredBlockID != "" {
		t.Fatal("old successful probe cleared concurrent new 403")
	}
}

func TestBPSRecoveryConfigurationChangeFencesProbe(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	tr, host, _ := newBPSRecoveryTest(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	now := time.Now()
	dueBPSRecovery(tr, host, now)
	done := make(chan struct{})
	go func() { runBPSRecoveryTestPass(tr, host, now); close(done) }()
	<-entered
	tr.mu.Lock()
	next := tr.cfg.Clone()
	next.EnabledModels = []string{}
	tr.configureBPSRecovery(tr.cfg, next)
	tr.cfg = next
	tr.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("configuration change did not cancel recovery inference")
	}
	if !tr.isBPSAccountDisabled(7, tr.cfg) || host.record(7).RecoveredBlockID != "" {
		t.Fatal("configuration change allowed stale recovery")
	}
}

func TestBPSRecoveryLegacyBlocksWaitSixHours(t *testing.T) {
	host := &bpsRecoveryTestHost{bpsKVTestHost: newBPSKVTestHost()}
	host.seed(7, strings.Repeat("a", 32))
	tr := newBPSAccountTransport(t)
	tr.host = host
	tr.bindBPSAccountStore(host)
	waitBPSStore(t, tr)
	now := time.Now()
	runBPSRecoveryTestPass(tr, host, now)
	record := host.record(7)
	if !record.NextCheckAt.Equal(now.Add(bpsRecoveryInterval)) || record.CheckStatus != "waiting" || host.lookups.Load() != 0 {
		t.Fatal("legacy restriction was probed without future durable deadline")
	}
	fresh := newBPSAccountTransport(t)
	fresh.host = host
	fresh.bindBPSAccountStore(host)
	waitBPSStore(t, fresh)
	runBPSRecoveryTestPass(fresh, host, now.Add(time.Hour))
	if !host.record(7).NextCheckAt.Equal(record.NextCheckAt) {
		t.Fatal("restart reset an existing recovery schedule")
	}
}

func TestBPSRecoveryUsesRestrictedAccountsOwnProxy(t *testing.T) {
	var direct, proxied atomic.Int32
	tr, host, upstream := newBPSRecoveryTest(t, func(w http.ResponseWriter, r *http.Request) { direct.Add(1) })
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		if r.URL.String() != upstream.URL+"/backend-api/basispoints/responses" || r.Header.Get("Authorization") != "Bearer "+host.identity.Token {
			t.Error("probe did not preserve account identity through its proxy")
		}
		fmt.Fprint(w, `{"status":"completed","output_text":"OK"}`)
	}))
	defer proxy.Close()
	host.identity.ProxyUrl = proxy.URL
	now := time.Now()
	dueBPSRecovery(tr, host, now)
	runBPSRecoveryTestPass(tr, host, now)
	if direct.Load() != 0 || proxied.Load() != 1 || tr.isBPSAccountDisabled(7, tr.cfg) {
		t.Fatal("recovery ignored account proxy or failed to restore verified account")
	}
}

func TestBPSRecoveryTransportErrorNeverRestoresOrLeaks(t *testing.T) {
	tr, host, _ := newBPSRecoveryTest(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected network request") })
	var requests atomic.Int32
	tr.client = &http.Client{Transport: degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("private-proxy-password private-token")
	})}
	now := time.Now()
	dueBPSRecovery(tr, host, now)
	runBPSRecoveryTestPass(tr, host, now)
	if requests.Load() != 1 || !tr.isBPSAccountDisabled(7, tr.cfg) || host.record(7).CheckStatus != "transport_error" {
		t.Fatal("transport error restored account or replayed request")
	}
	status := tr.bpsAccountStatusJSON("{}", tr.cfg)
	if strings.Contains(status, "private-") {
		t.Fatal("recovery leaked upstream diagnostic")
	}
}

func TestBPSRecoveryInflightLeaseSurvivesRestartWithoutReplaying(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	tr, host, _ := newBPSRecoveryTest(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		fmt.Fprint(w, `{"status":"completed","output_text":"OK"}`)
	})
	now := time.Now()
	dueBPSRecovery(tr, host, now)
	done := make(chan struct{})
	go func() { runBPSRecoveryTestPass(tr, host, now); close(done) }()
	<-entered
	fresh := newBPSAccountTransport(t)
	fresh.host = host
	fresh.bindBPSAccountStore(host)
	waitBPSStore(t, fresh)
	runBPSRecoveryTestPass(fresh, host, now.Add(time.Hour))
	close(release)
	<-done
	if host.lookups.Load() != 1 {
		t.Fatal("restart replayed an in-flight durable lease")
	}
}

func TestBPSRecoveryAutomaticRouteCanKeepAccountNative(t *testing.T) {
	tr, host, _ := newBPSRecoveryTest(t, func(w http.ResponseWriter, r *http.Request) { t.Error("automatic native route was ignored") })
	tr.autoDegradation.records = map[int64]autoDegradationRecord{7: {AccountID: 7, SelectionBase: autoSelectionBase(tr.cfg), Decided: true, BPSEnabled: false}}
	now := time.Now()
	dueBPSRecovery(tr, host, now)
	runBPSRecoveryTestPass(tr, host, now)
	if host.lookups.Load() != 0 || !tr.isBPSAccountDisabled(7, tr.cfg) {
		t.Fatal("recovery overrode automatic native routing")
	}
}

func TestBPSRecoveryResultMustPersistBeforeRestoration(t *testing.T) {
	var host *bpsRecoveryTestHost
	tr, localHost, _ := newBPSRecoveryTest(t, func(w http.ResponseWriter, r *http.Request) {
		host.mu.Lock()
		host.failSet = true
		host.mu.Unlock()
		fmt.Fprint(w, `{"status":"completed","output_text":"OK"}`)
	})
	host = localHost
	now := time.Now()
	dueBPSRecovery(tr, host, now)
	runBPSRecoveryTestPass(tr, host, now)
	if !tr.isBPSAccountDisabled(7, tr.cfg) || host.record(7).RecoveredBlockID != "" {
		t.Fatal("unpersisted recovery restored account")
	}
	if host.record(7).CheckStatus != "checking" {
		t.Fatal("failed completion persistence lost pre-send lease")
	}
}

func TestBPSRecoveryLifecycleChangeFencesOutstandingProbe(t *testing.T) {
	for _, kind := range []string{"shutdown", "rebind"} {
		t.Run(kind, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			tr, host, _ := newBPSRecoveryTest(t, func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-r.Context().Done():
				case <-release:
				}
				fmt.Fprint(w, `{"status":"completed","output_text":"OK"}`)
			})
			now := time.Now()
			dueBPSRecovery(tr, host, now)
			done := make(chan struct{})
			go func() { runBPSRecoveryTestPass(tr, host, now); close(done) }()
			<-entered
			if kind == "shutdown" {
				tr.Shutdown()
			} else {
				tr.mu.Lock()
				tr.bindBPSRecovery(nil)
				tr.bindBPSAccountStore(host)
				tr.mu.Unlock()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("lifecycle change did not stop recovery probe")
			}
			if !tr.isBPSAccountDisabled(7, tr.cfg) || host.record(7).RecoveredBlockID != "" {
				t.Fatal("lifecycle change published stale successful recovery")
			}
		})
	}
}

func TestBPSRecoveryBackgroundWorkerRestoresDueAccount(t *testing.T) {
	var requests atomic.Int32
	tr, host, _ := newBPSRecoveryTest(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"status":"completed","output_text":"OK"}`)
	})
	dueBPSRecovery(tr, host, time.Now())
	tr.mu.Lock()
	tr.bindBPSRecovery(host)
	tr.mu.Unlock()
	waitBPSCondition(t, func() bool { return !tr.isBPSAccountDisabled(7, tr.cfg) })
	tr.Shutdown()
	if requests.Load() != 1 || host.record(7).RecoveredBlockID == "" {
		t.Fatal("background lifecycle worker did not recover exactly once")
	}
}

func TestBPSRecoveryNew403DuringLeaseWriteWinsStorage(t *testing.T) {
	tr, host, _ := newBPSRecoveryTest(t, func(w http.ResponseWriter, r *http.Request) { t.Error("superseded lease sent inference") })
	now := time.Now()
	old := dueBPSRecovery(tr, host, now)
	entered, release := make(chan struct{}), make(chan struct{})
	var writes atomic.Int32
	host.mu.Lock()
	host.beforeSet = func(ctx context.Context, key string) {
		if writes.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	}
	host.mu.Unlock()
	done := make(chan struct{})
	go func() { runBPSRecoveryTestPass(tr, host, now); close(done) }()
	<-entered
	disabled := make(chan struct{})
	go func() { tr.disableBPSAccount(context.Background(), 7); close(disabled) }()
	waitBPSCondition(t, func() bool {
		tr.bpsAccounts.mu.RLock()
		defer tr.bpsAccounts.mu.RUnlock()
		return tr.bpsAccounts.records[7].BlockID != old.BlockID
	})
	close(release)
	<-done
	<-disabled
	current := host.record(7)
	if current.BlockID == old.BlockID || current.RecoveredBlockID != "" || !tr.isBPSAccountDisabled(7, tr.cfg) {
		t.Fatal("lease write replaced a newer persisted restriction")
	}
}
