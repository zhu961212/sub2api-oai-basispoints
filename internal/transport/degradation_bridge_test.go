package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestLegacyBridgeTestRejectsPersistedAccountCommandsWithoutSideEffects(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	host := &degradationTestHost{fakeHost: &fakeHost{accounts: []*pluginv1.AccountInfo{
		{Id: 1, Schedulable: true}, {Id: 2, Schedulable: true},
	}}}
	tr.host = host
	var calls atomic.Int32
	tr.client = &http.Client{Transport: degradationRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, fmt.Errorf("unexpected charged account request")
	})}
	ctx := context.Background()
	// Official v1 saves target 1, then target 2; both pages can test target 2.
	// Reject each saved snapshot and its replays without any account request.
	for _, id := range []int64{1, 2, 0} {
		cfg := protocol.DefaultConfig()
		cfg.DegradationCheck, cfg.DegradationCheckAccountID = true, id
		shared := protocol.JSONBytes(cfg)
		for i := 0; i < 2; i++ {
			applied, err := tr.ApplyConfig(ctx, &pluginv1.ApplyConfigRequest{ConfigJson: shared})
			if err != nil || !applied.GetApplied() {
				t.Fatalf("apply/replay failed: %v %v", applied, err)
			}
		}
		var wg sync.WaitGroup
		for page := 0; page < 2; page++ {
			wg.Go(func() {
				result, err := tr.TestConfig(ctx, &pluginv1.TestConfigRequest{ConfigJson: shared})
				if err != nil || result.GetSuccess() || !strings.Contains(result.GetMessage(), "cannot atomically bind") {
					t.Errorf("legacy check must be rejected: %+v %v", result, err)
				}
			})
		}
		wg.Wait()
	}
	if calls.Load() != 0 || len(host.resolvedAccountIDs) != 0 {
		t.Fatalf("legacy commands caused side effects: calls=%d identities=%v", calls.Load(), host.resolvedAccountIDs)
	}
}

func TestLegacyBridgeOrdinaryConnectivityAndPassiveAccountStatusRemainAvailable(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	tr := New()
	defer tr.Shutdown()
	host := &degradationTestHost{fakeHost: &fakeHost{accounts: []*pluginv1.AccountInfo{{Id: 7, Name: "available", Schedulable: true}}}}
	tr.host = host
	cfg := protocol.DefaultConfig()
	cfg.ResponsesURL = server.URL
	result, err := tr.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: protocol.JSONBytes(cfg)})
	if err != nil || !result.GetSuccess() {
		t.Fatalf("ordinary connectivity failed: %v %v", result, err)
	}
	var status map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result.GetStatusJson()), &status); err != nil {
		t.Fatal(err)
	}
	var accounts []accountSummary
	if err := json.Unmarshal(status["accounts"], &accounts); err != nil || len(accounts) != 1 || accounts[0].ID != 7 {
		t.Fatalf("account status unavailable: %+v %v", accounts, err)
	}
	if requests.Load() != 0 || len(host.resolvedAccountIDs) != 0 {
		t.Fatal("connectivity test sent HTTP or resolved an account credential")
	}
}

func TestOfficialV1TestConfigContractHasNoSeparateTargetBinding(t *testing.T) {
	fields := (&pluginv1.TestConfigRequest{}).ProtoReflect().Descriptor().Fields()
	if fields.Len() != 1 || string(fields.Get(0).Name()) != "config_json" {
		t.Fatal("v1 TestConfig contract changed; reassess atomic account diagnostics support")
	}
}
