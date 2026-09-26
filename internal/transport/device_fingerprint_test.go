package transport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestBPSAccountDeviceIDUsesTrustedMetadata(t *testing.T) {
	const seed = "12345678-1234-4234-8234-123456789abc"
	// Independently computed from the official Sub2API SHA-256/UUID algorithm.
	const derived = "3257e27e-afd8-4a18-bb2f-c6329fd78afd"
	for _, test := range []struct {
		name, casing string
		extra        map[string]any
		want         string
	}{
		{"official exported snapshot", "Extra", map[string]any{"codex_fingerprint_seed": seed}, derived},
		{"lowercase snapshot", "extra", map[string]any{"codex_fingerprint_seed": seed}, derived},
		{"configured device wins", "Extra", map[string]any{"codex_fingerprint_seed": seed, "openai_device_id": " admin-device "}, "admin-device"},
		{"explicit BPS device without host opt-in", "Extra", map[string]any{"openai_device_id": "admin-device", "codex_fingerprint_mode": "off"}, "admin-device"},
		{"invalid device uses seed", "Extra", map[string]any{"codex_fingerprint_seed": seed, "openai_device_id": string([]byte{'x', 13, 10, 'y'})}, derived},
		{"oversized device uses seed", "Extra", map[string]any{"codex_fingerprint_seed": seed, "openai_device_id": strings.Repeat("x", 513)}, derived},
		{"numeric fields ignored", "Extra", map[string]any{"codex_fingerprint_seed": 42, "openai_device_id": 42}, ""},
		{"noncanonical seed ignored", "Extra", map[string]any{"codex_fingerprint_seed": strings.ToUpper(seed)}, ""},
		{"nil seed ignored", "Extra", map[string]any{"codex_fingerprint_seed": "00000000-0000-0000-0000-000000000000"}, ""},
		{"malformed seed ignored", "Extra", map[string]any{"codex_fingerprint_seed": "not-a-uuid"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{test.casing: test.extra})
			if err != nil {
				t.Fatal(err)
			}
			if got := bpsAccountDeviceID(raw); got != test.want {
				t.Fatalf("device=%q want %q", got, test.want)
			}
		})
	}
	for _, raw := range []string{"null", "[]", "{", "{}"} {
		if got := bpsAccountDeviceID([]byte(raw)); got != "" {
			t.Fatalf("invalid metadata produced %q", got)
		}
	}
}

func TestPrepareBPSHeadersStableAcrossClientsAndRestart(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy fallback", true: "host configured"}[configured], func(t *testing.T) {
			var devices []string
			for _, credential := range []string{"first-access-token", "refreshed-access-token"} {
				tr := New()
				var host pluginv1.HostServiceClient
				if configured {
					raw, _ := json.Marshal(map[string]any{"Extra": map[string]any{"openai_device_id": "configured-device-7", "codex_fingerprint_seed": "12345678-1234-4234-8234-123456789abc"}})
					host = &fakeHost{accounts: []*pluginv1.AccountInfo{{Id: 7, MetadataJson: raw}, {Id: 9}}}
					tr.host = host
				}
				start := &pluginv1.ForwardRequestStart{AccountId: 7, Headers: map[string]*pluginv1.HeaderValues{
					"Authorization":           {Values: []string{"Bearer " + credential}},
					"ChatGPT-Account-ID":      {Values: []string{"acct-7"}},
					"X-Codex-Installation-ID": {Values: []string{"untrusted-client-" + credential}},
					"X-Codex-Window-ID":       {Values: []string{"independent-window"}},
				}}
				headers, _, err := tr.prepareBPSHeaders(context.Background(), start, host, protocol.Config{AuthMode: "chatgpt", BPSDeviceConvergence: true})
				if err != nil {
					t.Fatal(err)
				}
				devices = append(devices, headers.Get("X-Codex-Installation-ID"))
				if headers.Get("X-Codex-Window-ID") != "independent-window" {
					t.Fatal("device mode changed window")
				}
				if headers.Get("Authorization") != "Bearer "+credential {
					t.Fatal("device mode changed credentials")
				}
				if configured {
					health, err := tr.Health(context.Background(), &pluginv1.HealthRequest{})
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(health.GetStatusJson(), "configured-device") || strings.Contains(health.GetStatusJson(), "12345678") {
						t.Fatal("private device or seed leaked in Health")
					}
				}
				start.AccountId = 9
				start.Headers["ChatGPT-Account-ID"] = &pluginv1.HeaderValues{Values: []string{"acct-9"}}
				other, _, err := tr.prepareBPSHeaders(context.Background(), start, host, protocol.Config{AuthMode: "chatgpt", BPSDeviceConvergence: true})
				if err != nil {
					t.Fatal(err)
				}
				if other.Get("X-Codex-Installation-ID") == devices[len(devices)-1] {
					t.Fatal("separate account reused previous device")
				}
				tr.Shutdown()
			}
			want := "d7f9154f-deba-4da1-b7e4-bbf5de2ef100"
			if configured {
				want = "configured-device-7"
			}
			if devices[0] != want || devices[1] != want {
				t.Fatalf("unstable device across restart/refresh: %v want %q", devices, want)
			}
		})
	}
}
