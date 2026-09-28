package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestBackgroundDegradationConcurrentAccountsKeepOwnProxyAndCredential(t *testing.T) {
	for _, sharedProxy := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared_proxy=%t", sharedProxy), func(t *testing.T) {
			testBackgroundDegradationAccountProxies(t, sharedProxy)
		})
	}
}

func testBackgroundDegradationAccountProxies(t *testing.T, sharedProxy bool) {
	entered := make(chan int64, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	host := &degradationTestHost{fakeHost: &fakeHost{
		accounts: []*pluginv1.AccountInfo{{Id: 1, Schedulable: true}, {Id: 2, Schedulable: true}, {Id: 198, Schedulable: false}},
		tokenFor: map[int64]string{}, proxyURLFor: map[int64]string{},
	}}
	allowedProxyHosts := map[string]bool{}
	var localTransport *http.Transport
	var tunnels [2]atomic.Int32
	for accountID := int64(1); accountID <= 2; accountID++ {
		host.tokenFor[accountID] = token(t, fmt.Sprintf("local-proxy-account-%d", accountID))
	}
	proxyCount := 2
	if sharedProxy {
		proxyCount = 1
	}
	for index := 0; index < proxyCount; index++ {
		accountID := int64(index + 1)
		upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestAccountID := accountID
			if sharedProxy && r.Header.Get("ChatGPT-Account-ID") == "local-proxy-account-2" {
				requestAccountID = 2
			}
			identity := fmt.Sprintf("local-proxy-account-%d", requestAccountID)
			accountToken := host.tokenFor[requestAccountID]
			if r.Host != "chatgpt.com" || r.URL.Path != "/backend-api/codex/responses" || r.Header.Get("Authorization") != "Bearer "+accountToken || r.Header.Get("ChatGPT-Account-ID") != identity {
				t.Error("concurrent probe crossed account credentials or proxy routes")
			}
			entered <- requestAccountID
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			answer := "iPhone 17 Pro Max"
			if requestAccountID == 2 {
				answer = "iPhone 15 Pro Max"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(protocol.JSONBytes(map[string]any{"output_text": answer}))
		}))
		defer upstream.Close()
		upstreamURL, _ := url.Parse(upstream.URL)
		if localTransport == nil {
			localTransport = upstream.Client().Transport.(*http.Transport).Clone()
			localTransport.TLSClientConfig = localTransport.TLSClientConfig.Clone()
			localTransport.TLSClientConfig.ServerName = upstreamURL.Hostname()
		}
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodConnect || r.Host != "chatgpt.com:443" {
				t.Error("native probe bypassed the expected proxy CONNECT target")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			tunnels[index].Add(1)
			remote, err := net.DialTimeout("tcp", upstreamURL.Host, time.Second)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			defer remote.Close()
			client, buffered, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer client.Close()
			_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = buffered.Flush()
			go func() { _, _ = io.Copy(remote, buffered); _ = remote.Close() }()
			_, _ = io.Copy(client, remote)
		}))
		defer proxy.Close()
		proxyURL, _ := url.Parse(proxy.URL)
		allowedProxyHosts[proxyURL.Host] = true
		host.proxyURLFor[accountID] = proxy.URL
		if sharedProxy {
			host.proxyURLFor[2] = proxy.URL
		}
	}
	// Block every dial outside configured loopback proxies, including regressions.
	localTransport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if !allowedProxyHosts[address] {
			return nil, fmt.Errorf("test blocked a request outside its account proxies")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	tr := New()
	defer tr.Shutdown()
	tr.host, tr.client = host, &http.Client{Transport: localTransport}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := protocol.DefaultConfig()
	cfg.DegradationCheck = true
	cfg.DegradationCheckAccountIDs = []int64{1, 2, 198}
	type outcome struct {
		check degradationCheckResult
		err   error
	}
	finished := make(chan outcome, 1)
	go func() { check, err := tr.runBackgroundDegradationCheck(ctx, cfg); finished <- outcome{check, err} }()
	seen := map[int64]bool{}
	for len(seen) < 2 {
		select {
		case accountID := <-entered:
			seen[accountID] = true
		case <-ctx.Done():
			t.Fatal("both account proxies must receive requests before either response is released")
		}
	}
	releaseOnce.Do(func() { close(release) })
	result := <-finished
	if result.err != nil || !result.check.Completed || len(result.check.Results) != 3 {
		t.Fatalf("background scan failed: %#v err=%v", result.check, result.err)
	}
	for index, want := range []string{"ok", "degraded", "skipped"} {
		if result.check.Results[index].Status != want {
			t.Fatalf("account result %d = %#v, want %s", index, result.check.Results[index], want)
		}
	}
	invalidTunnels := tunnels[0].Load() != 1 || tunnels[1].Load() != 1
	if sharedProxy {
		invalidTunnels = tunnels[0].Load() < 1 || tunnels[1].Load() != 0
	}
	if invalidTunnels || len(host.resolvedAccountIDs) != 2 {
		t.Fatalf("unexpected probes: proxy1=%d proxy2=%d identities=%v", tunnels[0].Load(), tunnels[1].Load(), host.resolvedAccountIDs)
	}
}
