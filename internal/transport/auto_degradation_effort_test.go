package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestAutoDegradationEffortChangesDoNotCombineConfirmations(t *testing.T) {
	for _, previous := range []string{"", "low"} {
		for _, status := range []string{"degraded", "ok"} {
			t.Run(previous+"_"+status, func(t *testing.T) {
				cfg := protocol.DefaultConfig()
				cfg.ExcludedAccountIDs = []int64{7}
				now := time.Now()
				old := autoDegradationRecord{AccountID: 7, SelectionBase: autoSelectionBase(cfg),
					Model: degradationModel(cfg), ReasoningEffort: previous, NativeTimezoneByIP: cfg.NativeTimezoneByIP,
					Decided: true, BPSEnabled: status == "ok", PendingStatus: status, Consecutive: 1, CheckedAt: now}
				result := degradationAccountResult{AccountID: 7, Status: status}
				first := nextAutoDegradationRecord(cfg, old, result, now.Add(autoDegradationRetry))
				if !first.Decided || first.BPSEnabled != old.BPSEnabled || first.Consecutive != 1 ||
					first.PendingStatus != status || first.ReasoningEffort != "xhigh" || first.SelectionBase != old.SelectionBase {
					t.Fatalf("effort change combined confirmations or discarded a route: %+v", first)
				}
				second := nextAutoDegradationRecord(cfg, first, result, now.Add(2*autoDegradationRetry))
				if !second.Decided || second.BPSEnabled != (status == "degraded") || second.Consecutive != 2 || second.PendingStatus != "" {
					t.Fatalf("two xhigh confirmations did not switch routing: %+v", second)
				}
				for _, failure := range []string{"error", "skipped"} {
					failed := nextAutoDegradationRecord(cfg, old, degradationAccountResult{AccountID: 7, Status: failure}, now.Add(autoDegradationRetry))
					if !failed.Decided || failed.BPSEnabled != old.BPSEnabled || failed.Consecutive != 0 ||
						failed.PendingStatus != "" || failed.ReasoningEffort != "xhigh" || failed.SelectionBase != old.SelectionBase {
						t.Fatalf("failed effort migration lost routing or retained a confirmation: %+v", failed)
					}
				}
			})
		}
	}
}

func TestAutoDegradationEffortInterruptedLeasePersistsMigration(t *testing.T) {
	for _, previous := range []string{"", "low"} {
		t.Run(previous, func(t *testing.T) {
			tr, host, _ := newAutoDetectionTestTransport(t)
			tr.cfg.NativeTimezoneByIP = false
			now := time.Now()
			old := autoDegradationRecord{AccountID: 7, SelectionBase: autoSelectionBase(tr.cfg),
				Model: degradationModel(tr.cfg), ReasoningEffort: previous,
				Decided: true, BPSEnabled: true, PendingStatus: "ok", Consecutive: 1, CheckedAt: now}
			tr.autoDegradation.records[7] = old
			host.mu.Lock()
			host.values[autoDegradationNamespace+"/"+autoDegradationIndexKey] = protocol.JSONBytes([]int64{7})
			host.mu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tr.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
				cancel()
				return nil, context.Canceled
			})}
			tr.runAutomaticBatch(ctx, host, 1, 1, tr.cfg, []int64{7})
			restored, err := loadAutoDegradationRecords(context.Background(), host)
			if err != nil {
				t.Fatal(err)
			}
			lease := restored[7]
			if !lease.InFlight || lease.PendingStatus != "" || lease.Consecutive != 0 ||
				lease.ReasoningEffort != "xhigh" || !lease.Decided || !lease.BPSEnabled || lease.SelectionBase != old.SelectionBase {
				t.Fatalf("restored effort lease lost routing or retained stale confirmation: %+v", lease)
			}
			host.mu.Lock()
			raw := append([]byte(nil), host.values[autoDegradationNamespace+"/"+autoRecordKey(7)]...)
			host.mu.Unlock()
			var fields map[string]any
			if json.Unmarshal(raw, &fields) != nil || fields["reasoning_effort"] != "xhigh" {
				t.Fatal("lease did not durably store reasoning_effort")
			}
			result := degradationAccountResult{AccountID: 7, Status: "ok"}
			first := nextAutoDegradationRecord(tr.cfg, lease, result, now.Add(autoDegradationRetry))
			if !first.BPSEnabled || first.Consecutive != 1 {
				t.Fatal("first answer after restart combined with the old effort confirmation")
			}
			second := nextAutoDegradationRecord(tr.cfg, first, result, now.Add(2*autoDegradationRetry))
			if second.BPSEnabled || !second.Decided || second.Consecutive != 2 {
				t.Fatal("two new answers after restart did not switch routing")
			}
		})
	}
}

func TestAutoDegradationEffortChangeReschedulesDistantRecord(t *testing.T) {
	for _, previous := range []string{"", "low"} {
		t.Run(previous, func(t *testing.T) {
			tr, host, calls := newAutoDetectionTestTransport(t)
			tr.cfg.NativeTimezoneByIP = false
			host.fakeHost.accounts = []*pluginv1.AccountInfo{{Id: 7, Schedulable: true}}
			old := autoDegradationRecord{AccountID: 7, SelectionBase: autoSelectionBase(tr.cfg),
				Model: degradationModel(tr.cfg), ReasoningEffort: previous, Status: "degraded", Decided: true,
				PendingStatus: "degraded", Consecutive: 1, CheckedAt: time.Now().Add(-time.Minute), NextCheckAt: time.Now().Add(time.Hour)}
			var fields map[string]any
			if err := json.Unmarshal(protocol.JSONBytes(old), &fields); err != nil {
				t.Fatal(err)
			}
			if previous == "" {
				delete(fields, "reasoning_effort") // Legacy records had no effort field.
			}
			host.mu.Lock()
			host.values[autoDegradationNamespace+"/"+autoDegradationIndexKey] = protocol.JSONBytes([]int64{7})
			host.values[autoDegradationNamespace+"/"+autoRecordKey(7)] = protocol.JSONBytes(fields)
			host.mu.Unlock()
			tr.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				raw := protocol.JSONBytes(map[string]any{"output_text": "iPhone 16"})
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
			})}
			tr.mu.Lock()
			tr.bindAutoDegradation(host)
			tr.mu.Unlock()
			waitFor := func(condition func() bool) {
				t.Helper()
				deadline := time.Now().Add(3 * time.Second)
				for !condition() {
					if time.Now().After(deadline) {
						t.Fatal("effort migration scheduling did not settle")
					}
					time.Sleep(time.Millisecond)
				}
			}
			waitFor(func() bool {
				tr.autoDegradation.mu.Lock()
				defer tr.autoDegradation.mu.Unlock()
				return tr.autoDegradation.loaded && !tr.autoDegradation.nextRun.IsZero()
			})
			tr.autoDegradation.mu.Lock()
			nextRun := tr.autoDegradation.nextRun
			tr.autoDegradation.firstDue[7] = time.Now().Add(-time.Second)
			wake := tr.autoDegradation.wake
			tr.autoDegradation.mu.Unlock()
			if nextRun.After(time.Now().Add(31 * time.Second)) {
				t.Fatal("legacy effort retained its distant deadline")
			}
			if route, err := tr.automaticBPSSelection(tr.cfg, 7); err != nil || route != old.BPSEnabled {
				t.Fatal("loading legacy effort discarded the decided route")
			}
			select {
			case wake <- struct{}{}:
			default:
			}
			waitFor(func() bool {
				tr.autoDegradation.mu.Lock()
				defer tr.autoDegradation.mu.Unlock()
				record := tr.autoDegradation.records[7]
				return calls.Load() == 1 && !tr.autoDegradation.running && !record.InFlight && record.ReasoningEffort == "xhigh"
			})
			tr.autoDegradation.mu.Lock()
			record := tr.autoDegradation.records[7]
			tr.autoDegradation.mu.Unlock()
			if !record.Decided || record.BPSEnabled != old.BPSEnabled || record.Consecutive != 1 || record.PendingStatus != "degraded" {
				t.Fatalf("rescheduled query combined different effort confirmations: %+v", record)
			}
		})
	}
}
