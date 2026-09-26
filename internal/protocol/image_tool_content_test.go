package protocol

import (
	"strings"
	"testing"
)

func TestCapabilityInlineImagesUseNativeToolOutputShape(t *testing.T) {
	for _, kind := range []string{"function_call_output", "custom_tool_call_output"} {
		t.Run(kind, func(t *testing.T) {
			image := map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA", "detail": "high"}
			source := map[string]any{"input": []any{map[string]any{"type": kind, "call_id": "call_screenshot", "output": []any{
				map[string]any{"type": "input_text", "text": "tool screenshot"}, image,
			}}}}
			before := string(JSONBytes(source))
			if err := ValidateRequestCapabilities(source); err != nil {
				t.Fatal(err)
			}
			if string(JSONBytes(source)) != before {
				t.Fatal("validation changed tool screenshot content")
			}
			for _, raw := range []string{" data:image/png;base64,AAAA", "data:image/png;base64,AAAA "} {
				image["image_url"] = raw
				if err := ValidateRequestCapabilities(source); err == nil || !strings.Contains(err.Error(), "input[0].output[1]") {
					t.Fatalf("whitespace bypassed tool image validation: %v", err)
				}
			}
			image["image_url"] = "data:image/png;base64,AAAA"
			image["detail"] = "original"
			if err := ValidateRequestCapabilities(source); err == nil || !strings.Contains(err.Error(), "input[0].output[1]") {
				t.Fatalf("unsupported detail did not identify the tool image: %v", err)
			}
		})
	}
	message := map[string]any{"input": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
	}}}}
	if err := ValidateRequestCapabilities(message); err == nil {
		t.Fatal("inline user image bypassed native attachment processing")
	}
	if err := ValidateImageUploadCapabilities(message); err != nil {
		t.Fatalf("user image rejected before attachment processing: %v", err)
	}
}
