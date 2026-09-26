package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDegradationAccountSelectorValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		check   bool
		account any
		wantID  int64
		wantErr bool
	}{
		{name: "bulk", check: true, account: 0},
		{name: "single", check: true, account: 42, wantID: 42},
		{name: "normal config", account: 0},
		{name: "negative", check: true, account: -1, wantErr: true},
		{name: "missing command", account: 42, wantErr: true},
		{name: "string", check: true, account: "42", wantErr: true},
		{name: "fraction", check: true, account: 42.5, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"degradation_check": tc.check, "degradation_check_account_id": tc.account})
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := Parse(raw)
			if tc.wantErr {
				if err == nil {
					t.Fatal("invalid selector accepted")
				}
				return
			}
			if err != nil || cfg.DegradationCheck != tc.check || cfg.DegradationCheckAccountID != tc.wantID {
				t.Fatalf("selector parsed as %+v, err=%v", cfg, err)
			}
		})
	}
	raw, err := json.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "degradation_check") {
		t.Fatal("ordinary configuration contains a degradation check command")
	}
}
