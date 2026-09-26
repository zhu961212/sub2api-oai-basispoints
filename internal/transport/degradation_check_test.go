package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestDegradationModelFollowsEnabledModel(t *testing.T) {
	if got := degradationModel(protocol.Config{EnabledModels: []string{"gpt-5.6-sol"}}); got != "gpt-5.6-sol" {
		t.Fatalf("degradationModel = %q, want configured model", got)
	}
	if got := degradationModel(protocol.Config{EnabledModels: []string{}}); got != protocol.DefaultModelID {
		t.Fatalf("degradationModel with empty selection = %q, want %q", got, protocol.DefaultModelID)
	}
}

func TestDegradationAnswerUsesConfiguredHeuristic(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
		err  bool
	}{
		{name: "exact responses", body: `{"output":[{"type":"message","content":[{"type":"output_text","text":"苹果17"}]}]}`, want: "苹果17"},
		{name: "explanation is preserved", body: `{"output":[{"type":"message","content":[{"type":"output_text","text":"苹果17，因为这是最新型号"}]}]}`, want: "苹果17，因为这是最新型号"},
		{name: "iphone series answer", body: `{"output":[{"type":"message","content":[{"type":"output_text","text":"我知道的是 iPhone 17 系列"}]}]}`, want: "我知道的是 iPhone 17 系列"},
		{name: "chat completions fallback", body: `{"choices":[{"message":{"content":"苹果16"}}]}`, want: "苹果16"},
		{name: "missing text", body: `{"output":[]}`, err: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := degradationAnswer([]byte(test.body), "application/json")
			if test.err {
				if err == nil {
					t.Fatal("missing output text was accepted")
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("answer = %q, err = %v; want %q", got, err, test.want)
			}
		})
	}
	for _, test := range []struct {
		answer string
		want   bool
	}{
		{answer: "苹果17", want: true},
		{answer: "苹果17。", want: true},
		{answer: "我知道的是 iPhone 17 系列，包括 Pro Max", want: true},
		{answer: "Apple 17", want: true},
		{answer: "苹果16", want: false},
		{answer: "我无法确定", want: false},
	} {
		if got := isExpectedDegradationAnswer(test.answer); got != test.want {
			t.Errorf("isExpectedDegradationAnswer(%q) = %v, want %v", test.answer, got, test.want)
		}
	}
}

func TestDegradationCheckReturnsResultsAndOnlySelectsWrongAnswers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method; got != http.MethodPost {
			t.Errorf("method = %s, want POST", got)
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) == 0 {
			t.Error("empty check body")
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("invalid check body: %v", err)
		} else {
			if !strings.Contains(string(protocol.JSONBytes(payload["input"])), degradationCheckPrompt) || payload["stream"] != true || payload["reasoning_effort"] != "low" || payload["model_selection"] != "explicit" {
				t.Errorf("unexpected check payload: %#v", payload)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		answer := "苹果17"
		if r.Header.Get("ChatGPT-Account-ID") == "acct-9" {
			answer = "苹果16"
		}
		_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"` + answer + `"}]}]}`))
	}))
	defer upstream.Close()

	transport := New()
	defer transport.Shutdown()
	host := &fakeHost{
		accounts: []*pluginv1.AccountInfo{
			{Id: 7, Name: "healthy", Schedulable: true},
			{Id: 9, Name: "degraded", Schedulable: true},
			{Id: 11, Name: "paused", Schedulable: false},
		},
		tokenFor: map[int64]string{
			7:  token(t, "acct-7"),
			9:  token(t, "acct-9"),
			11: token(t, "acct-11"),
		},
	}
	transport.mu.Lock()
	transport.host = &degradationTestHost{fakeHost: host}
	transport.mu.Unlock()
	cfg := protocol.DefaultConfig()
	cfg.ResponsesURL = upstream.URL
	cfg.DegradationCheck = true
	check, err := transport.runDegradationCheck(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	status := struct {
		DegradationCheck degradationCheckResult
	}{DegradationCheck: check}
	if !status.DegradationCheck.Completed {
		t.Fatal("check was not marked completed")
	}
	if len(status.DegradationCheck.DegradedAccountIDs) != 1 || status.DegradationCheck.DegradedAccountIDs[0] != 9 {
		t.Fatalf("degraded IDs = %#v, want [9]", status.DegradationCheck.DegradedAccountIDs)
	}
	if len(status.DegradationCheck.Results) != 3 {
		t.Fatalf("results = %#v, want three accounts", status.DegradationCheck.Results)
	}
	if status.DegradationCheck.Results[0].Status != "ok" || status.DegradationCheck.Results[1].Status != "degraded" || status.DegradationCheck.Results[2].Status != "skipped" {
		t.Fatalf("unexpected result statuses: %#v", status.DegradationCheck.Results)
	}
}
