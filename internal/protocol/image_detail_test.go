package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestNormalizeOriginalImageDetailPreservesSourceHistory(t *testing.T) {
	for _, kind := range []string{"", "message", "function_call_output", "custom_tool_call_output"} {
		for _, reference := range []string{"inline", "https", "file_id"} {
			t.Run(kind+"/"+reference, func(t *testing.T) {
				image := map[string]any{"type": "input_image", "detail": "original", "id": "image-id", "large_integer": json.Number("9007199254740993")}
				switch reference {
				case "inline":
					image["image_url"] = "data:image/png;base64,AbCd+/=="
				case "https":
					image["image_url"] = "https://images.example/Original.PNG?key=Exact%2fBytes"
				case "file_id":
					image["file_id"] = "file-original-ref"
				}
				parts := []any{map[string]any{"type": "input_text", "text": "before original"}, image, map[string]any{"type": "input_image", "image_url": "https://images.example/low.png", "detail": "low"}}
				field := "content"
				if kind == "function_call_output" || kind == "custom_tool_call_output" {
					field = "output"
				}
				item := map[string]any{"type": kind, field: parts, "id": "message-id", "call_id": "call-original"}
				items := []any{item}
				source := map[string]any{"input": items}
				history := map[string]any{"input": items}
				before := JSONBytes(source)
				if !HasOriginalImageDetail(source) || !NormalizeImageDetails(source) {
					t.Fatal("typed original image was not normalized")
				}
				if !bytes.Equal(JSONBytes(history), before) || image["detail"] != "original" {
					t.Fatal("normalization mutated source history or image")
				}
				updated := objectValue(source["input"].([]any)[0])[field].([]any)
				got := objectValue(updated[1])
				want := copyImageDetailObject(image)
				want["detail"] = "high"
				if !bytes.Equal(JSONBytes(got), JSONBytes(want)) || !bytes.Equal(JSONBytes(updated[0]), JSONBytes(parts[0])) || !bytes.Equal(JSONBytes(updated[2]), JSONBytes(parts[2])) {
					t.Fatal("image bytes, IDs, precise numbers, or neighboring content changed")
				}
				if HasOriginalImageDetail(source) || NormalizeImageDetails(source) {
					t.Fatal("normalization is not idempotent")
				}
				got["detail"] = "low"
				if image["detail"] != "original" {
					t.Fatal("normalized image remains aliased to original history")
				}
			})
		}
	}
}

func TestOriginalImageDetailIgnoresUntypedContent(t *testing.T) {
	image := map[string]any{"type": "input_image", "detail": "original", "image_url": "https://images.example/a.png"}
	for _, source := range []map[string]any{
		nil, {}, {"input": "original"},
		{"input": []any{nil, image}},
		{"input": []any{map[string]any{"type": "function_call", "arguments": string(JSONBytes(image)), "content": []any{image}}}},
		{"input": []any{map[string]any{"type": "custom_tool_call", "input": image, "output": []any{image}}}},
		{"input": []any{map[string]any{"type": "message", "output": []any{image}, "content": []any{map[string]any{"type": "input_text", "text": string(JSONBytes(image)), "image": image}}}}},
		{"input": []any{map[string]any{"type": "function_call_output", "content": []any{image}, "output": string(JSONBytes(image))}}},
		{"input": []any{map[string]any{"type": "FUNCTION_CALL_OUTPUT", "output": []any{image}}}},
		{"input": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "INPUT_IMAGE", "detail": "original", "image_url": "https://images.example/a.png"}}}}},
	} {
		before := JSONBytes(source)
		if HasOriginalImageDetail(source) || NormalizeImageDetails(source) || !bytes.Equal(JSONBytes(source), before) {
			t.Fatalf("untyped content changed: %s", before)
		}
	}
}

func TestOriginalImageDetailValidationStages(t *testing.T) {
	for _, kind := range []string{"message", "function_call_output", "custom_tool_call_output"} {
		for _, reference := range []string{"https", "file_id"} {
			t.Run(fmt.Sprintf("%s/%s", kind, reference), func(t *testing.T) {
				image := map[string]any{"type": "input_image", "detail": "original"}
				if reference == "file_id" {
					image["file_id"] = "file-existing-image"
				} else {
					image["image_url"] = "https://images.example/image.png"
				}
				field := "content"
				if kind != "message" {
					field = "output"
				}
				source := map[string]any{"input": []any{map[string]any{"type": kind, field: []any{image}}}}
				if err := ValidateImageUploadCapabilities(source); err != nil {
					t.Fatalf("original detail rejected before normalization: %v", err)
				}
				if err := ValidateRequestCapabilities(source); err == nil {
					t.Fatal("unnormalized original detail reached final wire validation")
				}
				NormalizeImageDetails(source)
				if err := ValidateRequestCapabilities(source); err != nil {
					t.Fatalf("normalized detail rejected: %v", err)
				}
			})
		}
	}
}
