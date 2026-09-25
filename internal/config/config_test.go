package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseEmptyObjectYieldsCompleteDefaults(t *testing.T) {
	c, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.ResponsesURL != DefaultResponsesURL ||
		c.AuthMode != DefaultAuthMode || c.TimeoutSeconds != DefaultTimeoutSeconds ||
		c.MaxResponseBytes != DefaultMaxResponseBytes {
		t.Fatalf("defaults = %#v", c)
	}
	if !reflect.DeepEqual(c.EnabledModels, []string{"gpt-6-astra", "gpt-5.6-sol"}) {
		t.Fatalf("enabled_models = %#v", c.EnabledModels)
	}
	if !c.RewriteTools || !c.TransformResponses {
		t.Fatalf("rewrite/transform should default to enabled: %#v", c)
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"models", "model_map", "upstream_model", "image_relay_enabled", "image_relay_public_url", "image_relay_listen", "image_relay_storage_dir"} {
		if _, exists := fields[removed]; exists {
			t.Fatalf("default configuration still contains removed field %q", removed)
		}
	}
}

func TestParseRejectsRemovedConfigurationFields(t *testing.T) {
	for field, value := range map[string]any{
		"models": []string{"gpt-6-astra"}, "model_map": map[string]string{}, "upstream_model": "gpt-6-astra",
		"image_relay_enabled": false, "image_relay_public_url": "", "image_relay_listen": "", "image_relay_storage_dir": "",
	} {
		t.Run(field, func(t *testing.T) {
			for _, input := range []any{value, nil} {
				raw, err := json.Marshal(map[string]any{field: input})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := Parse(raw); err == nil || !strings.Contains(err.Error(), "unknown field") {
					t.Fatalf("removed configuration field %q should be rejected, got %v", field, err)
				}
			}
		})
	}
}

func TestParseNilPayloadYieldsDefaults(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.ResponsesURL != DefaultResponsesURL {
		t.Fatalf("responses_url = %q", c.ResponsesURL)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	if _, err := Parse([]byte(`{"responses_urll":"https://example.test"}`)); err == nil {
		t.Fatal("unknown field was accepted")
	}
}

func TestParseRejectsTrailingContent(t *testing.T) {
	if _, err := Parse([]byte(`{"auth_mode":"chatgpt"} {"auth_mode":"chatgpt"}`)); err == nil {
		t.Fatal("multi-object config was accepted")
	}
}

func TestParseRejectsInvalidResponsesURL(t *testing.T) {
	for _, value := range []string{
		"ftp://example.test/basispoints",
		"https://user:pass@example.test/basispoints",
		"https://example.test/basispoints?token=1",
		"https://example.test/basispoints#frag",
		"/relative/path",
	} {
		if _, err := Parse([]byte(`{"responses_url":"` + value + `"}`)); err == nil {
			t.Fatalf("responses_url %q was accepted", value)
		}
	}
}

func TestParseEnforcesBoundaries(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"timeout too small", `{"timeout_seconds":9}`},
		{"timeout too large", `{"timeout_seconds":1801}`},
		{"response too small", `{"max_response_bytes":65535}`},
		{"response too large", `{"max_response_bytes":134217729}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.body)); err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
		})
	}
}

// 配置只接受 account_ids，已删除的 account_id 必须拒绝。
func TestParseRejectsLegacyAccountIDField(t *testing.T) {
	if _, err := Parse([]byte(`{"account_id":42}`)); err == nil {
		t.Fatal("legacy account_id field should be rejected")
	}
}

func TestSelectedAccountIDs(t *testing.T) {
	// 空列表 = 不限制，跟随宿主调度。
	if got := Default().SelectedAccountIDs(); got != nil {
		t.Fatalf("default selection = %#v, want nil", got)
	}
	multi := Default()
	multi.AccountIDs = []int64{1, 2, 2, 0, -3}
	if got := multi.SelectedAccountIDs(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("multi selection = %#v, want [1 2]", got)
	}
	// 去重与非法值在 Normalize 时同样会被清理。
	parsed, err := Parse([]byte(`{"account_ids":[5,5,9,-1,0]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.AccountIDs; len(got) != 2 || got[0] != 5 || got[1] != 9 {
		t.Fatalf("normalized account_ids = %#v, want [5 9]", got)
	}
}

func TestUpstreamModelFor(t *testing.T) {
	configs := map[string]Config{
		"default": Default(), "zero value": {},
		"empty selection": {EnabledModels: []string{}},
	}
	for name, cfg := range configs {
		t.Run(name, func(t *testing.T) {
			// HandlesModel performs routing first; supported requests retain their model.
			for _, requested := range []string{"gpt-6-astra", " gpt-6-astra ", "gpt-5.6-sol", " gpt-5.6-sol ", "\tgpt-5.6-sol\n", "gpt-6-sol", " gpt-6-luna ", "gpt-5.6-terra", "gpt-5.6-luna"} {
				if got, want := cfg.UpstreamModelFor(requested), strings.TrimSpace(requested); got != want {
					t.Errorf("UpstreamModelFor(%q) = %q, want %q", requested, got, want)
				}
			}
			// Direct resolution of unsupported models keeps the existing default fallback.
			for _, requested := range []string{"GPT-6-ASTRA", "GPT-5.6-SOL", "gpt-5.6-Sol", "gpt-5.6-sol-excel", "gpt-5.6-luna-excel", "custom-alias", "future-model-excel", "unknown-model", ""} {
				if got := cfg.UpstreamModelFor(requested); got != "gpt-6-astra" {
					t.Errorf("UpstreamModelFor(%q) = %q, want gpt-6-astra fallback", requested, got)
				}
			}
		})
	}
}

// 旧 models 清单不能启用额外模型，也不能屏蔽固定模型。
func TestAvailableModelsReturnsIndependentCatalog(t *testing.T) {
	want := []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-terra", "gpt-5.6-luna"}
	models := AvailableModels()
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("catalog = %#v, want %#v", models, want)
	}
	models[0] = "mutated"
	if !reflect.DeepEqual(AvailableModels(), want) || !Default().HandlesModel(want[0]) {
		t.Fatal("AvailableModels exposes mutable routing catalog")
	}
}

func TestParseEnabledModelsDefaults(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"enabled_models":null}`,
	} {
		cfg, err := Parse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"gpt-6-astra", "gpt-5.6-sol"}
		if !reflect.DeepEqual(cfg.EnabledModels, want) {
			t.Fatalf("configuration %s selected %#v, want %#v", body, cfg.EnabledModels, want)
		}
	}
}

func TestEnabledModelsAllSubsetsNormalizeAndPersist(t *testing.T) {
	catalog := AvailableModels()
	for mask := 0; mask < 1<<len(catalog); mask++ {
		selected, want := make([]string, 0), make([]string, 0)
		for index, model := range catalog {
			if mask&(1<<index) != 0 {
				want = append(want, model)
			}
		}
		// Reverse order, duplicate, and surrounding whitespace must normalize.
		for index := len(want) - 1; index >= 0; index-- {
			selected = append(selected, " \t"+want[index]+"\n", want[index])
		}
		raw, err := json.Marshal(map[string]any{"enabled_models": selected})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		for stage := 0; stage < 3; stage++ {
			if !reflect.DeepEqual(cfg.EnabledModels, want) {
				t.Fatalf("selection mask %d stage %d = %#v, want %#v", mask, stage, cfg.EnabledModels, want)
			}
			for index, model := range catalog {
				if got, expected := cfg.HandlesModel(model), mask&(1<<index) != 0; got != expected || SupportsModel(model, cfg) != expected {
					t.Fatalf("selection mask %d routes %s = %t, want %t", mask, model, got, expected)
				}
			}
			if stage == 0 {
				cfg = cfg.Clone()
				if err := cfg.Normalize(); err != nil {
					t.Fatal(err)
				}
			} else {
				encoded, err := json.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				cfg, err = Parse(encoded)
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestParseRejectsUnsupportedEnabledModelsWithoutEchoingInput(t *testing.T) {
	for _, value := range []string{"gpt-5.4", "gpt-6-astra-excel", "gpt-5.6-sol-excel", "GPT-6-ASTRA", "GPT-5.6-SOL", "gpt-6-Sol", "", " ", "sensitive-value@example.test"} {
		raw, _ := json.Marshal(map[string]any{"enabled_models": []string{"gpt-6-astra", value}})
		if _, err := Parse(raw); err == nil || err.Error() != "enabled_models contains an unsupported model" {
			t.Fatalf("unsupported selection should return a safe validation error, got %v", err)
		}
	}
	for _, body := range []string{`{"enabled_models":"gpt-6-astra"}`, `{"enabled_models":[42]}`, `{"enabled_models":{}}`} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Fatal("invalid enabled_models shape was accepted")
		}
	}
}

func TestHandlesModelSelectionCannotExpandCatalog(t *testing.T) {
	invalid := []string{"gpt-5.4", "gpt-6-astra-excel", "gpt-5.6-sol-excel", "GPT-6-ASTRA", "GPT-5.6-SOL", "future-model", ""}
	cfg := Config{EnabledModels: append(AvailableModels(), invalid...)}
	for _, model := range invalid {
		if cfg.HandlesModel(model) || SupportsModel(model, cfg) {
			t.Fatalf("unnormalized enabled_models expanded routing to %q", model)
		}
	}
	for _, model := range AvailableModels() {
		if !cfg.HandlesModel(" \t" + model + "\n") {
			t.Fatalf("known selected model %q was not routed", model)
		}
	}
}

func TestClonePreservesNilAndEmptyModelSelections(t *testing.T) {
	for _, models := range [][]string{nil, {}} {
		cfg := Config{EnabledModels: models}.Clone()
		if (cfg.EnabledModels == nil) != (models == nil) {
			t.Fatalf("Clone changed nil/empty model selection: %#v", cfg)
		}
		if err := cfg.Normalize(); err != nil {
			t.Fatal(err)
		}
		if got, want := cfg.HandlesModel(DefaultModelID), models == nil; got != want {
			t.Fatalf("cloned selection routes Astra = %t, want %t", got, want)
		}
	}
}

func TestParseIsIdempotent(t *testing.T) {
	c, err := Parse([]byte(`{"responses_url":"https://example.test/basispoints/api/responses"}`))
	if err != nil {
		t.Fatal(err)
	}
	first, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("normalization is not idempotent:\n%s\n%s", first, second)
	}
}

func TestCloneIsDeepCopy(t *testing.T) {
	original := Default()
	original.AccountIDs = []int64{7}
	clone := original.Clone()
	clone.EnabledModels[0] = "mutated"
	clone.AccountIDs[0] = 42
	if original.EnabledModels[0] == "mutated" || original.AccountIDs[0] != 7 {
		t.Fatal("Clone shared the model or account selection backing array")
	}
}

func TestNormalizeEffortTable(t *testing.T) {
	cases := []struct {
		input any
		want  string
	}{
		{"low", "low"},
		{"medium", "medium"},
		{"high", "high"},
		{"xhigh", "xhigh"},
		{"ultra", "xhigh"},
		{"none", "low"},
		{"minimal", "low"},
		// Basis Points 没有 max 档位，统一映射为 xhigh。
		{"max", "xhigh"},
		{" MAX ", "xhigh"},
		{"extra-high", "xhigh"},
		{"unknown", "medium"},
		{"", "medium"},
		{nil, "medium"},
		{42, "medium"},
	}
	for _, tc := range cases {
		if got := NormalizeEffort(tc.input); got != tc.want {
			t.Errorf("NormalizeEffort(%#v) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// 只有插件对外提供的模型才走 Basis Points。宿主用普通 Codex 模型（默认 gpt-5.4）
// 对账号做连通性测试，这个请求同样会被交给插件 —— 把它打到 Basis Points 会得到
// 403 basispoints_model_access_changed，宿主随即把账号判成异常/限流。
func TestHandlesModelRouting(t *testing.T) {
	configs := map[string]Config{
		"default": Default(), "zero value": {},
	}
	for name, cfg := range configs {
		t.Run(name, func(t *testing.T) {
			for _, model := range []string{"gpt-6-astra", " gpt-6-astra ", "\tgpt-6-astra\n", "gpt-5.6-sol", " gpt-5.6-sol ", "\tgpt-5.6-sol\n"} {
				if !cfg.HandlesModel(model) || !SupportsModel(model, cfg) {
					t.Fatalf("%q should be served by the plugin", model)
				}
			}
			for _, model := range []string{
				"gpt-5.4", "gpt-5-codex", "gpt-5.6-luna", "gpt-5.6-terra", "gpt-6-sol", "gpt-6-luna", "claude-sonnet-4-5",
				"gpt-5.6-sol-excel", "gpt-5.6-luna-excel", "gpt-5.6-terra-excel",
				"gpt-6-astra-excel", "gpt-6-astra-preview", "GPT-6-ASTRA", "gpt-6-Astra",
				"gpt-5.6-sol-preview", "GPT-5.6-SOL", "gpt-5.6-Sol",
				"custom-alias", "whatever-model", "", "   ",
			} {
				if cfg.HandlesModel(model) || SupportsModel(model, cfg) {
					t.Fatalf("%q must not be routed to Basis Points", model)
				}
			}
		})
	}
}

func TestSupportedModel(t *testing.T) {
	c := Default()
	if !SupportsModel(DefaultModelID, c) {
		t.Fatal("default model should be supported")
	}
	if SupportsModel("gpt-other", c) || SupportsModel("", c) {
		t.Fatal("unsupported model was accepted")
	}
}

// TestManifestSourceMatchesPluginIdentity 防止清单与运行时身份漂移：宿主启动时
// 会逐字段比对 GetInfo 与已校验清单，任何不一致都会直接让插件无法启用。
func TestManifestSourceMatchesPluginIdentity(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "manifest.source.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SchemaVersion int    `json:"schema_version"`
		ID            string `json:"id"`
		Version       string `json:"version"`
		Requires      struct {
			PluginProtocol int `json:"plugin_protocol"`
			TransportAPI   int `json:"transport_api"`
			UIBridge       int `json:"ui_bridge"`
		} `json:"requires"`
		Capabilities []struct {
			ID          string `json:"id"`
			Platform    string `json:"platform"`
			AccountType string `json:"account_type"`
		} `json:"capabilities"`
		UI struct {
			Entrypoint string `json:"entrypoint"`
		} `json:"ui"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d", manifest.SchemaVersion)
	}
	if manifest.ID != PluginID {
		t.Fatalf("manifest id %q != PluginID %q", manifest.ID, PluginID)
	}
	if manifest.Version != Version {
		t.Fatalf("manifest version %q != plugin version %q", manifest.Version, Version)
	}
	if manifest.Requires.PluginProtocol != 1 || manifest.Requires.TransportAPI != 1 || manifest.Requires.UIBridge != 1 {
		t.Fatalf("requires = %#v", manifest.Requires)
	}
	if len(manifest.Capabilities) != 1 {
		t.Fatalf("capabilities = %#v", manifest.Capabilities)
	}
	capability := manifest.Capabilities[0]
	if capability.ID != Capability || capability.Platform != "openai" || capability.AccountType != "oauth" {
		t.Fatalf("capability = %#v", capability)
	}
	if !strings.HasPrefix(manifest.UI.Entrypoint, "ui/") {
		t.Fatalf("ui.entrypoint = %q", manifest.UI.Entrypoint)
	}
	if _, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(manifest.UI.Entrypoint))); err != nil {
		t.Fatalf("ui.entrypoint is missing: %v", err)
	}
}
