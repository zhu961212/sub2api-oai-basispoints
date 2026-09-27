package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestAutoDegradationTimezoneChangesDoNotCombineConfirmations(t *testing.T) {
	for _, previous := range []bool{false, true} {
		for _, status := range []string{"degraded", "ok"} {
			t.Run(strconv.FormatBool(previous)+"_"+status, func(t *testing.T) {
				cfg := protocol.DefaultConfig()
				cfg.NativeTimezoneByIP = !previous
				now := time.Now()
				old := autoDegradationRecord{AccountID: 7, SelectionBase: autoSelectionBase(cfg),
					Model: cfg.DegradationCheckModel, NativeTimezoneByIP: previous,
					Decided: true, BPSEnabled: status == "ok", PendingStatus: status, Consecutive: 1, CheckedAt: now}
				result := degradationAccountResult{AccountID: 7, Status: status}
				first := nextAutoDegradationRecord(cfg, old, result, now.Add(autoDegradationRetry))
				if !first.Decided || first.BPSEnabled != old.BPSEnabled || first.Consecutive != 1 ||
					first.PendingStatus != status || first.NativeTimezoneByIP != cfg.NativeTimezoneByIP {
					t.Fatalf("timezone change combined confirmations or discarded a route: %+v", first)
				}
				second := nextAutoDegradationRecord(cfg, first, result, now.Add(2*autoDegradationRetry))
				if second.BPSEnabled != (status == "degraded") || second.Consecutive != 2 || second.PendingStatus != "" {
					t.Fatal("two confirmations with the new timezone setting did not switch routing")
				}
			})
		}
	}
}

func TestAutoDegradationTimezoneConfigCancelsAndRevisesWithoutDroppingRoute(t *testing.T) {
	for _, previous := range []bool{false, true} {
		t.Run(strconv.FormatBool(previous), func(t *testing.T) {
			tr, _, _ := newAutoDetectionTestTransport(t)
			old := tr.cfg.Clone()
			old.NativeTimezoneByIP = previous
			tr.cfg = old
			tr.autoDegradation.records[7] = autoDegradationRecord{AccountID: 7,
				SelectionBase: autoSelectionBase(old), NativeTimezoneByIP: previous, Decided: true, BPSEnabled: true}
			probe, cancel := context.WithCancel(context.Background())
			defer cancel()
			tr.autoDegradation.probeCancel = cancel
			tr.autoDegradation.revision = 11
			tr.autoDegradation.firstDue[7] = time.Now().Add(time.Hour)
			tr.autoDegradation.nextRun = time.Now().Add(time.Hour)
			next := old.Clone()
			next.NativeTimezoneByIP = !previous
			tr.mu.Lock()
			tr.configureAutoDegradation(old, next)
			tr.cfg = next
			tr.mu.Unlock()
			if probe.Err() != context.Canceled || tr.autoDegradation.revision != 12 ||
				len(tr.autoDegradation.firstDue) != 0 || !tr.autoDegradation.nextRun.IsZero() {
				t.Fatal("timezone change did not cancel and invalidate old scheduling")
			}
			select {
			case <-tr.autoDegradation.wake:
			default:
				t.Fatal("timezone change did not wake the scheduler")
			}
			if route, err := tr.automaticBPSSelection(next, 7); err != nil || !route {
				t.Fatal("timezone change discarded the previously confirmed route")
			}
			tr.mu.Lock()
			tr.configureAutoDegradation(next, next)
			tr.mu.Unlock()
			if tr.autoDegradation.revision != 12 {
				t.Fatal("unchanged timezone setting invalidated the configuration")
			}
		})
	}
}

func TestAutoDegradationTimezoneInterruptedLeaseRestoresWithoutOldConfirmation(t *testing.T) {
	for _, previous := range []bool{false, true} {
		t.Run(strconv.FormatBool(previous), func(t *testing.T) {
			tr, host, _ := newAutoDetectionTestTransport(t)
			tr.cfg.NativeTimezoneByIP = !previous
			now := time.Now()
			tr.autoDegradation.records[7] = autoDegradationRecord{AccountID: 7, SelectionBase: autoSelectionBase(tr.cfg),
				Model: tr.cfg.DegradationCheckModel, NativeTimezoneByIP: previous,
				Decided: true, BPSEnabled: true, PendingStatus: "ok", Consecutive: 1, CheckedAt: now}
			host.mu.Lock()
			host.values[autoDegradationNamespace+"/"+autoDegradationIndexKey] = protocol.JSONBytes([]int64{7})
			host.mu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tr.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
				if request.URL.String() == nativeTimezoneLookupURL {
					return nativeTimezoneResponse("UTC"), nil
				}
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
				lease.NativeTimezoneByIP != tr.cfg.NativeTimezoneByIP || !lease.Decided || !lease.BPSEnabled {
				t.Fatalf("restored timezone lease lost routing or retained stale confirmation: %+v", lease)
			}
			first := nextAutoDegradationRecord(tr.cfg, lease, degradationAccountResult{AccountID: 7, Status: "ok"}, now.Add(autoDegradationRetry))
			if !first.BPSEnabled || first.Consecutive != 1 {
				t.Fatal("first answer after restart combined with the old timezone confirmation")
			}
		})
	}
}

func TestAutoDegradationTimezoneLegacyRecordsDefaultOff(t *testing.T) {
	var legacy autoDegradationRecord
	if err := json.Unmarshal(protocol.JSONBytes(map[string]any{"account_id": 7, "model": "gpt-5.4"}), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.NativeTimezoneByIP {
		t.Fatal("old persisted records must default to timezone lookup disabled")
	}
	for _, enabled := range []bool{false, true} {
		legacy.NativeTimezoneByIP = enabled
		var fields map[string]any
		if err := json.Unmarshal(protocol.JSONBytes(legacy), &fields); err != nil {
			t.Fatal(err)
		}
		if value, present := fields["native_timezone_by_ip"]; !present || value != enabled {
			t.Fatal("timezone setting was not explicitly persisted")
		}
	}
}

func TestAutoDegradationTimezoneChangeReschedulesDistantRecord(t *testing.T) {
	for _, previous := range []bool{false, true} {
		t.Run(strconv.FormatBool(previous), func(t *testing.T) {
			tr, host, calls := newAutoDetectionTestTransport(t)
			tr.cfg.NativeTimezoneByIP = !previous
			host.fakeHost.accounts = []*pluginv1.AccountInfo{{Id: 7, Schedulable: true}}
			old := autoDegradationRecord{AccountID: 7, SelectionBase: autoSelectionBase(tr.cfg),
				Model: tr.cfg.DegradationCheckModel, NativeTimezoneByIP: previous, Status: "degraded",
				PendingStatus: "degraded", Consecutive: 1, CheckedAt: time.Now().Add(-time.Minute), NextCheckAt: time.Now().Add(time.Hour)}
			host.mu.Lock()
			host.values[autoDegradationNamespace+"/"+autoDegradationIndexKey] = protocol.JSONBytes([]int64{7})
			host.values[autoDegradationNamespace+"/"+autoRecordKey(7)] = protocol.JSONBytes(old)
			host.mu.Unlock()
			tr.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
				if request.URL.String() == nativeTimezoneLookupURL {
					return nativeTimezoneResponse("UTC"), nil
				}
				calls.Add(1)
				return nativeTimezoneResponse(string(protocol.JSONBytes(map[string]any{"output_text": "iPhone 16"}))), nil
			})}
			tr.mu.Lock()
			tr.bindAutoDegradation(host)
			tr.mu.Unlock()
			waitFor := func(condition func() bool) {
				t.Helper()
				deadline := time.Now().Add(3 * time.Second)
				for !condition() {
					if time.Now().After(deadline) {
						t.Fatal("timezone scheduling did not settle")
					}
					time.Sleep(time.Millisecond)
				}
			}
			waitFor(func() bool {
				tr.autoDegradation.mu.Lock()
				defer tr.autoDegradation.mu.Unlock()
				return tr.autoDegradation.loaded
			})
			tr.autoDegradation.mu.Lock()
			tr.autoDegradation.firstDue[7] = time.Now().Add(-time.Second)
			wake := tr.autoDegradation.wake
			tr.autoDegradation.mu.Unlock()
			select {
			case wake <- struct{}{}:
			default:
			}
			waitFor(func() bool {
				tr.autoDegradation.mu.Lock()
				defer tr.autoDegradation.mu.Unlock()
				record := tr.autoDegradation.records[7]
				return calls.Load() == 1 && !tr.autoDegradation.running && !record.InFlight && record.NativeTimezoneByIP == tr.cfg.NativeTimezoneByIP
			})
			tr.autoDegradation.mu.Lock()
			record := tr.autoDegradation.records[7]
			tr.autoDegradation.mu.Unlock()
			if record.Decided || record.BPSEnabled || record.Consecutive != 1 {
				t.Fatal("rescheduled query switched after one answer with changed timezone configuration")
			}
		})
	}
}
