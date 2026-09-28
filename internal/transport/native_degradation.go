package transport

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/auth"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/config"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// Always measure native Codex, including accounts currently routed to BPS.
// BPS responses_url must never redirect the native account probe.
const nativeDegradationResponsesURL = "https://chatgpt.com/backend-api/codex/responses"

const nativeDegradationModel = config.DefaultDegradationCheckModel
const nativeDegradationReasoningEffort = "xhigh"

// The synthetic probe owns its client identity. The old host's 0.146.0
// identity is rejected for the fixed model. This version matches the locally
// installed Codex client verified with a real native probe on 2026-09-27.
const nativeDegradationClientVersion = "0.158.0-alpha.2.1"
const nativeDegradationUserAgent = "codex-tui/" + nativeDegradationClientVersion + " (Ubuntu 22.4.0; x86_64) xterm-256color"

// The legacy model argument cannot override the fixed probe, including when
// a previously prepared job or persisted state supplies an older value.
func prepareNativeDegradationRequest(ctx context.Context, host pluginv1.HostServiceClient, accountID int64, _ string) (*http.Request, string, error) {
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
	// Carry native account identity, but do not inherit the old host's
	// client-version defaults into this plugin-owned synthetic request.
	for key, values := range identity.GetHeaders() {
		switch strings.ToLower(key) {
		case "chatgpt-account-id", "x-openai-account-id", "x-openai-fedramp", "x-codex-installation-id", "x-codex-turn-metadata":
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
	headers.Set("User-Agent", nativeDegradationUserAgent)
	headers.Set("Originator", "codex-tui")
	headers.Set("Version", nativeDegradationClientVersion)
	body := protocol.JSONBytes(map[string]any{
		"model":        nativeDegradationModel,
		"input":        []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_text", "text": degradationCheckPrompt}}}},
		"instructions": "Answer the user's question directly and accurately.",
		"stream":       true, "store": false,
		"reasoning": map[string]any{"effort": nativeDegradationReasoningEffort},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, nativeDegradationResponsesURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("cannot create native check request")
	}
	req.Header = headers
	return req, identity.GetProxyUrl(), nil
}
