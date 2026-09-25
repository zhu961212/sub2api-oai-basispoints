package protocol

import (
	"encoding/json"
	"testing"
)

// 协作工具（collaboration.spawn_agent / send_message / followup_task）的 message
// 参数在目录里声明 encrypted:true。还原出的 function_call 必须带显式的
// encrypted_function_args 明文标记，否则 Codex 会把明文当密文发给子代理
// （客户端报 Encrypted function output content could not be decrypted or
// decoded 并断流）。对齐上游 PR#78 的语义：relay 信封恒为明文（显式空列表），
// 直接调用只保留上游已有的非 null 加密元数据。

func collaborationToolSource(names ...string) map[string]any {
	props := map[string]any{
		"message":    map[string]any{"type": "string", "encrypted": true},
		"task_name":  map[string]any{"type": "string"},
		"target":     map[string]any{"type": "string"},
		"fork_turns": map[string]any{"type": "string"},
	}
	tools := make([]any, 0, len(names))
	for _, name := range names {
		tools = append(tools, map[string]any{
			"type": "function", "name": name,
			"parameters": map[string]any{"type": "object", "properties": props},
		})
	}
	return map[string]any{
		"tools": []any{map[string]any{
			"type": "namespace", "name": "collaboration", "tools": tools,
		}},
	}
}

func wrapCollaborationEnvelope(toolName string, args map[string]any) map[string]any {
	inner := map[string]any{"tool": toolName, "args": args}
	outerArgs := map[string]any{
		"summary": "relay", "extended_summary": "relay", "destructive": false,
		"references": []any{}, "code": string(JSONBytes(inner)),
	}
	return map[string]any{
		"type": "function_call", "id": "fc_1", "call_id": "call_1",
		"name": "run_officejs", "arguments": string(JSONBytes(outerArgs)), "status": "completed",
	}
}

func requireExplicitPlaintextMarker(t *testing.T, call map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(call["encrypted_function_args"])
	if err != nil || string(encoded) != "[]" {
		t.Fatalf("expected explicit empty encryption list, got %s (%v)", encoded, err)
	}
}

// relay 信封：还原出的 function_call 必须带显式空明文标记，且中文/换行/引号原文保留。
func TestCollaborationEnvelopeCallMarkedPlaintext(t *testing.T) {
	for _, name := range []string{"spawn_agent", "send_message", "followup_task"} {
		t.Run(name, func(t *testing.T) {
			source := collaborationToolSource(name)
			args := map[string]any{
				"message": "核对 SVG 动画.\nPreserve \"quotes\", tabs\tand \r\nline endings.",
				"target":  "worker",
			}
			if name == "spawn_agent" {
				args["task_name"], args["fork_turns"] = "worker", "none"
				delete(args, "target")
			}
			native := wrapCollaborationEnvelope("collaboration."+name, args)
			response := map[string]any{"id": "resp_p", "status": "completed", "output": []any{native}}
			translated, _, changed, err := transformResponseBody(JSONBytes(response), source)
			if err != nil {
				t.Fatalf("transform failed: %v", err)
			}
			if !changed {
				t.Fatal("expected transport call to be rewritten")
			}
			var resp map[string]any
			if err := json.Unmarshal(translated, &resp); err != nil {
				t.Fatal(err)
			}
			output, _ := resp["output"].([]any)
			if len(output) != 1 {
				t.Fatalf("expected one output item, got %d", len(output))
			}
			call, _ := output[0].(map[string]any)
			if call["type"] != "function_call" || call["name"] != name || call["namespace"] != "collaboration" {
				t.Fatalf("unexpected call shape: %+v", call)
			}
			requireExplicitPlaintextMarker(t, call)
			var gotArgs map[string]any
			if err := json.Unmarshal([]byte(StringValue(call["arguments"])), &gotArgs); err != nil {
				t.Fatal(err)
			}
			if !jsonValuesEqual(gotArgs, args) {
				t.Fatalf("arguments changed: %v", gotArgs)
			}
		})
	}
}

// 直接调用（模型绕过 run_officejs）：只保留上游已有的非 null 加密元数据。
func TestCollaborationDirectCallPreservesUpstreamEncryption(t *testing.T) {
	source := collaborationToolSource("send_message")
	args := map[string]any{"message": "hello", "target": "worker"}
	native := map[string]any{
		"type": "function_call", "id": "fc_2", "call_id": "call_2",
		"name": "collaboration.send_message", "arguments": string(JSONBytes(args)),
		"encrypted_function_args": []any{"message"}, "status": "completed",
	}
	response := map[string]any{"id": "resp_d", "status": "completed", "output": []any{native}}
	translated, _, _, err := transformResponseBody(JSONBytes(response), source)
	if err != nil {
		t.Fatalf("transform failed: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(translated, &resp); err != nil {
		t.Fatal(err)
	}
	call := resp["output"].([]any)[0].(map[string]any)
	encoded, _ := json.Marshal(call["encrypted_function_args"])
	if string(encoded) != `["message"]` {
		t.Fatalf("expected upstream encryption metadata preserved, got %s", encoded)
	}
}

// 直接调用且上游没有加密元数据：按 PR 语义不再补标记（保持缺失，由 Codex 按目录声明处理）。
func TestCollaborationDirectCallWithoutMetadataStaysUnmarked(t *testing.T) {
	source := collaborationToolSource("send_message")
	args := map[string]any{"message": "hello", "target": "worker"}
	native := map[string]any{
		"type": "function_call", "id": "fc_3", "call_id": "call_3",
		"name": "collaboration.send_message", "arguments": string(JSONBytes(args)), "status": "completed",
	}
	response := map[string]any{"id": "resp_e", "status": "completed", "output": []any{native}}
	translated, _, _, err := transformResponseBody(JSONBytes(response), source)
	if err != nil {
		t.Fatalf("transform failed: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(translated, &resp); err != nil {
		t.Fatal(err)
	}
	call := resp["output"].([]any)[0].(map[string]any)
	if _, exists := call["encrypted_function_args"]; exists {
		t.Fatalf("direct call without upstream metadata must stay unmarked, got %v", call["encrypted_function_args"])
	}
}

// 流式 relay 的终点同样来自 transformResponseBody，emitRelayTool 会原样复制
// item 的全部字段，所以这里只需确认终点转换产物带显式空明文标记。
func TestCollaborationStreamRelayMarksPlaintext(t *testing.T) {
	source := collaborationToolSource("spawn_agent")
	args := map[string]any{"message": "开始设计 2D 动画", "task_name": "worker", "fork_turns": "none"}
	native := wrapCollaborationEnvelope("collaboration.spawn_agent", args)
	response := map[string]any{"id": "resp_s", "status": "completed", "output": []any{native}}
	translated, _, _, err := transformResponseBody(JSONBytes(response), source)
	if err != nil {
		t.Fatalf("transform failed: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(translated, &resp); err != nil {
		t.Fatal(err)
	}
	call := resp["output"].([]any)[0].(map[string]any)
	requireExplicitPlaintextMarker(t, call)
}
