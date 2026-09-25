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
		{"legacy model", `{"model":"gpt-5.6-sol-excel"}`, "gpt-5.6-sol-excel"},
		{"uppercase key alone", `{"MODEL":"gpt-6-astra"}`, ""},
		{"uppercase key cannot expand routing", `{"model":"gpt-5.6-sol","MODEL":"gpt-6-astra"}`, "gpt-5.6-sol"},
		{"uppercase key cannot suppress routing", `{"model":"gpt-6-astra","MODEL":"gpt-5.6-sol"}`, "gpt-6-astra"},
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
