package protocol

import "testing"

func TestRequestedModelUsesExactTopLevelField(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"astra", `{"model":"gpt-6-astra"}`, "gpt-6-astra"},
		{"trimmed astra", `{"model":" gpt-6-astra "}`, "gpt-6-astra"},
		{"sol", `{"model":"gpt-5.6-sol"}`, "gpt-5.6-sol"},
		{"trimmed sol", `{"model":"\tgpt-5.6-sol\n"}`, "gpt-5.6-sol"},
		{"six sol", `{"model":"gpt-6-sol"}`, "gpt-6-sol"},
		{"six luna", `{"model":" gpt-6-luna "}`, "gpt-6-luna"},
		{"terra", `{"model":"gpt-5.6-terra"}`, "gpt-5.6-terra"},
		{"luna", `{"model":"\tgpt-5.6-luna\n"}`, "gpt-5.6-luna"},
		{"sol casing preserved", `{"model":"GPT-5.6-SOL"}`, "GPT-5.6-SOL"},
		{"legacy model", `{"model":"gpt-5.6-sol-excel"}`, "gpt-5.6-sol-excel"},
		{"uppercase key alone", `{"MODEL":"gpt-6-astra"}`, ""},
		{"uppercase key cannot expand routing", `{"model":"gpt-6-sol","MODEL":"gpt-6-astra"}`, "gpt-6-sol"},
		{"uppercase key cannot suppress astra routing", `{"model":"gpt-6-astra","MODEL":"gpt-6-sol"}`, "gpt-6-astra"},
		{"uppercase key cannot expand sol routing", `{"model":"gpt-6-sol","MODEL":"gpt-5.6-sol"}`, "gpt-6-sol"},
		{"uppercase key cannot suppress sol routing", `{"model":"gpt-5.6-sol","MODEL":"gpt-6-sol"}`, "gpt-5.6-sol"},
		{"nested field", `{"metadata":{"model":"gpt-6-astra"}}`, ""},
		{"missing field", `{}`, ""},
		{"null field", `{"model":null}`, ""},
		{"numeric field", `{"model":42}`, ""},
		{"null body", `null`, ""},
		{"array body", `[{"model":"gpt-6-astra"}]`, ""},
		{"malformed body", `{"model":"gpt-6-astra"`, ""},
		{"trailing object", `{"model":"gpt-6-astra"}{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequestedModel([]byte(tc.body)); got != tc.want {
				t.Fatalf("RequestedModel(%s) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}
