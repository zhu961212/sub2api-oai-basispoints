package transport

// BASISPOINTS_LOCAL_PERF=1 enables this bounded localhost-only harness. Reported
// latency includes Forward, local HTTP, scheduling, and simulated upstream time.
// It excludes request fixture construction and process/gRPC overhead. Throughput
// is wave-based, includes fixture construction, and is not a saturation test.
// It does not measure real model speed, WAN latency, or production capacity.
import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

type localPerformanceSample struct {
	total, first time.Duration
	err          error
}

func localPerformancePercentile(samples []time.Duration, percentile int) time.Duration {
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	return sorted[(len(sorted)*percentile+99)/100-1]
}

func TestLocalForwardPerformance(t *testing.T) {
	if os.Getenv("BASISPOINTS_LOCAL_PERF") != "1" {
		t.Skip("set BASISPOINTS_LOCAL_PERF=1 for bounded localhost latency measurements")
	}
	const initialDelay = 10 * time.Millisecond
	const completionDelay = 10 * time.Millisecond
	t.Logf("LOCAL ONLY runtime=%s GOOS=%s GOARCH=%s CPUs=%d GOMAXPROCS=%d first_delay=%s completion_delay=%s", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.GOMAXPROCS(0), initialDelay, completionDelay)
	for _, mode := range []string{"transformed", "passthrough"} {
		for _, concurrency := range []int{1, 8, 32, 64} {
			t.Run(fmt.Sprintf("%s/concurrency=%d", mode, concurrency), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				var active, peak atomic.Int32
				var upstreamRequests atomic.Int64
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upstreamRequests.Add(1)
					n := active.Add(1)
					defer active.Add(-1)
					for old := peak.Load(); n > old; old = peak.Load() {
						if peak.CompareAndSwap(old, n) {
							break
						}
					}
					_, _ = io.Copy(io.Discard, r.Body)
					owner := r.Header.Get("X-Test-Owner")
					expectedPath := "/host"
					if mode == "transformed" {
						expectedPath = "/basispoints"
					}
					if r.URL.Path != expectedPath || owner == "" {
						http.Error(w, "wrong routing or missing owner", http.StatusBadRequest)
						return
					}
					select {
					case <-time.After(initialDelay):
					case <-r.Context().Done():
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.output_text.delta", "delta": owner}))
					w.(http.Flusher).Flush()
					select {
					case <-time.After(completionDelay):
					case <-r.Context().Done():
						return
					}
					_, _ = io.WriteString(w, localPerformanceTerminal(owner, mode))
				}))
				defer upstream.Close()
				transport := New()
				defer transport.Shutdown()
				applyConfig(t, transport, map[string]any{"responses_url": upstream.URL + "/basispoints"})
				accessToken := token(t, "local-performance-account")
				sequence := 0
				runWave := func() []localPerformanceSample {
					results := make(chan localPerformanceSample, concurrency)
					start := make(chan struct{})
					for worker := 0; worker < concurrency; worker++ {
						sequence++
						owner := fmt.Sprintf("%s-%d-%d", mode, concurrency, sequence)
						model := "gpt-6-astra"
						if mode == "passthrough" {
							model = "gpt-6-sol"
						}
						body := protocol.JSONBytes(map[string]any{"model": model, "stream": true, "input": owner, "tools": []any{map[string]any{"type": "function", "name": "echo", "parameters": map[string]any{"type": "object", "required": []any{"owner"}, "properties": map[string]any{"owner": map[string]any{"type": "string"}}, "additionalProperties": false}}}})
						stub := &timingStreamStub{streamStub: streamStub{ctx: ctx, requests: requestFrames(t, upstream.URL+"/host", accessToken, map[string]string{"X-Test-Owner": owner, "session_id": t.Name() + owner}, body)}, firstBodyChunk: make(chan time.Time, 1)}
						go func() {
							<-start
							began := time.Now()
							err := transport.Forward(stub)
							sample := localPerformanceSample{total: time.Since(began), err: err}
							select {
							case first := <-stub.firstBodyChunk:
								sample.first = first.Sub(began)
							default:
								sample.err = fmt.Errorf("%s: no first output", owner)
							}
							if err := localPerformanceValidate(stub, owner, mode); err != nil {
								sample.err = err
							}
							results <- sample
						}()
					}
					close(start)
					wave := make([]localPerformanceSample, 0, concurrency)
					for range concurrency {
						select {
						case sample := <-results:
							if sample.err != nil {
								t.Fatal(sample.err)
							}
							wave = append(wave, sample)
						case <-ctx.Done():
							t.Fatal("local performance wave timed out")
						}
					}
					return wave
				}
				runWave() // Warm connections and code paths before measurements.
				peak.Store(0)
				count := max(128, concurrency*8)
				totals, firsts := make([]time.Duration, 0, count), make([]time.Duration, 0, count)
				started := time.Now()
				for len(totals) < count {
					for _, sample := range runWave() {
						totals = append(totals, sample.total)
						firsts = append(firsts, sample.first)
					}
				}
				elapsed := time.Since(started)
				if got := upstreamRequests.Load(); got != int64(count+concurrency) {
					t.Fatalf("upstream requests = %d, want %d", got, count+concurrency)
				}
				t.Logf("LOCAL mode=%s concurrency=%d samples=%d peak_upstreams=%d total_p50=%s total_p95=%s first_p50=%s first_p95=%s throughput=%.1f_req/s errors=0", mode, concurrency, count, peak.Load(), localPerformancePercentile(totals, 50).Round(time.Microsecond), localPerformancePercentile(totals, 95).Round(time.Microsecond), localPerformancePercentile(firsts, 50).Round(time.Microsecond), localPerformancePercentile(firsts, 95).Round(time.Microsecond), float64(count)/elapsed.Seconds())
			})
		}
	}
}

func localPerformanceTerminal(owner, mode string) string {
	output := []any{}
	if mode == "transformed" {
		output = append(output, relayNativeCall("shared_call", "echo", map[string]any{"owner": owner}))
	}
	return streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_" + owner, "status": "completed", "output": output}}) + "data: [DONE]\n\n"
}

func localPerformanceValidate(stub *timingStreamStub, owner, mode string) error {
	starts, ends := 0, 0
	var body []byte
	for _, frame := range stub.responses {
		if start := frame.GetStart(); start != nil {
			starts++
			if start.GetStatusCode() != http.StatusOK {
				return fmt.Errorf("%s: HTTP %d", owner, start.GetStatusCode())
			}
		}
		if failure := frame.GetError(); failure != nil {
			return fmt.Errorf("%s: error frame %s", owner, failure.GetCode())
		}
		if frame.GetEnd() != nil {
			ends++
		}
		body = append(body, frame.GetBodyChunk()...)
	}
	if starts != 1 || ends != 1 || strings.Count(string(body), "data: [DONE]") != 1 || bytes.Contains(body, []byte("response.failed")) {
		return fmt.Errorf("%s: invalid stream lifecycle: %s", owner, body)
	}
	if !bytes.Contains(body, protocol.JSONBytes(owner)) {
		return fmt.Errorf("%s: response does not contain request owner", owner)
	}
	if mode == "passthrough" {
		expected := streamData(map[string]any{"type": "response.output_text.delta", "delta": owner}) + localPerformanceTerminal(owner, mode)
		if string(body) != expected {
			return fmt.Errorf("%s: passthrough stream changed bytes", owner)
		}
	} else if !bytes.Contains(body, []byte("\"name\":\"echo\"")) || bytes.Contains(body, []byte("run_officejs")) {
		return fmt.Errorf("%s: native tool response was not transformed correctly", owner)
	}
	return nil
}
