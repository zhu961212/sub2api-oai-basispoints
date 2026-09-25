package protocol

import (
	"strings"
	"testing"
)

func TestInlineImageSchemeValidationPreservesPayload(t *testing.T) {
	for _, scheme := range []string{"data:", "DATA:", "DaTa:", " data:"} {
		raw := scheme + "image/png;base64,AbCdEF+/=="
		part := map[string]any{"type": "input_image", "image_url": raw}
		if err := validateCapabilityImage(part, true); err != nil {
			t.Fatalf("inline scheme rejected: %v", err)
		}
		if err := validateCapabilityImage(part, false); err == nil {
			t.Fatal("inline scheme accepted with relay disabled")
		}
		if part["image_url"] != raw {
			t.Fatal("image payload changed during validation")
		}
	}
}

func BenchmarkInlineImageCapabilityValidation(b *testing.B) {
	raw := "data:image/png;base64," + strings.Repeat("AbCdEF+/", 512*1024)
	part := map[string]any{"type": "input_image", "image_url": raw}
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := validateCapabilityImage(part, true); err != nil {
			b.Fatal(err)
		}
	}
}
