package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	RequestID          string                     `json:"request_id"`
	TargetAccountIDs   []int64                    `json:"target_account_ids"`
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
	return t.runDegradationCheckWithBudgets(ctx, c, budget, degradationCheckTimeout)
}

func (t *Transport) runDegradationCheckWithBudgets(ctx context.Context, c protocol.Config, budget, accountBudget time.Duration) (degradationCheckResult, error) {
	// Bound enumeration and all queued waves independently of the per-account
	// timeout. Synchronous and background callers supply different budgets.
	scanCtx, cancelScan := context.WithTimeout(ctx, budget)
	defer cancelScan()
	result := degradationCheckResult{
		TargetAccountIDs:   append([]int64{}, c.DegradationCheckAccountIDs...),
		Expected:           degradationExpectedReply,
		Prompt:             degradationCheckPrompt,
		Results:            []degradationAccountResult{},
		DegradedAccountIDs: []int64{},
	}
	if c.DegradationCheckAccountID > 0 {
		result.TargetAccountIDs = []int64{c.DegradationCheckAccountID}
	}
	for _, id := range result.TargetAccountIDs {
		result.Results = append(result.Results, degradationAccountResult{AccountID: id, Status: "error", Error: "degradation check could not run"})
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
	if len(result.TargetAccountIDs) > 0 {
		selected := make([]degradationAccount, 0, len(result.TargetAccountIDs))
		for _, id := range result.TargetAccountIDs {
			if index, exists := seen[id]; exists {
				selected = append(selected, accounts[index])
			} else {
				selected = append(selected, degradationAccount{id: id, unavailable: true})
			}
		}
		accounts = selected
	} else {
		// Unscoped enumeration remains private; public diagnostics always have
		// an explicit target snapshot validated before reaching this runner.
		for _, account := range accounts {
			result.TargetAccountIDs = append(result.TargetAccountIDs, account.id)
		}
	}
	if len(accounts) == 0 {
		return result, fmt.Errorf("no OpenAI OAuth accounts available for degradation check")
	}
	result.Results = make([]degradationAccountResult, len(accounts))
	workerCount := 0
	for index, account := range accounts {
		result.Results[index] = degradationAccountResult{AccountID: account.id, Name: account.name}
		if account.unavailable {
			result.Results[index].Status = "skipped"
			result.Results[index].Error = "account is not available for degradation check"
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
				checkCtx, cancel := context.WithTimeout(scanCtx, accountBudget)
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
	result.Completed = scanCtx.Err() == nil
	if scanCtx.Err() != nil {
		return result, scanCtx.Err()
	}
	return result, nil
}

type degradationAccount struct {
	id          int64
	name        string
	schedulable bool
	unavailable bool
}

func degradationModel(_ protocol.Config) string {
	return nativeDegradationModel
}

func (t *Transport) checkDegradationAccount(ctx context.Context, c protocol.Config, host pluginv1.HostServiceClient, base *http.Client, accountID int64, model string) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "error", "", err
	}
	req, proxyURL, err := prepareNativeDegradationRequest(ctx, host, accountID, model)
	if err != nil {
		return "error", "", err
	}
	requestClient, err := t.clientForProxy(base, proxyURL)
	if err != nil {
		return "error", "", err
	}
	// Do not replay native credentials or account headers through a redirect.
	isolated := *requestClient
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	requestClient = &isolated
	// This request owns the client copy. A production save can replace the
	// shared client between prepare and commit, but must not replace the
	// diagnostic snapshot's timeout. The account/scan contexts still cap it.
	requestClient.Timeout = time.Duration(c.TimeoutSeconds) * time.Second
	t.applyNativeTimezone(c, accountID, proxyURL, requestClient, req)
	resp, err := requestClient.Do(req)
	if err != nil {
		return "error", "", degradationReadError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "error", "", fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
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
	if choices, ok := object["choices"].([]any); ok {
		for _, value := range choices {
			choice, ok := value.(map[string]any)
			if !ok {
				return "", fmt.Errorf("invalid upstream response choice")
			}
			if err := degradationResponseError(choice); err != nil {
				return "", err
			}
			if reason, ok := choice["finish_reason"].(string); ok && reason != "" && reason != "stop" {
				return "", fmt.Errorf("upstream response did not complete")
			}
			if message, ok := choice["message"].(map[string]any); ok {
				if err := degradationResponseError(message); err != nil {
					return "", err
				}
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
	if terminal := protocol.ClassifyResponseTerminal("", object); terminal.Failed() {
		return protocol.ResponseTerminalError(terminal, object)
	}
	if status, ok := object["status"].(string); ok && status != "" && status != "completed" {
		return fmt.Errorf("upstream response did not complete")
	}
	return nil
}

func responsesOutputText(object map[string]any) string {
	// A convenience summary or second completion can contradict the main
	// output. Include all explicit answers for the ambiguity classifier, and
	// deduplicate equivalent copies without changing the displayed answer.
	var answers []string
	seen := make(map[string]bool)
	add := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" || seen[text] {
			return
		}
		seen[text] = true
		answers = append(answers, text)
	}
	if text, ok := object["output_text"].(string); ok {
		add(text)
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
			add(strings.Join(texts, "\n"))
		}
	}
	if choices, ok := object["choices"].([]any); ok {
		for _, value := range choices {
			choice, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if message, ok := choice["message"].(map[string]any); ok {
				if role := protocol.StringValue(message["role"]); role == "" || role == "assistant" {
					add(contentText(message["content"]))
				}
			}
			if text, ok := choice["text"].(string); ok {
				add(text)
			}
		}
	}
	return strings.Join(answers, "\n")
}

func outputItemText(item map[string]any) string {
	if role := protocol.StringValue(item["role"]); role != "" && role != "assistant" {
		return ""
	}
	if text, ok := item["text"].(string); ok && item["type"] == "output_text" {
		return text
	}
	if kind := protocol.StringValue(item["type"]); kind != "" && kind != "message" {
		return ""
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
		if kind := protocol.StringValue(part["type"]); kind != "" && kind != "text" && kind != "output_text" {
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
