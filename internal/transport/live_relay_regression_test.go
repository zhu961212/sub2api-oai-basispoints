package transport

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// The probe collects generated calls and supplies synthetic outputs. It never
// invokes generated tools. A mandatory account proxy carries at most 3 HTTP
// requests, including any automatic correction. Logs contain no raw bodies.
type liveRelayBudgetTransport struct {
	base          http.RoundTripper
	mu            sync.Mutex
	sent          int
	networkReason string
}

func (r *liveRelayBudgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	if r.sent >= 3 {
		r.mu.Unlock()
		return nil, errors.New("live relay request budget exhausted")
	}
	r.sent++
	r.mu.Unlock()
	response, err := r.base.RoundTrip(req)
	if err != nil {
		category := "network_failure"
		message := strings.ToLower(err.Error())
		switch {
		case strings.Contains(message, "socks") || strings.Contains(message, "proxy"):
			category = "proxy_failure"
		case strings.Contains(message, "timeout") || strings.Contains(message, "deadline"):
			category = "network_timeout"
		case strings.Contains(message, "tls") || strings.Contains(message, "certificate"):
			category = "tls_failure"
		}
		r.mu.Lock()
		r.networkReason = category
		r.mu.Unlock()
	}
	return response, err
}

func (r *liveRelayBudgetTransport) snapshot() (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent, r.networkReason
}

func TestLiveRelayRegressionLifecycle(t *testing.T) {
	if os.Getenv("BASISPOINTS_LIVE_RELAY_REGRESSION") != "1" {
		t.Skip("requires explicit BASISPOINTS_LIVE_RELAY_REGRESSION=1")
	}
	accessToken, accountID, proxyURL := liveCredentials(t)
	if !strings.HasPrefix(proxyURL, "socks5://") {
		t.Fatal("supplied socks5 proxy required; direct egress is disabled")
	}
	transport := New()
	defer transport.Shutdown()
	applyConfig(t, transport, map[string]any{
		"enabled_models": []string{"gpt-6-astra"}, "timeout_seconds": 65,
		"max_response_bytes":      2 << 20,
		"bps_auto_disable_on_403": false, "auto_degradation_enabled": false,
	})
	client, err := transport.clientForProxy(transport.client, proxyURL)
	if err != nil || client == nil || client == transport.client || client.Transport == nil {
		t.Fatal("supplied proxy configuration invalid")
	}
	budget := &liveRelayBudgetTransport{base: client.Transport}
	client.Transport = budget
	stages := []map[string]any{}
	lifecycleOK := false
	startedAt := time.Now().UTC().Format(time.RFC3339)
	t.Cleanup(func() {
		count, reason := budget.snapshot()
		report := map[string]any{"started_at_utc": startedAt, "model": "gpt-6-astra", "reasoning": "low", "proxy_used": true, "network_requests": count, "network_category": reason, "client_tools_executed": 0, "lifecycle_passed": lifecycleOK, "stages": stages}
		raw, marshalErr := json.MarshalIndent(report, "", "  ")
		if marshalErr != nil {
			t.Error("could not serialize sanitized live report")
			return
		}
		if directory := os.Getenv("BASISPOINTS_LIVE_RELAY_RESULTS"); directory != "" {
			if os.MkdirAll(directory, 0700) != nil || os.WriteFile(filepath.Join(directory, "live-relay-regression.json"), raw, 0600) != nil {
				t.Error("could not save sanitized live report")
			}
		}
		t.Logf("live_summary=%s", raw)
	})
	inputText := "first \"quote\"\nsecond\tvalue"
	inputPath := "C:\\relay\\folder\\sample.txt"
	rawInput := "  echo \"relay\"\r\npath=C:\\relay\\folder\\sample.txt\nliteral=\\n\t  "
	functionTool := map[string]any{"type": "function", "name": "relay_echo", "description": "Synthetic serializer probe; the collector supplies the result.", "parameters": map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"text", "path"},
		"properties": map[string]any{"text": map[string]any{"type": "string", "const": inputText}, "path": map[string]any{"type": "string", "const": inputPath}},
	}}
	customTool := map[string]any{"type": "custom", "name": "relay_raw_echo", "description": "Synthetic raw text serializer probe. Preserve the exact requested text including whitespace, quotes, CRLF and backslashes.", "format": map[string]any{"type": "text"}}
	message := func(text string) map[string]any {
		return map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}
	}
	input := []any{message("Call relay_echo exactly once through the required native relay. Use precisely these JSON arguments: " + string(protocol.JSONBytes(map[string]any{"text": inputText, "path": inputPath})) + ". Return the tool call only.")}
	source := map[string]any{"model": "gpt-6-astra", "reasoning": map[string]any{"effort": "low"}, "stream": true, "store": false, "parallel_tool_calls": false, "session_id": fmt.Sprintf("live-relay-%d", time.Now().UnixNano()), "tools": []any{functionTool}, "input": input}
	run := func(stage string) (map[string]any, map[string]any) {
		frames := requestFrames(t, "https://unused.invalid/responses", accessToken, map[string]string{"ChatGPT-Account-ID": accountID, "X-OpenAI-Account-ID": accountID}, protocol.JSONBytes(source))
		frames[0].GetStart().ProxyUrl = proxyURL
		start := time.Now()
		result := runForward(t, transport, frames)
		entry := map[string]any{"stage": stage, "http_status": result.status, "bytes": len(result.body), "elapsed_ms": time.Since(start).Milliseconds(), "ended": result.ended, "category": "validation_pending", "passed": false}
		stages = append(stages, entry)
		fail := func(category string) {
			entry["category"] = category
			t.Fatalf("stage=%s category=%s HTTP=%d bytes=%d", stage, category, result.status, len(result.body))
		}
		if _, category := budget.snapshot(); category != "" {
			fail(category)
		}
		if result.status == 401 || result.status == 403 {
			fail("authentication_or_account_denial")
		}
		if result.status == 429 {
			fail("rate_limit")
		}
		if result.errFrame != nil {
			fail("transport_error_frame")
		}
		if result.status != http.StatusOK {
			fail("non_success_http")
		}
		final, parseErr := protocol.ParseFinalStreamResponse(result.body)
		if parseErr != nil {
			fail("invalid_or_failed_response")
		}
		if !result.ended || result.received != int64(len(result.body)) || !strings.Contains(string(result.body), "data: [DONE]") || protocol.StringValue(final["status"]) != "completed" {
			fail("incomplete_response")
		}
		return final, entry
	}
	toolCall := func(final map[string]any, kind, name string, entry map[string]any) map[string]any {
		output, _ := final["output"].([]any)
		var found map[string]any
		count := 0
		for _, value := range output {
			item, _ := value.(map[string]any)
			itemKind := protocol.StringValue(item["type"])
			if itemKind == "function_call" || itemKind == "custom_tool_call" {
				found = item
				count++
			}
		}
		if count != 1 || protocol.StringValue(found["type"]) != kind || protocol.StringValue(found["name"]) != name || protocol.StringValue(found["call_id"]) == "" {
			entry["category"] = "unexpected_tool_identity"
			t.Fatal("generated tool count or identity differed from probe")
		}
		return found
	}
	final, entry := run("function_call")
	functionCall := toolCall(final, "function_call", "relay_echo", entry)
	arguments, err := protocol.RawObject([]byte(protocol.StringValue(functionCall["arguments"])))
	if err != nil || len(arguments) != 2 || arguments["text"] != inputText || arguments["path"] != inputPath {
		entry["category"] = "function_input_mismatch"
		t.Fatal("generated function arguments did not preserve fixed probe input")
	}
	entry["passed"], entry["category"] = true, "passed"
	input = append(input, functionCall, map[string]any{"type": "function_call_output", "call_id": functionCall["call_id"], "output": "Synthetic serializer test result accepted; no tool executed."})
	input = append(input, message("Now call relay_raw_echo exactly once through the required native relay. The raw custom input must equal the decoded value of this JSON string byte for byte: "+string(protocol.JSONBytes(rawInput))+". Preserve whitespace and return the tool call only."))
	source["tools"], source["input"] = []any{functionTool, customTool}, input
	final, entry = run("custom_call")
	customCall := toolCall(final, "custom_tool_call", "relay_raw_echo", entry)
	if customCall["input"] != rawInput {
		entry["category"] = "custom_input_mismatch"
		t.Fatal("generated custom input did not preserve fixed probe bytes")
	}
	entry["passed"], entry["category"] = true, "passed"
	input = append(input, customCall, map[string]any{"type": "custom_tool_call_output", "call_id": customCall["call_id"], "output": "Synthetic serializer test result accepted; no tool executed."}, message("Both synthetic tool results are complete. Reply with exactly RELAY_OK and no tool calls."))
	source["input"] = input
	final, entry = run("tool_results_acknowledged")
	output, _ := final["output"].([]any)
	text := ""
	for _, value := range output {
		item, _ := value.(map[string]any)
		if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
			entry["category"] = "unexpected_final_tool_call"
			t.Fatal("model emitted another tool call after complete probe lifecycle")
		}
		content, _ := item["content"].([]any)
		for _, segment := range content {
			part, _ := segment.(map[string]any)
			if part["type"] == "output_text" {
				text += protocol.StringValue(part["text"])
			}
		}
	}
	if strings.TrimSpace(text) != "RELAY_OK" {
		entry["category"] = "acknowledgement_mismatch"
		t.Fatal("model did not acknowledge the complete probe lifecycle")
	}
	entry["passed"], entry["category"] = true, "passed"
	lifecycleOK = true
}
