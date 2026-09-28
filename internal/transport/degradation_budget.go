package transport

import (
	"context"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/config"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

const (
	degradationBackgroundMaxGeneration = 30 * time.Minute
	degradationBackgroundOverhead      = 5 * time.Second
)

// Background jobs outlive the host's short configuration RPC. Budget each
// worker from the immutable snapshot, including queued waves, while keeping
// the entire scan bounded even for a large account directory.
func backgroundDegradationBudgets(c protocol.Config) (scanBudget, accountBudget time.Duration) {
	seconds := c.TimeoutSeconds
	if seconds <= 0 {
		seconds = config.DefaultTimeoutSeconds
	}
	seconds = max(10, min(seconds, 1800))
	accountBudget = time.Duration(seconds) * time.Second
	count := max(1, len(c.DegradationCheckAccountIDs))
	if c.DegradationCheckAccountID > 0 {
		count = 1
	}
	waves := (count-1)/degradationCheckParallel + 1
	generationBudget := degradationBackgroundMaxGeneration
	if waves <= int(degradationBackgroundMaxGeneration/accountBudget) {
		generationBudget = time.Duration(waves) * accountBudget
	}
	return generationBudget + degradationBackgroundOverhead, accountBudget
}

func (t *Transport) runBackgroundDegradationCheck(ctx context.Context, c protocol.Config) (degradationCheckResult, error) {
	scanBudget, accountBudget := backgroundDegradationBudgets(c)
	c.TimeoutSeconds = int(accountBudget / time.Second)
	return t.runDegradationCheckWithBudgets(ctx, c, scanBudget, accountBudget)
}
