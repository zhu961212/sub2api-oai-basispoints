package protocol

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCapabilityValidationDoesNotConfuseClientAndHostedTools(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		for _, name := range []string{"web_search", "image_generation"} {
			source := map[string]any{"tools": []any{map[string]any{"type": kind, "name": name}}}
			if err := ValidateRequestCapabilities(source); err != nil {
				t.Fatalf("client %s %s rejected: %v", kind, name, err)
			}
			if warning := RequestCapabilityInstructions(source); warning != "" {
				t.Fatalf("client tool incorrectly omitted: %s", warning)
			}
		}
	}
	for _, tool := range []map[string]any{
		{"type": "image_generation"},
		{"type": "web_search", "external_web_access": true},
		{"type": "web_search_preview", "search_context_size": "high"},
	} {
		for _, source := range []map[string]any{
			{"tools": []any{tool}},
			{"tools": []any{map[string]any{"type": "namespace", "name": "helpers", "tools": []any{tool}}}},
			{"input": []any{map[string]any{"type": "additional_tools", "tools": []any{tool}}}},
		} {
			if err := ValidateRequestCapabilities(source); err == nil {
				t.Fatalf("hosted capability accepted: %#v", source)
			}
			source["tool_choice"] = "none"
			if err := ValidateRequestCapabilities(source); err != nil {
				t.Fatalf("disabled hosted tool rejected: %v", err)
			}
		}
	}
	source := map[string]any{"tools": []any{map[string]any{"type": "web_search"}, map[string]any{"type": "file_search"}}}
	if err := ValidateRequestCapabilities(source); err != nil {
		t.Fatal(err)
	}
	if warning := RequestCapabilityInstructions(source); !strings.Contains(warning, "file_search, web_search") || !strings.Contains(warning, "Do not claim") {
		t.Fatalf("hosted omission was not made explicit: %s", warning)
	}
}

func TestCapabilityValidationRejectsSilentSemanticLoss(t *testing.T) {
	for name, source := range map[string]map[string]any{
		"previous response":    {"previous_response_id": "resp_private"},
		"forced tool":          {"tool_choice": map[string]any{"type": "function", "name": "shell"}},
		"required tool":        {"tool_choice": "required"},
		"reasoning mode":       {"reasoning": map[string]any{"mode": "fast"}},
		"invalid effort":       {"reasoning": map[string]any{"effort": "extreme"}},
		"numeric effort":       {"reasoning_effort": 2},
		"legacy format":        {"response_format": map[string]any{"type": "json_object"}},
		"item reference":       {"input": []any{map[string]any{"type": "item_reference", "id": "item_private"}}},
		"configuration update": {"input": []any{map[string]any{"type": "configuration_update"}}},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateRequestCapabilities(source)
			apiErr, ok := err.(*APIError)
			if !ok || apiErr.Status != http.StatusBadRequest {
				t.Fatalf("request should fail before upstream: %v", err)
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatalf("request data echoed into error: %v", err)
			}
		})
	}
	for _, effort := range []string{"", "low", "medium", "high", "max", "ultra", "none", "minimal", "x-high"} {
		if err := ValidateRequestCapabilities(map[string]any{"reasoning": map[string]any{"effort": effort}}); err != nil {
			t.Fatalf("supported effort %q: %v", effort, err)
		}
	}
}

func TestCapabilityImagesPreserveHTTPSAndRejectUnsupportedSources(t *testing.T) {
	valid := map[string]any{"type": "input_image", "image_url": "https://images.example/a.png?token=PRIVATE%2FVALUE", "detail": "high"}
	before := string(JSONBytes(valid))
	for _, parent := range []map[string]any{
		{"role": "user", "content": []any{valid}},
		{"type": "function_call_output", "output": []any{map[string]any{"type": "text", "text": "Screenshot"}, valid}},
		{"type": "custom_tool_call_output", "output": []any{valid}},
	} {
		if err := ValidateRequestCapabilities(map[string]any{"input": []any{parent}}); err != nil {
			t.Fatal(err)
		}
		if string(JSONBytes(valid)) != before {
			t.Fatal("signed HTTPS image URL changed")
		}
	}
	for _, invalid := range []map[string]any{
		{"image_url": "data:image/png;base64,PRIVATE"},
		{"image_url": "http://images.example/PRIVATE"},
		{"image_url": "file:///PRIVATE.png"},
		{"image_url": "https:///PRIVATE.png"},
		{"image_url": "https://user:PRIVATE@images.example/a.png"},
		{"image_url": "https://images.example/a.png", "file_id": "file_PRIVATE"},
		{"image_url": "https://images.example/a.png", "detail": "original"},
		{"file_id": 42},
		{"file_id": "file_PRIVATE/path"},
		{"file_id": " file_PRIVATE"},
	} {
		invalid["type"] = "input_image"
		for _, field := range []string{"content", "output"} {
			item := map[string]any{"type": "function_call_output", field: []any{invalid}}
			err := ValidateRequestCapabilities(map[string]any{"input": []any{item}})
			if err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("unsupported private image was accepted or exposed: %v", err)
			}
		}
	}
	// Tool arguments may contain arbitrary client data, which is not image input.
	source := map[string]any{"input": []any{map[string]any{"type": "function_call", "arguments": `{"type":"input_image","image_url":"file:///client/path"}`}}}
	if err := ValidateRequestCapabilities(source); err != nil {
		t.Fatalf("client tool arguments were treated as BPS images: %v", err)
	}
}

func structuredTestSource(schema map[string]any) map[string]any {
	format := map[string]any{"type": "json_object"}
	if schema != nil {
		format = map[string]any{"type": "json_schema", "name": "result", "strict": true, "schema": schema}
	}
	return map[string]any{"text": map[string]any{"format": format}}
}

func structuredTestResponse(text string) map[string]any {
	return map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": text}}}}}
}

func TestStructuredJSONRequiresExactlyOneValueWithoutChangingSource(t *testing.T) {
	source := structuredTestSource(nil)
	before := string(JSONBytes(source))
	prompt, err := prepareStructuredOutput(source)
	if err != nil || !strings.Contains(prompt, "exactly one JSON value") || !HasStructuredOutput(source) {
		t.Fatalf("structured instructions missing: %q, %v", prompt, err)
	}
	for _, answer := range []string{`{"ok":true}`, ` {"large":9007199254740993} `} {
		if err := validateStructuredResponse(structuredTestResponse(answer), source); err != nil {
			t.Fatalf("valid JSON rejected: %v", err)
		}
	}
	for _, answer := range []string{"", "```json\n{}\n```", `{} {}`, `{"x":`, `{"x": NaN}`} {
		err := validateStructuredResponse(structuredTestResponse(answer), source)
		if api, ok := err.(*APIError); !ok || api.Status != http.StatusBadGateway {
			t.Fatalf("invalid answer accepted: %q, %v", answer, err)
		}
	}
	if string(JSONBytes(source)) != before {
		t.Fatal("structured preparation or validation mutated request")
	}
}

func TestStructuredSchemaValidatesLocalRefsAndExactIntegers(t *testing.T) {
	schema := map[string]any{
		"type": "object", "properties": map[string]any{"id": map[string]any{"$ref": "#/$defs/id"}},
		"required": []any{"id"}, "additionalProperties": false,
		"$defs": map[string]any{"id": map[string]any{"type": "integer", "const": json.Number("9007199254740993")}},
	}
	source := structuredTestSource(schema)
	if _, err := prepareStructuredOutput(source); err != nil {
		t.Fatal(err)
	}
	for answer, valid := range map[string]bool{
		`{"id":9007199254740993}`:              true,
		`{"id":9007199254740992}`:              false,
		`{"id":"9007199254740993"}`:            false,
		`{"id":9007199254740993,"extra":true}`: false,
		`{}`:                                   false,
	} {
		if err := validateStructuredResponse(structuredTestResponse(answer), source); (err == nil) != valid {
			t.Fatalf("schema result for %s: %v", answer, err)
		}
	}
}

func TestStructuredSchemaNeverLoadsExternalResources(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"type":"object"}`))
	}))
	defer server.Close()
	for _, ref := range []string{server.URL + "/PRIVATE.json", "file:///PRIVATE.json"} {
		_, err := prepareStructuredOutput(structuredTestSource(map[string]any{"$ref": ref}))
		if err == nil || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatalf("external reference allowed or exposed: %v", err)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("schema compilation fetched an external resource")
	}
	if _, err := prepareStructuredOutput(structuredTestSource(map[string]any{"description": strings.Repeat("x", (1<<20)+1)})); err == nil {
		t.Fatal("unbounded schema accepted")
	}
}

func TestStructuredToolContinuationsRefusalsAndFailuresRemainProtocolItems(t *testing.T) {
	source := structuredTestSource(nil)
	for _, kind := range []string{"function_call", "custom_tool_call"} {
		response := structuredTestResponse("Preparing the tool")
		response["output"] = append(response["output"].([]any), map[string]any{"type": kind, "name": "shell"})
		if err := validateStructuredResponse(response, source); err != nil {
			t.Fatalf("tool continuation forced to JSON: %v", err)
		}
	}
	refusal := map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "refusal", "refusal": "Cannot comply"}}}}}
	if err := validateStructuredResponse(refusal, source); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"failed", "incomplete", "cancelled"} {
		response := structuredTestResponse("Partial non-JSON answer")
		response["status"] = status
		if err := validateStructuredResponse(response, source); err != nil {
			t.Fatalf("failure status overwritten by output validation: %v", err)
		}
	}
}

func TestCapabilityNativeImageFileIDPreservesDetails(t *testing.T) {
	for _, field := range []string{"content", "output"} {
		image := map[string]any{"type": "input_image", "file_id": "file-native-id", "detail": "high"}
		source := map[string]any{"input": []any{map[string]any{"type": "function_call_output", field: []any{image}}}}
		before := string(JSONBytes(source))
		if err := ValidateRequestCapabilities(source); err != nil {
			t.Fatal(err)
		}
		if string(JSONBytes(source)) != before {
			t.Fatal("native image changed")
		}
	}
}
