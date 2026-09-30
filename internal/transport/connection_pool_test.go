package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// Each wave holds all requests in flight before releasing any response, so
// connection reuse is measured independently of local scheduler timing.
type connectionBurstGate struct {
	arrived chan struct{}
	release chan struct{}
}

type connectionBurstServer struct {
	server      *httptest.Server
	gate        atomic.Pointer[connectionBurstGate]
	connections atomic.Int64
}

func newConnectionBurstServer(tls bool) *connectionBurstServer {
	h := &connectionBurstServer{}
	h.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gate := h.gate.Load()
		select {
		case gate.arrived <- struct{}{}:
		case <-r.Context().Done():
			return
		}
		select {
		case <-gate.release:
			_, _ = io.WriteString(w, "ok")
		case <-r.Context().Done():
		}
	}))
	h.server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			h.connections.Add(1)
		}
	}
	if tls {
		h.server.StartTLS()
	} else {
		h.server.Start()
	}
	return h
}

func (h *connectionBurstServer) wave(client *http.Client, target string, concurrency int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gate := &connectionBurstGate{arrived: make(chan struct{}, concurrency), release: make(chan struct{})}
	h.gate.Store(gate)
	released := false
	defer func() {
		if !released {
			close(gate.release)
		}
	}()
	results := make(chan error, concurrency)
	for range concurrency {
		go func() {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err == nil {
				var response *http.Response
				response, err = client.Do(request)
				if err == nil {
					var body []byte
					body, err = io.ReadAll(response.Body)
					_ = response.Body.Close()
					if err == nil && (response.StatusCode != http.StatusOK || string(body) != "ok") {
						err = fmt.Errorf("unexpected response status/body")
					}
				}
			}
			results <- err
		}()
	}
	for range concurrency {
		select {
		case <-gate.arrived:
		case err := <-results:
			return fmt.Errorf("request finished before burst release: %v", err)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	close(gate.release)
	released = true
	for range concurrency {
		select {
		case err := <-results:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func TestClientRetainsConnectionsAcrossConcurrentBursts(t *testing.T) {
	for _, useProxy := range []bool{false, true} {
		t.Run(fmt.Sprintf("proxy=%t", useProxy), func(t *testing.T) {
			const concurrency = 64
			h := newConnectionBurstServer(false)
			defer h.server.Close()
			transport := New()
			defer transport.Shutdown()
			client, target := transport.client, h.server.URL
			if useProxy {
				var err error
				client, err = transport.clientForProxy(client, h.server.URL)
				if err != nil {
					t.Fatal(err)
				}
				target = "http://upstream.invalid/response"
			}
			for wave := range 3 {
				if err := h.wave(client, target, concurrency); err != nil {
					t.Fatal(err)
				}
				if got := h.connections.Load(); got != concurrency {
					t.Fatalf("after wave %d: opened %d connections, want %d reused connections", wave+1, got, concurrency)
				}
			}
		})
	}
}

// Local TLS HTTP/1.1 only; this isolates burst connection churn, not model
// generation latency or production request capacity. One operation is 64 requests.
func BenchmarkTLSConnectionBursts(b *testing.B) {
	for _, idle := range []int{20, 64} {
		b.Run(fmt.Sprintf("idle=%d", idle), func(b *testing.B) {
			const concurrency = 64
			h := newConnectionBurstServer(true)
			defer h.server.Close()
			client := newClient(protocol.DefaultConfig())
			transport := client.Transport.(*http.Transport)
			defer transport.CloseIdleConnections()
			transport.TLSClientConfig = h.server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			transport.MaxIdleConnsPerHost = idle
			if err := h.wave(client, h.server.URL, concurrency); err != nil {
				b.Fatal(err)
			}
			warmConnections := h.connections.Load()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := h.wave(client, h.server.URL, concurrency); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(h.connections.Load()-warmConnections)/float64(b.N), "new_connections/op")
			b.ReportMetric(concurrency, "requests/op")
		})
	}
}
