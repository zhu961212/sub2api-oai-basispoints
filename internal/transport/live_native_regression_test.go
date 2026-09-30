package transport

import (
	"context"
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

const liveNativeProbeURL = "https://chatgpt.com/backend-api/codex/responses"

// Count and allowlist requests before handing them to the supplied account's
// proxy transport. No redirects, retries, BPS requests or direct egress.
type liveNativeBudgetTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	sent int
}

func (b *liveNativeBudgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	b.mu.Lock()
	if b.base == nil || b.sent >= 2 || req.Method != http.MethodPost || req.URL.String() != liveNativeProbeURL {
		b.mu.Unlock()
		return nil, errors.New("native probe route or budget rejected")
	}
	for name := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-basispoints-") {
			b.mu.Unlock()
			return nil, errors.New("native probe BPS header rejected")
		}
	}
	b.sent++
	b.mu.Unlock()
	return b.base.RoundTrip(req)
}

func (b *liveNativeBudgetTransport) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sent
}

// This test is opt-in and sends at most two inference requests in total.
// Generated function calls are inspected only, never executed or answered.
func TestLiveNativeSelectionAndSearch(t *testing.T) {
	if os.Getenv("BASISPOINTS_LIVE_NATIVE_REGRESSION") != "1" {
		t.Skip("requires explicit BASISPOINTS_LIVE_NATIVE_REGRESSION=1")
	}
	selected := os.Getenv("BASISPOINTS_LIVE_NATIVE_STAGE")
	if selected != "" && selected != "explicit_runtime_function" && selected != "hosted_search" {
		t.Fatal("invalid native probe stage selection")
	}
	accessToken, accountID, proxyURL := liveCredentials(t)
	if !strings.HasPrefix(proxyURL, "socks5://") {
		t.Fatal("supplied socks5 proxy required")
	}
	tr := New()
	defer tr.Shutdown()
	applyConfig(t, tr, map[string]any{
		"enabled_models": []string{"gpt-6-astra"}, "timeout_seconds": 65,
		"max_response_bytes":      2 << 20,
		"bps_auto_disable_on_403": false, "auto_degradation_enabled": false,
	})
	client, err := tr.clientForProxy(tr.client, proxyURL)
	if err != nil || client == nil || client == tr.client || client.Transport == nil {
		t.Fatal("supplied proxy configuration invalid")
	}
	budget := &liveNativeBudgetTransport{base: client.Transport}
	client.Transport = budget
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// Defense against a routing regression selecting the default HTTP client.
	tr.client.Transport = &liveNativeBudgetTransport{}
	stages := []map[string]any{}
	t.Cleanup(func() {
		report := map[string]any{"network_requests": budget.count(), "proxy_used": true, "client_tools_executed": 0, "stages": stages}
		raw, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Error("sanitized report serialization failed")
			return
		}
		if directory := os.Getenv("BASISPOINTS_LIVE_NATIVE_RESULTS"); directory != "" {
			if os.MkdirAll(directory, 0700) != nil || os.WriteFile(filepath.Join(directory, "live-native-regression.json"), raw, 0600) != nil {
				t.Error("sanitized report write failed")
			}
		}
		t.Logf("native_summary=%s", raw)
	})
	message := func(text string) map[string]any {
		return map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}
	}
	base := func() map[string]any {
		return map[string]any{"model": "gpt-6-astra", "instructions": "Follow the requested protocol probe precisely.", "stream": true, "store": false, "reasoning": map[string]any{"effort": "low"}, "parallel_tool_calls": false}
	}
	function := base()
	function["input"] = []any{
		map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{map[string]any{
			"type": "function", "name": "native_probe", "description": "Synthetic protocol collector; nothing is executed.", "strict": true,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string", "enum": []any{"NATIVE_OK"}}}, "required": []any{"text"}, "additionalProperties": false},
		}}},
		message("Emit exactly one native_probe function call with text NATIVE_OK. Do not execute anything."),
	}
	function["tool_choice"] = map[string]any{"type": "function", "name": "native_probe"}
	search := base()
	search["input"] = []any{message("Search the public web for IANA's explanation of example.com. Give one concise sentence with a source citation.")}
	search["tools"] = []any{map[string]any{"type": "web_search", "search_context_size": "low"}}
	search["tool_choice"] = map[string]any{"type": "web_search"}
	for _, stage := range []struct {
		name   string
		source map[string]any
	}{{"explicit_runtime_function", function}, {"hosted_search", search}} {
		if selected != "" && stage.name != selected {
			continue
		}
		before := budget.count()
		frames := requestFrames(t, liveNativeProbeURL, accessToken, map[string]string{
			"ChatGPT-Account-ID": accountID, "Content-Type": "application/json", "Accept": "text/event-stream",
			"User-Agent": nativeDegradationUserAgent, "originator": "codex-tui", "version": nativeDegradationClientVersion, "OpenAI-Beta": "responses=experimental",
		}, protocol.JSONBytes(stage.source))
		frames[0].GetStart().ProxyUrl = proxyURL
		ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
		stub := &streamStub{ctx: ctx, requests: frames}
		forwardErr := tr.Forward(stub)
		cancel()
		result := forwardResult{}
		for _, frame := range stub.responses {
			if start := frame.GetStart(); start != nil {
				result.status = int(start.GetStatusCode())
			}
			result.body = append(result.body, frame.GetBodyChunk()...)
			if end := frame.GetEnd(); end != nil {
				result.ended, result.received = true, end.GetBytesReceived()
			}
			if failure := frame.GetError(); failure != nil {
				result.errFrame = failure
			}
		}
		category := "passed"
		final, streamDiagnostic, parseErr := liveNativeProbeResponse(result.body)
		switch {
		case forwardErr != nil || result.errFrame != nil:
			category = "transport_failure"
		case result.status == 401 || result.status == 403:
			category = "authentication_or_account_denial"
		case result.status == 429:
			category = "rate_limit"
		case result.status == 400:
			category = "native_request_rejected"
		case result.status != http.StatusOK:
			category = "non_success_http"
		case parseErr != nil || final["status"] != "completed":
			category = "invalid_or_failed_response"
		case !result.ended || result.received != int64(len(result.body)):
			category = "incomplete_response"
		case budget.count()-before != 1:
			category = "unexpected_request_count"
		default:
			output, _ := final["output"].([]any)
			matched, clientCalls := 0, 0
			for _, raw := range output {
				item, _ := raw.(map[string]any)
				if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
					clientCalls++
				}
				if stage.name == "explicit_runtime_function" && item["type"] == "function_call" && (item["name"] == "native_probe" || item["name"] == "functions.native_probe") && protocol.StringValue(item["call_id"]) != "" {
					args, err := protocol.RawObject([]byte(protocol.StringValue(item["arguments"])))
					if err == nil && len(args) == 1 && args["text"] == "NATIVE_OK" {
						matched++
					}
				}
				if stage.name == "hosted_search" && item["type"] == "web_search_call" && item["status"] == "completed" {
					matched++
				}
			}
			if (stage.name == "explicit_runtime_function" && (matched != 1 || clientCalls != 1)) || (stage.name == "hosted_search" && (matched == 0 || clientCalls != 0)) {
				category = "unexpected_tool_output"
			}
		}
		entry := map[string]any{"stage": stage.name, "category": category, "http_status": result.status, "bytes": len(result.body), "network_requests": budget.count() - before, "stream": streamDiagnostic}
		if category != "passed" && result.status != http.StatusOK {
			entry["diagnostic"] = liveNativeRejectionDiagnostic(result.body)
		}
		stages = append(stages, entry)
		if category != "passed" {
			t.Errorf("stage=%s category=%s HTTP=%d bytes=%d", stage.name, category, result.status, len(result.body))
			if category == "authentication_or_account_denial" || category == "rate_limit" || category == "transport_failure" {
				break
			}
		}
	}
}

// This diagnostic exposes only fixed classifications, never response text,
// arbitrary code/param values, account identifiers or request headers.
func liveNativeRejectionDiagnostic(body []byte) map[string]any {
	result := map[string]any{"shape": "non_json", "code": "unclassified", "type": "unclassified", "param": "unclassified", "summary": "unclassified"}
	payload, err := protocol.RawObject(body)
	if err != nil {
		return result
	}
	result["shape"] = "json_other"
	failure, ok := payload["error"].(map[string]any)
	if ok {
		result["shape"] = "json_error_object"
	} else if _, ok := payload["detail"].(string); ok {
		result["shape"] = "json_detail_string"
	} else if _, ok := payload["error"].(string); ok {
		result["shape"] = "json_error_string"
	}
	if failure == nil {
		// Native SSE can put code/type/message directly on an error event.
		failure = payload
	}
	switch code := protocol.StringValue(failure["code"]); code {
	case "invalid_request_error", "unsupported_value", "model_not_found", "unknown_parameter", "invalid_value", "invalid_tool", "access_denied", "invalid_api_key", "invalid_tool_choice", "unsupported_tool", "invalid_tool_call", "tool_execution_error", "server_error", "unsupported_model", "invalid_request", "rate_limit_exceeded", "fatal_error", "internal_error", "upstream_error", "temporarily_unavailable":
		result["code"] = code
	}
	switch kind := protocol.StringValue(failure["type"]); kind {
	case "error", "invalid_request_error", "server_error", "fatal_error", "internal_error", "upstream_error", "temporarily_unavailable", "rate_limit_error", "authentication_error", "permission_error":
		result["type"] = kind
	}
	switch param := protocol.StringValue(failure["param"]); param {
	case "model", "reasoning", "reasoning.effort", "tool_choice", "tools", "input", "instructions", "stream", "store", "parallel_tool_calls":
		result["param"] = param
	}
	message := strings.ToLower(protocol.StringValue(failure["message"]) + " " + protocol.StringValue(payload["detail"]) + " " + protocol.StringValue(payload["error"]))
	switch {
	case strings.Contains(message, "upgrade") || strings.Contains(message, "version") || strings.Contains(message, "update your"):
		result["summary"] = "client_version_rejected"
	case strings.Contains(message, "reasoning") || strings.Contains(message, "effort"):
		result["summary"] = "reasoning_option_rejected"
	case strings.Contains(message, "additional_tools"):
		result["summary"] = "runtime_declaration_rejected"
	case strings.Contains(message, "tool_choice"):
		result["summary"] = "tool_choice_rejected"
	case strings.Contains(message, "web_search"):
		result["summary"] = "hosted_search_rejected"
	case strings.Contains(message, "model"):
		result["summary"] = "model_rejected"
	case strings.Contains(message, "instructions"):
		result["summary"] = "instructions_rejected"
	case strings.Contains(message, "account") || strings.Contains(message, "token") || strings.Contains(message, "unauthorized"):
		result["summary"] = "account_or_authentication_rejected"
	case strings.Contains(message, "internal server error"), strings.Contains(message, "internal error"), strings.Contains(message, "server error"), strings.Contains(message, "upstream error"), strings.Contains(message, "service unavailable"), strings.Contains(message, "temporarily unavailable"), strings.Contains(message, "temporary failure"), strings.Contains(message, "temporary error"), strings.Contains(message, "try again"), strings.Contains(message, "something went wrong"), strings.Contains(message, "error occurred while processing"), strings.Contains(message, "encountered an error"):
		result["summary"] = "upstream_service_failure"
	}
	if result["summary"] == "unclassified" {
		for _, key := range []string{"code", "type"} {
			switch result[key] {
			case "fatal_error", "internal_error", "upstream_error", "temporarily_unavailable", "server_error":
				result["summary"] = "upstream_service_failure"
			}
		}
	}
	return result
}

func TestNativeRejectionDiagnosticKeepsOnlyFixedValues(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"PRIVATE","param":"PRIVATE","message":"PRIVATE account"}}`,
		`{"detail":"PRIVATE upgrade client version"}`,
		`{"error":"PRIVATE model is unavailable"}`,
		`PRIVATE not JSON`,
	} {
		if strings.Contains(string(protocol.JSONBytes(liveNativeRejectionDiagnostic([]byte(body)))), "PRIVATE") {
			t.Fatal("native rejection diagnostic exposed response content")
		}
	}
	got := liveNativeRejectionDiagnostic([]byte(`{"error":{"code":"unsupported_value","param":"reasoning.effort","message":"unsupported reasoning effort"}}`))
	if got["code"] != "unsupported_value" || got["param"] != "reasoning.effort" || got["summary"] != "reasoning_option_rejected" {
		t.Fatal("fixed classification lost")
	}
	for _, code := range []string{"fatal_error", "internal_error", "upstream_error", "temporarily_unavailable"} {
		for _, nested := range []bool{false, true} {
			failure := map[string]any{"code": code, "type": code, "message": "PRIVATE"}
			payload := failure
			if nested {
				payload = map[string]any{"error": failure}
			}
			got := liveNativeRejectionDiagnostic(protocol.JSONBytes(payload))
			if got["code"] != code || got["type"] != code || got["summary"] != "upstream_service_failure" || strings.Contains(string(protocol.JSONBytes(got)), "PRIVATE") {
				t.Fatal("native service error classification failed")
			}
		}
	}
	for _, message := range []string{"An error occurred while processing your request. PRIVATE", "Something went wrong. PRIVATE", "The service is temporarily unavailable. PRIVATE", "Please try again. PRIVATE"} {
		got := liveNativeRejectionDiagnostic(protocol.JSONBytes(map[string]any{"type": "error", "message": message}))
		if got["type"] != "error" || got["summary"] != "upstream_service_failure" || strings.Contains(string(protocol.JSONBytes(got)), "PRIVATE") {
			t.Fatal("native service message classification failed")
		}
	}
}

// The production degradation helper deliberately retains assistant message
// text only. This probe reuses its SSE decoder and terminal classifier, but
// collects completed tool items as well, solely for offline assertions.
func liveNativeProbeResponse(body []byte) (map[string]any, map[string]any, error) {
	counts := map[string]int{}
	diagnostics := map[string]any{"event_types": counts, "completed_items": []any{}, "terminal_events": []any{}, "sparse_output_recovered": false, "parse_category": "pending"}
	items := []any{}
	byID, byIndex := map[string]string{}, map[int]string{}
	var final map[string]any
	var failure error
	reject := func(category string) {
		if failure == nil {
			failure = errors.New(category)
			diagnostics["parse_category"] = category
		}
	}
	var decoder sseRelayDecoder
	consume := func(event sseRelayEvent) error {
		if strings.TrimSpace(event.data) == "" {
			return nil
		}
		if strings.TrimSpace(event.data) == "[DONE]" {
			counts["done_marker"]++
			return nil
		}
		payload, err := protocol.RawObject([]byte(event.data))
		if err != nil {
			counts["invalid_json"]++
			reject("invalid_json_event")
			return nil
		}
		kind := protocol.StringValue(payload["type"])
		if kind == "" {
			kind = event.event
		}
		switch kind {
		case "response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.completed", "response.done", "response.failed", "response.incomplete", "error", "response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.output_text.delta", "response.output_text.done":
			counts[kind]++
		default:
			counts["other"]++
		}
		if kind == "response.output_item.done" {
			item, ok := payload["item"].(map[string]any)
			if !ok {
				reject("invalid_completed_item")
				return nil
			}
			diagnostics["completed_items"] = append(diagnostics["completed_items"].([]any), liveNativeProbeItemDiagnostic(item))
			id := protocol.StringValue(item["id"])
			index, indexed := relayOutputIndex(payload["output_index"])
			if id == "" || !indexed {
				reject("invalid_item_identity")
				return nil
			}
			encoded := string(protocol.JSONBytes(item))
			if saved, exists := byID[id]; exists {
				if saved != encoded || byIndex[index] != id {
					reject("conflicting_completed_items")
				}
				return nil
			}
			if _, exists := byIndex[index]; exists {
				reject("conflicting_completed_items")
				return nil
			}
			byID[id], byIndex[index] = encoded, id
			items = append(items, item)
		}
		terminal := protocol.ClassifyResponseTerminal(event.event, payload)
		if terminal != protocol.TerminalNone {
			response, _ := payload["response"].(map[string]any)
			entry := map[string]any{"terminal": string(terminal), "status": liveNativeProbeStatus(response["status"])}
			if terminal.Failed() {
				entry["error"] = liveNativeRejectionDiagnostic(protocol.JSONBytes(response))
				entry["envelope_error"] = liveNativeRejectionDiagnostic(protocol.JSONBytes(payload))
				reject("terminal_failure")
			} else if response == nil {
				reject("missing_terminal_response")
			} else if final != nil {
				reject("multiple_terminal_responses")
			} else {
				final = response
				terminalItems := []any{}
				for _, value := range liveNativeProbeItems(response["output"]) {
					terminalItems = append(terminalItems, liveNativeProbeItemDiagnostic(relayObject(value)))
				}
				entry["items"] = terminalItems
			}
			diagnostics["terminal_events"] = append(diagnostics["terminal_events"].([]any), entry)
		}
		return nil
	}
	if err := decoder.feed(append(append([]byte(nil), body...), '\n', '\n'), consume); err != nil {
		reject("invalid_sse")
	}
	if final == nil && failure == nil {
		reject("missing_terminal")
	}
	if failure != nil {
		return nil, diagnostics, failure
	}
	if len(liveNativeProbeItems(final["output"])) == 0 && len(items) > 0 {
		final["output"] = items
		diagnostics["sparse_output_recovered"] = true
	}
	diagnostics["parse_category"] = "passed"
	return final, diagnostics, nil
}

func liveNativeProbeStatus(value any) string {
	switch status := protocol.StringValue(value); status {
	case "", "completed", "failed", "incomplete", "in_progress", "cancelled", "canceled":
		return status
	default:
		return "other"
	}
}

func liveNativeProbeItems(value any) []any {
	items, _ := value.([]any)
	return items
}

func liveNativeProbeItemDiagnostic(item map[string]any) map[string]any {
	kind := protocol.StringValue(item["type"])
	switch kind {
	case "function_call", "custom_tool_call", "web_search_call", "message", "reasoning":
	default:
		kind = "other"
	}
	result := map[string]any{"type": kind, "status": liveNativeProbeStatus(item["status"])}
	if kind == "function_call" {
		name := protocol.StringValue(item["name"])
		if name != "native_probe" && name != "functions.native_probe" {
			name = "other"
		}
		result["name"] = name
		namespace := protocol.StringValue(item["namespace"])
		if namespace != "" && namespace != "functions" {
			namespace = "other"
		}
		result["namespace"] = namespace
		args, err := protocol.RawObject([]byte(protocol.StringValue(item["arguments"])))
		result["arguments_exact"] = err == nil && len(args) == 1 && args["text"] == "NATIVE_OK"
		result["has_call_id"] = protocol.StringValue(item["call_id"]) != ""
	}
	if item["error"] != nil {
		result["error"] = liveNativeRejectionDiagnostic(protocol.JSONBytes(item))
	}
	return result
}

func TestNativeProbeResponseCollectsSparseToolsAndSafeFailures(t *testing.T) {
	function := map[string]any{"id": "fc_probe", "type": "function_call", "status": "completed", "name": "functions.native_probe", "call_id": "call_probe", "arguments": `{"text":"NATIVE_OK"}`}
	search := map[string]any{"id": "ws_probe", "type": "web_search_call", "status": "completed"}
	for index, item := range []map[string]any{function, search} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			body := streamData(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}) + streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{}}})
			final, diagnostic, err := liveNativeProbeResponse([]byte(body))
			if err != nil || len(liveNativeProbeItems(final["output"])) != 1 || diagnostic["sparse_output_recovered"] != true {
				t.Fatal("sparse terminal lost completed tool item")
			}
		})
	}
	failed := streamData(map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "error": map[string]any{"code": "unsupported_tool", "message": "PRIVATE web_search account"}}})
	_, diagnostic, err := liveNativeProbeResponse([]byte(failed))
	if err == nil || diagnostic["parse_category"] != "terminal_failure" || strings.Contains(string(protocol.JSONBytes(diagnostic)), "PRIVATE") {
		t.Fatal("failed native response lost safe diagnostic")
	}
	detail := liveNativeProbeItemDiagnostic(function)
	if detail["name"] != "functions.native_probe" || detail["arguments_exact"] != true {
		t.Fatal("function name or argument classifier lost known match")
	}
	bad := map[string]any{"id": "fc_probe", "type": "function_call", "status": "completed", "name": "PRIVATE", "call_id": "call_probe", "arguments": `{"text":"PRIVATE"}`}
	body := streamData(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": function}) + streamData(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": bad}) + streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{}}})
	_, diagnostic, err = liveNativeProbeResponse([]byte(body))
	if err == nil || diagnostic["parse_category"] != "conflicting_completed_items" || strings.Contains(string(protocol.JSONBytes(diagnostic)), "PRIVATE") {
		t.Fatal("conflicting completion was accepted or exposed")
	}
}
