package transport

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
)

// Opt-in real TestConfig coverage. The fixed prompt may consume account quota.
// Credentials come only from liveCredentials and are never included in logs.
func TestLiveDegradationCheck(t *testing.T) {
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
	configJSON, err := json.Marshal(map[string]bool{"degradation_check": true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.TestConfig(ctx, &pluginv1.TestConfigRequest{
		ConfigJson: configJSON,
	})
	if err != nil || response == nil {
		t.Fatal("degradation TestConfig did not return a response")
	}
	var status struct {
		Check degradationCheckResult `json:"degradation_check"`
	}
	if err := json.Unmarshal([]byte(response.GetStatusJson()), &status); err != nil {
		t.Fatal("degradation result is not valid JSON")
	}
	if !response.GetSuccess() || !status.Check.Completed || len(status.Check.Results) != 1 {
		t.Fatalf("degradation check success=%t completed=%t result_count=%d", response.GetSuccess(), status.Check.Completed, len(status.Check.Results))
	}
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
