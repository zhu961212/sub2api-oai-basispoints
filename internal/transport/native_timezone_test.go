package transport

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func timezoneLocation(t *testing.T, zone string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	return location
}

func timezoneInput(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	var input []json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(object["input"], &input); err != nil {
		t.Fatal(err)
	}
	return input
}

func timezoneText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var message struct{ Content json.RawMessage }
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatal(err)
	}
	var text string
	if json.Unmarshal(message.Content, &text) == nil {
		return text
	}
	var parts []struct {
		Type string
		Text string
	}
	if err := json.Unmarshal(message.Content, &parts); err != nil {
		t.Fatal(err)
	}
	for _, part := range parts {
		if part.Type == "input_text" {
			return part.Text
		}
	}
	return ""
}

func TestNativeTimezoneBodyUpdatesOnlyLatestEnvironment(t *testing.T) {
	old := "<environment_context>\n  <cwd>E:/project</cwd>\n  <current_date>2020-01-01</current_date>\n  <timezone>Asia/Shanghai</timezone>\n  <shell>powershell</shell>\n</environment_context>"
	last := strings.ReplaceAll(old, "2020-01-01", "2026-09-27")
	tool := protocol.JSONBytes(map[string]any{"type": "function_call_output", "call_id": "call1", "output": last})
	task := nativeTimezoneMessage("Explain <timezone> in this code without changing it")
	input := []json.RawMessage{nativeTimezoneMessage(old), tool, nativeTimezoneMessage(last), task}
	body := protocol.JSONBytes(map[string]any{"input": input, "model": "gpt-5.4", "instructions": last, "custom_large_number": json.RawMessage("9007199254740993123456789")})
	now := time.Date(2026, 9, 27, 1, 30, 0, 0, time.UTC)
	got, ok := nativeTimezoneBody(body, timezoneLocation(t, "America/Los_Angeles"), now)
	if !ok {
		t.Fatal("valid context rejected")
	}
	result := timezoneInput(t, got)
	if len(result) != len(input) {
		t.Fatalf("context count changed: %d", len(result))
	}
	for _, i := range []int{0, 1, 3} {
		var wantValue, gotValue any
		_ = json.Unmarshal(input[i], &wantValue)
		_ = json.Unmarshal(result[i], &gotValue)
		if !reflect.DeepEqual(wantValue, gotValue) {
			t.Fatalf("history/tool/task changed at %d", i)
		}
	}
	content := timezoneText(t, result[2])
	for _, want := range []string{"<current_date>2026-09-26</current_date>", "<timezone>America/Los_Angeles</timezone>", "<cwd>E:/project</cwd>", "<shell>powershell</shell>"} {
		if !strings.Contains(content, want) {
			t.Fatalf("missing %s in %s", want, content)
		}
	}
	var output map[string]json.RawMessage
	_ = json.Unmarshal(got, &output)
	if string(output["custom_large_number"]) != "9007199254740993123456789" {
		t.Fatal("number precision lost")
	}
	var instructions string
	_ = json.Unmarshal(output["instructions"], &instructions)
	if instructions != last {
		t.Fatal("instructions modified")
	}
}

func TestNativeTimezoneBodyLocalDateUsesIANASeasonalRules(t *testing.T) {
	cases := []struct{ name, zone, utc, date string }{
		{"summer", "America/Los_Angeles", "2026-07-01T07:30:00Z", "2026-07-01"},
		{"winter", "America/Los_Angeles", "2026-01-01T07:30:00Z", "2025-12-31"},
		{"fractional", "Asia/Kathmandu", "2026-09-27T18:30:00Z", "2026-09-28"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now, _ := time.Parse(time.RFC3339, tc.utc)
			source := protocol.JSONBytes(map[string]any{"input": "task", "stream": true})
			got, ok := nativeTimezoneBody(source, timezoneLocation(t, tc.zone), now)
			if !ok {
				t.Fatal("rewrite rejected")
			}
			input := timezoneInput(t, got)
			if len(input) != 2 || timezoneText(t, input[1]) != "task" {
				t.Fatal("plain input changed")
			}
			context := timezoneText(t, input[0])
			if !strings.Contains(context, "<current_date>"+tc.date+"</current_date>") || !strings.Contains(context, "<timezone>"+tc.zone+"</timezone>") {
				t.Fatal(context)
			}
			again, ok := nativeTimezoneBody(got, timezoneLocation(t, tc.zone), now)
			if !ok || !bytes.Equal(got, again) {
				t.Fatal("repeat application changed payload")
			}
		})
	}
}

func TestNativeTimezoneBodyDoesNotRewriteEmbeddedExamples(t *testing.T) {
	context := "<environment_context>\n<timezone>Asia/Shanghai</timezone>\n</environment_context>"
	examples := []string{"Please translate: \n" + context, "~~~xml\n" + context + "\n~~~", context + "\nThe above is an example."}
	for _, example := range examples {
		source := protocol.JSONBytes(map[string]any{"input": []json.RawMessage{nativeTimezoneMessage(example)}})
		got, ok := nativeTimezoneBody(source, time.UTC, time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC))
		if !ok {
			t.Fatal("rewrite rejected")
		}
		input := timezoneInput(t, got)
		if len(input) != 2 || timezoneText(t, input[1]) != example {
			t.Fatal("embedded example rewritten")
		}
	}
}

func TestNativeTimezoneEnvironmentSupportsIncrementalContext(t *testing.T) {
	for _, source := range []string{
		"<environment_context>\n  <cwd>E:/project</cwd>\n</environment_context>",
		"<environment_context>\r\n  <current_date>2020-01-01</current_date>\r\n</environment_context>",
		"<environment_context>\n  <timezone>Etc/UTC</timezone>\n</environment_context>",
		"<environment_context>\n  <current_date status=\"unavailable\" />\n</environment_context>",
	} {
		got, ok := nativeTimezoneEnvironment(source, "Europe/London", "2026-09-27")
		if !ok || strings.Count(got, "<current_date>") != 1 || strings.Count(got, "<timezone>") != 1 || !strings.Contains(got, "Europe/London") {
			t.Fatalf("%v: %s", ok, got)
		}
	}
}

func TestNativeTimezoneRespectsContentKindAnnotations(t *testing.T) {
	context := "<environment_context>\n<timezone>Asia/Shanghai</timezone>\n</environment_context>"
	for _, annotated := range []bool{true, false} {
		kind := "user_input"
		if annotated {
			kind = "environments.environment_context"
		}
		metadata := map[string]any{"content_item_kinds": []string{"user_input", kind}, "turn_id": "existing-turn"}
		source := protocol.JSONBytes(map[string]any{"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]string{{"type": "input_text", "text": "actual task"}, {"type": "input_text", "text": context}},
			"internal_chat_message_metadata_passthrough": metadata,
		}}})
		got, ok := nativeTimezoneBody(source, time.UTC, time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC))
		if !ok {
			t.Fatal("rewrite rejected")
		}
		input := timezoneInput(t, got)
		expectedCount := 2
		if annotated {
			expectedCount = 1
		}
		if len(input) != expectedCount {
			t.Fatalf("kind=%s count=%d", kind, len(input))
		}
		var last map[string]json.RawMessage
		_ = json.Unmarshal(input[len(input)-1], &last)
		var actualMeta any
		_ = json.Unmarshal(last["internal_chat_message_metadata_passthrough"], &actualMeta)
		var originalMeta any
		_ = json.Unmarshal(protocol.JSONBytes(metadata), &originalMeta)
		if !reflect.DeepEqual(actualMeta, originalMeta) {
			t.Fatal("per-part annotations changed")
		}
		var parts []map[string]string
		_ = json.Unmarshal(last["content"], &parts)
		if len(parts) != 2 || parts[0]["text"] != "actual task" {
			t.Fatal("content alignment changed")
		}
		if annotated {
			if !strings.Contains(parts[1]["text"], "<timezone>UTC</timezone>") {
				t.Fatal("annotated environment not updated")
			}
		} else {
			if parts[1]["text"] != context {
				t.Fatal("annotated user input rewritten")
			}
			var added map[string]json.RawMessage
			_ = json.Unmarshal(input[0], &added)
			if !nativeTimezoneEnvironmentKind(added, 0) {
				t.Fatal("new context annotation missing")
			}
		}
	}
}

func TestNativeTimezoneBodyFailsOpenForUnsupportedInput(t *testing.T) {
	for _, source := range []string{"null", "[]", "{", "{}", "{\"input\":null}", "{\"input\":42}", "{\"input\":{}}"} {
		got, ok := nativeTimezoneBody([]byte(source), time.UTC, time.Now())
		if ok || string(got) != source {
			t.Fatalf("unsupported input changed: %s", source)
		}
	}
}

func TestNativeTimezoneLeavesToolContinuationOrderIntact(t *testing.T) {
	call := protocol.JSONBytes(map[string]any{"type": "function_call", "call_id": "call1", "name": "get_time", "arguments": "unchanged timezone example"})
	result := protocol.JSONBytes(map[string]any{"type": "function_call_output", "call_id": "call1", "output": "tool output"})
	source := protocol.JSONBytes(map[string]any{"input": []json.RawMessage{call, result}})
	got, ok := nativeTimezoneBody(source, time.UTC, time.Now())
	if !ok {
		t.Fatal("rewrite rejected")
	}
	input := timezoneInput(t, got)
	if len(input) != 3 || !bytes.Equal(input[0], call) || !bytes.Equal(input[1], result) {
		t.Fatal("tool call/result reordered")
	}
	if !strings.Contains(timezoneText(t, input[2]), "<environment_context>") {
		t.Fatal("context missing")
	}
}

func TestNativeTimezoneEndpointIsStrictlyScoped(t *testing.T) {
	for _, tc := range []struct {
		url, host, method string
		want              bool
	}{
		{nativeDegradationResponsesURL, "", "POST", true},
		{nativeDegradationResponsesURL + "/compact", "chatgpt.com", "POST", true},
		{nativeDegradationResponsesURL, "other.example", "POST", false},
		{nativeDegradationResponsesURL, "", "GET", false},
		{"http://chatgpt.com/backend-api/codex/responses", "", "POST", false},
		{"https://chatgpt.com:8443/backend-api/codex/responses", "", "POST", false},
		{"https://gateway.example/backend-api/codex/responses", "chatgpt.com", "POST", false},
		{"https://chatgpt.com.evil.test/backend-api/codex/responses", "", "POST", false},
		{"https://chatgpt.com/backend-api/codex/responses/other", "", "POST", false},
		{"https://chatgpt.com/backend-api/codex/%72esponses", "", "POST", false},
		{"https://bps.openai.com/basispoints/api/responses", "", "POST", false},
	} {
		req, err := http.NewRequest(tc.method, tc.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = tc.host
		if got := isNativeTimezoneRequest(req); got != tc.want {
			t.Fatalf("%s %s host=%s: got %v", tc.method, tc.url, tc.host, got)
		}
	}
}
