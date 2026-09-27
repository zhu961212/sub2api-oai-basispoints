package config

import (
	"encoding/json"
	"testing"
)

func TestNativeTimezoneByIPConfiguration(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		enabled bool
	}{
		{"", false}, {`{}`, false}, {`{"account_ids":[7]}`, false},
		{`{"native_timezone_by_ip":false}`, false}, {`{"native_timezone_by_ip":true}`, true},
		{`{"native_timezone_by_ip":null}`, false},
	} {
		cfg, err := Parse([]byte(tc.raw))
		if err != nil || cfg.NativeTimezoneByIP != tc.enabled {
			t.Fatalf("parse %s: %+v, %v", tc.raw, cfg, err)
		}
		raw, err := json.Marshal(cfg.Clone())
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["native_timezone_by_ip"] != tc.enabled {
			t.Fatalf("boolean missing or changed: %s", raw)
		}
		again, err := Parse(raw)
		if err != nil || again.NativeTimezoneByIP != tc.enabled {
			t.Fatalf("round trip changed toggle: %+v, %v", again, err)
		}
	}
	for _, value := range []any{0, 1, "true", "false", []any{}, map[string]any{}} {
		raw, _ := json.Marshal(map[string]any{"native_timezone_by_ip": value})
		if _, err := Parse(raw); err == nil {
			t.Fatalf("non-boolean setting accepted: %s", raw)
		}
	}
}
