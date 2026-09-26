package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The BPS endpoint does not share the native Responses history or hosted-tool
// execution service. Reject requests whose meaning would otherwise be lost.
func ValidateRequestCapabilities(source map[string]any) error {
	return validateRequestCapabilities(source, false)
}

// ValidateImageUploadCapabilities checks the request before image processing.
// User images become attachments; tool screenshots use native inline content.
func ValidateImageUploadCapabilities(source map[string]any) error {
	return validateRequestCapabilities(source, true)
}

// Retained for callers compiled against the former relay helper.
func ValidateImageRelayCapabilities(source map[string]any) error {
	return ValidateImageUploadCapabilities(source)
}

func validateRequestCapabilities(source map[string]any, allowInline bool) error {
	for _, key := range []string{"previous_response_id", "previousResponseId"} {
		if stringValue(source[key]) != "" {
			return capabilityError("Basis Points requires expanded conversation history instead of previous_response_id")
		}
	}
	if choice := source["tool_choice"]; choice != nil && stringValue(choice) != "auto" && stringValue(choice) != "none" {
		return capabilityError("Basis Points supports tool_choice auto or none only; forced or hosted tool selection is unavailable")
	}
	reasoning := objectValue(source["reasoning"])
	if mode := stringValue(reasoning["mode"]); mode != "" && mode != "standard" {
		return capabilityError("Basis Points supports standard reasoning mode only")
	}
	effort := source["reasoning_effort"]
	if reasoning != nil {
		effort = reasoning["effort"]
	}
	if effort != nil {
		value, ok := effort.(string)
		if !ok {
			return capabilityError("reasoning effort must be a supported string")
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "low", "medium", "high", "xhigh", "x-high", "extra-high", "extra_high", "max", "ultra", "none", "minimal":
		default:
			return capabilityError("Basis Points reasoning effort is unsupported")
		}
	}
	if source["response_format"] != nil {
		return capabilityError("Responses structured output must use text.format instead of response_format")
	}
	if stringValue(source["tool_choice"]) != "none" {
		if err := validateDeclaredCapabilities(source["tools"]); err != nil {
			return err
		}
		if err := validateDeclaredCapabilities(additionalRequestTools(source)); err != nil {
			return err
		}
	}
	items, _ := source["input"].([]any)
	for index, raw := range items {
		item := objectValue(raw)
		switch kind := strings.ToLower(stringValue(item["type"])); kind {
		case "item_reference":
			return capabilityError("Basis Points requires expanded conversation history instead of item_reference")
		case "configuration_update":
			return capabilityError("Basis Points does not support configuration_update; send the desired reasoning effort on the request")
		case "function_call_output", "custom_tool_call_output":
			if item["type"] != kind {
				return capabilityError(fmt.Sprintf("tool output type must use its exact protocol spelling (path=input[%d])", index))
			}
			// The official BPS client returns tool screenshots as data URLs.
			// The attachment layer still validates their bytes and resource
			// limits, but must not convert them into user attachment IDs.
			if err := validateCapabilityContent(item["output"], fmt.Sprintf("input[%d].output", index), true); err != nil {
				return err
			}
		}
		if err := validateCapabilityContent(item["content"], fmt.Sprintf("input[%d].content", index), allowInline); err != nil {
			return err
		}
	}
	return nil
}

func capabilityError(message string) error {
	return fail(http.StatusBadRequest, "unsupported_capability", message)
}

func isHostedSearch(kind string) bool {
	return kind == "web_search" || strings.HasPrefix(kind, "web_search_")
}

func validateDeclaredCapabilities(value any) error {
	items, _ := value.([]any)
	for _, raw := range items {
		tool := objectValue(raw)
		kind := strings.ToLower(stringValue(tool["type"]))
		if kind == "namespace" {
			if err := validateDeclaredCapabilities(namespaceChildren(tool)); err != nil {
				return err
			}
			continue
		}
		if kind == "image_generation" {
			return capabilityError("Basis Points cannot run hosted image_generation; use a native Codex route or a declared client function/custom tool")
		}
		if isHostedSearch(kind) && (tool["external_web_access"] == true || stringValue(tool["search_context_size"]) == "high") {
			return capabilityError("Basis Points cannot satisfy this hosted web_search request; use a native Codex route or a declared client search tool")
		}
	}
	return nil
}

// RequestCapabilityInstructions makes omitted hosted declarations explicit.
// Function/custom tool names are not hosted capability names.
func RequestCapabilityInstructions(source map[string]any) string {
	if stringValue(source["tool_choice"]) == "none" {
		return ""
	}
	kinds := map[string]bool{}
	var collect func(any)
	collect = func(value any) {
		items, _ := value.([]any)
		for _, raw := range items {
			tool := objectValue(raw)
			kind := strings.ToLower(stringValue(tool["type"]))
			switch kind {
			case "namespace":
				collect(namespaceChildren(tool))
			case "function", "custom", "":
			default:
				kinds[kind] = true
			}
		}
	}
	collect(source["tools"])
	collect(additionalRequestTools(source))
	if len(kinds) == 0 {
		return ""
	}
	names := make([]string, 0, len(kinds))
	for kind := range kinds {
		names = append(names, kind)
	}
	sort.Strings(names)
	return "Hosted tools unavailable through Basis Points: " + strings.Join(names, ", ") + ". These declarations were omitted. Do not claim to have used them. Use a suitable declared client tool, or explain the limitation."
}

func validateCapabilityContent(value any, path string, allowInline bool) error {
	parts, _ := value.([]any)
	for index, raw := range parts {
		part := objectValue(raw)
		switch strings.ToLower(stringValue(part["type"])) {
		case "input_image":
			if part["type"] != "input_image" {
				return capabilityError(fmt.Sprintf("image type must be input_image (path=%s[%d])", path, index))
			}
			if err := validateCapabilityImage(part, allowInline); err != nil {
				return capabilityError(fmt.Sprintf("%s (path=%s[%d]; type=input_image)", err.Error(), path, index))
			}
		case "input_file", "input_audio", "input_video":
			// Only fixed protocol type names reach this branch. Never echo URLs,
			// file IDs, inline bytes, or arbitrary client-controlled type names.
			return capabilityError(fmt.Sprintf("Basis Points accepts text and input_image content only (path=%s[%d]; type=%s)", path, index, stringValue(part["type"])))
		}
	}
	return nil
}

func validateCapabilityImage(part map[string]any, allowInline bool) error {
	if fileID, exists := part["file_id"]; exists && fileID != nil && fileID != "" {
		value, ok := fileID.(string)
		if !ok || len(value) > 512 || strings.TrimSpace(value) != value || strings.IndexFunc(value, func(r rune) bool { return r <= ' ' || r == 127 || strings.ContainsRune("/?#", r) }) >= 0 || (part["image_url"] != nil && part["image_url"] != "") {
			return capabilityError("Basis Points input_image requires either a valid file_id or image_url, not both")
		}
		return validateImageDetail(part)
	}
	raw, ok := part["image_url"].(string)
	if !ok || raw == "" {
		return capabilityError("Basis Points input_image requires image_url or file_id")
	}
	// Only the scheme is case-insensitive. Lowercasing a multi-megabyte
	// base64 payload needlessly scans and allocates the entire image.
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) >= 5 && strings.EqualFold(trimmed[:5], "data:") {
		if raw != trimmed {
			return capabilityError("inline image data URL must not contain surrounding whitespace")
		}
		if !allowInline {
			return capabilityError("inline image was not uploaded as a native attachment")
		}
	} else {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" || strings.TrimSpace(raw) != raw {
			return capabilityError("Basis Points input_image requires an absolute HTTPS URL without embedded credentials")
		}
	}
	return validateImageDetail(part)
}

func validateImageDetail(part map[string]any) error {
	if detail, exists := part["detail"]; exists && detail != nil {
		switch stringValue(detail) {
		case "auto", "low", "high":
		default:
			return capabilityError("Basis Points image detail must be auto, low or high")
		}
	}
	return nil
}

const structuredSchemaURL = "https://basispoints.invalid/structured-output.json"

var structuredFormatName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type structuredOutput struct {
	format map[string]any
	schema *jsonschema.Schema
}

type localSchemaLoader struct{}

func (localSchemaLoader) Load(string) (any, error) {
	return nil, fmt.Errorf("external structured output schema references are not supported")
}

func structuredOutputForSource(source map[string]any) (*structuredOutput, error) {
	raw := source["text"]
	if raw == nil {
		return nil, nil
	}
	config, ok := raw.(map[string]any)
	if !ok {
		return nil, capabilityError("text must be an object")
	}
	if config["format"] == nil {
		return nil, nil
	}
	format, ok := config["format"].(map[string]any)
	if !ok {
		return nil, capabilityError("text.format must be an object")
	}
	kind := stringValue(format["type"])
	if kind == "text" {
		return nil, nil
	}
	if kind != "json_object" && kind != "json_schema" {
		return nil, capabilityError("text.format requires text, json_object or json_schema")
	}
	for key := range format {
		if key == "type" || (kind == "json_schema" && (key == "name" || key == "schema" || key == "strict" || key == "description")) {
			continue
		}
		return nil, capabilityError("text.format contains an unsupported field")
	}
	result := &structuredOutput{format: format}
	if kind == "json_object" {
		return result, nil
	}
	if !structuredFormatName.MatchString(stringValue(format["name"])) {
		return nil, capabilityError("json_schema requires a name of 1-64 letters, digits, underscores or hyphens")
	}
	if value := format["strict"]; value != nil {
		if _, ok := value.(bool); !ok {
			return nil, capabilityError("json_schema strict must be a boolean")
		}
	}
	if value := format["description"]; value != nil {
		if _, ok := value.(string); !ok {
			return nil, capabilityError("json_schema description must be a string")
		}
	}
	schema, ok := format["schema"].(map[string]any)
	if !ok || schema == nil {
		return nil, capabilityError("json_schema requires a schema object")
	}
	encoded, err := json.Marshal(schema)
	if err != nil || len(encoded) > 1<<20 {
		return nil, capabilityError("structured output schema exceeds 1 MiB")
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.UseLoader(localSchemaLoader{})
	if err := compiler.AddResource(structuredSchemaURL, schema); err != nil {
		return nil, capabilityError("structured output schema is invalid")
	}
	result.schema, err = compiler.Compile(structuredSchemaURL)
	if err != nil {
		return nil, capabilityError("structured output schema is invalid or references an external resource")
	}
	return result, nil
}

func prepareStructuredOutput(source map[string]any) (string, error) {
	structured, err := structuredOutputForSource(source)
	if err != nil || structured == nil {
		return "", err
	}
	prompt := "The client requires a structured final answer. Your final assistant answer must be exactly one JSON value, with no Markdown fences or surrounding prose. Tool calls and refusals remain separate protocol items; use the client tool transport as needed before the final answer. The gateway validates the final JSON before returning it to the client."
	if structured.schema != nil {
		prompt += " The final answer must satisfy the schema in this output format:\n" + string(JSONBytes(structured.format))
	}
	return prompt, nil
}

// HasStructuredOutput is only used after request preparation has validated the
// format. It must not compile schemas on every streaming event.
func HasStructuredOutput(source map[string]any) bool {
	kind := stringValue(objectValue(objectValue(source["text"])["format"])["type"])
	return kind == "json_object" || kind == "json_schema"
}

func ValidateStructuredResponse(response, source map[string]any) error {
	return validateStructuredResponse(response, source)
}

func validateStructuredResponse(response, source map[string]any) error {
	structured, err := structuredOutputForSource(source)
	if err != nil || structured == nil {
		return err
	}
	// Failures/refusals are protocol results, never fabricated JSON answers.
	if status := stringValue(response["status"]); status != "" && status != "completed" {
		return nil
	}
	var answer strings.Builder
	hasTool, hasRefusal := false, false
	output, _ := response["output"].([]any)
	for _, raw := range output {
		item := objectValue(raw)
		kind := stringValue(item["type"])
		hasTool = hasTool || kind == "function_call" || kind == "custom_tool_call"
		if kind != "message" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, rawPart := range content {
			part := objectValue(rawPart)
			switch stringValue(part["type"]) {
			case "output_text":
				value, ok := part["text"].(string)
				if !ok || answer.Len()+len(value) > 16<<20 {
					return fail(http.StatusBadGateway, "invalid_structured_output", "Basis Points structured output text is invalid or exceeds 16 MiB")
				}
				answer.WriteString(value)
			case "refusal":
				hasRefusal = true
			default:
				return fail(http.StatusBadGateway, "invalid_structured_output", "Basis Points structured output contains unsupported message content")
			}
		}
	}
	if hasTool || (hasRefusal && answer.Len() == 0) {
		return nil
	}
	var instance any
	decoder := json.NewDecoder(bytes.NewBufferString(answer.String()))
	decoder.UseNumber()
	if err := decoder.Decode(&instance); err != nil {
		return fail(http.StatusBadGateway, "invalid_structured_output", "Basis Points structured output is not one valid JSON value")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fail(http.StatusBadGateway, "invalid_structured_output", "Basis Points structured output is not one valid JSON value")
	}
	if structured.schema != nil && structured.schema.Validate(instance) != nil {
		return fail(http.StatusBadGateway, "invalid_structured_output", "Basis Points structured output does not satisfy the requested JSON schema")
	}
	return nil
}
