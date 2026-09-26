package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAccountSelectionPreservesLegacyAndAutomaticPolicies(t *testing.T) {
	tests := []struct {
		name              string
		raw               string
		allowed, excluded []int64
	}{
		{"legacy default", `{}`, []int64{0, 7, 999}, nil},
		{"legacy empty list", `{"account_ids":[]}`, []int64{0, 7, 999}, nil},
		{"legacy whitelist", `{"account_ids":[7,9]}`, []int64{0, 7, 9}, []int64{42, 999}},
		{"inactive exclusion list", `{"account_ids":[7],"excluded_account_ids":[7]}`, []int64{0, 7}, []int64{9, 999}},
		{"automatic future account", `{"auto_select_new_accounts":true,"account_ids":[7]}`, []int64{0, 7, 42, 999}, nil},
		{"exclusion overrides selected snapshot", `{"auto_select_new_accounts":true,"account_ids":[7,9],"excluded_account_ids":[9]}`, []int64{0, 7, 999}, []int64{9}},
		{"all known excluded future included", `{"auto_select_new_accounts":true,"account_ids":[],"excluded_account_ids":[7,9]}`, []int64{0, 42, 999}, []int64{7, 9}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Parse([]byte(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range test.allowed {
				if !cfg.HandlesAccount(id) {
					t.Errorf("account %d should use BPS", id)
				}
			}
			for _, id := range test.excluded {
				if cfg.HandlesAccount(id) {
					t.Errorf("account %d should pass through", id)
				}
			}
		})
	}
}

func TestAccountSelectionNormalizationAndCloneOwnTheirSlices(t *testing.T) {
	selected := []int64{7, 7, 0, -1, 9}
	excluded := []int64{9, 9, -2, 0, 11}
	cfg := Default()
	cfg.AccountIDs, cfg.ExcludedAccountIDs = selected, excluded
	cfg.AutoSelectNewAccounts = true
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.AccountIDs, []int64{7, 9}) || !reflect.DeepEqual(cfg.ExcludedAccountIDs, []int64{9, 11}) {
		t.Fatalf("unexpected normalized selection: selected=%v excluded=%v", cfg.AccountIDs, cfg.ExcludedAccountIDs)
	}
	selected[0], excluded[0] = 101, 102
	if cfg.AccountIDs[0] != 7 || cfg.ExcludedAccountIDs[0] != 9 {
		t.Fatal("normalization retained caller-owned slices")
	}
	clone := cfg.Clone()
	clone.AccountIDs[0], clone.ExcludedAccountIDs[0], clone.EnabledModels[0] = 201, 202, "changed"
	if cfg.AccountIDs[0] != 7 || cfg.ExcludedAccountIDs[0] != 9 || cfg.EnabledModels[0] == "changed" {
		t.Fatal("clone shares mutable slice storage")
	}
	if !clone.AutoSelectNewAccounts {
		t.Fatal("clone lost automatic selection policy")
	}
	before := cfg.Clone()
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, cfg) {
		t.Fatal("normalization is not idempotent")
	}
}

func TestAccountSelectionUnnormalizedLegacyCompatibility(t *testing.T) {
	for _, ids := range [][]int64{nil, {}, {0, -1, -2}, {7, 7, 0, -1, 9}} {
		cfg := Default()
		cfg.AccountIDs = ids
		normalized := cfg.SelectedAccountIDs()
		for _, id := range []int64{-1, 0, 7, 9, 11} {
			want := id == 0 || len(normalized) == 0
			for _, selected := range normalized {
				want = want || selected == id
			}
			if got := cfg.HandlesAccount(id); got != want {
				t.Errorf("selection %v account %d: got %t, want %t", ids, id, got, want)
			}
		}
	}
}

func TestAccountSelectionPolicySurvivesPersistedJSON(t *testing.T) {
	cfg, err := Parse([]byte(`{"auto_select_new_accounts":true,"account_ids":[7,9],"excluded_account_ids":[9,11]}`))
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := Parse(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, reloaded) {
		t.Fatalf("policy changed after persistence: %s", persisted)
	}
	if !reloaded.HandlesAccount(999) || reloaded.HandlesAccount(9) || reloaded.HandlesAccount(11) {
		t.Fatal("reloaded policy lost future enrollment or exclusions")
	}
}
