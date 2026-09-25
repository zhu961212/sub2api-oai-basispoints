package transport

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestModelSelectionSurvivesApplyAndHealthSnapshots(t *testing.T) {
	for _, test := range []struct {
		name   string
		config map[string]any
		want   []string
	}{
		{"defaults", map[string]any{}, []string{"gpt-6-astra", "gpt-5.6-sol"}},
		{"all models", map[string]any{"enabled_models": protocol.AvailableModels()}, protocol.AvailableModels()},
		{"single selected model", map[string]any{"enabled_models": []string{"gpt-6-luna"}}, []string{"gpt-6-luna"}},
		{"empty selection", map[string]any{"enabled_models": []string{}}, []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := New()
			defer transport.Shutdown()
			applyConfig(t, transport, test.config)
			for range 2 {
				health, err := transport.Health(context.Background(), &pluginv1.HealthRequest{})
				if err != nil {
					t.Fatal(err)
				}
				var status struct {
					EnabledModels   []string `json:"enabled_models"`
					AvailableModels []string `json:"available_models"`
				}
				if err := json.Unmarshal([]byte(health.GetStatusJson()), &status); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(status.EnabledModels, test.want) {
					t.Fatalf("selection changed in health snapshot: %#v, want %#v", status, test.want)
				}
				if !reflect.DeepEqual(status.AvailableModels, protocol.AvailableModels()) {
					t.Fatalf("available model catalog changed: %#v", status.AvailableModels)
				}
			}
		})
	}
}
