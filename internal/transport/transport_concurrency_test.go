package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"google.golang.org/grpc"
)

func TestClientForProxyRejectsStoppedTransport(t *testing.T) {
	for _, proxyURL := range []string{"", "http://proxy.example.test:8080"} {
		t.Run(proxyURL, func(t *testing.T) {
			transport := New()
			base := transport.client
			transport.Shutdown()
			client, err := transport.clientForProxy(base, proxyURL)
			if err == nil || client != nil {
				t.Fatalf("stopped transport returned client %p, error %v", client, err)
			}
			transport.proxyMu.Lock()
			defer transport.proxyMu.Unlock()
			if len(transport.proxyClients) != 0 {
				t.Fatalf("stopped transport rebuilt %d proxy clients", len(transport.proxyClients))
			}
		})
	}
}

func TestApplyConfigCannotRestartStoppedTransport(t *testing.T) {
	transport := New()
	base := transport.client
	transport.Shutdown()
	response, err := transport.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{
		ConfigJson: protocol.JSONBytes(map[string]any{"timeout_seconds": 301}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetApplied() {
		t.Fatal("ApplyConfig restarted the stopped transport")
	}
	transport.mu.RLock()
	defer transport.mu.RUnlock()
	if !transport.closed || transport.client != base {
		t.Fatal("ApplyConfig changed the stopped transport lifecycle")
	}
}

type gatedIdentityHost struct {
	pluginv1.HostServiceClient
	entered  chan struct{}
	release  chan struct{}
	proxyURL string
}

func (host *gatedIdentityHost) ListAccounts(context.Context, *pluginv1.ListAccountsRequest, ...grpc.CallOption) (*pluginv1.ListAccountsResponse, error) {
	return &pluginv1.ListAccountsResponse{}, nil
}

func (host *gatedIdentityHost) ResolveOutboundIdentity(ctx context.Context, request *pluginv1.ResolveOutboundIdentityRequest, _ ...grpc.CallOption) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	close(host.entered)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-host.release:
	}
	return &pluginv1.ResolveOutboundIdentityResponse{
		Found: true, AccountId: request.GetAccountId(), Token: "test-token",
		ProxyUrl: host.proxyURL,
		Headers:  map[string]*pluginv1.HeaderValues{"ChatGPT-Account-ID": {Values: []string{"test-account"}}},
	}, nil
}

func TestForwardStoppedDuringIdentityLookupDoesNotDispatch(t *testing.T) {
	for _, proxyURL := range []string{"", "http://proxy.example.test:8080"} {
		t.Run(proxyURL, func(t *testing.T) {
			received := make(chan struct{}, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				received <- struct{}{}
				writer.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			transport := New()
			defer transport.Shutdown()
			applyConfig(t, transport, map[string]any{"responses_url": upstream.URL, "rewrite_tools": false, "transform_responses": false})
			host := &gatedIdentityHost{entered: make(chan struct{}), release: make(chan struct{}), proxyURL: proxyURL}
			transport.host = host
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream := &streamStub{ctx: ctx, requests: requestFrames(t, upstream.URL, "", nil, protocol.JSONBytes(map[string]any{"model": protocol.DefaultModelID}))}
			done := make(chan error, 1)
			go func() { done <- transport.Forward(stream) }()
			select {
			case <-host.entered:
			case <-ctx.Done():
				t.Fatal("request did not enter identity lookup")
			}
			transport.Shutdown()
			close(host.release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("request did not finish after shutdown")
			}
			if len(stream.responses) != 1 || stream.responses[0].GetError().GetCode() != "plugin_stopped" || stream.responses[0].GetError().GetRequestSent() {
				t.Fatalf("stopped request response = %v", stream.responses)
			}
			select {
			case <-received:
				t.Fatal("stopped request was sent upstream")
			default:
			}
			if len(transport.proxyClients) != 0 {
				t.Fatal("stopped request repopulated the proxy cache")
			}
		})
	}
}

func TestConcurrentProxyConfigAndShutdown(t *testing.T) {
	transport := New()
	defer transport.Shutdown()
	base := transport.client
	const workers = 48
	const attempts = 500
	start := make(chan struct{})
	shutdown := make(chan struct{})
	errorsFound := make(chan error, workers+1)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			<-start
			proxyURL := fmt.Sprintf("http://proxy-%d.example.test:8080", worker%8)
			for attempt := 0; attempt < attempts; attempt++ {
				client, err := transport.clientForProxy(base, proxyURL)
				if err != nil && !errors.Is(err, errTransportStopped) {
					errorsFound <- err
					return
				}
				if err == nil && client == nil {
					errorsFound <- errors.New("proxy lookup returned nil without error")
					return
				}
			}
		}(worker)
	}
	group.Add(2)
	go func() {
		defer group.Done()
		<-start
		for iteration := 0; iteration < 50; iteration++ {
			if iteration == 25 {
				close(shutdown)
			}
			_, err := transport.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{
				ConfigJson: protocol.JSONBytes(map[string]any{"timeout_seconds": 300 + iteration}),
			})
			if err != nil {
				errorsFound <- err
			}
		}
	}()
	go func() {
		defer group.Done()
		<-shutdown
		transport.Shutdown()
	}()
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	close(start)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent proxy/config/shutdown operations deadlocked")
	}
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	transport.mu.RLock()
	defer transport.mu.RUnlock()
	transport.proxyMu.Lock()
	defer transport.proxyMu.Unlock()
	if !transport.closed || len(transport.proxyClients) != 0 {
		t.Fatalf("final lifecycle = closed %v, cached clients %d", transport.closed, len(transport.proxyClients))
	}
}

func TestForwardAuthModeUsesRequestConfigSnapshot(t *testing.T) {
	captured := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured <- request.Header.Get("X-Basispoints-Auth-Mode")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(protocol.JSONBytes(map[string]any{"id": "snapshot", "output": []any{}, "status": "completed"}))
	}))
	defer upstream.Close()
	transport := New()
	defer transport.Shutdown()
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL, "auth_mode": "snapshot-before", "rewrite_tools": false, "transform_responses": false})
	host := &gatedIdentityHost{entered: make(chan struct{}), release: make(chan struct{})}
	transport.host = host
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := &streamStub{ctx: ctx, requests: requestFrames(t, upstream.URL, "", nil, protocol.JSONBytes(map[string]any{"model": protocol.DefaultModelID}))}
	done := make(chan error, 1)
	go func() { done <- transport.Forward(stream) }()
	select {
	case <-host.entered:
	case <-ctx.Done():
		t.Fatal("request did not enter identity lookup")
	}
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL, "auth_mode": "snapshot-after", "rewrite_tools": false, "transform_responses": false})
	close(host.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("request did not finish")
	}
	select {
	case mode := <-captured:
		if mode != "snapshot-before" {
			t.Fatalf("in-flight request changed auth_mode to %q", mode)
		}
	default:
		t.Fatal("upstream did not receive request")
	}
}
