package protocol

import "testing"

func TestToolRepairCustomHistoryAcceptsFunctionOutputAlias(t *testing.T) {
	for _, variation := range []string{"custom output", "function output alias", "orphan", "pending", "unknown output", "mismatched id", "duplicate output", "duplicate call", "empty id", "response reuses id", "reverse alias"} {
		t.Run(variation, func(t *testing.T) {
			source := repairTestSource()
			source["session_id"] = t.Name()
			call := map[string]any{"type": "custom_tool_call", "name": "functions.exec", "call_id": "call_previous", "input": "text(1)"}
			output := map[string]any{"type": "function_call_output", "call_id": "call_previous", "output": "already executed"}
			source["input"] = []any{call, output}
			response := map[string]any{"status": "completed", "output": []any{codeIdentityMalformedNative("functions.exec")}}
			switch variation {
			case "custom output":
				output["type"] = "custom_tool_call_output"
			case "orphan":
				source["input"] = []any{output}
			case "pending":
				source["input"] = []any{call}
			case "unknown output":
				output["type"] = "unrelated_call_output"
			case "mismatched id":
				output["call_id"] = "different"
			case "duplicate output":
				source["input"] = []any{call, output, cloneJSONValue(output)}
			case "duplicate call":
				source["input"] = []any{call, output, cloneJSONValue(call), cloneJSONValue(output)}
			case "empty id":
				call["call_id"], output["call_id"] = "", ""
			case "response reuses id":
				objectValue(response["output"].([]any)[0])["call_id"] = call["call_id"]
			case "reverse alias":
				call["type"], output["type"] = "function_call", "custom_tool_call_output"
			}
			want := variation == "custom output" || variation == "function output alias"
			if got := ToolRepairEligible(source, response); got != want {
				t.Fatalf("history eligibility=%t, want %t", got, want)
			}
			if !want {
				return
			}
			before := string(jsonBytes(source["input"]))
			prepared, err := PrepareResponsesBody(source, DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			body, ok := PrepareToolRepairBody(prepared, source, response)
			if !ok || string(jsonBytes(source["input"])) != before {
				t.Fatal("compatibility mutated explicit history")
			}
			history := prepared["input"].([]any)
			if !jsonValuesEqual(body["input"].([]any)[:len(history)], history) {
				t.Fatal("correction changed normalized history")
			}
			pairs := 0
			for _, value := range history {
				item := objectValue(value)
				if item["call_id"] == "call_previous" && item["type"] == "function_call_output" {
					pairs++
				}
			}
			if pairs != 1 {
				t.Fatal("explicit custom result was not normalized exactly once")
			}
		})
	}
}
