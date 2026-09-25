package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEmptyObjectYieldsCompleteDefaults(t *testing.T) {
	c, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.ResponsesURL != DefaultResponsesURL || c.UpstreamModel != DefaultUpstreamModel ||
		c.AuthMode != DefaultAuthMode || c.TimeoutSeconds != DefaultTimeoutSeconds ||
		c.MaxResponseBytes != DefaultMaxResponseBytes {
		t.Fatalf("defaults = %#v", c)
	}
	if len(c.Models) != 1 || c.Models[0] != "gpt-6-astra" {
		t.Fatalf("models = %#v", c.Models)
	}
	if c.ModelMap == nil || len(c.ModelMap) != 0 {
		t.Fatalf("model_map = %#v", c.ModelMap)
	}
	// 模型固定为 gpt-6-astra，旧别名与配置不能改变上游模型。
	if c.UpstreamModel != "gpt-6-astra" {
		t.Fatalf("upstream_model = %q, want gpt-6-astra", c.UpstreamModel)
	}
	if !c.RewriteTools || !c.TransformResponses {
		t.Fatalf("rewrite/transform should default to enabled: %#v", c)
	}
	if c.ImageRelayEnabled || c.ImageRelayPublicURL != "" || c.ImageRelayStorageDir != "" || c.ImageRelayListen != "" {
		t.Fatalf("image relay defaults = %#v", c)
	}
}

func TestParseMigratesLegacyImageSettingsWithoutConfiguration(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		raw, _ := json.Marshal(map[string]any{
			"image_relay_enabled": enabled, "image_relay_public_url": "invalid legacy origin",
			"image_relay_listen": "unavailable legacy listener", "image_relay_storage_dir": "old-dir",
			"account_ids": []int64{7}, "timeout_seconds": 120, "rewrite_tools": false,
		})
		cfg, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ImageRelayEnabled || cfg.ImageRelayPublicURL != "" || cfg.ImageRelayListen != "" || cfg.ImageRelayStorageDir != "" {
			t.Fatalf("legacy image configuration not cleared: %#v", cfg)
		}
		if len(cfg.AccountIDs) != 1 || cfg.AccountIDs[0] != 7 || cfg.TimeoutSeconds != 120 || cfg.RewriteTools {
			t.Fatalf("unrelated settings changed: %#v", cfg)
		}
		encoded, _ := json.Marshal(cfg)
		if strings.Contains(string(encoded), "image_relay_") {
			t.Fatal("normalized settings still expose manual image setup")
		}
		again, err := Parse(encoded)
		if err != nil {
			t.Fatal(err)
		}
		second, _ := json.Marshal(again)
		if string(encoded) != string(second) {
			t.Fatal("migration is not idempotent")
		}
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

// 旧版本的单值 account_id 字段已废弃：DisallowUnknownFields 会直接拒绝，
// 这是受控语义（升级后需要在配置页重新保存一次）。
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
		"unnormalized overrides": {
			UpstreamModel: "forced-model", Models: []string{"custom-alias"},
			ModelMap: map[string]string{"gpt-6-astra": "other-upstream", "custom-alias": "custom-upstream"},
		},
	}
	for name, cfg := range configs {
		t.Run(name, func(t *testing.T) {
			// HandlesModel performs routing first; the resolver always uses the fixed model.
			for _, requested := range []string{"gpt-6-astra", " gpt-6-astra ", "GPT-6-ASTRA", "gpt-5.6-sol-excel", "gpt-5.6-luna-excel", "gpt-5.6-sol", "custom-alias", "future-model-excel", "unknown-model", ""} {
				if got := cfg.UpstreamModelFor(requested); got != "gpt-6-astra" {
					t.Errorf("UpstreamModelFor(%q) = %q, want gpt-6-astra", requested, got)
				}
			}
		})
	}
}

// 旧 models 清单不能启用额外模型，也不能屏蔽固定模型。
func TestModelListCannotExpandFixedRouting(t *testing.T) {
	for _, models := range [][]string{nil, {}, {"brand-new-model", "brand-new-model-excel", "GPT-6-ASTRA"}} {
		cfg := Default()
		cfg.Models = models
		if !SupportsModel("gpt-6-astra", cfg) {
			t.Fatalf("legacy model list %#v suppressed the fixed model", models)
		}
		for _, model := range []string{"brand-new-model", "brand-new-model-excel", "GPT-6-ASTRA", "gpt-5.4"} {
			if SupportsModel(model, cfg) || cfg.HandlesModel(model) {
				t.Fatalf("legacy model list %#v enabled routing for %q", models, model)
			}
		}
	}
}

func TestParseNormalizesModelMap(t *testing.T) {
	for _, body := range []string{
		`{"model_map":{" alias ":" upstream ","":"x","empty":"  "}}`,
		`{"model_map":{"gpt-6-astra":"forced-model","gpt-5.4":"gpt-6-astra"},"upstream_model":"forced-model"}`,
		`{"model_map":null}`,
	} {
		cfg, err := Parse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ModelMap == nil || len(cfg.ModelMap) != 0 {
			t.Fatalf("model_map = %#v, want an empty object", cfg.ModelMap)
		}
		if cfg.UpstreamModel != "gpt-6-astra" || cfg.UpstreamModelFor("gpt-6-astra") != "gpt-6-astra" {
			t.Fatalf("legacy settings changed the fixed upstream model: %#v", cfg)
		}
		if cfg.HandlesModel("alias") || cfg.HandlesModel("gpt-5.4") {
			t.Fatal("a legacy model mapping expanded routing")
		}
	}
}

func TestCloneDeepCopiesModelMap(t *testing.T) {
	original := Default()
	clone := original.Clone()
	clone.ModelMap["gpt-5.6-sol-excel"] = "mutated"
	clone.Models[0] = "mutated"
	if original.ModelMap["gpt-5.6-sol-excel"] == "mutated" {
		t.Fatal("Clone shared the ModelMap backing map")
	}
	if original.Models[0] == "mutated" {
		t.Fatal("Clone shared the Models backing array")
	}
}

func TestParseNormalizesModelsList(t *testing.T) {
	for _, body := range []string{
		`{"models":[" demo ","demo","","second"]}`,
		`{"models":[]}`, `{"models":null}`,
		`{"models":["GPT-6-ASTRA","gpt-6-astra-excel"]}`,
	} {
		cfg, err := Parse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Models) != 1 || cfg.Models[0] != "gpt-6-astra" {
			t.Fatalf("models = %#v, want [gpt-6-astra]", cfg.Models)
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
	clone := original.Clone()
	clone.Models[0] = "mutated"
	if original.Models[0] == "mutated" {
		t.Fatal("Clone shared the Models backing array")
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
		"unnormalized legacy overrides": {
			UpstreamModel: "gpt-5.6-sol", Models: []string{"custom-alias", "gpt-5.4"},
			ModelMap: map[string]string{"custom-alias": "gpt-6-astra", "gpt-5.4": "gpt-6-astra"},
		},
	}
	for name, cfg := range configs {
		t.Run(name, func(t *testing.T) {
			for _, model := range []string{"gpt-6-astra", " gpt-6-astra ", "\tgpt-6-astra\n"} {
				if !cfg.HandlesModel(model) || !SupportsModel(model, cfg) {
					t.Fatalf("%q should be served by the plugin", model)
				}
			}
			for _, model := range []string{
				"gpt-5.4", "gpt-5-codex", "gpt-5.6-sol", "claude-sonnet-4-5",
				"gpt-5.6-sol-excel", "gpt-5.6-luna-excel", "gpt-5.6-terra-excel",
				"gpt-6-astra-excel", "gpt-6-astra-preview", "GPT-6-ASTRA", "gpt-6-Astra",
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
