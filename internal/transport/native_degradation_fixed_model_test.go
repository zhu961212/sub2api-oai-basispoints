package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeDegradationFixedModelAcrossEntryPoints(t *testing.T) {
	for _, mode := range []string{"single", "bulk", "automatic"} {
		t.Run(mode, func(t *testing.T) {
			tr, host, _ := newAutoDetectionTestTransport(t)
			tr.cfg.EnabledModels = []string{"gpt-6-astra"}
			tr.cfg.AccountIDs = []int64{7, 9}
			tr.cfg.AutoSelectNewAccounts = false
			tr.cfg.ExcludedAccountIDs = nil
			tr.cfg.ResponsesURL = "https://must-not-use-bps.invalid/responses"
			tr.cfg.DegradationCheckModel = "legacy-native-probe-model"
			var calls atomic.Int32
			tr.client = &http.Client{Transport: degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if r.URL.String() != nativeDegradationResponsesURL {
					t.Errorf("probe entered business/BPS routing: %s", r.URL)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					return nil, err
				}
				if body["model"] != "gpt-6-astra" || body["model_selection"] != nil {
					t.Errorf("legacy config changed native probe body: %#v", body)
				}
				if r.Header.Get("ChatGPT-Account-ID") == "" || r.Header.Get("Authorization") == "" {
					t.Error("native probe lost account identity")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output_text":"iPhone 16"}`))}, nil
			})}
			cfg := tr.cfg.Clone()
			cfg.DegradationCheck = true
			want := int32(2)
			switch mode {
			case "single", "bulk":
				if mode == "single" {
					cfg.DegradationCheckAccountID, want = 7, 1
				} else {
					cfg.DegradationCheckAccountIDs = []int64{7, 9}
				}
				result, err := tr.runDegradationCheck(context.Background(), cfg)
				if err != nil || !result.Completed {
					t.Fatalf("manual probe failed: %+v, %v", result, err)
				}
			case "automatic":
				for _, id := range []int64{7, 9} {
					tr.autoDegradation.records[id] = autoDegradationRecord{AccountID: id, SelectionBase: autoSelectionBase(cfg), Model: "gpt-5.4", PendingStatus: "degraded", Consecutive: 1, CheckedAt: time.Now()}
				}
				tr.runAutomaticBatch(context.Background(), host, 1, 1, cfg, []int64{7, 9})
				for _, id := range []int64{7, 9} {
					record := tr.autoDegradation.records[id]
					if record.Model != "gpt-6-astra" || record.Consecutive != 1 || record.Decided {
						t.Fatalf("old model state contaminated fixed probe: %+v", record)
					}
				}
			}
			if got := calls.Load(); got != want {
				t.Fatalf("probe requests=%d, want %d", got, want)
			}
		})
	}
}

func TestAutoDegradationIgnoresLegacyModelConfigurationChanges(t *testing.T) {
	tr, _, _ := newAutoDetectionTestTransport(t)
	old, next := tr.cfg.Clone(), tr.cfg.Clone()
	old.DegradationCheckModel, next.DegradationCheckModel = "gpt-5.4", "arbitrary-old-setting"
	canceled := false
	tr.autoDegradation.probeCancel = func() { canceled = true }
	revision := tr.autoDegradation.revision
	tr.configureAutoDegradation(old, next)
	if canceled || tr.autoDegradation.revision != revision {
		t.Fatal("ignored legacy model setting canceled or fenced the fixed probe")
	}
	now := time.Now()
	first := nextAutoDegradationRecord(old, autoDegradationRecord{AccountID: 7}, degradationAccountResult{AccountID: 7, Status: "degraded"}, now)
	second := nextAutoDegradationRecord(next, first, degradationAccountResult{AccountID: 7, Status: "degraded"}, now.Add(autoDegradationRetry))
	if !second.Decided || second.Consecutive != 2 || second.Model != "gpt-6-astra" {
		t.Fatalf("ignored setting reset fixed-model confirmations: %+v", second)
	}
}
