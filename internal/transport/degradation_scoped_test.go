package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"google.golang.org/grpc/metadata"
)

func degradationScopedContext(id string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-sub2api-test-scope", "request-v1", "x-sub2api-test-request-id", id))
}

func degradationScopedResult(t *testing.T, response *pluginv1.TestConfigResponse) degradationCheckResult {
	t.Helper()
	var status map[string]json.RawMessage
	if response == nil || json.Unmarshal([]byte(response.GetStatusJson()), &status) != nil {
		t.Fatalf("invalid status: %v", response)
	}
	var check degradationCheckResult
	if err := json.Unmarshal(status["degradation_check"], &check); err != nil {
		t.Fatalf("missing diagnostic status: %v, %v", response, err)
	}
	return check
}

func TestScopedDegradationRequiresBothValidIncomingMarkers(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	host := &degradationTestHost{fakeHost: &fakeHost{accounts: []*pluginv1.AccountInfo{{Id: 7, Schedulable: true}}}}
	tr.host = host
	var calls atomic.Int32
	tr.client = &http.Client{Transport: degradationRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, fmt.Errorf("unauthorized probe")
	})}
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"legacy", context.Background()},
		{"scope only", metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-sub2api-test-scope", "request-v1"))},
		{"id only", metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-sub2api-test-request-id", "request1"))},
		{"wrong scope", metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-sub2api-test-scope", "request-v2", "x-sub2api-test-request-id", "request1"))},
		{"duplicate scope", metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-sub2api-test-scope", "request-v1", "x-sub2api-test-scope", "request-v1", "x-sub2api-test-request-id", "request1"))},
		{"duplicate id", metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-sub2api-test-scope", "request-v1", "x-sub2api-test-request-id", "request1", "x-sub2api-test-request-id", "request2"))},
		{"empty id", degradationScopedContext("")},
		{"space id", degradationScopedContext("request 1")},
		{"unicode id", degradationScopedContext("请求1")},
		{"oversized id", degradationScopedContext(strings.Repeat("a", 129))},
		{"outgoing only", metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-sub2api-test-scope", "request-v1", "x-sub2api-test-request-id", "request1"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := protocol.DefaultConfig()
			cfg.DegradationCheck, cfg.DegradationCheckAccountID = true, 7
			response, err := tr.TestConfig(tc.ctx, &pluginv1.TestConfigRequest{ConfigJson: protocol.JSONBytes(cfg)})
			if err != nil || response.GetSuccess() || !strings.Contains(response.GetMessage(), "cannot atomically bind") {
				t.Fatalf("invalid scope accepted: %v, %v", response, err)
			}
		})
	}
	if calls.Load() != 0 || len(host.resolvedAccountIDs) != 0 {
		t.Fatal("invalid scope triggered an account request")
	}
}

func TestScopedDegradationConcurrentTargetsNeverApplyOrCrossAccounts(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	host := &degradationTestHost{fakeHost: &fakeHost{
		accounts: []*pluginv1.AccountInfo{{Id: 7, Schedulable: true}, {Id: 9, Schedulable: true}},
		tokenFor: map[int64]string{7: token(t, "acct-7"), 9: token(t, "acct-9")},
	}}
	tr.host = host
	applyConfig(t, tr, map[string]any{"account_ids": []int64{42}, "excluded_account_ids": []int64{9}, "auto_select_new_accounts": true})
	original := tr.cfg.Clone()
	tr.client = &http.Client{Transport: degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
		var id int64
		_, _ = fmt.Sscanf(r.Header.Get("ChatGPT-Account-ID"), "acct-%d", &id)
		if (id != 7 && id != 9) || r.Header.Get("Authorization") != "Bearer "+host.tokenFor[id] {
			t.Error("diagnostic used another account credential")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(protocol.JSONBytes(map[string]any{"output_text": "iPhone 17"}))))}, nil
	})}
	var wg sync.WaitGroup
	for _, id := range []int64{7, 9, 7, 9} {
		wg.Go(func() {
			cfg := protocol.DefaultConfig()
			cfg.DegradationCheck, cfg.DegradationCheckAccountID = true, id
			cfg.AccountIDs = []int64{id}
			requestID := fmt.Sprintf("scoped-%d", id)
			response, err := tr.TestConfig(degradationScopedContext(requestID), &pluginv1.TestConfigRequest{ConfigJson: protocol.JSONBytes(cfg)})
			if err != nil || !response.GetSuccess() {
				t.Errorf("request failed: %v, %v", response, err)
				return
			}
			check := degradationScopedResult(t, response)
			if check.RequestID != requestID || !check.Completed || !reflect.DeepEqual(check.TargetAccountIDs, []int64{id}) || len(check.Results) != 1 || check.Results[0].AccountID != id || check.Results[0].Status != "ok" {
				t.Errorf("request leaked another target: %+v", check)
			}
		})
	}
	wg.Wait()
	if !reflect.DeepEqual(tr.cfg, original) || len(host.resolvedAccountIDs) != 4 {
		t.Fatalf("diagnostics applied config or lost requests: config=%+v resolved=%v", tr.cfg, host.resolvedAccountIDs)
	}
}

func TestScopedDegradationBulkSnapshotSkipsUnknownAndNewDirectoryAccounts(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	host := &degradationTestHost{fakeHost: &fakeHost{
		accounts: []*pluginv1.AccountInfo{{Id: 7, Schedulable: true}, {Id: 9, Schedulable: true}, {Id: 11, Schedulable: false}},
		tokenFor: map[int64]string{7: token(t, "acct-7")},
	}}
	tr.host = host
	var calls atomic.Int32
	tr.client = &http.Client{Transport: degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Header.Get("ChatGPT-Account-ID") != "acct-7" {
			t.Error("bulk scan picked an account outside its snapshot")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(protocol.JSONBytes(map[string]any{"output_text": "iPhone 16"}))))}, nil
	})}
	cfg := protocol.DefaultConfig()
	cfg.DegradationCheck = true
	cfg.DegradationCheckAccountIDs = []int64{11, 7, 99, 7}
	response, err := tr.TestConfig(degradationScopedContext("bulk-1"), &pluginv1.TestConfigRequest{ConfigJson: protocol.JSONBytes(cfg)})
	if err != nil || !response.GetSuccess() {
		t.Fatalf("bulk request failed: %v, %v", response, err)
	}
	check := degradationScopedResult(t, response)
	if !reflect.DeepEqual(check.TargetAccountIDs, []int64{11, 7, 99}) || check.RequestID != "bulk-1" || !check.Completed || len(check.Results) != 3 || !reflect.DeepEqual(check.DegradedAccountIDs, []int64{7}) {
		t.Fatalf("bulk target changed: %+v", check)
	}
	for i, want := range []string{"skipped", "degraded", "skipped"} {
		if check.Results[i].AccountID != check.TargetAccountIDs[i] || check.Results[i].Status != want {
			t.Fatalf("unexpected target result: %+v", check.Results[i])
		}
	}
	if calls.Load() != 1 || !reflect.DeepEqual(host.resolvedAccountIDs, []int64{7}) {
		t.Fatalf("bulk probed extra identities: calls=%d IDs=%v", calls.Load(), host.resolvedAccountIDs)
	}
}

func TestScopedDegradationInvalidTargetsNeverResolveCredentials(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	host := &degradationTestHost{fakeHost: &fakeHost{accounts: []*pluginv1.AccountInfo{{Id: 7, Schedulable: true}}}}
	tr.host = host
	var calls atomic.Int32
	tr.client = &http.Client{Transport: degradationRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, fmt.Errorf("unexpected diagnostic")
	})}
	for _, cfg := range []map[string]any{
		{"degradation_check": true},
		{"degradation_check": true, "degradation_check_account_ids": []int64{}},
		{"degradation_check": true, "degradation_check_account_ids": []int64{0}},
		{"degradation_check": true, "degradation_check_account_ids": []int64{-7}},
		{"degradation_check": true, "degradation_check_account_id": 7, "degradation_check_account_ids": []int64{9}},
		{"degradation_check": true, "degradation_check_account_ids": []any{7.5}},
		{"degradation_check": true, "degradation_check_account_ids": []any{"7"}},
		{"degradation_check_account_id": 7},
		{"degradation_check_account_ids": []int64{7}},
	} {
		response, err := tr.TestConfig(degradationScopedContext("invalid-target"), &pluginv1.TestConfigRequest{ConfigJson: protocol.JSONBytes(cfg)})
		if err != nil || response.GetSuccess() {
			t.Errorf("invalid target accepted: config=%v response=%v err=%v", cfg, response, err)
		}
	}
	if calls.Load() != 0 || len(host.resolvedAccountIDs) != 0 {
		t.Fatal("invalid target resolved credentials or sent a request")
	}
}

func TestScopedDegradationIgnoresUnsavedRecoveryAcknowledgement(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	host := &degradationTestHost{fakeHost: &fakeHost{accounts: []*pluginv1.AccountInfo{{Id: 7, Schedulable: true}}, token: token(t, "acct-7")}}
	tr.host = host
	tr.disableBPSAccount(context.Background(), 7)
	tr.bpsAccounts.mu.RLock()
	block := tr.bpsAccounts.records[7].BlockID
	tr.bpsAccounts.mu.RUnlock()
	var calls atomic.Int32
	tr.client = &http.Client{Transport: degradationRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(protocol.JSONBytes(map[string]any{"output_text": "iPhone 17"}))))}, nil
	})}
	cfg := protocol.DefaultConfig()
	cfg.DegradationCheck, cfg.DegradationCheckAccountID = true, 7
	cfg.BPSReenabledAccounts = map[string]string{"7": block}
	response, err := tr.TestConfig(degradationScopedContext("pending-recovery"), &pluginv1.TestConfigRequest{ConfigJson: protocol.JSONBytes(cfg)})
	if err != nil || !response.GetSuccess() {
		t.Fatalf("blocked diagnostic failed: %v, %v", response, err)
	}
	check := degradationScopedResult(t, response)
	if len(check.Results) != 1 || check.Results[0].Status != "ok" || calls.Load() != 1 || len(host.resolvedAccountIDs) != 1 || !tr.isBPSAccountDisabled(7, tr.cfg) {
		t.Fatalf("native probe failed or cleared BPS protection: %+v calls=%d IDs=%v", check, calls.Load(), host.resolvedAccountIDs)
	}
	var status map[string]json.RawMessage
	_ = json.Unmarshal([]byte(response.GetStatusJson()), &status)
	var disabled []int64
	_ = json.Unmarshal(status["bps_disabled_account_ids"], &disabled)
	if !reflect.DeepEqual(disabled, []int64{7}) || len(tr.cfg.BPSReenabledAccounts) != 0 {
		t.Fatal("diagnostic status confirmed an unsaved recovery")
	}
	applyConfig(t, tr, map[string]any{"bps_reenabled_accounts": map[string]string{"7": block}})
	cfg.BPSReenabledAccounts = nil
	response, err = tr.TestConfig(degradationScopedContext("saved-recovery"), &pluginv1.TestConfigRequest{ConfigJson: protocol.JSONBytes(cfg)})
	if err != nil || !response.GetSuccess() || degradationScopedResult(t, response).Results[0].Status != "ok" || calls.Load() != 2 {
		t.Fatalf("persisted acknowledgement not honored: %v, %v calls=%d", response, err, calls.Load())
	}
}

func TestScopedNativeDegradation403DoesNotDisableBPS(t *testing.T) {
	for _, wire := range []string{"http", "json", "sse"} {
		t.Run(wire, func(t *testing.T) {
			tr := New()
			defer tr.Shutdown()
			host := &degradationTestHost{fakeHost: &fakeHost{accounts: []*pluginv1.AccountInfo{{Id: 7, Schedulable: true}, {Id: 9, Schedulable: true}}, tokenFor: map[int64]string{9: token(t, "acct-9")}}}
			tr.host = host
			var calls atomic.Int32
			tr.client = &http.Client{Transport: degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if r.Header.Get("ChatGPT-Account-ID") != "acct-9" {
					t.Error("403 probe used the wrong identity")
				}
				failure := map[string]any{"status": "failed", "error": map[string]any{"status_code": 403}, "output_text": "iPhone 16"}
				status, contentType, body := 200, "application/json", string(protocol.JSONBytes(failure))
				if wire == "http" {
					status = 403
				} else if wire == "sse" {
					contentType, body = "text/event-stream", streamData(map[string]any{"type": "response.failed", "response": failure})
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			cfg := protocol.DefaultConfig()
			cfg.DegradationCheck, cfg.DegradationCheckAccountID = true, 9
			// A native error must never be fed to the production BPS 403 policy.
			cfg.BPSAutoDisableOn403 = false
			response, err := tr.TestConfig(degradationScopedContext("forbidden-"+wire), &pluginv1.TestConfigRequest{ConfigJson: protocol.JSONBytes(cfg)})
			if err != nil || !response.GetSuccess() {
				t.Fatalf("403 diagnostic failed: %v, %v", response, err)
			}
			check := degradationScopedResult(t, response)
			if len(check.Results) != 1 || check.Results[0].Status != "error" || len(check.DegradedAccountIDs) != 0 || calls.Load() != 1 || !reflect.DeepEqual(host.resolvedAccountIDs, []int64{9}) || tr.isBPSAccountDisabled(9, tr.cfg) || tr.isBPSAccountDisabled(7, tr.cfg) {
				t.Fatalf("native 403 changed BPS protection: %+v calls=%d IDs=%v", check, calls.Load(), host.resolvedAccountIDs)
			}
		})
	}
}
