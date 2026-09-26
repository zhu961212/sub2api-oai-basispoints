package transport

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/net/http/httpguts"
)

func deviceTestJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func deviceTestObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		t.Fatal(err)
	}
	return object
}

func TestBPSDeviceHeadersUnifyAliasesAndKeepSessionIdentity(t *testing.T) {
	const installation = "account-installation"
	originalMetadata := map[string]any{
		"installation_id": "old-installation", "deviceId": "old-device",
		"session_id": "session-one", "thread_id": "thread-one", "turn_id": "turn-one", "window_id": "window-one",
		"turn_started_at_unix_ms": json.Number("9007199254740993"),
		"sandbox":                 map[string]any{"device_id": "nested-user-value"},
	}
	h := http.Header{
		"X-Codex-Installation-Id": {"old-1", "old-2"},
		"x-codex-installation-id": {"old-3"},
		"X-CODEX-INSTALLATION-ID": {"old-4"},
		"X-Device-Id":             {"old-5", "old-6"},
		"x-device-id":             {"old-7"},
		"X-OpenAI-Device-ID":      {"old-8"},
		"oai-device-id":           {"old-9"},
		"device_id":               {"old-10"},
		"installationId":          {"old-11"},
		"X-Codex-Turn-Metadata":   {deviceTestJSON(t, originalMetadata), deviceTestJSON(t, map[string]any{"installation_id": "old-12"})},
		"x-codex-turn-metadata":   {deviceTestJSON(t, map[string]any{"installation_id": "old-13"})},
		"Session-Id":              {"session-one", "session-two"},
		"session_id":              {"session-under"},
		"X-Codex-Window-Id":       {"window-one"},
		"X-Client-Request-Id":     {"request-one"},
		"X-Codex-Turn-Id":         {"turn-one"},
		"X-Session-Id":            {"session-alias"},
	}
	unrelated := make(http.Header)
	for key, values := range h {
		if !isBPSDeviceField(key) && !isBPSTurnMetadataField(key) {
			unrelated[key] = append([]string(nil), values...)
		}
	}
	applyBPSDeviceHeaders(h, installation)
	if !reflect.DeepEqual(h[bpsInstallationHeader], []string{installation}) {
		t.Fatalf("canonical installation header: %v", h[bpsInstallationHeader])
	}
	for key, values := range h {
		if isBPSDeviceField(key) && (key != http.CanonicalHeaderKey(key) || !reflect.DeepEqual(values, []string{installation})) {
			t.Errorf("device alias retained repeated/case-specific identity: %s=%v", key, values)
		}
	}
	if len(h[bpsTurnMetadataHeader]) != 1 || h["x-codex-turn-metadata"] != nil {
		t.Fatalf("duplicate turn metadata survived: %v", h)
	}
	wantMetadata := originalMetadata
	wantMetadata["installation_id"], wantMetadata["deviceId"] = installation, installation
	if got := deviceTestObject(t, h.Get(bpsTurnMetadataHeader)); !reflect.DeepEqual(got, wantMetadata) {
		t.Errorf("embedded metadata changed non-device values: got=%v want=%v", got, wantMetadata)
	}
	for key, values := range unrelated {
		if !reflect.DeepEqual(h[key], values) {
			t.Errorf("session field %s changed: %v", key, h[key])
		}
	}
	first := deviceTestJSON(t, h)
	applyBPSDeviceHeaders(h, installation)
	if deviceTestJSON(t, h) != first {
		t.Fatal("header convergence is not idempotent")
	}
}

func TestBPSDeviceBodyOnlyUpdatesExplicitCarriers(t *testing.T) {
	const installation = "account-installation"
	embeddedString := deviceTestJSON(t, map[string]any{
		"installation_id": "old", "device_id": "old", "session_id": "session", "thread_id": "thread", "turn_id": "turn",
		"window_id": "window", "prompt_cache_key": "cache", "large": json.Number("9007199254740993"),
	})
	metadata := map[string]any{"installationId": "old", "device_id": "old", "task_id": "bps-task", "turn_id": "bps-turn", "agent_iteration": "4", "x-codex-turn-metadata": embeddedString}
	embeddedObject := map[string]any{"installation_id": "old", "DeviceID": "old", "session_id": "session", "sandbox": map[string]any{"device_id": "user-value"}}
	clientMetadata := map[string]any{"x-codex-installation-id": "old", "xOpenaiDeviceId": "old", "session_id": "client-session", "X-Codex-Turn-Metadata": embeddedObject}
	body := map[string]any{
		"deviceId": "old", "Installation_ID": "old", "metadata": metadata, "client_metadata": clientMetadata,
		"x-codex-turn-metadata": embeddedString,
		"session_id":            "root-session", "thread_id": "root-thread", "conversation_id": "conversation", "prompt_cache_key": "cache",
		"device_identity": "not-an-allowlisted-field",
		"input":           []any{map[string]any{"role": "user", "device_id": "user-device", "content": "installation_id: literal user text"}, map[string]any{"type": "function_call", "arguments": embeddedString}},
		"tools":           []any{map[string]any{"device_id": "tool-device", "parameters": map[string]any{"installation_id": "schema"}}},
	}
	preserved := make(map[string]string)
	for _, key := range []string{"session_id", "thread_id", "conversation_id", "prompt_cache_key", "device_identity", "input", "tools"} {
		preserved[key] = deviceTestJSON(t, body[key])
	}
	if !applyBPSDeviceBody(body, installation) {
		t.Fatal("existing device carriers reported unchanged")
	}
	for _, carrier := range []map[string]any{body, metadata, clientMetadata, embeddedObject} {
		for key, value := range carrier {
			if isBPSDeviceField(key) && value != installation {
				t.Errorf("device alias %s retained %v", key, value)
			}
		}
	}
	for _, carrier := range []map[string]any{body, metadata} {
		got := deviceTestObject(t, carrier["x-codex-turn-metadata"].(string))
		want := deviceTestObject(t, embeddedString)
		want["installation_id"], want["device_id"] = installation, installation
		if !reflect.DeepEqual(got, want) {
			t.Errorf("JSON metadata lost session fields or numeric precision: got=%v want=%v", got, want)
		}
	}
	if metadata["task_id"] != "bps-task" || metadata["turn_id"] != "bps-turn" || metadata["agent_iteration"] != "4" || clientMetadata["session_id"] != "client-session" {
		t.Fatal("body convergence changed generated BPS or client session fields")
	}
	if embeddedObject["sandbox"].(map[string]any)["device_id"] != "user-value" {
		t.Fatal("body convergence recursively changed unrelated nested objects")
	}
	for key, original := range preserved {
		if deviceTestJSON(t, body[key]) != original {
			t.Errorf("non-device root field %s changed", key)
		}
	}
	if applyBPSDeviceBody(body, installation) {
		t.Fatal("body convergence must report no change on the second application")
	}
}

func TestBPSDeviceRewritesMalformedEmbeddedMetadata(t *testing.T) {
	const installation = "account-installation"
	valid := deviceTestJSON(t, map[string]any{"session_id": "session"})
	for _, raw := range []string{"", "null", "[]", "42", "broken", "{broken", valid + " null"} {
		t.Run(raw, func(t *testing.T) {
			want := map[string]any{"installation_id": installation}
			h := http.Header{"x-codex-turn-metadata": {raw, "old-duplicate"}}
			applyBPSDeviceHeaders(h, installation)
			if len(h[bpsTurnMetadataHeader]) != 1 || !reflect.DeepEqual(deviceTestObject(t, h.Get(bpsTurnMetadataHeader)), want) {
				t.Errorf("malformed header did not become minimal device metadata: %v", h)
			}
			body := map[string]any{"client_metadata": map[string]any{"x-codex-turn-metadata": raw}}
			if !applyBPSDeviceBody(body, installation) {
				t.Fatal("malformed body carrier reported unchanged")
			}
			got := body["client_metadata"].(map[string]any)["x-codex-turn-metadata"].(string)
			if !reflect.DeepEqual(deviceTestObject(t, got), want) {
				t.Errorf("malformed body retained old values: %s", got)
			}
		})
	}
	for _, value := range []any{nil, false, 42, []any{"old-device"}, map[string]any(nil)} {
		body := map[string]any{"x-codex-turn-metadata": value}
		if !applyBPSDeviceBody(body, installation) || !reflect.DeepEqual(body["x-codex-turn-metadata"], map[string]any{"installation_id": installation}) {
			t.Errorf("malformed typed metadata not rebuilt: %v", body)
		}
	}
}

func TestBPSDeviceDoesNotCreateBodyCarriersOrTouchMissingIdentity(t *testing.T) {
	const installation = "account-installation"
	for _, body := range []map[string]any{nil, {}, {"metadata": map[string]any{"task_id": "task", "turn_id": "turn"}}, {"client_metadata": nil}, {"metadata": "opaque", "client_metadata": []any{"opaque"}}} {
		original := deviceTestJSON(t, body)
		if applyBPSDeviceBody(body, installation) || deviceTestJSON(t, body) != original {
			t.Errorf("body without device carriers changed: %v", body)
		}
	}
	h := make(http.Header)
	applyBPSDeviceHeaders(h, installation)
	if !reflect.DeepEqual(h, http.Header{bpsInstallationHeader: {installation}}) {
		t.Errorf("headers without carriers gained extra values: %v", h)
	}
	applyBPSDeviceHeaders(nil, installation)
	for _, missing := range []string{"", "  "} {
		h = http.Header{"X-Device-Id": {"original"}}
		applyBPSDeviceHeaders(h, missing)
		if !reflect.DeepEqual(h, http.Header{"X-Device-Id": {"original"}}) {
			t.Error("empty installation overwrote header")
		}
		body := map[string]any{"device_id": "original"}
		if applyBPSDeviceBody(body, missing) || body["device_id"] != "original" {
			t.Error("empty installation overwrote body")
		}
	}
}

func TestBPSDeviceAddsIdentityOnlyInsideExistingTurnMetadata(t *testing.T) {
	const installation = "account-installation"
	for _, encoded := range []bool{false, true} {
		var value any = map[string]any{"session_id": "session", "sandbox": true}
		if encoded {
			value = deviceTestJSON(t, value)
		}
		body := map[string]any{"x-codex-turn-metadata": value}
		if !applyBPSDeviceBody(body, installation) {
			t.Fatal("existing turn metadata did not receive device identity")
		}
		if len(body) != 1 {
			t.Fatalf("body gained a new carrier: %v", body)
		}
		got := body["x-codex-turn-metadata"]
		if encoded {
			got = deviceTestObject(t, got.(string))
		}
		want := map[string]any{"installation_id": installation, "session_id": "session", "sandbox": true}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("existing metadata changed unrelated fields: %v", got)
		}
	}
}

func TestBPSDeviceTurnMetadataKeepsASCIIHeaderSafetyAndUnicodeValues(t *testing.T) {
	const installation = "account-installation"
	escape := string([]byte{92})
	for _, fixture := range []struct{ name, value, wantEscape string }{
		{"DEL", string(rune(0x7f)), escape + "u007f"},
		{"Chinese", "中文", escape + "u4e2d" + escape + "u6587"},
		{"emoji", "😀", escape + "ud83d" + escape + "ude00"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			original := map[string]any{"installation_id": "old", "session_id": fixture.value, "description": fixture.value, "large": json.Number("9007199254740993")}
			raw := deviceTestJSON(t, original)
			// Start with legal ASCII JSON escapes, as actual incoming headers do.
			raw = strings.ReplaceAll(raw, fixture.value, fixture.wantEscape)
			if !httpguts.ValidHeaderFieldValue(raw) {
				t.Fatal("fixture must be an accepted header before rewriting")
			}
			h := http.Header{bpsTurnMetadataHeader: {raw}}
			applyBPSDeviceHeaders(h, installation)
			body := map[string]any{"metadata": map[string]any{"x-codex-turn-metadata": raw}}
			if !applyBPSDeviceBody(body, installation) {
				t.Fatal("embedded string did not change identity")
			}
			encoded := h.Get(bpsTurnMetadataHeader)
			if encoded != body["metadata"].(map[string]any)["x-codex-turn-metadata"] {
				t.Fatal("header and body embedded strings use different encodings")
			}
			if !httpguts.ValidHeaderFieldValue(encoded) || !strings.Contains(encoded, fixture.wantEscape) {
				t.Errorf("rewritten header is unsafe or lost the Unicode escape: %q", encoded)
			}
			for _, value := range []byte(encoded) {
				if value >= 0x7f || value < 0x20 {
					t.Errorf("rewritten metadata contains a literal non-header byte: %d", value)
				}
			}
			original["installation_id"] = installation
			if got := deviceTestObject(t, encoded); !reflect.DeepEqual(got, original) {
				t.Errorf("ASCII serialization changed unrelated values: got=%v want=%v", got, original)
			}
			// Unsafe literals are normalized even if the device already matches.
			body = map[string]any{"x-codex-turn-metadata": deviceTestJSON(t, original)}
			if !applyBPSDeviceBody(body, installation) || body["x-codex-turn-metadata"] != encoded {
				t.Fatal("existing identity bypassed ASCII-safe normalization")
			}
			if applyBPSDeviceBody(body, installation) {
				t.Fatal("normalized ASCII metadata is not idempotent")
			}
		})
	}
}
