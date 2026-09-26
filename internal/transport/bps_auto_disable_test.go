package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestBPSAutoDisableOffDoesNotCreateRestriction(t *testing.T) {
	host, tr := newBPSKVTestHost(), newBPSAccountTransport(t)
	tr.bindBPSAccountStore(host)
	waitBPSStore(t, tr)
	applyConfig(t, tr, map[string]any{"bps_auto_disable_on_403": false})
	tr.disableBPSAccount(context.Background(), 7)
	tr.bpsAccounts.mu.RLock()
	recordCount, dirtyCount := len(tr.bpsAccounts.records), len(tr.bpsAccounts.dirty)
	tr.bpsAccounts.mu.RUnlock()
	host.mu.Lock()
	writes := host.sets
	host.mu.Unlock()
	if recordCount != 0 || dirtyCount != 0 || writes != 0 {
		t.Fatalf("disabled policy created restriction: records=%d dirty=%d writes=%d", recordCount, dirtyCount, writes)
	}
}

func TestBPSAutoDisableTogglePreservesExistingRestriction(t *testing.T) {
	host, tr := newBPSKVTestHost(), newBPSAccountTransport(t)
	tr.bindBPSAccountStore(host)
	waitBPSStore(t, tr)
	tr.disableBPSAccount(context.Background(), 7)
	original := host.record(7)
	applyConfig(t, tr, map[string]any{"bps_auto_disable_on_403": false})
	tr.disableBPSAccount(context.Background(), 7)
	tr.disableBPSAccount(context.Background(), 8)
	cfg := protocol.DefaultConfig()
	cfg.BPSAutoDisableOn403 = false
	if !tr.isBPSAccountDisabled(7, cfg) || tr.isBPSAccountDisabled(8, cfg) || host.record(7).BlockID != original.BlockID {
		t.Fatal("turning policy off cleared or replaced existing restrictions")
	}
	fresh := newBPSAccountTransport(t)
	applyConfig(t, fresh, map[string]any{"bps_auto_disable_on_403": false})
	fresh.bindBPSAccountStore(host)
	waitBPSStore(t, fresh)
	if !fresh.isBPSAccountDisabled(7, cfg) {
		t.Fatal("restart with policy off lost existing restriction")
	}
	cfg.BPSReenabledAccounts = map[string]string{"7": original.BlockID}
	if fresh.isBPSAccountDisabled(7, cfg) {
		t.Fatal("explicit reenable did not acknowledge the existing restriction")
	}
	applyConfig(t, fresh, map[string]any{"bps_auto_disable_on_403": true, "bps_reenabled_accounts": cfg.BPSReenabledAccounts})
	fresh.disableBPSAccount(context.Background(), 7)
	if host.record(7).BlockID == original.BlockID || !fresh.isBPSAccountDisabled(7, cfg) {
		t.Fatal("turning policy back on did not create a new restriction")
	}
}

func TestBPSForbiddenForwardPolicyOffKeepsRouting(t *testing.T) {
	for _, wire := range []string{"http", "json", "sse"} {
		for _, transform := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/transform_%t", wire, transform), func(t *testing.T) {
				fixture := newBPSForbiddenForwardFixture(t, transform, func(w http.ResponseWriter, _ *http.Request) {
					failure := map[string]any{"id": "resp_explicit403", "status": "failed", "output": []any{}, "error": map[string]any{"status_code": 403, "code": "any_error_code"}}
					switch wire {
					case "http":
						w.WriteHeader(http.StatusForbidden)
					case "sse":
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.failed", "response": failure}))
					default:
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(protocol.JSONBytes(failure))
					}
				})
				fixture.transport.mu.RLock()
				url := fixture.transport.cfg.ResponsesURL
				fixture.transport.mu.RUnlock()
				applyConfig(t, fixture.transport, map[string]any{"responses_url": url, "transform_responses": transform, "account_ids": []int64{7, 8}, "bps_auto_disable_on_403": false})
				source := map[string]any{"model": "gpt-6-astra", "input": "hi", "stream": wire == "sse"}
				fixture.forward(t, 7, source)
				fixture.forward(t, 7, source)
				if fixture.disabled(7) || fixture.bpsCalls.Load() != 2 || fixture.hostCalls.Load() != 0 {
					t.Fatalf("policy off changed routing: disabled=%t bps=%d host=%d", fixture.disabled(7), fixture.bpsCalls.Load(), fixture.hostCalls.Load())
				}
			})
		}
	}
}

func TestBPSForbiddenForwardUsesPolicyAtResponseTime(t *testing.T) {
	var fixture *bpsForbiddenForwardFixture
	fixture = newBPSForbiddenForwardFixture(t, true, func(w http.ResponseWriter, _ *http.Request) {
		// The request already captured the enabled policy before reaching this
		// upstream. Apply the user's new policy before returning its response.
		fixture.transport.mu.RLock()
		cfg := fixture.transport.cfg.Clone()
		fixture.transport.mu.RUnlock()
		cfg.BPSAutoDisableOn403 = false
		result, err := fixture.transport.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: protocol.JSONBytes(cfg)})
		if err != nil || !result.GetApplied() {
			t.Errorf("could not disable policy during request: %v, %v", result, err)
		}
		w.WriteHeader(http.StatusForbidden)
	})
	result := fixture.forward(t, 7, map[string]any{"model": "gpt-6-astra", "input": "hi"})
	if result.errFrame == nil || !result.errFrame.GetRequestSent() || fixture.disabled(7) {
		t.Fatalf("in-flight request ignored current policy: result=%+v disabled=%t", result, fixture.disabled(7))
	}
}

func TestBPSAutoDisableRepeatedStreamFailureCreatesOneRestriction(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, transform := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%t/transform=%t", enabled, transform), func(t *testing.T) {
				fixture := newBPSForbiddenForwardFixture(t, transform, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.output_text.delta", "delta": "working"}))
					failure := map[string]any{"status": "failed", "output": []any{}, "error": map[string]any{"status_code": 403, "message": "PRIVATE forbidden diagnostic"}}
					for range 2 {
						_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.failed", "response": failure}))
					}
				})
				host := newBPSKVTestHost()
				fixture.transport.bindBPSAccountStore(host)
				defer fixture.transport.bindBPSAccountStore(nil)
				waitBPSStore(t, fixture.transport)
				fixture.transport.mu.RLock()
				url := fixture.transport.cfg.ResponsesURL
				fixture.transport.mu.RUnlock()
				applyConfig(t, fixture.transport, map[string]any{"responses_url": url, "transform_responses": transform, "account_ids": []int64{7, 8}, "bps_auto_disable_on_403": enabled})
				result := fixture.forward(t, 7, map[string]any{"model": "gpt-6-astra", "input": "hi", "stream": true})
				host.mu.Lock()
				writes, records := host.sets, len(host.values)
				host.mu.Unlock()
				want := 0
				if enabled {
					want = 1
				}
				if result.errFrame != nil || result.status != http.StatusOK || !result.ended || writes != want || records != want || fixture.disabled(7) != enabled || fixture.disabled(8) {
					t.Fatalf("repeated failure changed restriction scope: writes=%d records=%d disabled7=%t disabled8=%t result=%+v", writes, records, fixture.disabled(7), fixture.disabled(8), result)
				}
			})
		}
	}
}
