package transport

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/auth"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// Always measure native Codex, including accounts currently routed to BPS.
// BPS responses_url must never redirect the native account probe.
const nativeDegradationResponsesURL = "https://chatgpt.com/backend-api/codex/responses"

// Matches the supported host's canonical native OAuth probe identity.
const nativeDegradationUserAgent = "codex-tui/0.146.0 (Ubuntu 22.4.0; x86_64) xterm-256color"

func prepareNativeDegradationRequest(ctx context.Context, host pluginv1.HostServiceClient, accountID int64, model string) (*http.Request, string, error) {
	if host == nil {
		return nil, "", fmt.Errorf("host services are unavailable; cannot resolve account")
	}
	identity, err := host.ResolveOutboundIdentity(ctx, &pluginv1.ResolveOutboundIdentityRequest{AccountId: accountID})
	if err != nil {
		return nil, "", fmt.Errorf("host identity lookup failed")
	}
	if !identity.GetFound() || strings.TrimSpace(identity.GetToken()) == "" {
		return nil, "", fmt.Errorf("account %d has no usable OAuth credential", accountID)
	}
	if (identity.GetAccountId() != 0 && identity.GetAccountId() != accountID) ||
		(identity.GetPlatform() != "" && identity.GetPlatform() != "openai") ||
		(identity.GetAccountType() != "" && identity.GetAccountType() != "oauth") {
		return nil, "", fmt.Errorf("host returned an incompatible native OAuth identity")
	}
	headers := make(http.Header)
	// Only native account/client identity may be carried from the host.
	for key, values := range identity.GetHeaders() {
		switch strings.ToLower(key) {
		case "chatgpt-account-id", "x-openai-account-id", "x-openai-fedramp", "user-agent", "originator", "version", "x-codex-installation-id", "x-codex-turn-metadata":
			for _, value := range values.GetValues() {
				headers.Add(key, value)
			}
		}
	}
	parsed := auth.ParseToken(identity.GetToken(), firstHeader(headers, "ChatGPT-Account-ID", "X-OpenAI-Account-ID"))
	if parsed.AccessToken == "" || parsed.AccountID == "" {
		return nil, "", fmt.Errorf("ChatGPT account identity is missing")
	}
	headers.Set("Authorization", "Bearer "+parsed.AccessToken)
	headers.Set("ChatGPT-Account-ID", parsed.AccountID)
	headers.Set("X-OpenAI-Account-ID", parsed.AccountID)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	headers.Set("OpenAI-Beta", "responses=experimental")
	if headers.Get("User-Agent") == "" {
		headers.Set("User-Agent", nativeDegradationUserAgent)
	}
	if headers.Get("Originator") == "" {
		headers.Set("Originator", "codex-tui")
	}
	if headers.Get("Version") == "" {
		headers.Set("Version", "0.146.0")
	}
	body := protocol.JSONBytes(map[string]any{
		"model":        model,
		"input":        []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_text", "text": degradationCheckPrompt}}}},
		"instructions": "Answer the user's question directly and accurately.",
		"stream":       true, "store": false,
		"reasoning": map[string]any{"effort": "low"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, nativeDegradationResponsesURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("cannot create native check request")
	}
	req.Header = headers
	return req, identity.GetProxyUrl(), nil
}
