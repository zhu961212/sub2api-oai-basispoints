package protocol

import "testing"

func TestImagePreparationFreezesIdentityWithoutMutatingOriginalInput(t *testing.T) {
	image := map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA", "detail": "high"}
	source := map[string]any{"model": DefaultModelID, "input": []any{map[string]any{"role": "user", "content": []any{image}}}}
	before := string(JSONBytes(source["input"]))
	if _, err := PrepareResponsesBody(source, DefaultConfig()); err == nil {
		t.Fatal("normal preparation accepted unuploaded image")
	}
	prepared, err := PrepareResponsesBodyForImageUpload(source, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	for _, raw := range prepared["input"].([]any) {
		item := raw.(map[string]any)
		if item["role"] != "user" {
			continue
		}
		part := item["content"].([]any)[0].(map[string]any)
		delete(part, "image_url")
		part["file_id"] = "file-native-upload"
		changed = true
	}
	if !changed || string(JSONBytes(source["input"])) != before {
		t.Fatal("upload rewrite mutated original history")
	}
	if err := ValidateRequestCapabilities(prepared); err != nil {
		t.Fatal(err)
	}
}
