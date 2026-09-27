package attachments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestMixedPreflightValidatesDocumentsWithoutMutatingImages(t *testing.T) {
	for _, kind := range []string{"message", "function_call_output", "custom_tool_call_output"} {
		t.Run(kind, func(t *testing.T) {
			img := imagePart(dataURL(testPNG(t), "image/png"))
			delete(img, "detail")
			bad := filePart(dataURL([]byte("PRIVATE not a PDF"), pdfMIME), "report.pdf")
			source := message(img)
			wantPath := "path=input[1].output[0]"
			if kind == "message" {
				source = message(img, bad)
				wantPath = "path=input[0].content[1]"
			} else {
				source["input"] = append(source["input"].([]any), toolImageInput(kind, bad))
			}
			before, _ := json.Marshal(source)
			err := ValidateMixedInputs(context.Background(), source)
			var typed *Error
			if !errors.As(err, &typed) || typed.Code() != "invalid_file" || !strings.Contains(err.Error(), wantPath) || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("unexpected preflight error: %v", err)
			}
			after, _ := json.Marshal(source)
			if !bytes.Equal(before, after) || img["detail"] != nil {
				t.Fatal("preflight changed the original request")
			}
		})
	}
}

func TestMixedPreflightSharesDecodedByteBudget(t *testing.T) {
	imageBytes := make([]byte, 17<<20)
	copy(imageBytes, testPNG(t))
	fileBytes := make([]byte, 16<<20)
	copy(fileBytes, []byte("%PDF-1.7"))
	img := imagePart(dataURL(imageBytes, "image/png"))
	file := filePart(dataURL(fileBytes, pdfMIME), "report.pdf")
	source := message(img, file)
	err := ValidateMixedInputs(context.Background(), source)
	var typed *Error
	if !errors.As(err, &typed) || typed.Code() != "invalid_attachment" || !strings.Contains(err.Error(), "shared 32 MiB") || !strings.Contains(err.Error(), "path=input[0].content[1]") {
		t.Fatalf("combined byte budget not enforced: %v", err)
	}
	if img["image_url"] == nil || file["file_data"] == nil || img["file_id"] != nil || file["file_id"] != nil {
		t.Fatal("preflight rewrote attachments")
	}
}

func TestMixedPreflightKeepsIndependentOccurrenceLimits(t *testing.T) {
	imageRaw := dataURL(testPNG(t), "image/png")
	fileRaw := dataURL([]byte("%PDF-1.7\n%%EOF"), pdfMIME)
	parts := make([]map[string]any, 0, maxRequestImages+maxRequestFiles)
	for i := 0; i < maxRequestImages; i++ {
		parts = append(parts, imagePart(imageRaw))
	}
	for i := 0; i < maxRequestFiles; i++ {
		parts = append(parts, filePart(fileRaw, "report.pdf"))
	}
	if err := ValidateMixedInputs(context.Background(), message(parts...)); err != nil {
		t.Fatalf("valid independent counts rejected: %v", err)
	}
	for _, tc := range []struct {
		part map[string]any
		want string
	}{{imagePart(imageRaw), "128 inline images"}, {filePart(fileRaw, "report.pdf"), "20 inline files"}} {
		err := ValidateMixedInputs(context.Background(), message(append(parts, tc.part)...))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("occurrence limit missing: %v", err)
		}
	}
}
