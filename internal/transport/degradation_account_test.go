package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"google.golang.org/grpc"
)

func TestDegradationCheckTargetsOneAccountUsingItsCredentialAndProxy(t *testing.T) {
	accountToken := token(t, "acct-9")
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Host != "upstream.example.test" || r.Header.Get("Authorization") != "Bearer "+accountToken || r.Header.Get("ChatGPT-Account-ID") != "acct-9" {
			t.Error("check did not use the target account credential and proxy")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("invalid request body: %v", err)
		} else if body["model"] != "gpt-5.6-sol" {
			t.Errorf("check model = %v, want configured model", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(protocol.JSONBytes(map[string]any{"output_text": "iPhone 17"}))
	}))
	defer proxy.Close()
	host := &degradationTestHost{fakeHost: &fakeHost{
		accounts: []*pluginv1.AccountInfo{
			{Id: 7, Name: "other", Schedulable: true},
			{Id: 9, Name: "target", Schedulable: true},
		},
		tokenFor:    map[int64]string{9: accountToken},
		proxyURLFor: map[int64]string{9: proxy.URL},
	}}
	tr := New()
	defer tr.Shutdown()
	tr.host = host
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprintf("automatic=%t", automatic), func(t *testing.T) {
			cfg := protocol.DefaultConfig()
			cfg.ResponsesURL = "http://upstream.example.test/basispoints/api/responses"
			cfg.EnabledModels = []string{"gpt-5.6-sol"}
			cfg.AccountIDs = []int64{7}
			cfg.AutoSelectNewAccounts = automatic
			cfg.ExcludedAccountIDs = []int64{9}
			cfg.DegradationCheck = true
			cfg.DegradationCheckAccountID = 9
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			response, err := tr.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: raw})
			if err != nil || !response.GetSuccess() {
				t.Fatalf("targeted check failed: response=%v err=%v", response, err)
			}
			var status map[string]json.RawMessage
			if err := json.Unmarshal([]byte(response.GetStatusJson()), &status); err != nil {
				t.Fatal(err)
			}
			var check degradationCheckResult
			if err := json.Unmarshal(status["degradation_check"], &check); err != nil {
				t.Fatal(err)
			}
			if !check.Completed || len(check.Results) != 1 || check.Results[0].AccountID != 9 || check.Results[0].Name != "target" || check.Results[0].Status != "ok" || len(check.DegradedAccountIDs) != 0 {
				t.Fatalf("unexpected single account result: %+v", check)
			}
			if !reflect.DeepEqual(cfg.AccountIDs, []int64{7}) || !reflect.DeepEqual(cfg.ExcludedAccountIDs, []int64{9}) {
				t.Fatal("targeted check changed account selection")
			}
		})
	}
	if calls.Load() != 2 || !reflect.DeepEqual(host.resolvedAccountIDs, []int64{9, 9}) {
		t.Fatalf("targeted check probed other accounts: calls=%d resolved=%v", calls.Load(), host.resolvedAccountIDs)
	}
}

func TestDegradationCheckSkipsUnavailableTargetWithoutProbingOthers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accounts []*pluginv1.AccountInfo
		wantErr  string
	}{
		{name: "missing", accounts: []*pluginv1.AccountInfo{{Id: 7, Schedulable: true}}, wantErr: "account is not available for degradation check"},
		{name: "empty directory", wantErr: "account is not available for degradation check"},
		{name: "unschedulable", accounts: []*pluginv1.AccountInfo{{Id: 7, Schedulable: true}, {Id: 9, Schedulable: false}}, wantErr: "account is not schedulable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &degradationTestHost{fakeHost: &fakeHost{accounts: tc.accounts}}
			tr := New()
			defer tr.Shutdown()
			tr.host = host
			var calls atomic.Int32
			tr.client = &http.Client{Transport: degradationRoundTripper(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return nil, fmt.Errorf("unexpected probe")
			})}
			cfg := protocol.DefaultConfig()
			cfg.DegradationCheck = true
			cfg.DegradationCheckAccountID = 9
			check, err := tr.runDegradationCheck(context.Background(), cfg)
			if err != nil || !check.Completed || len(check.Results) != 1 || len(check.DegradedAccountIDs) != 0 {
				t.Fatalf("unavailable target failed: %+v err=%v", check, err)
			}
			if got := check.Results[0]; got.AccountID != 9 || got.Status != "skipped" || got.Error != tc.wantErr || got.Answer != "" {
				t.Fatalf("unexpected unavailable result: %+v", got)
			}
			if calls.Load() != 0 || len(host.resolvedAccountIDs) != 0 {
				t.Fatal("unavailable target resolved credentials or probed another account")
			}
		})
	}
}

type degradationLegacyTestHost struct {
	*degradationTestHost
}

func (h *degradationLegacyTestHost) ListAccounts(context.Context, *pluginv1.ListAccountsRequest, ...grpc.CallOption) (*pluginv1.ListAccountsResponse, error) {
	return &pluginv1.ListAccountsResponse{AccountIds: []int64{7, 9}}, nil
}

func TestDegradationCheckTargetsLegacyAccountDirectory(t *testing.T) {
	host := &degradationLegacyTestHost{degradationTestHost: &degradationTestHost{fakeHost: &fakeHost{token: token(t, "acct-9")}}}
	tr := New()
	defer tr.Shutdown()
	tr.host = host
	tr.client = &http.Client{Transport: degradationRoundTripper(func(*http.Request) (*http.Response, error) {
		body := protocol.JSONBytes(map[string]any{"output_text": "iPhone 16"})
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	cfg := protocol.DefaultConfig()
	cfg.DegradationCheck = true
	cfg.DegradationCheckAccountID = 9
	check, err := tr.runDegradationCheck(context.Background(), cfg)
	if err != nil || !check.Completed || len(check.Results) != 1 || check.Results[0].AccountID != 9 || check.Results[0].Status != "degraded" || !reflect.DeepEqual(check.DegradedAccountIDs, []int64{9}) || !reflect.DeepEqual(host.resolvedAccountIDs, []int64{9}) {
		t.Fatalf("legacy targeted check: %+v resolved=%v err=%v", check, host.resolvedAccountIDs, err)
	}
}
