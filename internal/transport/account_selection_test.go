package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

type accountSelectionCapture struct {
	target, authorization, identity string
	body                            []byte
}

func accountSelectionUpstream(t *testing.T, target string, captured chan<- accountSelectionCapture) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		captured <- accountSelectionCapture{target, r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID"), body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_account_selection","status":"completed","output":[]}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func assertAccountSelectionForward(t *testing.T, tr *Transport, hostURL string, captured <-chan accountSelectionCapture, accountID int64, model, wantTarget string) {
	t.Helper()
	accessToken := token(t, "scheduled-account")
	body := protocol.JSONBytes(map[string]any{"model": model, "input": "preserve scheduled identity"})
	frames := requestFrames(t, hostURL, accessToken, map[string]string{"ChatGPT-Account-ID": "scheduled-account"}, body)
	frames[0].GetStart().AccountId = accountID
	result := runForward(t, tr, frames)
	if result.errFrame != nil || result.status != http.StatusOK || !result.ended {
		t.Fatalf("forward failed: %#v", result)
	}
	select {
	case got := <-captured:
		if got.target != wantTarget {
			t.Errorf("account %d routed to %s, want %s", accountID, got.target, wantTarget)
		}
		if got.authorization != "Bearer "+accessToken || got.identity != "scheduled-account" {
			t.Fatal("routing changed the scheduled account credentials")
		}
		if !bytes.Equal(got.body, body) {
			t.Fatalf("routing changed request body: %s", got.body)
		}
	default:
		t.Fatal("neither local upstream received request")
	}
	select {
	case extra := <-captured:
		t.Fatalf("request sent to multiple upstreams: %s", extra.target)
	default:
	}
}

func TestAccountSelectionForwardRoutesCurrentAndFutureAccounts(t *testing.T) {
	tests := []struct {
		name               string
		automatic          bool
		selected, excluded []int64
		accountID          int64
		enabled            []string
		wantTarget         string
	}{
		{"legacy unrestricted", false, nil, nil, 999, nil, "bps"},
		{"legacy selected", false, []int64{7}, nil, 7, nil, "bps"},
		{"legacy unselected", false, []int64{7}, nil, 999, nil, "host"},
		{"new ID outside saved snapshot", true, []int64{7}, nil, 999, nil, "bps"},
		{"exclusion overrides snapshot", true, []int64{7, 9}, []int64{9}, 9, nil, "host"},
		{"all known excluded", true, nil, []int64{7, 9}, 7, nil, "host"},
		{"future after all known excluded", true, nil, []int64{7, 9}, 999, nil, "bps"},
		{"disabled model", true, []int64{7}, nil, 999, []string{"gpt-5.6-sol"}, "host"},
		{"all models disabled", true, []int64{7}, nil, 999, []string{}, "host"},
		{"missing account compatibility", true, []int64{7}, []int64{7}, 0, nil, "bps"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			captured := make(chan accountSelectionCapture, 4)
			host := accountSelectionUpstream(t, "host", captured)
			bps := accountSelectionUpstream(t, "bps", captured)
			tr := New()
			t.Cleanup(tr.Shutdown)
			applyConfig(t, tr, map[string]any{"responses_url": bps.URL, "account_ids": test.selected, "auto_select_new_accounts": test.automatic, "excluded_account_ids": test.excluded, "enabled_models": test.enabled, "rewrite_tools": false, "transform_responses": false})
			assertAccountSelectionForward(t, tr, host.URL, captured, test.accountID, "gpt-6-astra", test.wantTarget)
		})
	}
}

func TestAccountSelectionForwardSurvivesConfigReloadAndReportsHealth(t *testing.T) {
	captured := make(chan accountSelectionCapture, 4)
	host := accountSelectionUpstream(t, "host", captured)
	bps := accountSelectionUpstream(t, "bps", captured)
	original := New()
	t.Cleanup(original.Shutdown)
	validation, err := original.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: protocol.JSONBytes(map[string]any{"responses_url": bps.URL, "auto_select_new_accounts": true, "account_ids": []int64{7, 9}, "excluded_account_ids": []int64{9, 11}, "rewrite_tools": false, "transform_responses": false})})
	if err != nil || !validation.GetValid() {
		t.Fatalf("config validation failed: %v %#v", err, validation)
	}
	persisted := append([]byte(nil), validation.GetNormalizedConfigJson()...)
	restarted := New()
	t.Cleanup(restarted.Shutdown)
	for _, tr := range []*Transport{original, restarted} {
		result, err := tr.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: persisted})
		if err != nil || !result.GetApplied() {
			t.Fatalf("persisted config failed: %v %#v", err, result)
		}
		assertAccountSelectionForward(t, tr, host.URL, captured, 999, "gpt-6-astra", "bps")
		assertAccountSelectionForward(t, tr, host.URL, captured, 9, "gpt-6-astra", "host")
		health, err := tr.Health(context.Background(), &pluginv1.HealthRequest{})
		if err != nil || !health.GetHealthy() {
			t.Fatalf("health failed: %v %#v", err, health)
		}
		var state struct {
			AutoSelectNewAccounts *bool   `json:"auto_select_new_accounts"`
			AccountIDs            []int64 `json:"account_ids"`
			ExcludedAccountIDs    []int64 `json:"excluded_account_ids"`
		}
		if err := json.Unmarshal([]byte(health.GetStatusJson()), &state); err != nil {
			t.Fatal(err)
		}
		if state.AutoSelectNewAccounts == nil || !*state.AutoSelectNewAccounts || !reflect.DeepEqual(state.AccountIDs, []int64{7, 9}) || !reflect.DeepEqual(state.ExcludedAccountIDs, []int64{9, 11}) {
			t.Fatalf("health lost selection policy: %s", health.GetStatusJson())
		}
	}
}
