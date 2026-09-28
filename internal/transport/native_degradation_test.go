package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestNativeDegradationRequestIgnoresBPSRoutingAndPayloadSettings(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	host := &degradationTestHost{fakeHost: &fakeHost{
		token: token(t, "jwt-child"),
		headers: map[string]*pluginv1.HeaderValues{
			"User-Agent":              {Values: []string{"codex_cli_rs/0.146.0 (Linux)"}},
			"Originator":              {Values: []string{"codex_cli_rs"}},
			"Version":                 {Values: []string{"0.146.0"}},
			"ChatGPT-Account-ID":      {Values: []string{"host-parent"}},
			"X-OpenAI-FedRAMP":        {Values: []string{"true"}},
			"Authorization":           {Values: []string{"Bearer wrong-header-token"}},
			"X-Basispoints-Auth-Mode": {Values: []string{"chatgpt"}},
			"X-OpenAI-Internal-Basispoints-Client-Product": {Values: []string{"basispoints-excel-plugin"}},
			"Origin":              {Values: []string{"https://bps.openai.com"}},
			"Proxy-Authorization": {Values: []string{"must-not-forward"}},
		},
	}}
	cfg := protocol.DefaultConfig()
	cfg.ResponsesURL = "https://bps.example.invalid/responses"
	cfg.EnabledModels = []string{"gpt-6-astra"}
	cfg.DegradationCheckModel = "gpt-5.4-mini"
	cfg.AccountIDs = []int64{7}
	var calls int
	client := &http.Client{Transport: degradationRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != nativeDegradationResponsesURL || r.Method != http.MethodPost {
			t.Fatalf("unexpected native target: %s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer "+host.token || r.Header.Get("ChatGPT-Account-ID") != "host-parent" || r.Header.Get("X-OpenAI-FedRAMP") != "true" {
			t.Fatal("native request did not preserve resolved account identity")
		}
		for key := range r.Header {
			if strings.Contains(strings.ToLower(key), "basispoints") || strings.EqualFold(key, "Origin") || strings.EqualFold(key, "Proxy-Authorization") {
				t.Errorf("native probe leaked non-native header %s", key)
			}
		}
		if r.Header.Get("OpenAI-Beta") != "responses=experimental" || r.Header.Get("Accept") != "text/event-stream" {
			t.Fatal("native protocol headers missing")
		}
		if r.Header.Get("User-Agent") != nativeDegradationUserAgent || r.Header.Get("Version") != nativeDegradationClientVersion || r.Header.Get("Originator") != "codex-tui" {
			t.Fatal("host legacy client identity overrode the compatible native probe")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "gpt-6-astra" || body["store"] != false || body["stream"] != true || body["instructions"] == "" {
			t.Fatalf("wrong native payload: %#v", body)
		}
		if body["model_selection"] != nil || body["reasoning_effort"] != nil || body["metadata"] != nil {
			t.Fatal("native probe contains BPS-only payload fields")
		}
		reasoning, _ := body["reasoning"].(map[string]any)
		if reasoning["effort"] != "xhigh" || !strings.Contains(string(protocol.JSONBytes(body["input"])), degradationCheckPrompt) {
			t.Fatal("native probe prompt or reasoning changed")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(protocol.JSONBytes(map[string]any{"output_text": "iPhone 17"}))))}, nil
	})}
	// A legacy job argument must not override the model even before config normalization.
	status, answer, err := tr.checkDegradationAccount(context.Background(), cfg, host, client, 7, "stale-job-model")
	if err != nil || status != "ok" || answer != "iPhone 17" || calls != 1 {
		t.Fatalf("native check result = %s %q %v; calls=%d", status, answer, err, calls)
	}
}
