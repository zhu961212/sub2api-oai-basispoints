package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// This user-selected heuristic asks for an answer without supplying a candidate.
// It is an account-routing aid, not a reliable measure of model intelligence.
const (
	degradationCheckPrompt   = "不联网，不猜测，直接说出你知道的最新苹果手机。只输出手机型号，不要解释。"
	degradationExpectedReply = "苹果17"
	// PluginManager.Test gives the whole operation 30 seconds. Keep enough
	// room for returning results while allowing a slow account to finish. The
	// total scan budget still bounds queued batches and the UI request.
	degradationCheckTimeout  = 20 * time.Second
	degradationCheckBudget   = 24 * time.Second
	degradationCheckParallel = 8
)

type degradationAccountResult struct {
	AccountID int64  `json:"account_id"`
	Name      string `json:"name,omitempty"`
	Status    string `json:"status"` // ok, degraded, error, skipped
	Answer    string `json:"answer,omitempty"`
	Error     string `json:"error,omitempty"`
}

type degradationCheckResult struct {
	Completed          bool                       `json:"completed"`
	Expected           string                     `json:"expected"`
	Prompt             string                     `json:"prompt"`
	Results            []degradationAccountResult `json:"results"`
	DegradedAccountIDs []int64                    `json:"degraded_account_ids"`
}

// runDegradationCheck sends one real request per account through that
// account's own token and proxy. It deliberately ignores the configured
// account whitelist: the operation's purpose is to discover the accounts that
// should become the next whitelist. Administratively unavailable accounts are
// reported as skipped and are never probed.
func (t *Transport) runDegradationCheck(ctx context.Context, c protocol.Config) (degradationCheckResult, error) {
	return t.runDegradationCheckWithBudget(ctx, c, degradationCheckBudget)
}

func (t *Transport) runDegradationCheckWithBudget(ctx context.Context, c protocol.Config, budget time.Duration) (degradationCheckResult, error) {
	// Bound the complete scan, including queued batches and account enumeration,
	// so the host can return results before its 30-second TestConfig deadline.
	scanCtx, cancelScan := context.WithTimeout(ctx, budget)
	defer cancelScan()
	result := degradationCheckResult{
		Expected:           degradationExpectedReply,
		Prompt:             degradationCheckPrompt,
		Results:            []degradationAccountResult{},
		DegradedAccountIDs: []int64{},
	}

	t.mu.RLock()
	host, base, closed := t.host, t.client, t.closed
	t.mu.RUnlock()
	if closed {
		return result, errTransportStopped
	}
	if host == nil {
		return result, fmt.Errorf("host services are unavailable; cannot enumerate accounts")
	}
	if base == nil {
		return result, fmt.Errorf("HTTP client is unavailable")
	}
	model := degradationModel(c)

	accountsResponse, err := host.ListAccounts(scanCtx, &pluginv1.ListAccountsRequest{Platform: "openai", AccountType: "oauth"})
	if err != nil {
		return result, fmt.Errorf("list accounts failed")
	}
	accounts := make([]degradationAccount, 0)
	seen := make(map[int64]int)
	if accountsResponse != nil {
		for _, account := range accountsResponse.GetAccounts() {
			if account == nil || account.GetId() <= 0 {
				continue
			}
			if index, exists := seen[account.GetId()]; exists {
				// A duplicate row must not probe the same identity twice. If
				// availability conflicts, honor the host's unavailable verdict.
				accounts[index].schedulable = accounts[index].schedulable && account.GetSchedulable()
				continue
			}
			seen[account.GetId()] = len(accounts)
			accounts = append(accounts, degradationAccount{
				id: account.GetId(), name: account.GetName(), schedulable: account.GetSchedulable(),
			})
		}
		// HostService v1 only populated account_ids. Keep this fallback so the
		// feature remains useful against an older host; such rows have no name.
		if len(accountsResponse.GetAccounts()) == 0 {
			for _, id := range accountsResponse.GetAccountIds() {
				if _, exists := seen[id]; id > 0 && !exists {
					seen[id] = len(accounts)
					accounts = append(accounts, degradationAccount{id: id, schedulable: true})
				}
			}
		}
	}
	if c.DegradationCheckAccountID > 0 {
		var selected []degradationAccount
		for _, account := range accounts {
			if account.id == c.DegradationCheckAccountID {
				selected = []degradationAccount{account}
				break
			}
		}
		if len(selected) == 0 {
			result.Results = []degradationAccountResult{{
				AccountID: c.DegradationCheckAccountID,
				Status:    "skipped",
				Error:     "account is not available for degradation check",
			}}
			result.Completed = ctx.Err() == nil
			return result, ctx.Err()
		}
		accounts = selected
	}
	if len(accounts) == 0 {
		return result, fmt.Errorf("no OpenAI OAuth accounts available for degradation check")
	}
	result.Results = make([]degradationAccountResult, len(accounts))
	workerCount := 0
	for index, account := range accounts {
		result.Results[index] = degradationAccountResult{AccountID: account.id, Name: account.name}
		if t.isBPSAccountDisabled(account.id, c) {
			accounts[index].schedulable = false
			result.Results[index].Status = "skipped"
			result.Results[index].Error = "Basis Points is disabled after HTTP 403; re-enable this account before checking"
			continue
		}
		if !account.schedulable {
			result.Results[index].Status = "skipped"
			result.Results[index].Error = "account is not schedulable"
			continue
		}
		// Queued accounts keep a useful result when the scan budget expires.
		result.Results[index].Status = "error"
		result.Results[index].Error = "degradation check deadline reached or canceled"
		if workerCount < degradationCheckParallel {
			workerCount++
		}
	}
	// Bound goroutines as well as network concurrency: large directories do
	// not need one blocked goroutine per account while waiting for a slot.
	jobs := make(chan int)
	var wg sync.WaitGroup
	wg.Add(workerCount)
	for worker := 0; worker < workerCount; worker++ {
		go func() {
			defer wg.Done()
			for index := range jobs {
				if scanCtx.Err() != nil {
					return
				}
				account := accounts[index]
				checkCtx, cancel := context.WithTimeout(scanCtx, degradationCheckTimeout)
				status, answer, checkErr := t.checkDegradationAccount(checkCtx, c, host, base, account.id, model)
				cancel()
				result.Results[index].Status = status
				result.Results[index].Answer = answer
				result.Results[index].Error = ""
				if checkErr != nil {
					result.Results[index].Error = safeError(checkErr)
				}
			}
		}()
	}
dispatch:
	for index, account := range accounts {
		if !account.schedulable {
			continue
		}
		select {
		case jobs <- index:
		case <-scanCtx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()
	for _, account := range result.Results {
		if account.Status == "degraded" {
			result.DegradedAccountIDs = append(result.DegradedAccountIDs, account.AccountID)
		}
	}
	result.Completed = ctx.Err() == nil
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

type degradationAccount struct {
	id          int64
	name        string
	schedulable bool
}

func degradationModel(c protocol.Config) string {
	for _, model := range c.EnabledModels {
		if c.HandlesModel(model) {
			return strings.TrimSpace(model)
		}
	}
	return protocol.DefaultModelID
}

func (t *Transport) checkDegradationAccount(ctx context.Context, c protocol.Config, host pluginv1.HostServiceClient, base *http.Client, accountID int64, model string) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "error", "", err
	}
	if t.isBPSAccountDisabled(accountID, c) {
		return "skipped", "", fmt.Errorf("Basis Points is disabled after HTTP 403")
	}
	if err := t.bpsAccountStoreError(); err != nil {
		return "error", "", err
	}
	observeBPSStatus := newBasisPointsStatusObserver(func() { t.disableBPSAccount(ctx, accountID) })
	start := &pluginv1.ForwardRequestStart{AccountId: accountID}
	headers, proxyURL, err := t.prepareBPSHeaders(ctx, start, host, c)
	if err != nil {
		return "error", "", err
	}
	requestClient, err := t.clientForProxy(base, proxyURL)
	if err != nil {
		return "error", "", err
	}
	requestClient = withoutBPSRedirects(requestClient)
	// Match real forwarding: Basis Points expects normalized input, explicit
	// model selection and streamed Responses output.
	upstreamBody, err := protocol.PrepareResponsesBody(map[string]any{
		"model":            model,
		"input":            degradationCheckPrompt,
		"reasoning_effort": "low",
	}, c)
	if err != nil {
		return "error", "", fmt.Errorf("cannot prepare check request")
	}
	if c.BPSDeviceConvergence {
		applyBPSDeviceBody(upstreamBody, headers.Get("X-Codex-Installation-Id"))
	}
	body := protocol.JSONBytes(upstreamBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ResponsesURL, bytes.NewReader(body))
	if err != nil {
		return "error", "", fmt.Errorf("cannot create check request")
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := requestClient.Do(req)
	if err != nil {
		return "error", "", degradationReadError(ctx, err)
	}
	defer resp.Body.Close()
	observeBPSStatus(resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "error", "", fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	if err := prepareBasisPointsResponse(resp, c.MaxResponseBytes, observeBPSStatus); err != nil {
		return "error", "", err
	}
	responseBody, contentType, err := readDegradationResponse(ctx, resp.Body, resp.Header.Get("Content-Type"), c.MaxResponseBytes)
	if err != nil {
		return "error", "", err
	}
	answer, err := degradationAnswer(responseBody, contentType)
	if err != nil {
		return "error", "", err
	}
	status, verdictErr := classifyDegradationAnswer(answer)
	return status, answer, verdictErr
}

// isExpectedDegradationAnswer retains the fixed, user-selected generation rule.
// Ambiguous answers must not be promoted to a conclusive account verdict.
func isExpectedDegradationAnswer(answer string) bool {
	status, _ := classifyDegradationAnswer(answer)
	return status == "ok"
}

// degradationAnswer accepts both the JSON and SSE forms returned by Responses
// gateways. Chat-completions-shaped responses are accepted as a compatibility
// fallback because some configured upstreams expose both protocols.
func degradationAnswer(body []byte, contentType string) (string, error) {
	trimmed := bytes.TrimSpace(body)
	var object map[string]any
	var err error
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") ||
		bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("data:")) {
		object, err = protocol.ParseFinalStreamResponse(body)
	} else {
		object, err = protocol.RawObject(body)
	}
	if err != nil {
		return "", fmt.Errorf("invalid upstream response")
	}
	if err := degradationResponseError(object); err != nil {
		return "", err
	}
	if nested, ok := object["response"].(map[string]any); ok {
		object = nested
		if err := degradationResponseError(object); err != nil {
			return "", err
		}
	}
	if output, ok := object["output"].([]any); ok {
		for _, value := range output {
			if item, ok := value.(map[string]any); ok {
				if err := degradationResponseError(item); err != nil {
					return "", err
				}
			}
		}
	}
	if choices, ok := object["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if reason, ok := choice["finish_reason"].(string); ok && reason != "" && reason != "stop" {
				return "", fmt.Errorf("upstream response did not complete")
			}
		}
	}
	if answer := strings.TrimSpace(responsesOutputText(object)); answer != "" {
		return answer, nil
	}
	return "", fmt.Errorf("upstream response contains no output text")
}

// Gate both the envelope and the response before classifying output text. Some
// gateways keep partial output inside a failed envelope without copying the
// error or terminal status into the nested response.
func degradationResponseError(object map[string]any) error {
	if err := degradationExplicitFailure(object); err != nil {
		return err
	}
	if status, ok := object["status"].(string); ok && status != "" && status != "completed" {
		return fmt.Errorf("upstream response did not complete")
	}
	return nil
}

// Progress events may have an in_progress status. Explicit failure evidence is
// rejected immediately without treating ordinary progress as terminal.
func degradationExplicitFailure(object map[string]any) error {
	if object["error"] != nil {
		return fmt.Errorf("upstream response contains an error")
	}
	if degradationFailureKind(protocol.StringValue(object["type"])) || degradationFailureKind(protocol.StringValue(object["status"])) {
		return fmt.Errorf("upstream response did not complete")
	}
	for _, key := range []string{"success", "ok"} {
		if success, ok := object[key].(bool); ok && !success {
			return fmt.Errorf("upstream response reports a failed operation")
		}
	}
	for _, key := range []string{"status", "status_code", "http_status"} {
		status, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(object[key])))
		if err == nil && status >= 400 && status <= 599 {
			return fmt.Errorf("upstream response reports HTTP %d", status)
		}
	}
	return nil
}

func degradationFailureKind(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "error", "failed", "incomplete", "cancelled", "canceled", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		return true
	default:
		return false
	}
}

func responsesOutputText(object map[string]any) string {
	if text, ok := object["output_text"].(string); ok {
		return text
	}
	if output, ok := object["output"].([]any); ok {
		var texts []string
		for _, value := range output {
			item, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if text := outputItemText(item); text != "" {
				texts = append(texts, text)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}
	if choices, ok := object["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if message, ok := choice["message"].(map[string]any); ok {
				if text := contentText(message["content"]); text != "" {
					return text
				}
			}
			if text, ok := choice["text"].(string); ok {
				return text
			}
		}
	}
	return ""
}

func outputItemText(item map[string]any) string {
	if text, ok := item["text"].(string); ok && item["type"] == "output_text" {
		return text
	}
	content, ok := item["content"].([]any)
	if !ok {
		return ""
	}
	var builder strings.Builder
	for _, value := range content {
		part, ok := value.(map[string]any)
		if !ok || part["type"] != "output_text" {
			continue
		}
		if text, ok := part["text"].(string); ok {
			builder.WriteString(text)
		}
	}
	return builder.String()
}

func contentText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	items, ok := value.([]any)
	if !ok {
		return ""
	}
	var builder strings.Builder
	for _, item := range items {
		part, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := part["text"].(string); ok {
			builder.WriteString(text)
		}
	}
	return builder.String()
}

func mergeDegradationStatus(statusJSON string, check degradationCheckResult) string {
	var status map[string]any
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil || status == nil {
		status = make(map[string]any)
	}
	status["degradation_check"] = check
	encoded, err := json.Marshal(status)
	if err != nil {
		return statusJSON
	}
	return string(encoded)
}
