package transport

import (
	"context"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"google.golang.org/grpc/metadata"
)

// The host sets both markers only after binding the payload to one operation.
// Repeated or malformed metadata cannot authorize a probe.
func scopedDegradationRequestID(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	scopes, ids := md.Get("x-sub2api-test-scope"), md.Get("x-sub2api-test-request-id")
	if len(scopes) != 1 || scopes[0] != "request-v1" || len(ids) != 1 || len(ids[0]) == 0 || len(ids[0]) > 128 {
		return "", false
	}
	for _, ch := range ids[0] {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' || ch == ':') {
			return "", false
		}
	}
	return ids[0], true
}

func (t *Transport) testScopedDegradation(ctx context.Context, c protocol.Config, requestID string) *pluginv1.TestConfigResponse {
	if !c.DegradationCheck || (c.DegradationCheckAccountID <= 0 && len(c.DegradationCheckAccountIDs) == 0) {
		return &pluginv1.TestConfigResponse{Success: false, Message: "degradation check requires an explicit account or non-empty account snapshot"}
	}
	if !t.beginScopedDiagnostic() {
		return &pluginv1.TestConfigResponse{Success: false, Message: "automatic detection is busy; wait for the current batch to finish"}
	}
	defer t.endScopedDiagnostic()
	// A diagnostic cannot acknowledge an unsaved recovery click. Only the
	// active, previously applied configuration can clear a persisted 403 block.
	t.mu.RLock()
	c.BPSReenabledAccounts = t.cfg.Clone().BPSReenabledAccounts
	t.mu.RUnlock()
	started := time.Now()
	check, err := t.runDegradationCheck(ctx, c)
	check.RequestID = requestID
	message := "account degradation check completed"
	if err != nil {
		message = safeError(err)
	}
	status := t.bpsAccountStatusJSON(healthStatusJSON(c, t.accountDirectory(ctx)), c)
	return &pluginv1.TestConfigResponse{
		Success: err == nil && check.Completed, Message: message,
		LatencyMs: time.Since(started).Milliseconds(), StatusJson: mergeDegradationStatus(status, check),
	}
}
