package protocol

import "testing"

func TestRequiresNativeWebSearch(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		want   bool
	}{
		{"default hosted search", `{"tools":[{"type":"web_search"}]}`, true},
		{"cached hosted search", `{"tools":[{"type":"web_search","external_web_access":false}]}`, true},
		{"live search", `{"tools":[{"type":"web_search","external_web_access":true}]}`, true},
		{"high context preview", `{"tools":[{"type":"web_search_preview","search_context_size":"high"}]}`, true},
		{"versioned preview", `{"tools":[{"type":"web_search_preview_2025_03_11"}]}`, true},
		{"disabled declaration", `{"tool_choice":"none","tools":[{"type":"web_search"}]}`, false},
		{"required declaration", `{"tool_choice":"required","tools":[{"type":"web_search"}]}`, true},
		{"forced hosted search", `{"tool_choice":{"type":"web_search_preview"}}`, true},
		{"forced client function", `{"tool_choice":{"type":"function","name":"web_search"}}`, false},
		{"namespace", `{"tools":[{"type":"namespace","name":"outer","tools":[{"type":"namespace","name":"inner","children":[{"type":"web_search"}]}]}]}`, true},
		{"additional tools", `{"input":[{"type":"additional_tools","tools":[{"type":"web_search"}]}]}`, true},
		{"completed discovery", `{"input":[{"type":"tool_search_output","status":"completed","tools":[{"type":"web_search"}]}]}`, true},
		{"discovery without status", `{"input":[{"type":"tool_search_output","tools":[{"type":"web_search"}]}]}`, true},
		{"pending discovery", `{"input":[{"type":"tool_search_output","status":"in_progress","tools":[{"type":"web_search"}]}]}`, false},
		{"failed discovery", `{"input":[{"type":"tool_search_output","status":"failed","tools":[{"type":"web_search"}]}]}`, false},
		{"disabled additions", `{"tool_choice":"none","input":[{"type":"additional_tools","tools":[{"type":"web_search"}]}]}`, false},
		{"hosted history", `{"input":[{"type":"web_search_call","id":"ws_history","status":"completed"}]}`, true},
		{"disabled hosted history", `{"tool_choice":"none","input":[{"type":"web_search_call","id":"ws_history"}]}`, true},
		{"client search tools", `{"tools":[{"type":"function","name":"web_search"},{"type":"custom","name":"web_search_preview"}]}`, false},
		{"nested client search tool", `{"tools":[{"type":"namespace","name":"web","tools":[{"type":"function","name":"web_search"}]}]}`, false},
		{"client search history", `{"input":[{"type":"function_call","name":"web_search","call_id":"call_search"}]}`, false},
		{"message mentioning search", `{"input":[{"role":"user","content":"web_search_call","tools":[{"type":"web_search"}]}]}`, false},
		{"malformed fields", `{"tools":[null,"web_search",{}, {"type":"namespace","tools":{"type":"web_search"}}],"input":[null,"web_search_call"]}`, false},
		{"empty", `{}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			source, err := RawObject([]byte(test.source))
			if err != nil {
				t.Fatal(err)
			}
			before := string(JSONBytes(source))
			if got := RequiresNativeWebSearch(source); got != test.want {
				t.Fatalf("RequiresNativeWebSearch = %t, want %t", got, test.want)
			}
			if string(JSONBytes(source)) != before {
				t.Fatal("search routing modified the request")
			}
		})
	}
}
