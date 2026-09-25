package protocol

import "strings"

// 上游偶尔会绕过 run_officejs 直接调用目录里的计划工具，而且计划条目的
// 描述字段与状态值在不同客户端/模型之间存在多种写法。宿主内建实现（以及
// Codex 客户端）只认 step + pending/in_progress/completed，所以这里做一层
// 有界归一化：只处理目录里唯一声明的 update_plan，归一化结果仍要重新通过
// 该工具声明的 JSON Schema 校验，否则调用方按原样拒绝。绝不执行计划。

// planToolNames 是计划工具在客户端侧可能出现的两种写法。
var planToolNames = []string{"update_plan", "functions.update_plan"}

func isPlanToolName(name string) bool {
	for _, candidate := range planToolNames {
		if name == candidate {
			return true
		}
	}
	return false
}

// planTextAlias 读取一组互为别名的文本字段：同时出现且不一致时视为歧义。
func planTextAlias(value map[string]any, keys ...string) (string, bool, bool) {
	var result string
	present := false
	for _, key := range keys {
		raw, exists := value[key]
		if !exists {
			continue
		}
		candidate, ok := raw.(string)
		if !ok || (present && result != candidate) {
			return "", false, false
		}
		result, present = candidate, true
	}
	return result, present, true
}

// normalizePlanStatus 把常见状态写法折叠到 Codex 客户端实际接受的三个值。
func normalizePlanStatus(status string) string {
	switch strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(status))) {
	case "pending", "not_started", "todo", "planned", "queued", "blocked":
		return "pending"
	case "in_progress", "active", "started", "doing", "current":
		return "in_progress"
	case "completed", "complete", "done", "finished":
		return "completed"
	default:
		return ""
	}
}

// normalizeNativePlan 把一次直接的计划调用归一化成客户端声明的契约。
// 返回 false 表示无法确定性地归一化，调用方应当拒绝这次调用。
func normalizeNativePlan(arguments map[string]any, spec toolSpec, specs map[string]toolSpec) (map[string]any, bool) {
	if spec.Type != "function" || arguments == nil {
		return nil, false
	}
	// 目录里必须恰好有一条计划工具声明：同名 function 与 custom 同时存在时
	// 无法判断该用哪种契约，宁可拒绝也不猜。
	matches := 0
	for _, key := range planToolNames {
		if _, exists := specs[key]; exists {
			matches++
		}
	}
	if matches != 1 || !isPlanToolName(spec.Key) {
		return nil, false
	}
	parameters := firstMap(spec.Spec, "parameters", "inputSchema", "input_schema")
	if stringValue(parameters["type"]) != "object" || objectValue(parameters["properties"])["plan"] == nil {
		return nil, false
	}
	steps, ok := arguments["plan"].([]any)
	if !ok {
		return nil, false
	}
	plan := make([]any, 0, len(steps))
	for _, value := range steps {
		step := objectValue(value)
		if step == nil {
			return nil, false
		}
		description, present, ok := planTextAlias(step, "step", "description", "title")
		if !ok || !present || strings.TrimSpace(description) == "" {
			return nil, false
		}
		status := normalizePlanStatus(stringValue(step["status"]))
		if status == "" {
			return nil, false
		}
		plan = append(plan, map[string]any{"step": description, "status": status})
	}
	translated := map[string]any{"plan": plan}
	explanation, present, ok := planTextAlias(arguments, "explanation", "summary")
	if !ok {
		return nil, false
	}
	if present {
		translated["explanation"] = explanation
	}
	return translated, true
}
