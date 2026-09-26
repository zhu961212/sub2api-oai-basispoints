package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"google.golang.org/grpc"
)

type deviceToggleHost struct {
	*fakeHost
	listCalls atomic.Int32
}

func (host *deviceToggleHost) ListAccounts(ctx context.Context, request *pluginv1.ListAccountsRequest, opts ...grpc.CallOption) (*pluginv1.ListAccountsResponse, error) {
	host.listCalls.Add(1)
	return host.fakeHost.ListAccounts(ctx, request, opts...)
}

func deviceToggleBody() map[string]any {
	return map[string]any{
		"model": "gpt-6-astra", "input": "independent user message", "stream": false,
		"device_id": "root-device", "session_id": "root-session",
		"metadata":        map[string]any{"device_id": "body-device", "installation_id": "body-installation", "x-codex-turn-metadata": string(protocol.JSONBytes(map[string]any{"installation_id": "embedded-device", "session_id": "embedded-session"}))},
		"client_metadata": map[string]any{"installation_id": "client-device", "session_id": "client-session"},
	}
}

func TestForwardBPSDeviceConvergenceDefaultAndFalsePreserveDeviceCarriers(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		for _, rewrite := range []bool{false, true} {
			for _, supplied := range []bool{false, true} {
				t.Run(fmt.Sprintf("explicit_false=%t/rewrite=%t/host_device=%t", explicit, rewrite, supplied), func(t *testing.T) {
					captured := make(chan deviceIntegrationRequest, 1)
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
						captured <- captureDeviceIntegrationRequest(t, request)
						deviceIntegrationResponse(w)
					}))
					defer upstream.Close()
					host := &deviceToggleHost{fakeHost: &fakeHost{token: token(t, "off-account")}}
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
					if explicit {
						cfg["bps_device_convergence"] = false
					}
					applyConfig(t, tr, cfg)
					source := deviceToggleBody()
					raw, err := json.MarshalIndent(source, "", "  ")
					if err != nil {
						t.Fatal(err)
					}
					result := runForward(t, tr, requestFrames(t, "https://unused.invalid/responses", "", nil, raw))
					if result.errFrame != nil || result.status != http.StatusOK {
						t.Fatalf("off-mode request failed: %+v", result)
					}
					got := <-captured
					for _, name := range []string{"X-Codex-Installation-ID", "X-Device-ID"} {
						want := ""
						if supplied {
							want = host.headers[name].GetValues()[0]
						}
						if got.header.Get(name) != want {
							t.Errorf("disabled convergence added or replaced %s: %q want %q", name, got.header.Get(name), want)
						}
					}
					metadata, _ := got.body["metadata"].(map[string]any)
					for key, want := range source["metadata"].(map[string]any) {
						if metadata[key] != want {
							t.Errorf("off mode rewrote metadata.%s using a host device header: %v", key, metadata[key])
						}
					}
					if !rewrite && !bytes.Equal(got.raw, raw) {
						t.Fatal("off mode changed raw body device carriers or encoding")
					}
					if host.listCalls.Load() != 0 {
						t.Fatalf("off mode performed %d unnecessary account metadata lookups", host.listCalls.Load())
					}
				})
			}
		}
	}
}

func TestPrepareBPSHeadersDisabledMatchesOrdinaryHeadersExactly(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	host := &deviceToggleHost{fakeHost: &fakeHost{}}
	tr.host = host
	frames := requestFrames(t, "https://unused.invalid/responses", token(t, "off-account"), deviceIntegrationHeaders("off-client", "off-session"), nil)
	start := frames[0].GetStart()
	want, wantProxy, err := prepareHeaders(context.Background(), start, host, "chatgpt")
	if err != nil {
		t.Fatal(err)
	}
	got, gotProxy, err := tr.prepareBPSHeaders(context.Background(), start, host, protocol.Config{AuthMode: "chatgpt"})
	if err != nil || !reflect.DeepEqual(got, want) || gotProxy != wantProxy || host.listCalls.Load() != 0 {
		t.Fatalf("disabled wrapper changed normal preparation: got=%v want=%v lists=%d err=%v", got, want, host.listCalls.Load(), err)
	}
}

func TestBPSDeviceConvergenceHotUpdateOnOffOn(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		t.Run(fmt.Sprint(rewrite), func(t *testing.T) {
			captured := make(chan deviceIntegrationRequest, 3)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				captured <- captureDeviceIntegrationRequest(t, request)
				deviceIntegrationResponse(w)
			}))
			defer upstream.Close()
			host := &deviceToggleHost{fakeHost: &fakeHost{accounts: []*pluginv1.AccountInfo{{Id: 7, MetadataJson: protocol.JSONBytes(map[string]any{"extra": map[string]any{"openai_device_id": "trusted-account-device"}})}}}}
			tr := New()
			defer tr.Shutdown()
			tr.host = host
			for _, enabled := range []bool{true, false, true} {
				applyConfig(t, tr, map[string]any{"responses_url": upstream.URL, "rewrite_tools": rewrite, "bps_device_convergence": enabled})
				beforeLists := host.listCalls.Load()
				incoming := deviceIntegrationHeaders("hot-client", "hot-session")
				result := runForward(t, tr, requestFrames(t, "https://unused.invalid/responses", token(t, "hot-account"), incoming, protocol.JSONBytes(deviceToggleBody())))
				if result.errFrame != nil || result.status != http.StatusOK {
					t.Fatalf("hot update request failed: %+v", result)
				}
				got := <-captured
				metadata, _ := got.body["metadata"].(map[string]any)
				wantHeader, wantBody := incoming["X-Codex-Installation-Id"], "body-device"
				if enabled {
					wantHeader, wantBody = "trusted-account-device", "trusted-account-device"
				}
				if got.header.Get("X-Codex-Installation-ID") != wantHeader || metadata["device_id"] != wantBody {
					t.Errorf("enabled=%t: stale header/body convergence state: header=%q metadata=%v", enabled, got.header.Get("X-Codex-Installation-ID"), metadata)
				}
				if !enabled && host.listCalls.Load() != beforeLists {
					t.Fatal("switching off still looked up device metadata")
				}
			}
		})
	}
}

func TestDegradationProbeDeviceConvergenceDisabledPreservesHostHeaders(t *testing.T) {
	for _, supplied := range []bool{false, true} {
		t.Run(fmt.Sprint(supplied), func(t *testing.T) {
			captured := make(chan deviceIntegrationRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				captured <- captureDeviceIntegrationRequest(t, request)
				deviceIntegrationResponse(w)
			}))
			defer upstream.Close()
			host := &deviceToggleHost{fakeHost: &fakeHost{token: token(t, "probe-account")}}
			if supplied {
				host.headers = map[string]*pluginv1.HeaderValues{"X-Codex-Installation-ID": {Values: []string{"host-probe-device"}}, "X-Codex-Turn-Metadata": {Values: []string{string(protocol.JSONBytes(map[string]any{"installation_id": "host-embedded", "session_id": "host-session"}))}}}
			}
			tr := New()
			defer tr.Shutdown()
			tr.host = host
			// The probe's request config must win over a currently enabled default.
			applyConfig(t, tr, map[string]any{"responses_url": upstream.URL, "bps_device_convergence": true})
			cfg := protocol.DefaultConfig()
			cfg.ResponsesURL = upstream.URL
			status, answer, err := tr.checkDegradationAccount(context.Background(), cfg, host, tr.client, 7, "gpt-6-astra")
			if err != nil || status != "ok" || answer != "iPhone 17" {
				t.Fatalf("off-mode probe failed: %s %q %v", status, answer, err)
			}
			got := <-captured
			for _, key := range []string{"X-Codex-Installation-ID", "X-Codex-Turn-Metadata"} {
				want := ""
				if supplied {
					want = host.headers[key].GetValues()[0]
				}
				if got.header.Get(key) != want {
					t.Errorf("off-mode probe added or replaced %s: %q", key, got.header.Get(key))
				}
			}
			for key := range got.body["metadata"].(map[string]any) {
				if isBPSDeviceField(key) || isBPSTurnMetadataField(key) {
					t.Errorf("off-mode probe injected body device carrier %s", key)
				}
			}
			if host.listCalls.Load() != 0 {
				t.Fatalf("off-mode probe performed %d metadata lookups", host.listCalls.Load())
			}
		})
	}
}

func TestForwardBPSDeviceConvergenceUsesRequestSnapshot(t *testing.T) {
	for _, initial := range []bool{false, true} {
		for _, rewrite := range []bool{false, true} {
			t.Run(fmt.Sprintf("initial=%t/rewrite=%t", initial, rewrite), func(t *testing.T) {
				captured := make(chan deviceIntegrationRequest, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					captured <- captureDeviceIntegrationRequest(t, request)
					deviceIntegrationResponse(w)
				}))
				defer upstream.Close()
				tr := New()
				defer tr.Shutdown()
				apply := func(enabled bool) {
					applyConfig(t, tr, map[string]any{"responses_url": upstream.URL, "bps_device_convergence": enabled, "rewrite_tools": rewrite})
				}
				apply(initial)
				host := &gatedIdentityHost{entered: make(chan struct{}), release: make(chan struct{})}
				tr.host = host
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				incoming := deviceIntegrationHeaders("snapshot-client", "snapshot-session")
				stream := &streamStub{ctx: ctx, requests: requestFrames(t, upstream.URL, "", incoming, protocol.JSONBytes(deviceToggleBody()))}
				done := make(chan error, 1)
				go func() { done <- tr.Forward(stream) }()
				select {
				case <-host.entered:
				case <-ctx.Done():
					t.Fatal("request did not enter identity lookup")
				}
				apply(!initial)
				close(host.release)
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("request did not finish after hot update")
				}
				select {
				case got := <-captured:
					wantHeader, wantBody := incoming["X-Codex-Installation-Id"], "body-device"
					if initial {
						wantHeader = deriveBPSDeviceUUID("sub2api:bps-install-id:v1:test-account")
						wantBody = wantHeader
					}
					metadata, _ := got.body["metadata"].(map[string]any)
					if got.header.Get("X-Codex-Installation-ID") != wantHeader || metadata["device_id"] != wantBody {
						t.Errorf("in-flight request mixed convergence settings: initial=%t header=%q metadata=%v", initial, got.header.Get("X-Codex-Installation-ID"), metadata)
					}
				default:
					t.Fatalf("upstream did not receive request: %v", stream.responses)
				}
			})
		}
	}
}
