package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRetiredTimezoneSettingLoadsAndIsDiscarded(t *testing.T) {
	for _, value := range []any{false, true, nil} {
		raw, _ := json.Marshal(map[string]any{"native_timezone_by_ip": value, "account_ids": []int64{7}, "timeout_seconds": 123})
		cfg, err := Parse(raw)
		if err != nil {
			t.Fatalf("legacy setting %s: %v", raw, err)
		}
		if !reflect.DeepEqual(cfg.AccountIDs, []int64{7}) || cfg.TimeoutSeconds != 123 {
			t.Fatalf("migration changed supported settings: %+v", cfg)
		}
		for _, saved := range []Config{cfg, cfg.Clone(), Default()} {
			encoded, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if _, exists := fields["native_timezone_by_ip"]; exists {
				t.Fatalf("removed setting remains serializable: %s", encoded)
			}
			again, err := Parse(encoded)
			if err != nil || !reflect.DeepEqual(again, saved) {
				t.Fatalf("round trip changed supported settings: %+v, %v", again, err)
			}
		}
	}
}

func TestRetiredTimezoneMigrationKeepsStrictValidation(t *testing.T) {
	for _, value := range []any{0, 1, "true", "false", []any{}, map[string]any{}} {
		raw, _ := json.Marshal(map[string]any{"native_timezone_by_ip": value})
		if _, err := Parse(raw); err == nil {
			t.Fatalf("non-boolean legacy setting accepted: %s", raw)
		}
	}
	for _, raw := range []string{
		`{"native_timezone_by_ip":true,"native_timezone_by_ipp":false}`,
		`{"native_timezone_by_ip":true,"timeout_seconds":"123"}`,
		`{"native_timezone_by_ip":"invalid","native_timezone_by_ip":true}`,
		`{"native_timezone_by_ip":true} {}`,
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("invalid configuration accepted during migration: %s", raw)
		}
	}
}
