package transport

import (
	"context"
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
)

func TestDegradationRunnerKeepsEachAccountResultIsolated(t *testing.T) {
	fixtures := []struct {
		id           int64
		wantStatus   string
		wantAnswer   string
		response     map[string]any
		httpStatus   int
		stream       bool
		noCredential bool
		unavailable  bool
	}{
		{id: 7, wantStatus: "ok", wantAnswer: "苹果17", response: map[string]any{"output_text": "苹果17"}},
		{id: 9, wantStatus: "degraded", wantAnswer: "苹果16", response: map[string]any{"output_text": "苹果16"}},
		{id: 11, wantStatus: "error", httpStatus: http.StatusTooManyRequests, response: map[string]any{"output_text": "苹果17"}},
		{id: 13, wantStatus: "error", response: map[string]any{"error": map[string]any{"code": "unknown_failure"}, "response": map[string]any{"output_text": "苹果16"}}},
		{id: 15, wantStatus: "error", stream: true, response: map[string]any{"type": "response.completed", "error": map[string]any{"code": "unknown_failure"}, "response": map[string]any{"status": "completed", "output_text": "苹果17"}}},
		{id: 17, wantStatus: "error", noCredential: true},
		{id: 19, wantStatus: "skipped", unavailable: true},
		{id: 21, wantStatus: "error", wantAnswer: "可能是 iPhone 17", response: map[string]any{"output_text": "可能是 iPhone 17"}},
		{id: 23, wantStatus: "error", wantAnswer: "我不知道", response: map[string]any{"output_text": "我不知道"}},
		{id: 25, wantStatus: "error", wantAnswer: "不是 iPhone 17，是 iPhone 16", response: map[string]any{"output_text": "不是 iPhone 17，是 iPhone 16"}},
		{id: 27, wantStatus: "error", wantAnswer: "iPhone 170", response: map[string]any{"output_text": "iPhone 170"}},
		{id: 29, wantStatus: "error", wantAnswer: "pineapple17", response: map[string]any{"output_text": "pineapple17"}},
		{id: 31, wantStatus: "degraded", wantAnswer: "iPhone 18", response: map[string]any{"output_text": "iPhone 18"}},
		{id: 33, wantStatus: "error", response: map[string]any{"success": false, "response": map[string]any{"output_text": "苹果17"}}},
		{id: 35, wantStatus: "error", response: map[string]any{"ok": false, "output_text": "苹果16"}},
		{id: 37, wantStatus: "error", response: map[string]any{"status_code": 500, "response": map[string]any{"output_text": "苹果17"}}},
		{id: 39, wantStatus: "error", response: map[string]any{"http_status": "500", "output_text": "苹果16"}},
		{id: 41, wantStatus: "error", response: map[string]any{"status": "completed", "response": map[string]any{"success": false, "output_text": "苹果17"}}},
		{id: 43, wantStatus: "error", response: map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "status": "incomplete", "content": []any{map[string]any{"type": "output_text", "text": "苹果17"}}}}}},
		{id: 45, wantStatus: "error", wantAnswer: "苹果17\n苹果16", response: map[string]any{"status": "completed", "output": []any{map[string]any{"type": "output_text", "text": "苹果17"}, map[string]any{"type": "output_text", "text": "苹果16"}}}},
	}
	host := &degradationTestHost{fakeHost: &fakeHost{tokenFor: make(map[int64]string)}}
	calls := make(map[int64]*atomic.Int32)
	for _, fixture := range fixtures {
		host.accounts = append(host.accounts, &pluginv1.AccountInfo{Id: fixture.id, Name: "same name", Schedulable: !fixture.unavailable})
		if !fixture.noCredential {
			host.tokenFor[fixture.id] = token(t, fmt.Sprintf("acct-%d", fixture.id))
		}
		calls[fixture.id] = &atomic.Int32{}
	}
	tr := New()
	defer tr.Shutdown()
	tr.host = host
	originalConfig := tr.cfg
	tr.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
		for _, fixture := range fixtures {
			if request.Header.Get("ChatGPT-Account-ID") != fmt.Sprintf("acct-%d", fixture.id) {
				continue
			}
			calls[fixture.id].Add(1)
			if request.Header.Get("Authorization") != "Bearer "+host.tokenFor[fixture.id] {
				t.Error("account ID and credential belong to different accounts")
			}
			status := fixture.httpStatus
			if status == 0 {
				status = http.StatusOK
			}
			body, contentType := string(protocol.JSONBytes(fixture.response)), "application/json"
			if fixture.stream {
				body, contentType = streamData(fixture.response), "text/event-stream"
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		return nil, fmt.Errorf("probe used an unexpected account identity")
	})}
	run := func(index int) {
		fixture := fixtures[index]
		cfg := protocol.DefaultConfig()
		cfg.AccountIDs = []int64{7}
		cfg.ExcludedAccountIDs = []int64{fixture.id}
		cfg.DegradationCheck, cfg.DegradationCheckAccountID = true, fixture.id
		check, err := tr.runDegradationCheck(context.Background(), cfg)
		if err != nil {
			t.Errorf("account %d: check failed: %v", fixture.id, err)
			return
		}
		if !check.Completed || len(check.Results) != 1 {
			t.Errorf("account %d received other account results: %+v", fixture.id, check)
			return
		}
		result := check.Results[0]
		if result.AccountID != fixture.id || result.Status != fixture.wantStatus || result.Answer != fixture.wantAnswer {
			t.Errorf("account %d result: %+v", fixture.id, result)
		}
		if needsError := fixture.wantStatus == "error" || fixture.wantStatus == "skipped"; (result.Error != "") != needsError {
			t.Errorf("account %d error presence disagrees with status: %+v", fixture.id, result)
		}
		if fixture.wantStatus == "degraded" {
			if !reflect.DeepEqual(check.DegradedAccountIDs, []int64{fixture.id}) {
				t.Errorf("account %d selected wrong degraded IDs: %v", fixture.id, check.DegradedAccountIDs)
			}
		} else if len(check.DegradedAccountIDs) != 0 {
			t.Errorf("account %d incorrectly selected degraded IDs: %v", fixture.id, check.DegradedAccountIDs)
		}
	}
	// Repeat against the same transport both sequentially and concurrently so
	// previous verdicts, request headers and per-call configuration cannot leak.
	for index := range fixtures {
		run(index)
	}
	var wg sync.WaitGroup
	for index := range fixtures {
		wg.Go(func() { run(index) })
	}
	wg.Wait()
	resolved := make(map[int64]int)
	for _, id := range host.resolvedAccountIDs {
		resolved[id]++
	}
	for _, fixture := range fixtures {
		wantResolved, wantCalls := 2, int32(2)
		if fixture.unavailable {
			wantResolved = 0
		}
		if fixture.noCredential || fixture.unavailable {
			wantCalls = 0
		}
		if resolved[fixture.id] != wantResolved || calls[fixture.id].Load() != wantCalls {
			t.Errorf("account %d: identity calls=%d want %d; upstream calls=%d want %d", fixture.id, resolved[fixture.id], wantResolved, calls[fixture.id].Load(), wantCalls)
		}
	}
	if !reflect.DeepEqual(tr.cfg, originalConfig) {
		t.Fatal("account tests changed the active configuration")
	}
}
