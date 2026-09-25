package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareResponsesBodyStripsToolsAndUsesNaturalLanguageCatalog(t *testing.T) {
	cfg := DefaultConfig()
	source := map[string]any{
		"model": DefaultModelID,
		"input": []any{map[string]any{"role": "user", "content": "What is the weather?"}},
		"tools": []any{map[string]any{
			"type":        "function",
			"name":        "get_weather",
			"description": "Get current weather.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			},
		}},
		"reasoning": map[string]any{"effort": "max"},
	}
	body, err := prepareResponsesBody(source, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := body["tools"]; exists {
		t.Fatal("upstream body still contains tools")
	}
	// Basis Points 没有 max 档位，max 必须映射为 xhigh。
	if got := body["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("reasoning_effort = %v, want xhigh", got)
	}
	items, ok := body["input"].([]any)
	if !ok || len(items) < 2 {
		t.Fatalf("input = %#v, want developer prologue and user history", body["input"])
	}
	first := objectValue(items[0])
	if first["role"] != "developer" {
		t.Fatalf("first input role = %v, want developer", first["role"])
	}
	content, _ := first["content"].([]any)
	catalog := itemText(objectValue(content[0])["text"])
	if !strings.Contains(catalog, "get_weather") || !strings.Contains(catalog, "city (required)") {
		t.Fatalf("catalog omitted natural language tool directory: %s", catalog)
	}
	if !strings.Contains(catalog, `"properties"`) || !strings.Contains(catalog, `"type":"string"`) {
		t.Fatalf("catalog omitted complete parameter schema: %s", catalog)
	}
	metadata := objectValue(body["metadata"])
	if stringValue(metadata["turn_id"]) == "" || stringValue(metadata["task_id"]) == "" || metadata["agent_iteration"] != "1" {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
	if body["store"] != false {
		t.Fatalf("store = %v, want false", body["store"])
	}
}

func TestPrepareResponsesBodyPreservesImageInputAndEffort(t *testing.T) {
	images := []string{"https://example.com/image.png"}
	efforts := []struct{ input, want string }{
		{"low", "low"}, {"medium", "medium"}, {"high", "high"}, {"xhigh", "xhigh"},
		{"max", "xhigh"}, {"ultra", "xhigh"}, {" MAX ", "xhigh"}, {"", "medium"},
	}
	cfg := DefaultConfig()
	for _, image := range images {
		for _, effort := range efforts {
			for _, nested := range []bool{true, false} {
				message := map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{"type": "input_text", "text": "Describe this image"},
						map[string]any{"type": "input_image", "image_url": image, "detail": "high"},
					},
				}
				source := map[string]any{"model": DefaultModelID, "input": []any{message}}
				if nested {
					source["reasoning"] = map[string]any{"effort": effort.input}
				} else {
					source["reasoning_effort"] = effort.input
				}
				body, err := prepareResponsesBody(source, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if got := body["reasoning_effort"]; got != effort.want {
					t.Fatalf("effort %q = %v, want %s", effort.input, got, effort.want)
				}
				items := body["input"].([]any)
				if !reflect.DeepEqual(items[len(items)-1], message) {
					t.Fatal("image input changed during request preparation")
				}
			}
		}
	}
}

func TestTransportCodeUsesToolAndArgsAndPreservesNativeItem(t *testing.T) {
	source := map[string]any{
		"session_id": "native-replay-test",
		"tools": []any{map[string]any{
			"type": "function",
			"name": "get_weather",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			},
		}},
	}
	native := map[string]any{
		"type":       "function_call",
		"id":         "fc_native_weather",
		"call_id":    "call_native_weather",
		"name":       "run_officejs",
		"status":     "completed",
		"summary":    "Get current weather for Tokyo",
		"references": []any{"Tokyo weather"},
		"arguments": string(jsonBytes(map[string]any{
			"summary": "Get current weather for Tokyo",
			"code":    string(jsonBytes(map[string]any{"tool": "get_weather", "args": map[string]any{"city": "Tokyo"}})),
		})),
	}
	response := map[string]any{"output": []any{native}}
	call, ok := extractNativeClientToolCall(response, source)
	if !ok {
		t.Fatal("transport call was not decoded")
	}
	if call["name"] != "get_weather" || call["call_id"] != "call_native_weather" {
		t.Fatalf("decoded call = %#v", call)
	}
	if got := stringValue(call["arguments"]); got != `{"city":"Tokyo"}` {
		t.Fatalf("arguments = %s", got)
	}
	translated, _, changed, err := transformResponseBody(jsonBytes(response), source)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("response was not rewritten")
	}
	var clientResponse map[string]any
	if err := json.Unmarshal(translated, &clientResponse); err != nil {
		t.Fatal(err)
	}
	clientCall := objectValue(clientResponse["output"].([]any)[0])
	if clientCall["name"] != "get_weather" {
		t.Fatalf("client call = %#v", clientCall)
	}
	items := translateInputItemsInNamespace([]any{
		clientCall,
		map[string]any{
			"type":    "function_call_output",
			"call_id": "call_native_weather",
			"output":  "18°C",
		},
	}, clientToolSpecs(source), nativeCallNamespace(source))
	if !reflect.DeepEqual(items[0], native) {
		t.Fatalf("replayed native item = %#v, want %#v", items[0], native)
	}
	output := objectValue(items[1])
	if output["type"] != "function_call_output" || output["id"] != "fc_call_native_weather" || output["output"] != "18°C" {
		t.Fatalf("normalized output = %#v", output)
	}
}

func TestCustomToolTransportRoundTrip(t *testing.T) {
	source := map[string]any{
		"tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}},
	}
	native := map[string]any{
		"type":    "function_call",
		"id":      "fc_patch",
		"call_id": "call_patch",
		"name":    transportName,
		"arguments": string(jsonBytes(map[string]any{
			"summary": "Patch the file",
			"code":    string(jsonBytes(map[string]any{"tool": "apply_patch", "args": "*** Begin Patch"})),
		})),
	}
	call, ok := extractNativeClientToolCall(map[string]any{"output": []any{native}}, source)
	if !ok {
		t.Fatal("custom transport call was not decoded")
	}
	if call["type"] != "custom_tool_call" {
		t.Fatalf("call type = %v, want custom_tool_call", call["type"])
	}
	if call["input"] != "*** Begin Patch" {
		t.Fatalf("custom input = %v", call["input"])
	}
	items := translateInputItems([]any{
		call,
		map[string]any{
			"type":    "custom_tool_call_output",
			"call_id": "call_patch",
			"output":  map[string]any{"applied": true},
		},
	}, clientToolSpecs(source))
	if len(items) != 2 || !strings.Contains(string(jsonBytes(items[1])), "applied") {
		t.Fatalf("structured custom tool output was lost: %#v", items)
	}
}

func TestExtractNativeClientToolCallRejectsUnknownOrInvalidTools(t *testing.T) {
	source := map[string]any{
		"tools": []any{map[string]any{
			"type": "function",
			"name": "get_weather",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			},
		}},
	}
	cases := map[string]map[string]any{
		"unknown tool": map[string]any{
			"type": "function_call", "name": transportName, "call_id": "call_1",
			"arguments": string(jsonBytes(map[string]any{"code": `{"tool":"not_in_catalog","args":{}}`})),
		},
		"missing required argument": map[string]any{
			"type": "function_call", "name": transportName, "call_id": "call_2",
			"arguments": string(jsonBytes(map[string]any{"code": `{"tool":"get_weather","args":{}}`})),
		},
		"nested transport envelope": map[string]any{
			"type": "function_call", "name": transportName, "call_id": "call_3",
			"arguments": string(jsonBytes(map[string]any{"code": string(jsonBytes(map[string]any{
				"tool": transportName, "args": map[string]any{},
			}))})),
		},
	}
	for name, item := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := extractNativeClientToolCall(map[string]any{"output": []any{item}}, source); ok {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

func TestToolArgumentsSupportSchemaCombinatorsAndObjectValues(t *testing.T) {
	source := map[string]any{
		"model": "gpt-6-astra",
		"input": []any{map[string]any{"role": "user", "content": "use tool"}},
		"tools": []any{map[string]any{
			"type": "function", "name": "lookup",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"oneOf": []any{
						map[string]any{"type": "string"},
						map[string]any{"type": "null"},
					}},
					"mode": map[string]any{"const": "fast"},
				},
				"required": []any{"query", "mode"},
			},
		}},
	}
	native := map[string]any{
		"type": "function_call", "id": "fc_lookup", "call_id": "call_lookup",
		"name": transportName,
		// Arguments are an object here, while the outer Responses field remains JSON text.
		"arguments": string(jsonBytes(map[string]any{
			"code": string(jsonBytes(map[string]any{
				"tool": "lookup",
				"args": map[string]any{"query": nil, "mode": "fast"},
			})),
		})),
	}
	call, ok := extractNativeClientToolCall(map[string]any{"output": []any{native}}, source)
	if !ok {
		t.Fatal("valid schema-combinator arguments were rejected")
	}
	if got := stringValue(call["arguments"]); got != `{"mode":"fast","query":null}` {
		t.Fatalf("arguments = %s", got)
	}

	// A client function_call with object-valued arguments must survive the relay
	// fallback instead of silently becoming an empty argument object.
	items := translateInputItems([]any{map[string]any{
		"type": "function_call", "call_id": "call_client", "name": "lookup",
		"arguments": map[string]any{"query": "Tokyo", "mode": "fast"},
	}}, clientToolSpecs(source))
	if len(items) != 1 || !strings.Contains(string(jsonBytes(items[0])), "Tokyo") {
		t.Fatalf("object-valued client arguments were lost: %#v", items)
	}
}

func TestNamespacedToolKeepsClientToolName(t *testing.T) {
	source := map[string]any{
		"tools": []any{map[string]any{
			"type": "namespace", "name": "files",
			"tools": []any{map[string]any{"type": "function", "name": "read", "parameters": map[string]any{"type": "object"}}},
		}},
	}
	native := map[string]any{
		"type": "function_call", "id": "fc_read", "call_id": "call_read", "name": transportName,
		"arguments": string(jsonBytes(map[string]any{"code": string(jsonBytes(map[string]any{
			"tool": "files.read", "args": map[string]any{},
		}))})),
	}
	call, ok := extractNativeClientToolCall(map[string]any{"output": []any{native}}, source)
	if !ok || call["name"] != "read" || call["namespace"] != "files" {
		t.Fatalf("namespaced tool name = %#v, want files.read", call)
	}
}

func TestNativeCallReplayCacheIsConversationScoped(t *testing.T) {
	tool := map[string]any{
		"type": "function", "name": "lookup",
		"parameters": map[string]any{"type": "object"},
	}
	sourceA := map[string]any{
		"model": "gpt-6-astra",
		"input": []any{map[string]any{"role": "user", "content": "session A"}},
		"tools": []any{tool},
	}
	sourceB := cloneObject(sourceA)
	sourceB["input"] = []any{map[string]any{"role": "user", "content": "session B"}}
	native := map[string]any{
		"type": "function_call", "id": "fc_same", "call_id": "call_same", "name": transportName,
		"arguments": string(jsonBytes(map[string]any{"code": string(jsonBytes(map[string]any{
			"tool": "lookup", "args": map[string]any{"value": "A"},
		}))})),
	}
	if _, ok := extractNativeClientToolCall(map[string]any{"output": []any{native}}, sourceA); !ok {
		t.Fatal("session A native call was not decoded")
	}
	items := translateInputItemsInNamespace(
		[]any{map[string]any{"type": "function_call", "call_id": "call_same", "name": "lookup", "arguments": `{"value":"B"}`}},
		clientToolSpecs(sourceB), nativeCallNamespace(sourceB),
	)
	if len(items) != 1 || !strings.Contains(string(jsonBytes(items[0])), "B") {
		t.Fatalf("session B reused session A native call: %#v", items)
	}
}

func TestNativeCallReplayMatchesToolArgumentsAcrossCollidingSessions(t *testing.T) {
	tool := map[string]any{
		"type": "function", "name": "lookup",
		"parameters": map[string]any{"type": "object"},
	}
	newSource := func() map[string]any {
		return map[string]any{
			"model": "gpt-6-astra",
			"input": []any{map[string]any{"role": "user", "content": "Inspect the workbook"}},
			"tools": []any{tool},
		}
	}
	newNative := func(value string) map[string]any {
		return map[string]any{
			"type": "function_call", "id": "fc_collision", "call_id": "call_collision", "name": transportName,
			"arguments": string(jsonBytes(map[string]any{"code": string(jsonBytes(map[string]any{
				"tool": "lookup", "args": map[string]any{"value": value},
			}))})),
		}
	}
	sourceA := newSource()
	sourceB := newSource()
	if _, ok := extractNativeClientToolCall(map[string]any{"output": []any{newNative("A")}}, sourceA); !ok {
		t.Fatal("session A native call was not decoded")
	}
	if _, ok := extractNativeClientToolCall(map[string]any{"output": []any{newNative("B")}}, sourceB); !ok {
		t.Fatal("session B native call was not decoded")
	}
	items := translateInputItemsInNamespace(
		[]any{map[string]any{"type": "function_call", "call_id": "call_collision", "name": "lookup", "arguments": `{"value":"A"}`}},
		clientToolSpecs(sourceA), nativeCallNamespace(sourceA),
	)
	if len(items) != 1 {
		t.Fatalf("replayed items = %#v", items)
	}
	replayed := objectValue(items[0])
	wrapper := parseArguments(replayed["arguments"])
	inner := decodeTransportCode(wrapper["code"])
	args := objectValue(inner["args"])
	if stringValue(args["value"]) != "A" {
		t.Fatalf("wrong session native call replayed: %#v", items[0])
	}
}

func TestToolCatalogAndReplayContextSurviveFollowUpWithoutTools(t *testing.T) {
	source := map[string]any{
		"session_id": "session-follow-up",
		"model":      "gpt-6-astra",
		"input":      []any{map[string]any{"role": "user", "content": "Read a local file"}},
		"tools": []any{map[string]any{
			"type": "function", "name": "read_local",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
		}},
	}
	if _, err := prepareResponsesBody(source, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	followUp := map[string]any{
		"session_id": "session-follow-up",
		"model":      "gpt-6-astra",
		"input": []any{map[string]any{
			"type": "function_call", "id": "fc_read", "call_id": "call_read", "name": "read_local", "arguments": `{"path":"C:/workbook.xlsx"}`,
		}},
	}
	body, err := prepareResponsesBody(followUp, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	items := body["input"].([]any)
	catalog := itemText(objectValue(objectValue(items[0])["content"].([]any)[0])["text"])
	if !strings.Contains(catalog, "read_local") || !strings.Contains(catalog, `"path"`) {
		t.Fatalf("follow-up lost cached tool catalog: %s", catalog)
	}
}

func TestAdditionalToolsBecomeUsableCatalogAndAreNotSentAsUpstreamInput(t *testing.T) {
	source := map[string]any{
		"model": "gpt-6-astra",
		"input": []any{
			map[string]any{"type": "additional_tools", "tools": []any{map[string]any{
				"type": "function", "name": "read_local", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
			}}},
			map[string]any{"role": "user", "content": "Read a local file"},
		},
	}
	body, err := prepareResponsesBody(source, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	items := body["input"].([]any)
	catalog := itemText(objectValue(objectValue(items[0])["content"].([]any)[0])["text"])
	if !strings.Contains(catalog, "read_local") || !strings.Contains(catalog, `"properties"`) {
		t.Fatalf("additional tool was not described: %s", catalog)
	}
	for _, item := range items {
		if stringValue(objectValue(item)["type"]) == "additional_tools" {
			t.Fatal("additional_tools leaked into upstream input")
		}
	}
}

func TestExplicitSessionReplayDoesNotCrossFallback(t *testing.T) {
	newSource := func(session string) map[string]any {
		return map[string]any{"session_id": session, "tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}}
	}
	native := func(id string) map[string]any {
		return map[string]any{"type": "function_call", "id": id, "call_id": "call_explicit", "name": transportName, "arguments": string(jsonBytes(map[string]any{"code": string(jsonBytes(map[string]any{"tool": "lookup", "args": map[string]any{}}))}))}
	}
	sourceA, sourceB := newSource("session-A"), newSource("session-B")
	if _, ok := extractNativeClientToolCall(map[string]any{"output": []any{native("fc_A")}}, sourceA); !ok {
		t.Fatal("session A native call was not decoded")
	}
	if _, ok := extractNativeClientToolCall(map[string]any{"output": []any{native("fc_B")}}, sourceB); !ok {
		t.Fatal("session B native call was not decoded")
	}
	items := translateInputItemsInNamespace([]any{map[string]any{"type": "function_call", "id": "fc_A", "call_id": "call_explicit", "name": "lookup", "arguments": `{}`}}, clientToolSpecs(sourceA), nativeCallNamespace(sourceA))
	if len(items) != 1 || stringValue(objectValue(items[0])["id"]) != "fc_A" {
		t.Fatalf("explicit session replay crossed namespaces: %#v", items)
	}
}

func TestMultipleTransportCallsAreNotRewritten(t *testing.T) {
	source := map[string]any{
		"tools": []any{map[string]any{"type": "function", "name": "demo", "parameters": map[string]any{"type": "object"}}},
	}
	first := map[string]any{"type": "function_call", "name": transportName, "call_id": "call_a", "arguments": "{}"}
	second := map[string]any{"type": "function_call", "name": transportName, "call_id": "call_b", "arguments": "{}"}
	if _, ok := extractNativeClientToolCall(map[string]any{"output": []any{first, second}}, source); ok {
		t.Fatal("ambiguous parallel transport calls must not be rewritten")
	}
}

func TestTransformResponseBodyRewritesParallelTransportCalls(t *testing.T) {
	source := map[string]any{
		"prompt_cache_key": "parallel-tools-test",
		"tools": []any{
			map[string]any{"type": "function", "name": "get_weather", "parameters": map[string]any{
				"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []any{"city"},
			}},
			map[string]any{"type": "function", "name": "get_time", "parameters": map[string]any{
				"type": "object", "properties": map[string]any{"timezone": map[string]any{"type": "string"}}, "required": []any{"timezone"},
			}},
		},
	}
	nativeCall := func(callID, tool string, args map[string]any) map[string]any {
		return map[string]any{
			"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": transportName,
			"arguments": string(jsonBytes(map[string]any{
				"summary": "parallel tool call",
				"code":    string(jsonBytes(map[string]any{"tool": tool, "args": args})),
			})),
		}
	}
	body, _, changed, err := transformResponseBody(jsonBytes(map[string]any{
		"id": "resp_parallel", "status": "completed", "output": []any{
			nativeCall("call_weather", "get_weather", map[string]any{"city": "Tokyo"}),
			nativeCall("call_time", "get_time", map[string]any{"timezone": "UTC"}),
		},
	}), source)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("parallel transport calls were not rewritten")
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	output, ok := response["output"].([]any)
	if !ok || len(output) != 2 {
		t.Fatalf("output = %#v", response["output"])
	}
	if got := objectValue(output[0])["name"]; got != "get_weather" {
		t.Fatalf("first parallel call = %#v", output[0])
	}
	if got := objectValue(output[1])["name"]; got != "get_time" {
		t.Fatalf("second parallel call = %#v", output[1])
	}
}

func TestTurnIDStaysStableWhileAgentIterationAdvances(t *testing.T) {
	base := map[string]any{
		"model": "gpt-6-astra-basispoints",
		"input": []any{map[string]any{"role": "user", "content": "Inspect the workbook"}},
	}
	cfg := DefaultConfig()
	first, err := prepareResponsesBody(base, cfg)
	if err != nil {
		t.Fatal(err)
	}
	nextSource := cloneObject(base)
	nextSource["input"] = append(nextSource["input"].([]any),
		map[string]any{"type": "function_call", "call_id": "call_1", "name": transportName, "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "done"},
	)
	second, err := prepareResponsesBody(nextSource, cfg)
	if err != nil {
		t.Fatal(err)
	}
	firstMetadata := objectValue(first["metadata"])
	secondMetadata := objectValue(second["metadata"])
	if firstMetadata["turn_id"] != secondMetadata["turn_id"] {
		t.Fatalf("turn_id changed: %v -> %v", firstMetadata["turn_id"], secondMetadata["turn_id"])
	}
	if secondMetadata["agent_iteration"] != "2" {
		t.Fatalf("agent_iteration = %v, want 2", secondMetadata["agent_iteration"])
	}
}

func TestToolChoiceNoneDropsCatalog(t *testing.T) {
	source := map[string]any{
		"tool_choice": "none",
		"tools":       []any{map[string]any{"type": "function", "name": "demo"}},
	}
	if specs := clientToolSpecs(source); len(specs) != 0 {
		t.Fatalf("tool_choice=none still exposed %d tools", len(specs))
	}
	cfg := DefaultConfig()
	body, err := prepareResponsesBody(source, cfg)
	if err != nil {
		t.Fatal(err)
	}
	items := body["input"].([]any)
	content, _ := objectValue(items[0])["content"].([]any)
	instructions := itemText(objectValue(content[0])["text"])
	if !strings.Contains(instructions, "Do not call server-injected") {
		t.Fatalf("instructions = %s", instructions)
	}
}

func TestValidationErrorOmitsRejectedInput(t *testing.T) {
	raw := []byte(`{"detail":[{"loc":["body","reasoning_effort"],"msg":"unsupported level","type":"enum","input":"private-prompt","ctx":{"input":"private-context"}}]}`)
	message := ErrorMessage(raw)
	if !strings.Contains(message, "reasoning_effort") || !strings.Contains(message, "unsupported level") {
		t.Fatal("validation details missing")
	}
	if strings.Contains(message, "private-") {
		t.Fatal("rejected input exposed")
	}
}

func TestSyntheticStreamEndsWithCompletedResponse(t *testing.T) {
	response := map[string]any{
		"id":     "resp_test",
		"status": "completed",
		"output": []any{map[string]any{
			"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "demo",
			"arguments": `{}`, "status": "completed",
		}},
	}
	stream := string(SyntheticStream(response))
	for _, event := range []string{"response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.completed"} {
		if !strings.Contains(stream, "event: "+event) {
			t.Fatalf("stream is missing event %s:\n%s", event, stream)
		}
	}
	if !strings.HasSuffix(stream, "data: [DONE]\n\n") {
		t.Fatal("stream does not terminate with [DONE]")
	}
	parsed, err := ParseFinalStreamResponse([]byte(stream))
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(parsed["id"]) != "resp_test" || stringValue(parsed["status"]) != "completed" {
		t.Fatalf("parsed response = %#v", parsed)
	}
}

func TestSyntheticStreamIncludesCustomToolInput(t *testing.T) {
	stream := string(SyntheticStream(map[string]any{
		"id": "resp_custom", "status": "completed", "output": []any{map[string]any{
			"type": "custom_tool_call", "id": "ct_1", "call_id": "call_custom", "name": "apply_patch", "input": "*** Begin Patch",
		}},
	}))
	if !strings.Contains(stream, "event: response.custom_tool_call_input.done") || !strings.Contains(stream, "*** Begin Patch") {
		t.Fatalf("custom tool input event missing: %s", stream)
	}
}

func TestParseFinalStreamResponseRejectsTruncatedStream(t *testing.T) {
	truncated := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"
	if _, err := ParseFinalStreamResponse([]byte(truncated)); err == nil {
		t.Fatal("stream without response.completed was accepted")
	}
}

func TestParseFinalStreamResponseFlushesCompletedEventAtEOF(t *testing.T) {
	// Some HTTP servers close immediately after the final data line and omit
	// SSE's optional blank separator. The completed response is still valid.
	raw := `event: response.completed
data: {"type":"response.completed","response":{"id":"resp_eof","status":"completed"}}`
	parsed, err := ParseFinalStreamResponse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(parsed["id"]) != "resp_eof" || stringValue(parsed["status"]) != "completed" {
		t.Fatalf("parsed = %#v", parsed)
	}
}

func TestParseFinalStreamResponseAcceptsPlainJSON(t *testing.T) {
	parsed, err := ParseFinalStreamResponse([]byte(`{"id":"resp_plain","status":"completed"}`))
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(parsed["id"]) != "resp_plain" {
		t.Fatalf("parsed = %#v", parsed)
	}
}

func TestPrepareResponsesBodyPreservesSupportedModel(t *testing.T) {
	for name, cfg := range map[string]Config{"default": DefaultConfig(), "all models": {EnabledModels: AvailableModels()}} {
		t.Run(name, func(t *testing.T) {
			// The transport routes unsupported models away before preparation.
			// Supported models stay unchanged; direct unsupported preparation keeps its default.
			for _, test := range []struct{ requested, want string }{
				{DefaultModelID, "gpt-6-astra"}, {" gpt-6-astra ", "gpt-6-astra"},
				{"gpt-5.6-sol", "gpt-5.6-sol"}, {" \tgpt-5.6-sol\n", "gpt-5.6-sol"},
				{"gpt-6-sol", "gpt-6-sol"}, {" gpt-6-luna ", "gpt-6-luna"},
				{"gpt-5.6-terra", "gpt-5.6-terra"}, {"\tgpt-5.6-luna\n", "gpt-5.6-luna"},
				{"gpt-5.6-luna-excel", "gpt-6-astra"}, {"gpt-5.6-sol-excel", "gpt-6-astra"},
				{"GPT-5.6-SOL", "gpt-6-astra"}, {"", "gpt-6-astra"},
			} {
				source := map[string]any{
					"input": []any{map[string]any{"role": "user", "content": "hi"}},
				}
				if test.requested != "" {
					source["model"] = test.requested
				}
				body, err := prepareResponsesBody(source, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if body["model"] != test.want || body["model_selection"] != "explicit" {
					t.Fatalf("request %q: model = %v, want %q; selection = %v", test.requested, body["model"], test.want, body["model_selection"])
				}
			}
		})
	}
}

func TestToolCatalogLeadsAndReminderTrails(t *testing.T) {
	cfg := DefaultConfig()
	source := map[string]any{
		"model": DefaultModelID,
		"input": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{
			"type": "function", "name": "demo",
			"parameters": map[string]any{"type": "object"},
		}},
	}
	body, err := prepareResponsesBody(source, cfg)
	if err != nil {
		t.Fatal(err)
	}
	items := body["input"].([]any)
	if len(items) != 3 {
		t.Fatalf("input items = %d, want prologue + user + trailing reminder", len(items))
	}
	// 目录前置：让 instructions + 目录这段前缀保持字节稳定，便于上游缓存复用。
	first := objectValue(items[0])
	if first["role"] != "developer" {
		t.Fatalf("first role = %v", first["role"])
	}
	firstContent, _ := first["content"].([]any)
	if catalog := itemText(objectValue(firstContent[0])["text"]); !strings.Contains(catalog, "demo") {
		t.Fatalf("leading developer message has no catalog: %s", catalog)
	}
	// 短提醒尾声：模型需要这个 recency 才遵守两段式传输协议。
	last := objectValue(items[len(items)-1])
	if last["role"] != "developer" {
		t.Fatalf("last role = %v", last["role"])
	}
	lastContent, _ := last["content"].([]any)
	reminder := itemText(objectValue(lastContent[0])["text"])
	if !strings.Contains(reminder, "Reminder") {
		t.Fatalf("trailing reminder = %s", reminder)
	}
}

func TestNoTrailingReminderWithoutClientTools(t *testing.T) {
	cfg := DefaultConfig()
	source := map[string]any{
		"model": DefaultModelID,
		"input": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	body, err := prepareResponsesBody(source, cfg)
	if err != nil {
		t.Fatal(err)
	}
	items := body["input"].([]any)
	last := objectValue(items[len(items)-1])
	if last["role"] != "user" {
		t.Fatalf("last item should remain the user turn, got %#v", last)
	}
}
