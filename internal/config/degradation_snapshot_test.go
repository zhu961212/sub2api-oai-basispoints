package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDegradationSnapshotValidationAndClone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ids     any
		want    []int64
		invalid bool
	}{
		{name: "deduplicate in request order", ids: []int64{9, 7, 9}, want: []int64{9, 7}},
		{name: "zero", ids: []int64{0}, invalid: true},
		{name: "negative", ids: []int64{-1}, invalid: true},
		{name: "fraction", ids: []any{7.5}, invalid: true},
		{name: "string", ids: []any{"7"}, invalid: true},
		{name: "too many", ids: make([]int64, MaxDegradationCheckAccountIDs+1), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"degradation_check": true, "degradation_check_account_ids": tc.ids})
			cfg, err := Parse(raw)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid diagnostic snapshot accepted")
				}
				return
			}
			if err != nil || !reflect.DeepEqual(cfg.DegradationCheckAccountIDs, tc.want) {
				t.Fatalf("snapshot changed: %+v, %v", cfg, err)
			}
			clone := cfg.Clone()
			clone.DegradationCheckAccountIDs[0] = 42
			if !reflect.DeepEqual(cfg.DegradationCheckAccountIDs, tc.want) {
				t.Fatal("cloning shared diagnostic target storage")
			}
		})
	}
	ordinary, _ := json.Marshal(Default())
	if strings.Contains(string(ordinary), "degradation_check_account_ids") {
		t.Fatal("ordinary config serialized a temporary snapshot")
	}
}
