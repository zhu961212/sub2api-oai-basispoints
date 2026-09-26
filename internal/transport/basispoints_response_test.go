package transport

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestBasisPointsHTTP200AccountSignalsAreIsolated(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		for _, wire := range []string{"json", "sse"} {
			for _, image := range []bool{false, true} {
				for _, transform := range []bool{false, true} {
					t.Run(fmt.Sprintf("status=%d/wire=%s/image=%t/transform=%t", status, wire, image, transform), func(t *testing.T) {
						imageBytes, inlineImage := relayTestImage(t)
						var requests atomic.Int32
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.URL.Path == "/basispoints/api/attachments" {
								relayTestUpload(t, w, r, imageBytes, "file-isolation")
								return
							}
							requests.Add(1)
							setCodexQuotaHeaders(w.Header())
							w.Header().Set("X-Codex-Turn-State", "keep-turn")
							code := map[int]string{401: "invalid_api_key", 403: "workspace_suspended", 429: "rate_limit_exceeded"}[status]
							failure := map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_fail", "status": "failed", "output": []any{}, "error": map[string]any{"code": code, "status_code": status, "type": "rate_limit_error", "message": "PRIVATE rate_limit unauthorized permission diagnostic", "resets_at": 4102444800}}}
							if wire == "sse" {
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = fmt.Fprintf(w, "event: response.failed\ndata: %s\n\ndata: [DONE]\n\n", protocol.JSONBytes(failure))
							} else {
								_, _ = w.Write(protocol.JSONBytes(failure["response"]))
							}
						}))
						defer upstream.Close()
						tr := New()
						defer tr.Shutdown()
						applyConfig(t, tr, map[string]any{"responses_url": upstream.URL + "/basispoints/api/responses", "transform_responses": transform, "rewrite_tools": false, "auto_select_new_accounts": true, "account_ids": []int64{7}})
						source := map[string]any{"model": "gpt-6-astra", "input": "hi", "stream": wire == "sse"}
						if image {
							source, _ = protocol.RawObject(relayTestBody(inlineImage))
							source["stream"] = wire == "sse"
						}
						frames := requestFrames(t, "https://host.example.invalid/backend-api/codex/responses", token(t, "isolation-account"), nil, protocol.JSONBytes(source))
						frames[0].GetStart().AccountId = 999
						result := runForward(t, tr, frames)
						if result.errFrame != nil || result.status != http.StatusOK || !result.ended {
							t.Fatalf("failure framing changed: %+v", result)
						}
						if requests.Load() != 1 {
							t.Fatalf("unexpected request replay: %d", requests.Load())
						}
						body := string(result.body)
						for _, forbidden := range []string{"PRIVATE", "rate_limit", "invalid_api_key", "workspace_suspended", "permission", "status_code", "resets_at"} {
							if strings.Contains(body, forbidden) {
								t.Fatalf("host-account diagnostic leaked %q: %s", forbidden, body)
							}
						}
						if !strings.Contains(body, "bps_service_rejected") || !strings.Contains(body, "invalid_request_error") || !strings.Contains(body, fmt.Sprintf("HTTP %d", status)) {
							t.Fatalf("safe failure missing: %s", body)
						}
						if headerValue(result.headers, "X-Codex-Primary-Used-Percent") != "" || headerValue(result.headers, "Retry-After") != "" {
							t.Fatal("BPS quota headers reached the host")
						}
						if headerValue(result.headers, "X-Codex-Turn-State") != "keep-turn" {
							t.Fatal("session header lost")
						}
					})
				}
			}
		}
	}
}

func TestBasisPointsResponseReaderPreservesOrdinaryBytesAndErrors(t *testing.T) {
	normal := "id: chunk\r\nevent: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"rate_limit invalid_api_key workspace_suspended\"}\r\n\r\n"
	unknown := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"some unknown failure\"}}}\n\n"
	for _, raw := range []string{normal, unknown, normal + unknown, ": keepalive\n\n", "data: [DONE]\n\n", strings.TrimSuffix(unknown, "\n\n")} {
		reader := &basisPointsResponseReader{body: io.NopCloser(strings.NewReader(raw)), max: 1 << 20}
		got, err := io.ReadAll(reader)
		if err != nil || string(got) != raw {
			t.Fatalf("ordinary bytes changed: %q %v", got, err)
		}
	}
	original := errors.New("source read failure")
	reader := &basisPointsResponseReader{body: io.NopCloser(&relayStepReader{steps: []relayReadStep{{data: normal, err: original}}}), max: 1 << 20}
	got, err := io.ReadAll(reader)
	if !errors.Is(err, original) || !bytes.Equal(got, []byte(normal)) {
		t.Fatalf("read error or partial output lost: %q %v", got, err)
	}
	reader = &basisPointsResponseReader{body: io.NopCloser(strings.NewReader(normal)), max: len(normal) - 1}
	if _, err = io.ReadAll(reader); !errors.Is(err, errBasisPointsResponseLimit) {
		t.Fatalf("missing size failure: %v", err)
	}
}

func TestBasisPointsFlatStreamFailureKeepsRequestScopedType(t *testing.T) {
	raw := []byte("{\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"PRIVATE\"}")
	safe, changed := isolateBasisPointsFailureJSON(raw, "error")
	if !changed {
		t.Fatal("flat error was not isolated")
	}
	result := runRelayReader(t, strings.NewReader("event: error\ndata: "+string(safe)+"\n\n"), 1<<20)
	events := parsedStreamEvents(t, result)
	if len(events) != 1 || events[0]["type"] != "response.failed" {
		t.Fatalf("terminal lost: %s", result.body)
	}
	failure := relayObject(relayObject(events[0]["response"])["error"])
	if failure["code"] != "bps_service_rejected" || failure["type"] != "invalid_request_error" {
		t.Fatalf("host no-failover type lost: %s", result.body)
	}
}

func TestToolCorrectionBPSRejectionStaysRequestScoped(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		for _, wire := range []string{"http", "sse", "sse_completed", "sse_done", "json"} {
			for _, priorOutput := range []bool{false, true} {
				t.Run(fmt.Sprintf("status=%d/wire=%s/output=%t", status, wire, priorOutput), func(t *testing.T) {
					source := executorOnlyCatalogSource(t.Name())
					source["stream"] = true
					original := map[string]any{"id": "resp_repair_reject", "status": "completed", "output": []any{relayNativeCall("call_bad", "exec_command", map[string]any{"cmd": "pwd"})}}
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream")
						if calls.Add(1) == 1 {
							if priorOutput {
								_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.output_text.delta", "delta": "working"}))
							}
							_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.completed", "response": original}))
							return
						}
						if wire == "http" {
							w.WriteHeader(status)
							_, _ = io.WriteString(w, "PRIVATE")
							return
						}
						failure := map[string]any{"status": "failed", "error": map[string]any{"status_code": status, "code": "rate_limit_exceeded", "message": "PRIVATE"}}
						if strings.HasPrefix(wire, "sse") {
							event := map[string]string{"sse": "response.failed", "sse_completed": "response.completed", "sse_done": "response.done"}[wire]
							_, _ = io.WriteString(w, streamData(map[string]any{"type": event, "response": failure}))
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(protocol.JSONBytes(failure))
					}))
					defer upstream.Close()
					tr := New()
					defer tr.Shutdown()
					applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
					result := runForward(t, tr, requestFrames(t, "https://host.invalid/v1/responses", token(t, "repair-rejection"), nil, protocol.JSONBytes(source)))
					if calls.Load() != 2 {
						t.Fatalf("unexpected correction attempts: %d", calls.Load())
					}
					if !priorOutput {
						if result.errFrame == nil || result.errFrame.GetCode() != "bps_service_rejected" || !result.errFrame.GetRequestSent() {
							t.Fatalf("missing nonreplayable error frame: %+v", result)
						}
						if !strings.Contains(result.errFrame.GetMessage(), fmt.Sprintf("HTTP %d", status)) {
							t.Fatal("safe status lost")
						}
					} else {
						events := parsedStreamEvents(t, result)
						failure := relayObject(relayObject(events[len(events)-1]["response"])["error"])
						if failure["type"] != "invalid_request_error" || failure["code"] != "bps_service_rejected" {
							t.Fatalf("correction lost account isolation: %s", result.body)
						}
						if !strings.Contains(string(result.body), fmt.Sprintf("HTTP %d", status)) {
							t.Fatal("safe status lost")
						}
					}
					if strings.Contains(string(result.body), "PRIVATE") || strings.Contains(string(result.body), "rate_limit") {
						t.Fatal("private account diagnostic leaked")
					}
				})
			}
		}
	}
}

func TestBasisPointsHTTPErrorBareAccessStateIsIsolated(t *testing.T) {
	for _, status := range []int{400, 404, 500} {
		for _, raw := range []string{
			"{\"detail\":{\"code\":\"account_deactivated\",\"message\":\"PRIVATE\"}}",
			"{\"code\":\"api_key_disabled\",\"message\":\"PRIVATE\"}",
		} {
			response := &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(raw))}
			if err := prepareBasisPointsResponse(response, 1<<20); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != status || !bytes.Contains(got, []byte("bps_service_rejected")) || bytes.Contains(got, []byte("PRIVATE")) || bytes.Contains(got, []byte("api_key_disabled")) || bytes.Contains(got, []byte("account_deactivated")) {
				t.Fatalf("bare HTTP error leaked state: %s", got)
			}
		}
	}
}

func TestBasisPointsResponseIsolationOnlyTouchesFailureFields(t *testing.T) {
	for _, event := range []string{"error", "response.failed", "response.completed"} {
		output := []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "rate_limit invalid_api_key"}}}}
		payload := map[string]any{"type": event, "status_code": 429, "code": "rate_limit_exceeded", "message": "PRIVATE", "detail": map[string]any{"code": "account_disabled"}, "response": map[string]any{"status": "failed", "output": output, "usage": map[string]any{"input_tokens": 7}, "error": map[string]any{"code": "rate_limit_exceeded"}}}
		result, changed := isolateBasisPointsFailureJSON(protocol.JSONBytes(payload), event)
		if !changed {
			t.Fatal("failure not isolated")
		}
		parsed, _ := protocol.RawObject(result)
		response := relayObject(parsed["response"])
		if !bytes.Equal(protocol.JSONBytes(response["output"]), protocol.JSONBytes(output)) || !bytes.Equal(protocol.JSONBytes(response["usage"]), protocol.JSONBytes(map[string]any{"input_tokens": 7})) {
			t.Fatal("output or usage changed")
		}
		if basisPointsFailureStatus(parsed, event) != 0 {
			t.Fatalf("isolation did not remove account-state signals: %s", result)
		}
	}
	success := []byte("{\"status\":\"completed\",\"output_text\":\"rate_limit invalid_api_key\",\"output\":[]}")
	result, changed := isolateBasisPointsFailureJSON(success, "response.completed")
	if changed || !bytes.Equal(result, success) {
		t.Fatal("success payload changed")
	}
}
