package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBPSReenableAcknowledgementsOwnTheirStorage(t *testing.T) {
	blockID := strings.Repeat("a", 32)
	input := map[string]string{"7": blockID}
	cfg := Default()
	cfg.BPSReenabledAccounts = input
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	input["7"] = strings.Repeat("b", 32)
	clone := cfg.Clone()
	clone.BPSReenabledAccounts["7"] = strings.Repeat("c", 32)
	if cfg.BPSReenabledAccounts["7"] != blockID {
		t.Fatal("published acknowledgement shares mutable storage")
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := Parse(raw)
	if err != nil || reloaded.BPSReenabledAccounts["7"] != blockID {
		t.Fatalf("acknowledgement changed on save/load: %v", err)
	}
}

func TestBPSReenableAcknowledgementsRejectInvalidIDs(t *testing.T) {
	for _, test := range []struct{ id, token string }{
		{"0", strings.Repeat("a", 32)}, {"-7", strings.Repeat("a", 32)},
		{"07", strings.Repeat("a", 32)}, {"7", "short"},
		{"7", strings.Repeat("g", 32)}, {"7", strings.Repeat("A", 32)},
	} {
		raw, _ := json.Marshal(map[string]any{"bps_reenabled_accounts": map[string]string{test.id: test.token}})
		if _, err := Parse(raw); err == nil {
			t.Fatalf("invalid acknowledgement accepted: account=%s", test.id)
		}
	}
}
