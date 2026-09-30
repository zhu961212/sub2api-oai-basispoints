package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// The opt-in probe allows one PNG upload followed by one responses request.
// It never executes tools, follows redirects, retries, or records raw responses.
type liveAttachmentBudget struct {
	base            http.RoundTripper
	mu              sync.Mutex
	requests        int
	uploadStatus    int
	responseStatus  int
	blocked         int
	networkCategory string
}

func (b *liveAttachmentBudget) RoundTrip(req *http.Request) (*http.Response, error) {
	b.mu.Lock()
	stage := ""
	if req.Method == http.MethodPost && req.URL.Scheme == "https" && req.URL.Host == "bps.openai.com" && req.URL.RawQuery == "" {
		switch {
		case b.requests == 0 && req.URL.Path == "/basispoints/api/attachments":
			stage = "attachment"
		case b.requests == 1 && b.uploadStatus >= 200 && b.uploadStatus < 300 && req.URL.Path == "/basispoints/api/responses":
			stage = "response"
		}
	}
	if stage == "" || b.requests >= 2 {
		b.blocked++
		b.mu.Unlock()
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errors.New("live attachment request budget rejected")
	}
	b.requests++
	b.mu.Unlock()
	response, err := b.base.RoundTrip(req)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.networkCategory = "network_failure"
		message := strings.ToLower(err.Error())
		switch {
		case strings.Contains(message, "socks") || strings.Contains(message, "proxy"):
			b.networkCategory = "proxy_failure"
		case strings.Contains(message, "timeout") || strings.Contains(message, "deadline"):
			b.networkCategory = "network_timeout"
		case strings.Contains(message, "tls") || strings.Contains(message, "certificate"):
			b.networkCategory = "tls_failure"
		}
	}
	if response != nil {
		if stage == "attachment" {
			b.uploadStatus = response.StatusCode
		} else {
			b.responseStatus = response.StatusCode
		}
		// Test-side retry suppression preserves the real HTTP status and body.
		response.Header = response.Header.Clone()
		if response.Header == nil {
			response.Header = make(http.Header)
		}
		response.Header.Set("X-Should-Retry", "false")
	}
	return response, err
}

func (b *liveAttachmentBudget) report() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]any{"network_requests": b.requests, "attachment_http_status": b.uploadStatus, "response_http_status": b.responseStatus, "blocked_extra_requests": b.blocked, "network_category": b.networkCategory}
}

func liveAttachmentFailureCategory(result forwardResult, network map[string]any) string {
	for _, field := range []string{"attachment_http_status", "response_http_status"} {
		switch network[field] {
		case 401:
			return "upstream_auth_401"
		case 402:
			return "upstream_payment_402"
		case 403:
			return "upstream_forbidden_403"
		case 429:
			return "upstream_rate_limit_429"
		case 500, 502, 503, 504:
			return "upstream_server_failure"
		}
	}
	if reason, _ := network["network_category"].(string); reason != "" {
		return reason
	}
	if network["blocked_extra_requests"] != 0 {
		return "unexpected_network_attempt"
	}
	if result.errFrame != nil {
		switch result.errFrame.GetCode() {
		case "PLUGIN_AUTH":
			return "plugin_auth"
		case "PLUGIN_RATE_LIMITED":
			return "plugin_rate_limited"
		case "PLUGIN_UPSTREAM_REJECTED":
			return "plugin_upstream_rejected"
		case "PLUGIN_MODEL_NOT_FOUND":
			return "plugin_model_not_found"
		default:
			return "plugin_transport_error"
		}
	}
	if result.status != http.StatusOK {
		return "non_success_http"
	}
	return ""
}

func TestLiveAttachmentRegression(t *testing.T) {
	if os.Getenv("BASISPOINTS_LIVE_ATTACHMENT_REGRESSION") != "1" {
		t.Skip("requires explicit BASISPOINTS_LIVE_ATTACHMENT_REGRESSION=1")
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
	if err != nil || client == nil || client == transport.client {
		t.Fatal("supplied proxy configuration invalid")
	}
	base, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("unsupported proxy transport")
	}
	// Fresh non-reused connections avoid net/http's stale-connection retries.
	isolated := base.Clone()
	isolated.DisableKeepAlives = true
	defer isolated.CloseIdleConnections()
	budget := &liveAttachmentBudget{base: isolated}
	client.Transport = budget
	client.Timeout = 65 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	imageBytes, imageURL := relayTestImage(t)
	source := map[string]any{
		"model": "gpt-6-astra", "reasoning": map[string]any{"effort": "low"}, "stream": true, "store": false,
		"tools": []any{}, "tool_choice": "none",
		"input": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "This is a synthetic image attachment test. Confirm you received the image with exactly ATTACHMENT_OK. Do not call tools."},
			map[string]any{"type": "input_image", "image_url": imageURL},
		}}},
	}
	frames := requestFrames(t, "https://unused.invalid/responses", accessToken, map[string]string{"ChatGPT-Account-ID": accountID, "X-OpenAI-Account-ID": accountID}, protocol.JSONBytes(source))
	frames[0].GetStart().ProxyUrl = proxyURL
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	stub := &streamStub{ctx: ctx, requests: frames}
	summary := map[string]any{"proxy_used": true, "image_bytes": len(imageBytes), "client_tools_executed": 0, "category": "validation_pending", "passed": false}
	t.Cleanup(func() {
		for key, value := range budget.report() {
			summary[key] = value
		}
		raw, marshalErr := json.MarshalIndent(summary, "", "  ")
		if marshalErr != nil {
			t.Error("could not serialize sanitized attachment report")
			return
		}
		if directory := os.Getenv("BASISPOINTS_LIVE_ATTACHMENT_RESULTS"); directory != "" {
			if os.MkdirAll(directory, 0700) != nil || os.WriteFile(filepath.Join(directory, "live-attachment-regression.json"), raw, 0600) != nil {
				t.Error("could not save sanitized attachment report")
			}
		}
		t.Logf("live_attachment_summary=%s", raw)
	})
	fail := func(category string) { summary["category"] = category; t.Fatalf("attachment_category=%s", category) }
	if transport.Forward(stub) != nil {
		fail("forward_rpc_failure")
	}
	result := forwardResult{}
	for _, frame := range stub.responses {
		if start := frame.GetStart(); start != nil {
			result.status = int(start.GetStatusCode())
		}
		result.body = append(result.body, frame.GetBodyChunk()...)
		if end := frame.GetEnd(); end != nil {
			result.ended = true
			result.received = end.GetBytesReceived()
		}
		if failure := frame.GetError(); failure != nil {
			result.errFrame = failure
		}
	}
	summary["http_status"], summary["response_bytes"], summary["ended"] = result.status, len(result.body), result.ended
	if category := liveAttachmentFailureCategory(result, budget.report()); category != "" {
		fail(category)
	}
	final, parseErr := protocol.ParseFinalStreamResponse(result.body)
	if parseErr != nil {
		fail("invalid_or_failed_response")
	}
	if !result.ended || result.received != int64(len(result.body)) || !strings.Contains(string(result.body), "data: [DONE]") || final["status"] != "completed" {
		fail("incomplete_response")
	}
	output, _ := final["output"].([]any)
	text := ""
	for _, value := range output {
		item, _ := value.(map[string]any)
		kind, _ := item["type"].(string)
		if strings.Contains(kind, "call") {
			fail("unexpected_tool_call")
		}
		content, _ := item["content"].([]any)
		for _, value := range content {
			part, _ := value.(map[string]any)
			if part["type"] == "output_text" {
				text += protocol.StringValue(part["text"])
			}
		}
	}
	if strings.TrimSpace(text) != "ATTACHMENT_OK" {
		fail("acknowledgement_mismatch")
	}
	if budget.report()["network_requests"] != 2 {
		fail("unexpected_network_count")
	}
	summary["category"], summary["passed"] = "passed", true
}

func TestAttachmentLiveProbeBudgetAndRetrySuppression(t *testing.T) {
	calls := 0
	budget := &liveAttachmentBudget{base: retryPolicyRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		_ = req.Body.Close()
		status := 200
		if strings.HasSuffix(req.URL.Path, "/responses") {
			status = 503
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	client := &http.Client{Transport: budget}
	for _, endpoint := range []string{"attachments", "responses"} {
		req, err := http.NewRequest(http.MethodPost, "https://bps.openai.com/basispoints/api/"+endpoint, strings.NewReader("{}"))
		if err != nil {
			t.Fatal("could not create mock request")
		}
		resp, err := doBasisPointsRequest(client, req)
		if err != nil || resp == nil {
			t.Fatal("mock request failed")
		}
		_ = resp.Body.Close()
	}
	if calls != 2 || budget.report()["blocked_extra_requests"] != 0 {
		t.Fatal("probe did not suppress retries")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://bps.openai.com/basispoints/api/responses", strings.NewReader("{}"))
	if _, err := client.Do(req); err == nil || calls != 2 {
		t.Fatal("probe exceeded two-request budget")
	}
	if liveAttachmentFailureCategory(forwardResult{}, budget.report()) != "upstream_server_failure" {
		t.Fatal("upstream status was not classified safely")
	}
}
