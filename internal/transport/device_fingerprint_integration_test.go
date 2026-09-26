package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

type deviceIntegrationRequest struct {
	header http.Header
	body   map[string]any
	raw    []byte
	path   string
}

func captureDeviceIntegrationRequest(t *testing.T, r *http.Request) deviceIntegrationRequest {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read upstream request: %v", err)
	}
	body, err := protocol.RawObject(raw)
	if err != nil {
		t.Errorf("decode upstream request: %v", err)
	}
	return deviceIntegrationRequest{header: r.Header.Clone(), body: body, raw: raw, path: r.URL.Path}
}

func deviceIntegrationResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(protocol.JSONBytes(map[string]any{"id": "resp_device", "status": "completed", "output": []any{
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "iPhone 17"}}},
	}}))
}

func deviceIntegrationHeaders(client, session string) map[string]string {
	return map[string]string{
		"X-Codex-Installation-Id": client + "-installation",
		"X-Device-ID":             client + "-device",
		"X-OpenAI-Device-ID":      client + "-openai",
		"X-Codex-Session-Id":      session,
		"X-Thread-Id":             client + "-thread",
		"X-Codex-Window-Id":       client + "-window",
		"X-Codex-Turn-Id":         client + "-turn",
		"X-Codex-Turn-Metadata":   string(protocol.JSONBytes(map[string]any{"installation_id": client + "-embedded", "device_id": client + "-alias", "session_id": session, "task_id": client + "-task"})),
	}
}

func assertDeviceIntegrationHeader(t *testing.T, got http.Header, device string, incoming map[string]string) {
	t.Helper()
	for _, name := range []string{"X-Codex-Installation-Id", "X-Device-ID", "X-OpenAI-Device-ID"} {
		if got.Get(name) != device {
			t.Errorf("%s=%q, want account device %q", name, got.Get(name), device)
		}
	}
	for _, name := range []string{"X-Codex-Session-Id", "X-Thread-Id", "X-Codex-Window-Id", "X-Codex-Turn-Id"} {
		if got.Get(name) != incoming[name] {
			t.Errorf("device rewrite changed %s: %q", name, got.Get(name))
		}
	}
	metadata, err := protocol.RawObject([]byte(got.Get("X-Codex-Turn-Metadata")))
	if err != nil {
		t.Fatalf("invalid turn metadata: %v", err)
	}
	original, _ := protocol.RawObject([]byte(incoming["X-Codex-Turn-Metadata"]))
	if metadata["installation_id"] != device || metadata["device_id"] != device || metadata["session_id"] != original["session_id"] || metadata["task_id"] != original["task_id"] {
		t.Errorf("turn metadata changed beyond its device identity: %#v", metadata)
	}
}

func TestForwardDeviceIdentityIsStablePerAccountAcrossClients(t *testing.T) {
	for _, rewrite := range []bool{true, false} {
		t.Run(fmt.Sprintf("rewrite_tools=%t", rewrite), func(t *testing.T) {
			captured := make(chan deviceIntegrationRequest, 4)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- captureDeviceIntegrationRequest(t, r)
				deviceIntegrationResponse(w)
			}))
			defer upstream.Close()
			tr := New()
			defer tr.Shutdown()
			cfg := map[string]any{"responses_url": upstream.URL + "/basispoints/api/responses", "bps_device_convergence": true}
			if !rewrite {
				cfg["rewrite_tools"] = false
			}
			applyConfig(t, tr, cfg)
			cases := []struct {
				account, client, session string
				id                       int64
			}{
				{"acct-A", "client-A", "session-A", 7},
				{"acct-A", "client-B", "session-A", 7},
				{"acct-A", "client-C", "session-B", 7},
				{"acct-B", "client-D", "session-C", 9},
			}
			var devices, tasks []any
			for _, tc := range cases {
				incoming := deviceIntegrationHeaders(tc.client, tc.session)
				source := map[string]any{"model": "gpt-6-astra", "input": "same opening message", "stream": false,
					"session_id": tc.session, "prompt_cache_key": "shared-prefill-cache", "device_id": tc.client,
					"metadata":        map[string]any{"device_id": tc.client, "installation_id": tc.client, "session_id": tc.session, "x-codex-turn-metadata": incoming["X-Codex-Turn-Metadata"]},
					"client_metadata": map[string]any{"device_id": tc.client, "session_id": tc.session},
				}
				frames := requestFrames(t, "https://unused.invalid/responses", token(t, tc.account), incoming, protocol.JSONBytes(source))
				frames[0].GetStart().AccountId = tc.id
				result := runForward(t, tr, frames)
				if result.errFrame != nil || result.status != http.StatusOK || !result.ended {
					t.Fatalf("forward failed: %+v", result)
				}
				got := <-captured
				want := deriveBPSDeviceUUID("sub2api:bps-install-id:v1:" + tc.account)
				assertDeviceIntegrationHeader(t, got.header, want, incoming)
				if got.header.Get("ChatGPT-Account-ID") != tc.account {
					t.Fatal("device preparation changed scheduled account")
				}
				metadata, ok := got.body["metadata"].(map[string]any)
				if !ok || metadata["device_id"] != want || metadata["installation_id"] != want || metadata["session_id"] != tc.session {
					t.Fatalf("body identity was not isolated: %#v", got.body)
				}
				embedded, err := protocol.RawObject([]byte(metadata["x-codex-turn-metadata"].(string)))
				if err != nil || embedded["installation_id"] != want || embedded["device_id"] != want || embedded["session_id"] != tc.session {
					t.Fatalf("embedded body device mismatch: %#v %v", embedded, err)
				}
				if got.body["prompt_cache_key"] != "shared-prefill-cache" || got.body["__bps_session_scope"] != nil {
					t.Fatalf("cache key or private session marker changed: %#v", got.body)
				}
				if !rewrite {
					clientMetadata, _ := got.body["client_metadata"].(map[string]any)
					if got.body["device_id"] != want || got.body["session_id"] != tc.session || clientMetadata["device_id"] != want || clientMetadata["session_id"] != tc.session || got.body["input"] != source["input"] {
						t.Fatalf("non-rewriting body leaked device or changed session/input: %#v", got.body)
					}
				}
				devices = append(devices, got.header.Get("X-Codex-Installation-Id"))
				tasks = append(tasks, metadata["task_id"])
			}
			if devices[0] != devices[1] || devices[0] != devices[2] || devices[0] == devices[3] {
				t.Fatalf("device identity follows client or is shared across accounts: %v", devices)
			}
			if rewrite && (tasks[0] == nil || tasks[0] != tasks[1] || tasks[0] == tasks[2]) {
				t.Fatalf("device identity collapsed independent conversations: %v", tasks)
			}
		})
	}
}

func TestForwardDeviceIdentityKeepsRawBodyWithoutDeviceCarriers(t *testing.T) {
	captured := make(chan deviceIntegrationRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- captureDeviceIntegrationRequest(t, r)
		deviceIntegrationResponse(w)
	}))
	defer upstream.Close()
	tr := New()
	defer tr.Shutdown()
	applyConfig(t, tr, map[string]any{"responses_url": upstream.URL, "rewrite_tools": false, "transform_responses": false, "bps_device_convergence": true})
	source, _ := json.MarshalIndent(map[string]any{"model": "gpt-6-astra", "input": "raw body", "session_id": "raw-session", "prompt_cache_key": "raw-cache"}, "", "  ")
	incoming := deviceIntegrationHeaders("raw-client", "raw-session")
	result := runForward(t, tr, requestFrames(t, "https://unused.invalid/responses", token(t, "raw-account"), incoming, source))
	if result.errFrame != nil || result.status != http.StatusOK {
		t.Fatalf("raw forward failed: %+v", result)
	}
	got := <-captured
	if !bytes.Equal(got.raw, source) {
		t.Fatal("device header insertion unnecessarily rebuilt the no-image raw body")
	}
	assertDeviceIntegrationHeader(t, got.header, deriveBPSDeviceUUID("sub2api:bps-install-id:v1:raw-account"), incoming)
}

func TestForwardAndDegradationProbeUseSameTrustedAccountDevice(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(fmt.Sprintf("trusted_metadata=%t", trusted), func(t *testing.T) {
			captured := make(chan deviceIntegrationRequest, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- captureDeviceIntegrationRequest(t, r)
				deviceIntegrationResponse(w)
			}))
			defer upstream.Close()
			account := &pluginv1.AccountInfo{Id: 7, Schedulable: true}
			want := deriveBPSDeviceUUID("sub2api:bps-install-id:v1:acct-probe")
			if trusted {
				want = "1f4a8c90-5382-4b5d-8abd-9034adf60123"
				account.MetadataJson = protocol.JSONBytes(map[string]any{"extra": map[string]any{"openai_device_id": want}})
			}
			host := &fakeHost{accounts: []*pluginv1.AccountInfo{account}, tokenFor: map[int64]string{7: token(t, "acct-probe")}}
			tr := New()
			defer tr.Shutdown()
			tr.host = host
			applyConfig(t, tr, map[string]any{"responses_url": upstream.URL, "bps_device_convergence": true})
			source := protocol.JSONBytes(map[string]any{"model": "gpt-6-astra", "input": "normal conversation", "metadata": map[string]any{"device_id": "client-device"}})
			result := runForward(t, tr, requestFrames(t, "https://unused.invalid/responses", "", deviceIntegrationHeaders("normal-client", "normal-session"), source))
			if result.errFrame != nil || result.status != http.StatusOK {
				t.Fatalf("normal forward failed: %+v", result)
			}
			cfg := protocol.DefaultConfig()
			cfg.ResponsesURL = upstream.URL
			cfg.BPSDeviceConvergence = true
			status, answer, err := tr.checkDegradationAccount(context.Background(), cfg, host, tr.client, 7, "gpt-6-astra")
			if err != nil || status != "ok" || answer != "iPhone 17" {
				t.Fatalf("probe failed: status=%s answer=%q err=%v", status, answer, err)
			}
			normal, probe := <-captured, <-captured
			for _, request := range []deviceIntegrationRequest{normal, probe} {
				if request.header.Get("X-Codex-Installation-Id") != want || request.header.Get("ChatGPT-Account-ID") != "acct-probe" {
					t.Fatalf("normal/probe used a different device or account: %v", request.header)
				}
			}
			if probe.header.Get("X-Codex-Session-Id") != "" {
				t.Fatal("probe inherited normal client session")
			}
			normalMetadata, _ := normal.body["metadata"].(map[string]any)
			probeMetadata, _ := probe.body["metadata"].(map[string]any)
			if normalMetadata["device_id"] != want || normalMetadata["task_id"] == probeMetadata["task_id"] {
				t.Fatal("normal/probe body device or independent task identity is incorrect")
			}
		})
	}
}

func TestImageAttachmentAndResponsesShareAccountDevice(t *testing.T) {
	for _, rewrite := range []bool{true, false} {
		t.Run(fmt.Sprintf("rewrite_tools=%t", rewrite), func(t *testing.T) {
			imageBytes, inlineImage := relayTestImage(t)
			captured := make(chan deviceIntegrationRequest, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/basispoints/api/attachments" {
					captured <- deviceIntegrationRequest{header: r.Header.Clone(), path: r.URL.Path}
					relayTestUpload(t, w, r, imageBytes, "file-device-integration")
					return
				}
				captured <- captureDeviceIntegrationRequest(t, r)
				deviceIntegrationResponse(w)
			}))
			defer upstream.Close()
			tr := New()
			defer tr.Shutdown()
			applyConfig(t, tr, map[string]any{"responses_url": upstream.URL + "/basispoints/api/responses", "rewrite_tools": rewrite, "bps_device_convergence": true})
			source, _ := protocol.RawObject(relayTestBody(inlineImage))
			source["metadata"] = map[string]any{"device_id": "image-client-device"}
			incoming := deviceIntegrationHeaders("image-client", "image-session")
			result := runForward(t, tr, requestFrames(t, "https://unused.invalid/responses", token(t, "image-account"), incoming, protocol.JSONBytes(source)))
			if result.errFrame != nil || result.status != http.StatusOK {
				t.Fatalf("image forward failed: %+v", result)
			}
			attachment, response := <-captured, <-captured
			if attachment.path != "/basispoints/api/attachments" || response.path != "/basispoints/api/responses" {
				t.Fatalf("wrong image request sequence: %s then %s", attachment.path, response.path)
			}
			want := deriveBPSDeviceUUID("sub2api:bps-install-id:v1:image-account")
			assertDeviceIntegrationHeader(t, attachment.header, want, incoming)
			assertDeviceIntegrationHeader(t, response.header, want, incoming)
			metadata, _ := response.body["metadata"].(map[string]any)
			ids := relayTestFileIDs(response.body)
			if metadata["device_id"] != want || len(ids) != 1 || ids[0] != "file-device-integration" {
				t.Fatalf("response body lost image or device: %#v", response.body)
			}
		})
	}
}

func TestNonBPSPassthroughPreservesClientDeviceBytes(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		ids         []int64
	}{{"unselected_account", "gpt-6-astra", []int64{99}}, {"other_model", "gpt-5.4", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			captured := make(chan deviceIntegrationRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- captureDeviceIntegrationRequest(t, r)
				w.WriteHeader(http.StatusAccepted)
			}))
			defer upstream.Close()
			tr := New()
			defer tr.Shutdown()
			applyConfig(t, tr, map[string]any{"responses_url": "http://must-not-call.invalid/responses", "account_ids": tc.ids, "bps_device_convergence": true})
			incoming := deviceIntegrationHeaders("passthrough-client", "passthrough-session")
			source, _ := json.MarshalIndent(map[string]any{"model": tc.model, "input": "passthrough", "device_id": "original-device", "metadata": map[string]any{"installation_id": "original-installation"}}, "", "  ")
			result := runForward(t, tr, requestFrames(t, upstream.URL, token(t, "passthrough-account"), incoming, source))
			if result.errFrame != nil || result.status != http.StatusAccepted {
				t.Fatalf("passthrough failed: %+v", result)
			}
			got := <-captured
			if !bytes.Equal(got.raw, source) {
				t.Fatal("non-BPS body device data or encoding changed")
			}
			for key, value := range incoming {
				if got.header.Get(key) != value {
					t.Errorf("passthrough changed %s: %q", key, got.header.Get(key))
				}
			}
		})
	}
}

func TestBPSRetryAndToolRepairKeepPreparedAccountDevice(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(fmt.Sprintf("tool_repair=%t", repair), func(t *testing.T) {
			captured := make(chan deviceIntegrationRequest, 3)
			attempts := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- captureDeviceIntegrationRequest(t, r)
				attempts++
				if !repair {
					if attempts == 1 {
						w.Header().Set("Retry-After", "0")
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					deviceIntegrationResponse(w)
					return
				}
				call := relayNativeCall("call_wrong", "exec_command", map[string]any{"cmd": "pwd"})
				if attempts > 1 {
					call = relayNativeCall("call_fixed", "functions.exec", "text(await tools.exec_command({cmd: 'pwd'}));")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_repair_device", "status": "completed", "output": []any{call}}}))
			}))
			defer upstream.Close()
			tr := New()
			defer tr.Shutdown()
			applyConfig(t, tr, map[string]any{"responses_url": upstream.URL, "bps_device_convergence": true})
			source := executorOnlyCatalogSource(t.Name())
			source["stream"] = false
			source["metadata"] = map[string]any{"device_id": "retry-client"}
			incoming := deviceIntegrationHeaders("retry-client", "retry-session")
			result := runForward(t, tr, requestFrames(t, "https://unused.invalid/responses", token(t, "retry-account"), incoming, protocol.JSONBytes(source)))
			if result.errFrame != nil || result.status != http.StatusOK || len(captured) != 2 {
				t.Fatalf("retry/repair failed or attempt count changed: captured=%d result=%+v", len(captured), result)
			}
			first, second := <-captured, <-captured
			want := deriveBPSDeviceUUID("sub2api:bps-install-id:v1:retry-account")
			assertDeviceIntegrationHeader(t, first.header, want, incoming)
			assertDeviceIntegrationHeader(t, second.header, want, incoming)
			firstMetadata, _ := first.body["metadata"].(map[string]any)
			secondMetadata, _ := second.body["metadata"].(map[string]any)
			if firstMetadata["device_id"] != want || !bytes.Equal(protocol.JSONBytes(firstMetadata), protocol.JSONBytes(secondMetadata)) {
				t.Fatal("retry/repair changed prepared device, task or turn metadata")
			}
			if !repair && !bytes.Equal(first.raw, second.raw) {
				t.Fatal("HTTP retry changed prepared body bytes")
			}
		})
	}
}
