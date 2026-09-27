package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"google.golang.org/grpc"
)

type metadataLookupHost struct {
	*fakeHost
	listCalls atomic.Int32
}

func (host *metadataLookupHost) ListAccounts(ctx context.Context, request *pluginv1.ListAccountsRequest, opts ...grpc.CallOption) (*pluginv1.ListAccountsResponse, error) {
	host.listCalls.Add(1)
	return host.fakeHost.ListAccounts(ctx, request, opts...)
}

func identityCarrierBody() map[string]any {
	return map[string]any{
		"model": "gpt-6-astra", "input": "independent user message", "stream": false,
		"device_id": "root-device", "session_id": "root-session",
		"metadata":        map[string]any{"device_id": "body-device", "installation_id": "body-installation", "x-codex-turn-metadata": string(protocol.JSONBytes(map[string]any{"installation_id": "embedded-device", "session_id": "embedded-session"}))},
		"client_metadata": map[string]any{"installation_id": "client-device", "session_id": "client-session"},
	}
}

func TestForwardBPSPreservesIdentityWithoutMetadataLookups(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		for _, supplied := range []bool{false, true} {
			t.Run(fmt.Sprintf("rewrite=%t/host_device=%t", rewrite, supplied), func(t *testing.T) {
				captured := make(chan identityRequest, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					captured <- captureIdentityRequest(t, request)
					identityResponse(w)
				}))
				defer upstream.Close()
				host := &metadataLookupHost{fakeHost: &fakeHost{token: token(t, "off-account")}}
				if supplied {
					host.headers = map[string]*pluginv1.HeaderValues{
						"X-Codex-Installation-ID": {Values: []string{"host-installation"}},
						"X-Device-ID":             {Values: []string{"host-device"}},
					}
				}
				tr := New()
				defer tr.Shutdown()
				tr.host = host
				cfg := map[string]any{"responses_url": upstream.URL, "rewrite_tools": rewrite}

				applyConfig(t, tr, cfg)
				source := identityCarrierBody()
				raw, err := json.MarshalIndent(source, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				result := runForward(t, tr, requestFrames(t, "https://unused.invalid/responses", "", nil, raw))
				if result.errFrame != nil || result.status != http.StatusOK {
					t.Fatalf("identity passthrough request failed: %+v", result)
				}
				got := <-captured
				for _, name := range []string{"X-Codex-Installation-ID", "X-Device-ID"} {
					want := ""
					if supplied {
						want = host.headers[name].GetValues()[0]
					}
					if got.header.Get(name) != want {
						t.Errorf("forwarding added or replaced %s: %q want %q", name, got.header.Get(name), want)
					}
				}
				metadata, _ := got.body["metadata"].(map[string]any)
				for key, want := range source["metadata"].(map[string]any) {
					if metadata[key] != want {
						t.Errorf("forwarding rewrote metadata.%s using a host device header: %v", key, metadata[key])
					}
				}
				if !rewrite && !bytes.Equal(got.raw, raw) {
					t.Fatal("forwarding changed raw body device carriers or encoding")
				}
				if host.listCalls.Load() != 0 {
					t.Fatalf("forwarding performed %d unnecessary account metadata lookups", host.listCalls.Load())
				}
			})
		}
	}
}

func TestDegradationProbePreservesIdentityWithoutMetadataLookups(t *testing.T) {
	for _, supplied := range []bool{false, true} {
		t.Run(fmt.Sprint(supplied), func(t *testing.T) {
			captured := make(chan identityRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				captured <- captureIdentityRequest(t, request)
				identityResponse(w)
			}))
			defer upstream.Close()
			host := &metadataLookupHost{fakeHost: &fakeHost{token: token(t, "probe-account")}}
			if supplied {
				host.headers = map[string]*pluginv1.HeaderValues{"X-Codex-Installation-ID": {Values: []string{"host-probe-device"}}, "X-Codex-Turn-Metadata": {Values: []string{string(protocol.JSONBytes(map[string]any{"installation_id": "host-embedded", "session_id": "host-session"}))}}}
			}
			tr := New()
			defer tr.Shutdown()
			tr.host = host
			applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
			cfg := tr.cfg.Clone()
			cfg.ResponsesURL = upstream.URL
			tr.client = nativeDegradationTestClient(t, upstream)
			status, answer, err := tr.checkDegradationAccount(context.Background(), cfg, host, tr.client, 7, "gpt-6-astra")
			if err != nil || status != "ok" || answer != "iPhone 17" {
				t.Fatalf("probe failed: %s %q %v", status, answer, err)
			}
			got := <-captured
			for _, key := range []string{"X-Codex-Installation-ID", "X-Codex-Turn-Metadata"} {
				want := ""
				if supplied {
					want = host.headers[key].GetValues()[0]
				}
				if got.header.Get(key) != want {
					t.Errorf("probe added or replaced %s: %q", key, got.header.Get(key))
				}
			}
			metadata, _ := got.body["metadata"].(map[string]any)
			for _, key := range []string{"device_id", "installation_id", "x-codex-turn-metadata"} {
				if _, exists := metadata[key]; exists {
					t.Errorf("probe injected body device carrier %s", key)
				}
			}
			if host.listCalls.Load() != 0 {
				t.Fatalf("probe performed %d metadata lookups", host.listCalls.Load())
			}
		})
	}
}
