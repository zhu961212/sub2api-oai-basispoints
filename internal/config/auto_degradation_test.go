package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAutoDegradationDefaultsAndRoundTrip(t *testing.T) {
	for _, input := range []string{"", `{}`, `{"account_ids":[7],"bps_auto_disable_on_403":false}`} {
		cfg, err := Parse([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AutoDegradationEnabled || cfg.AutoDegradationIntervalMinutes != 30 || cfg.DegradationCheckModel != "gpt-6-astra" {
			t.Fatalf("unexpected default diagnostics: %+v", cfg)
		}
		for _, enabled := range []bool{false, true} {
			cfg.AutoDegradationEnabled = enabled
			cfg.AutoDegradationIntervalMinutes = 17
			cfg.DegradationCheckModel = "gpt-5.4-mini"
			raw, err := json.Marshal(cfg.Clone())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"auto_degradation_enabled":`) {
				t.Fatal("explicit disabled setting omitted")
			}
			again, err := Parse(raw)
			cfg.DegradationCheckModel = DefaultDegradationCheckModel
			if err != nil || !reflect.DeepEqual(again, cfg) {
				t.Fatalf("round trip: %+v, %v", again, err)
			}
		}
	}
}

func TestAutoDegradationManualRevision(t *testing.T) {
	defaults, err := Parse([]byte(`{}`))
	if err != nil || defaults.AutoDegradationManualRevision != 0 {
		t.Fatalf("legacy revision: %+v, %v", defaults, err)
	}
	for _, revision := range []int64{0, 1, 9007199254740991} {
		raw, _ := json.Marshal(map[string]any{"auto_degradation_manual_revision": revision})
		cfg, err := Parse(raw)
		if err != nil || cfg.AutoDegradationManualRevision != revision {
			t.Fatalf("revision %d: %+v, %v", revision, cfg, err)
		}
		saved, err := json.Marshal(cfg.Clone())
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(saved, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["auto_degradation_manual_revision"]; !ok {
			t.Fatal("revision omitted from persisted config")
		}
		again, err := Parse(saved)
		if err != nil || again.AutoDegradationManualRevision != revision {
			t.Fatalf("revision round trip: %+v, %v", again, err)
		}
	}
	for _, revision := range []any{int64(-1), int64(9007199254740992), 1.5, "1", true} {
		raw, _ := json.Marshal(map[string]any{"auto_degradation_manual_revision": revision})
		if _, err := Parse(raw); err == nil {
			t.Fatalf("accepted invalid revision %v", revision)
		}
	}
}

func TestAutoDegradationValidation(t *testing.T) {
	for _, minutes := range []any{-1, 0, 4, 1441, 5.5, "30"} {
		raw, _ := json.Marshal(map[string]any{"auto_degradation_interval_minutes": minutes})
		if _, err := Parse(raw); err == nil {
			t.Fatalf("accepted interval %v", minutes)
		}
	}
	for _, minutes := range []int{5, 30, 1440} {
		raw, _ := json.Marshal(map[string]any{"auto_degradation_interval_minutes": minutes})
		cfg, err := Parse(raw)
		if err != nil || cfg.AutoDegradationIntervalMinutes != minutes {
			t.Fatalf("interval %d: %v", minutes, err)
		}
	}
	for _, model := range []string{"", " gpt-5.4", "gpt 5.4", "gpt" + string(rune(10)) + "5.4", "gpt" + string(rune(9)) + "5.4", "gpt" + string(rune(0)) + "5.4", "gpt" + string(rune(0x85)) + "5.4", strings.Repeat("x", 129)} {
		raw, _ := json.Marshal(map[string]any{"degradation_check_model": model})
		if cfg, err := Parse(raw); err != nil || cfg.DegradationCheckModel != DefaultDegradationCheckModel {
			t.Fatalf("legacy model %q did not normalize to the fixed model: %+v, %v", model, cfg, err)
		}
	}
	for _, model := range []string{"gpt-5.4", "gpt-5.4-mini", "custom/native-model:2026-09-01", strings.Repeat("x", 128)} {
		raw, _ := json.Marshal(map[string]any{"degradation_check_model": model})
		cfg, err := Parse(raw)
		if err != nil || cfg.DegradationCheckModel != DefaultDegradationCheckModel {
			t.Fatalf("model %q: %v", model, err)
		}
	}
}
