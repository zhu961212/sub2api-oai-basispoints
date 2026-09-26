package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestDeviceCacheRefreshChangesActualForwardAndProbeTogether(t *testing.T) {
	captured := make(chan deviceIntegrationRequest, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- captureDeviceIntegrationRequest(t, r)
		deviceIntegrationResponse(w)
	}))
	defer upstream.Close()
	var metadata atomic.Value
	metadata.Store("")
	var fail atomic.Bool
	host := &cacheDirectoryHost{
		HostServiceClient: &fakeHost{tokenFor: map[int64]string{7: token(t, "acct-cache")}},
		list: func(context.Context) (*pluginv1.ListAccountsResponse, error) {
			if fail.Load() {
				return nil, context.DeadlineExceeded
			}
			return &pluginv1.ListAccountsResponse{Accounts: []*pluginv1.AccountInfo{{Id: 7, Schedulable: true, MetadataJson: []byte(metadata.Load().(string))}}}, nil
		},
	}
	tr := New()
	defer tr.Shutdown()
	tr.host = host
	applyConfig(t, tr, map[string]any{"responses_url": upstream.URL, "bps_device_convergence": true})
	fallback := deriveBPSDeviceUUID("sub2api:bps-install-id:v1:acct-cache")
	seed := "1f4a8c90-5382-4b5d-8abd-9034adf60123"
	seedID := deriveBPSDeviceUUID("sub2api:codex-install-id:v2:" + seed)
	explicitID := "18bd33c5-25e0-4ae7-95fc-943220320523"
	for index, stage := range []struct {
		name, metadata, want string
		fail                 bool
	}{
		{name: "fallback", want: fallback},
		{name: "seed added", metadata: string(protocol.JSONBytes(map[string]any{"extra": map[string]any{"codex_fingerprint_seed": seed}})), want: seedID},
		{name: "explicit replaces seed", metadata: string(protocol.JSONBytes(map[string]any{"extra": map[string]any{"codex_fingerprint_seed": seed, "openai_device_id": explicitID}})), want: explicitID},
		{name: "refresh fails", want: explicitID, fail: true},
		{name: "metadata removed", want: fallback},
	} {
		t.Run(stage.name, func(t *testing.T) {
			metadata.Store(stage.metadata)
			fail.Store(stage.fail)
			if index > 0 {
				tr.accounts.mu.Lock()
				tr.accounts.expires = time.Now().Add(-time.Second)
				tr.accounts.mu.Unlock()
				tr.accountDeviceID(context.Background(), 7)
				waitAccountRefresh(t, tr)
			}
			incoming := deviceIntegrationHeaders("cache-client", "cache-session")
			source := protocol.JSONBytes(map[string]any{"model": "gpt-6-astra", "input": "cache migration check", "metadata": map[string]any{"device_id": "cache-client"}})
			frames := requestFrames(t, "https://unused.invalid/responses", "", incoming, source)
			frames[0].GetStart().AccountId = 7
			result := runForward(t, tr, frames)
			if result.errFrame != nil || result.status != http.StatusOK {
				t.Fatalf("forward failed: %+v", result)
			}
			verdict, answer, err := tr.checkDegradationAccount(context.Background(), tr.cfg, host, tr.client, 7, protocol.DefaultModelID)
			if err != nil || verdict != "ok" || answer != "iPhone 17" {
				t.Fatalf("probe failed: verdict=%s answer=%q err=%v", verdict, answer, err)
			}
			normal, probe := <-captured, <-captured
			assertDeviceIntegrationHeader(t, normal.header, stage.want, incoming)
			if probe.header.Get("X-Codex-Installation-Id") != stage.want || probe.header.Get("ChatGPT-Account-ID") != "acct-cache" || probe.header.Get("X-Codex-Session-Id") != "" {
				t.Fatal("probe lost account device or inherited another request's session")
			}
			if host.calls.Load() != int32(index+1) {
				t.Fatalf("cached forward/probe made redundant metadata calls: got %d want %d", host.calls.Load(), index+1)
			}
		})
	}
}
