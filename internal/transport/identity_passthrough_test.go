package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

type identityRequest struct {
	header http.Header
	body   map[string]any
	raw    []byte
	path   string
}

func captureIdentityRequest(t *testing.T, r *http.Request) identityRequest {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read upstream request: %v", err)
	}
	body, err := protocol.RawObject(raw)
	if err != nil {
		t.Errorf("decode upstream request: %v", err)
	}
	return identityRequest{header: r.Header.Clone(), body: body, raw: raw, path: r.URL.Path}
}

func identityResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(protocol.JSONBytes(map[string]any{"id": "resp_device", "status": "completed", "output": []any{
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "iPhone 17"}}},
	}}))
}

func identityHeaders(client, session string) map[string]string {
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

func assertIdentityHeaders(t *testing.T, got http.Header, incoming map[string]string) {
	t.Helper()
	for name, want := range incoming {
		if got.Get(name) != want {
			t.Errorf("forwarding changed %s: %q, want %q", name, got.Get(name), want)
		}
	}
}
func TestForwardBPSKeepsRawBodyAndClientIdentity(t *testing.T) {
	captured := make(chan identityRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- captureIdentityRequest(t, r)
		identityResponse(w)
	}))
	defer upstream.Close()
	tr := New()
	defer tr.Shutdown()
	applyConfig(t, tr, map[string]any{"responses_url": upstream.URL, "rewrite_tools": false, "transform_responses": false})
	source, _ := json.MarshalIndent(map[string]any{"model": "gpt-6-astra", "input": "raw body", "session_id": "raw-session", "prompt_cache_key": "raw-cache"}, "", "  ")
	incoming := identityHeaders("raw-client", "raw-session")
	result := runForward(t, tr, requestFrames(t, "https://unused.invalid/responses", token(t, "raw-account"), incoming, source))
	if result.errFrame != nil || result.status != http.StatusOK {
		t.Fatalf("raw forward failed: %+v", result)
	}
	got := <-captured
	if !bytes.Equal(got.raw, source) {
		t.Fatal("forwarding unnecessarily rebuilt the no-image raw body")
	}
	assertIdentityHeaders(t, got.header, incoming)
}

func TestImageAttachmentAndResponsesPreserveClientIdentity(t *testing.T) {
	for _, rewrite := range []bool{true, false} {
		t.Run(fmt.Sprintf("rewrite_tools=%t", rewrite), func(t *testing.T) {
			imageBytes, inlineImage := relayTestImage(t)
			captured := make(chan identityRequest, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/basispoints/api/attachments" {
					captured <- identityRequest{header: r.Header.Clone(), path: r.URL.Path}
					relayTestUpload(t, w, r, imageBytes, "file-device-integration")
					return
				}
				captured <- captureIdentityRequest(t, r)
				identityResponse(w)
			}))
			defer upstream.Close()
			tr := New()
			defer tr.Shutdown()
			applyConfig(t, tr, map[string]any{"responses_url": upstream.URL + "/basispoints/api/responses", "rewrite_tools": rewrite})
			source, _ := protocol.RawObject(relayTestBody(inlineImage))
			source["metadata"] = map[string]any{"device_id": "image-client-device"}
			incoming := identityHeaders("image-client", "image-session")
			result := runForward(t, tr, requestFrames(t, "https://unused.invalid/responses", token(t, "image-account"), incoming, protocol.JSONBytes(source)))
			if result.errFrame != nil || result.status != http.StatusOK {
				t.Fatalf("image forward failed: %+v", result)
			}
			attachment, response := <-captured, <-captured
			if attachment.path != "/basispoints/api/attachments" || response.path != "/basispoints/api/responses" {
				t.Fatalf("wrong image request sequence: %s then %s", attachment.path, response.path)
			}
			assertIdentityHeaders(t, attachment.header, incoming)
			assertIdentityHeaders(t, response.header, incoming)
			metadata, _ := response.body["metadata"].(map[string]any)
			ids := relayTestFileIDs(response.body)
			if metadata["device_id"] != "image-client-device" || len(ids) != 1 || ids[0] != "file-device-integration" {
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
			captured := make(chan identityRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- captureIdentityRequest(t, r)
				w.WriteHeader(http.StatusAccepted)
			}))
			defer upstream.Close()
			tr := New()
			defer tr.Shutdown()
			applyConfig(t, tr, map[string]any{"responses_url": "http://must-not-call.invalid/responses", "account_ids": tc.ids})
			incoming := identityHeaders("passthrough-client", "passthrough-session")
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

func TestBPSRetryAndToolRepairPreserveClientIdentity(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(fmt.Sprintf("tool_repair=%t", repair), func(t *testing.T) {
			captured := make(chan identityRequest, 3)
			attempts := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- captureIdentityRequest(t, r)
				attempts++
				if !repair {
					if attempts == 1 {
						w.Header().Set("Retry-After", "0")
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					identityResponse(w)
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
			applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
			source := executorOnlyCatalogSource(t.Name())
			source["stream"] = false
			source["metadata"] = map[string]any{"device_id": "retry-client"}
			incoming := identityHeaders("retry-client", "retry-session")
			result := runForward(t, tr, requestFrames(t, "https://unused.invalid/responses", token(t, "retry-account"), incoming, protocol.JSONBytes(source)))
			if result.errFrame != nil || result.status != http.StatusOK || len(captured) != 2 {
				t.Fatalf("retry/repair failed or attempt count changed: captured=%d result=%+v", len(captured), result)
			}
			first, second := <-captured, <-captured
			assertIdentityHeaders(t, first.header, incoming)
			assertIdentityHeaders(t, second.header, incoming)
			firstMetadata, _ := first.body["metadata"].(map[string]any)
			secondMetadata, _ := second.body["metadata"].(map[string]any)
			if firstMetadata["device_id"] != "retry-client" || !bytes.Equal(protocol.JSONBytes(firstMetadata), protocol.JSONBytes(secondMetadata)) {
				t.Fatal("retry/repair changed client identity, task or turn metadata")
			}
			if !repair && !bytes.Equal(first.raw, second.raw) {
				t.Fatal("HTTP retry changed prepared body bytes")
			}
		})
	}
}
