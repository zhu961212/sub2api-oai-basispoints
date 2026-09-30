package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func legacyEnvironmentRecord(t *testing.T, record autoDegradationRecord, enabled any) []byte {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(protocol.JSONBytes(record), &fields); err != nil {
		t.Fatal(err)
	}
	if enabled != nil {
		fields["native_timezone_by_ip"] = enabled
	}
	return protocol.JSONBytes(fields)
}

func TestAutoDegradationRetiredEnvironmentMigrationPreservesRoutes(t *testing.T) {
	for _, enabled := range []any{nil, false, true} {
		for _, route := range []bool{false, true} {
			t.Run(fmt.Sprintf("legacy=%v/route=%t", enabled, route), func(t *testing.T) {
				cfg := protocol.DefaultConfig()
				now := time.Now().UTC().Truncate(time.Second)
				status := "degraded"
				if route {
					status = "ok"
				}
				old := autoDegradationRecord{AccountID: 7, SelectionBase: autoSelectionBase(cfg), Model: degradationModel(cfg),
					ReasoningEffort: nativeDegradationReasoningEffort, Decided: true, BPSEnabled: route, Status: status,
					PendingStatus: status, Consecutive: 1, InFlight: true, CheckedAt: now, NextCheckAt: now.Add(time.Hour)}
				var migrated autoDegradationRecord
				if err := json.Unmarshal(legacyEnvironmentRecord(t, old, enabled), &migrated); err != nil {
					t.Fatal(err)
				}
				expected := old
				if enabled == true {
					expected.PendingStatus, expected.Consecutive, expected.InFlight, expected.NextCheckAt = "", 0, false, time.Time{}
				}
				if !reflect.DeepEqual(migrated, expected) {
					t.Fatalf("migration changed unrelated state or retained old samples: %+v", migrated)
				}
				saved := protocol.JSONBytes(migrated)
				if bytes.Contains(saved, []byte("native_timezone_by_ip")) {
					t.Fatal("new record persisted the removed feature")
				}
				var restored autoDegradationRecord
				if err := json.Unmarshal(saved, &restored); err != nil || !reflect.DeepEqual(restored, expected) {
					t.Fatalf("migration was not stable across persistence: %+v, %v", restored, err)
				}
				if enabled == true {
					result := degradationAccountResult{AccountID: 7, Status: status}
					first := nextAutoDegradationRecord(cfg, restored, result, now.Add(autoDegradationRetry))
					if !first.Decided || first.BPSEnabled != route || first.Consecutive != 1 || first.PendingStatus != status {
						t.Fatalf("old rewritten sample confirmed a new probe: %+v", first)
					}
					second := nextAutoDegradationRecord(cfg, first, result, now.Add(2*autoDegradationRetry))
					if !second.Decided || second.BPSEnabled == route || second.Consecutive != 2 || second.PendingStatus != "" {
						t.Fatalf("two fresh confirmations did not switch routing: %+v", second)
					}
				}
			})
		}
	}
}

func TestAutoDegradationRetiredEnvironmentReschedulesAndPersists(t *testing.T) {
	tr, host, calls := newAutoDetectionTestTransport(t)
	host.fakeHost.accounts = []*pluginv1.AccountInfo{{Id: 7, Schedulable: true}}
	old := autoDegradationRecord{AccountID: 7, SelectionBase: autoSelectionBase(tr.cfg), Model: degradationModel(tr.cfg),
		ReasoningEffort: nativeDegradationReasoningEffort, Decided: true, BPSEnabled: true, Status: "ok",
		PendingStatus: "ok", Consecutive: 1, InFlight: true, CheckedAt: time.Now().Add(-time.Minute), NextCheckAt: time.Now().Add(time.Hour)}
	host.mu.Lock()
	host.values[autoDegradationNamespace+"/"+autoDegradationIndexKey] = protocol.JSONBytes([]int64{7})
	host.values[autoDegradationNamespace+"/"+autoRecordKey(7)] = legacyEnvironmentRecord(t, old, true)
	host.mu.Unlock()
	tr.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.URL.String() != nativeDegradationResponsesURL {
			t.Errorf("migrated probe attempted external lookup: %s", request.URL)
			return nil, fmt.Errorf("unexpected lookup")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(protocol.JSONBytes(map[string]any{"output_text": "iPhone 17"})))}, nil
	})}
	tr.mu.Lock()
	tr.bindAutoDegradation(host)
	tr.mu.Unlock()
	waitFor := func(condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for !condition() {
			if time.Now().After(deadline) {
				t.Fatal("retired environment migration did not settle")
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
	nextRun, loaded := tr.autoDegradation.nextRun, tr.autoDegradation.records[7]
	tr.autoDegradation.firstDue[7] = time.Now().Add(-time.Second)
	wake := tr.autoDegradation.wake
	tr.autoDegradation.mu.Unlock()
	if nextRun.After(time.Now().Add(31*time.Second)) || loaded.PendingStatus != "" || loaded.Consecutive != 0 || loaded.InFlight {
		t.Fatalf("legacy record kept old deadline/sample: %+v; next=%v", loaded, nextRun)
	}
	if route, err := tr.automaticBPSSelection(tr.cfg, 7); err != nil || !route {
		t.Fatalf("loading changed confirmed route: %t, %v", route, err)
	}
	select {
	case wake <- struct{}{}:
	default:
	}
	waitFor(func() bool {
		tr.autoDegradation.mu.Lock()
		defer tr.autoDegradation.mu.Unlock()
		return calls.Load() == 1 && !tr.autoDegradation.running && !tr.autoDegradation.records[7].InFlight
	})
	restored, err := loadAutoDegradationRecords(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	first := restored[7]
	if !first.Decided || !first.BPSEnabled || first.Consecutive != 1 || first.PendingStatus != "ok" {
		t.Fatalf("first fresh probe combined with retired sample: %+v", first)
	}
	host.mu.Lock()
	saved := append([]byte(nil), host.values[autoDegradationNamespace+"/"+autoRecordKey(7)]...)
	host.mu.Unlock()
	if bytes.Contains(saved, []byte("native_timezone_by_ip")) {
		t.Fatal("fresh persisted result retained the removed feature")
	}
	second := nextAutoDegradationRecord(tr.cfg, first, degradationAccountResult{AccountID: 7, Status: "ok"}, first.CheckedAt.Add(autoDegradationRetry))
	if !second.Decided || second.BPSEnabled || second.Consecutive != 2 {
		t.Fatalf("second fresh confirmation did not switch routing: %+v", second)
	}
}

func TestAutoDegradationRetiredEnvironmentRejectsCorruptCounters(t *testing.T) {
	for _, count := range []int{-1, 3} {
		var record autoDegradationRecord
		if err := json.Unmarshal(legacyEnvironmentRecord(t, autoDegradationRecord{Consecutive: count}, true), &record); err == nil {
			t.Fatalf("migration sanitized corrupt confirmation count %d", count)
		}
	}
}
