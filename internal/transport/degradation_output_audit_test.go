package transport

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestDiagnosticAnswerRejectsConflictingOrNonAssistantOutput(t *testing.T) {
	for _, fixture := range []struct {
		name string
		body string
	}{
		{"echoed_user_message", `{"output":[{"type":"message","role":"user","content":[{"type":"output_text","text":"iPhone 16"}]}]}`},
		{"tool_payload", `{"output":[{"type":"function_call","content":[{"type":"output_text","text":"iPhone 16"}]}]}`},
		{"chat_user_message", `{"choices":[{"message":{"role":"user","content":"iPhone 16"},"finish_reason":"stop"}]}`},
		{"chat_nontext_content", `{"choices":[{"message":{"role":"assistant","content":[{"type":"tool_result","text":"iPhone 16"}]},"finish_reason":"stop"}]}`},
		{"conflicting_summary", `{"output_text":"iPhone 17","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"iPhone 16"}]}]}`},
		{"conflicting_choices", `{"choices":[{"message":{"role":"assistant","content":"iPhone 17"},"finish_reason":"stop"},{"message":{"role":"assistant","content":"iPhone 16"},"finish_reason":"stop"}]}`},
		{"incomplete_secondary_choice", `{"choices":[{"message":{"content":"iPhone 17"},"finish_reason":"stop"},{"message":{"content":"iPhone 17"},"finish_reason":"length"}]}`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			tr, host, _ := newDiagnosticTransport(t)
			client := &http.Client{Transport: degradationRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(fixture.body))}, nil
			})}
			status, _, err := tr.checkDegradationAccount(context.Background(), protocol.DefaultConfig(), host, client, 7, protocol.DefaultModelID)
			if status != "error" || err == nil {
				t.Fatalf("non-answer or conflicting output became a conclusive account verdict: status=%s err=%v", status, err)
			}
		})
	}
}

func TestDiagnosticAnswerIgnoresToolAndUserTextBesideAssistant(t *testing.T) {
	body := []byte(`{"output":[{"type":"reasoning","content":[{"type":"output_text","text":"iPhone 16"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"iPhone 17"}]},{"type":"message","role":"user","content":[{"type":"output_text","text":"iPhone 15"}]}]}`)
	answer, err := degradationAnswer(body, "application/json")
	if err != nil || answer != "iPhone 17" {
		t.Fatalf("diagnostic did not isolate assistant answer: answer=%q err=%v", answer, err)
	}
}

func TestDiagnosticAnswerDeduplicatesEquivalentResponseFormats(t *testing.T) {
	response := map[string]any{
		"output_text": "iPhone 17",
		"output":      []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "iPhone 17"}}}},
		"choices":     []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "iPhone 17"}}},
	}
	answer, err := degradationAnswer(protocol.JSONBytes(response), "application/json")
	if err != nil || answer != "iPhone 17" {
		t.Fatalf("equivalent representations changed displayed answer: answer=%q err=%v", answer, err)
	}
}

func TestDiagnosticTaskConflictingAnswersNeverSelectAccounts(t *testing.T) {
	for _, ids := range [][]int64{{7}, {7, 9}} {
		name := "single"
		if len(ids) > 1 {
			name = "bulk"
		}
		t.Run(name, func(t *testing.T) {
			tr, _, calls := newDiagnosticTransport(t)
			production := string(protocol.JSONBytes(tr.cfg))
			response := protocol.JSONBytes(map[string]any{
				"output_text": "iPhone 17",
				"output":      []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "iPhone 16"}}}},
			})
			tr.client.Transport = degradationRoundTripper(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(response)))}, nil
			})
			diagnosticSave(t, tr, diagnosticPrepare(tr, name, ids...))
			if calls.Load() != 0 {
				t.Fatal("prepare unexpectedly ran account checks")
			}
			diagnosticSave(t, tr, diagnosticCommit(tr, name))
			job := waitDiagnosticJob(t, tr, name)
			check := degradationScopedResult(t, job.Result)
			if job.State != "completed" || !check.Completed || len(check.Results) != len(ids) || len(check.DegradedAccountIDs) != 0 || calls.Load() != int32(len(ids)) {
				t.Fatalf("task returned invalid completion or selection: state=%s check=%+v calls=%d", job.State, check, calls.Load())
			}
			for index, row := range check.Results {
				if row.AccountID != ids[index] || row.Status != "error" || row.Error == "" {
					t.Fatalf("ambiguous answer produced conclusive account verdict: %+v", row)
				}
			}
			if string(protocol.JSONBytes(tr.cfg)) != production {
				t.Fatal("diagnostic task changed saved routing configuration")
			}
		})
	}
}
