package transport

import (
	"context"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// Opt-in private probe coverage. This is not a UI Bridge integration test.
// The public legacy TestConfig entry point rejects diagnostic commands.
// The fixed prompt may consume account quota.
// Credentials come only from liveCredentials and are never included in logs.
func TestLiveDegradationRunner(t *testing.T) {
	accessToken, accountID, proxyURL := liveCredentials(t)
	transport := New()
	defer transport.Shutdown()
	transport.mu.Lock()
	transport.host = &fakeHost{
		accounts:    []*pluginv1.AccountInfo{{Id: 1, Schedulable: true}},
		tokenFor:    map[int64]string{1: accessToken},
		proxyURLFor: map[int64]string{1: proxyURL},
		headers: map[string]*pluginv1.HeaderValues{
			"ChatGPT-Account-ID":  {Values: []string{accountID}},
			"X-OpenAI-Account-ID": {Values: []string{accountID}},
		},
	}
	transport.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	cfg := protocol.DefaultConfig()
	cfg.DegradationCheck = true
	cfg.DegradationCheckAccountID = 1
	check, err := transport.runDegradationCheck(ctx, cfg)
	if err != nil || !check.Completed || len(check.Results) != 1 {
		t.Fatalf("degradation runner completed=%t result_count=%d error=%v", check.Completed, len(check.Results), err)
	}
	status := struct{ Check degradationCheckResult }{Check: check}
	result := status.Check.Results[0]
	t.Logf("degradation status=%s answer=%q elapsed=%s", result.Status, previewText(result.Answer, 160), time.Since(started).Round(time.Millisecond))
	if result.Status != "ok" && result.Status != "degraded" {
		t.Fatalf("degradation probe returned no classifiable answer: status=%s error=%s", result.Status, result.Error)
	}
	if result.AccountID != 1 || result.Answer == "" || status.Check.Prompt != degradationCheckPrompt || status.Check.Expected != degradationExpectedReply {
		t.Fatal("degradation result omitted fields required by configuration UI")
	}
	if result.Status == "degraded" {
		if len(status.Check.DegradedAccountIDs) != 1 || status.Check.DegradedAccountIDs[0] != 1 {
			t.Fatal("degraded result did not select its local account ID")
		}
	} else if len(status.Check.DegradedAccountIDs) != 0 {
		t.Fatal("healthy result selected a degraded account ID")
	}
}
