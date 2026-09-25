package protocol

import (
	"testing"
)

func planCatalogSource(tools ...map[string]any) map[string]any {
	list := make([]any, 0, len(tools))
	for _, tool := range tools {
		list = append(list, tool)
	}
	return map[string]any{"session_id": "plan-test", "tools": list}
}

func planFunctionTool(name string) map[string]any {
	return map[string]any{
		"type": "function", "name": name,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"plan": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"step":   map[string]any{"type": "string"},
							"status": map[string]any{"type": "string", "enum": []any{"pending", "in_progress", "completed"}},
						},
						"required": []any{"step", "status"},
					},
				},
				"explanation": map[string]any{"type": "string"},
			},
			"required": []any{"plan"},
		},
	}
}

func planResponse(name, arguments string) map[string]any {
	return map[string]any{
		"id": "resp_plan", "status": "completed",
		"output": []any{map[string]any{
			"type": "function_call", "id": "fc_plan", "call_id": "call_plan", "name": name,
			"arguments": arguments, "status": "completed",
		}},
	}
}

func translatedPlanArguments(t *testing.T, body []byte) map[string]any {
	t.Helper()
	translated, err := RawObject(body)
	if err != nil {
		t.Fatal(err)
	}
	output, _ := translated["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %#v", translated["output"])
	}
	item := objectValue(output[0])
	if stringValue(item["name"]) != "update_plan" || stringValue(item["type"]) != "function_call" {
		t.Fatalf("plan item not canonicalized: %#v", item)
	}
	arguments := parseArguments(item["arguments"])
	if arguments == nil {
		t.Fatalf("plan arguments are not a JSON object: %#v", item["arguments"])
	}
	return arguments
}

// 模型绕过 run_officejs 直接调用计划工具是很常见的；描述字段与状态值的写法
// 差异不应该让整条响应失败。
func TestDirectPlanCallNormalizesAliasesAndStatuses(t *testing.T) {
	source := planCatalogSource(planFunctionTool("update_plan"))
	response := planResponse("functions.update_plan", `{"plan":[{"title":"scan the workbook","status":"done"},{"description":"patch the sheet","status":"todo"}],"summary":"two steps"}`)
	translated, _, changed, err := TransformResponseBody(JSONBytes(response), source)
	if err != nil {
		t.Fatalf("plan aliases must not fail the whole response: %v", err)
	}
	if !changed {
		t.Fatal("plan call was not reported as translated")
	}
	arguments := translatedPlanArguments(t, translated)
	steps, _ := arguments["plan"].([]any)
	if len(steps) != 2 {
		t.Fatalf("plan steps = %#v", arguments["plan"])
	}
	first, second := objectValue(steps[0]), objectValue(steps[1])
	if first["step"] != "scan the workbook" || first["status"] != "completed" {
		t.Fatalf("first step not normalized: %#v", first)
	}
	if second["step"] != "patch the sheet" || second["status"] != "pending" {
		t.Fatalf("second step not normalized: %#v", second)
	}
	if arguments["explanation"] != "two steps" {
		t.Fatalf("summary alias was dropped: %#v", arguments)
	}
}

// 归一化必须 fail-closed：无法确定性地重写时整条响应仍然报错，绝不能放行
// 一个可能被误解的计划调用。
func TestPlanNormalizationFailsClosed(t *testing.T) {
	cases := []struct{ name, arguments string }{
		{"unknown status", `{"plan":[{"step":"scan","status":"blocked-by-aliens"}]}`},
		{"empty status", `{"plan":[{"step":"scan","status":""}]}`},
		{"missing description", `{"plan":[{"status":"done"}]}`},
		{"blank description", `{"plan":[{"step":"   ","status":"done"}]}`},
		{"conflicting description", `{"plan":[{"step":"scan","title":"other","status":"done"}]}`},
		{"non-string description", `{"plan":[{"step":7,"status":"done"}]}`},
		{"plan is not an array", `{"plan":{"step":"scan"}}`},
		{"step is not an object", `{"plan":["scan"]}`},
		{"conflicting explanation", `{"explanation":"one","summary":"two","plan":[{"step":"scan","status":"done"}]}`},
		{"arguments are not JSON", `not json at all`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source := planCatalogSource(planFunctionTool("update_plan"))
			if _, _, _, err := TransformResponseBody(JSONBytes(planResponse("functions.update_plan", testCase.arguments)), source); err == nil {
				t.Fatal("unrecoverable plan call was accepted")
			}
		})
	}
}

// 归一化只在目录里恰好声明了一条计划工具时生效，否则同名 function 与 custom
// 无法区分，必须拒绝。
func TestPlanNormalizationNeedsOneUnambiguousDeclaration(t *testing.T) {
	arguments := `{"plan":[{"step":"scan","status":"done"}]}`
	for name, source := range map[string]map[string]any{
		"not declared": planCatalogSource(map[string]any{"type": "function", "name": "shell", "parameters": map[string]any{"type": "object"}}),
		"declared twice": planCatalogSource(
			planFunctionTool("update_plan"),
			planFunctionTool("functions.update_plan"),
		),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := TransformResponseBody(JSONBytes(planResponse("functions.update_plan", arguments)), source); err == nil {
				t.Fatal("plan call was normalized without exactly one declared plan tool")
			}
		})
	}
}

// 已经符合契约的调用必须原样通过，只做 JSON 对象重建，不改语义。
func TestPlanNormalizationKeepsCanonicalArguments(t *testing.T) {
	source := planCatalogSource(planFunctionTool("update_plan"))
	arguments := `{"plan":[{"step":"scan","status":"in_progress"}],"explanation":"already canonical"}`
	translated, _, changed, err := TransformResponseBody(JSONBytes(planResponse("update_plan", arguments)), source)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("direct plan call must consume the native call")
	}
	got := translatedPlanArguments(t, translated)
	if !jsonValuesEqual(got, parseArguments(arguments)) {
		t.Fatalf("canonical plan arguments were rewritten: %s", JSONBytes(got))
	}
}

// 状态别名表：只认确定的写法，其余一律拒绝。
func TestPlanStatusAliasTable(t *testing.T) {
	aliases := map[string]string{
		"done": "completed", "finished": "completed", "complete": "completed", "COMPLETED": "completed",
		"todo": "pending", "not_started": "pending", "planned": "pending", "queued": "pending", "blocked": "pending",
		"active": "in_progress", "doing": "in_progress", "current": "in_progress", "started": "in_progress",
		"in-progress": "in_progress", "in progress": "in_progress",
	}
	for input, want := range aliases {
		source := planCatalogSource(planFunctionTool("update_plan"))
		body, _, _, err := TransformResponseBody(JSONBytes(planResponse("update_plan", `{"plan":[{"step":"scan","status":"`+input+`"}]}`)), source)
		if err != nil {
			t.Fatalf("status %q was not normalized: %v", input, err)
		}
		arguments := translatedPlanArguments(t, body)
		steps, _ := arguments["plan"].([]any)
		if got := stringValue(objectValue(steps[0])["status"]); got != want {
			t.Fatalf("status %q normalized to %q, want %q", input, got, want)
		}
	}
	// 归一化后的值仍受目录声明的枚举约束：声明里没有的取值必须被拒绝。
	tool := planFunctionTool("update_plan")
	properties := objectValue(objectValue(objectValue(tool["parameters"])["properties"])["plan"])
	objectValue(properties["items"])["properties"].(map[string]any)["status"] = map[string]any{"type": "string", "enum": []any{"queued", "running"}}
	source := planCatalogSource(tool)
	if _, _, _, err := TransformResponseBody(JSONBytes(planResponse("update_plan", `{"plan":[{"step":"scan","status":"done"}]}`)), source); err == nil {
		t.Fatal("normalized status ignored the declared client enum")
	}
}
