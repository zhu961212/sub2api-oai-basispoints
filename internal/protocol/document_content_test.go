package protocol

import (
	"strings"
	"testing"
)

func documentContentSource(part map[string]any, kind string) map[string]any {
	item := map[string]any{"role": "user", "content": []any{part}}
	if kind != "message" {
		item = map[string]any{"type": kind, "call_id": "call_document", "output": []any{part}}
	}
	return map[string]any{"model": DefaultModelID, "input": []any{item}}
}

func TestDocumentContentPreparationAndNativeReferences(t *testing.T) {
	for _, kind := range []string{"message", "function_call_output", "custom_tool_call_output"} {
		t.Run(kind, func(t *testing.T) {
			part := map[string]any{"type": "input_file", "file_data": "data:application/pdf;base64,JVBERi0xLjc=", "filename": "报告.pdf"}
			source := documentContentSource(part, kind)
			before := string(JSONBytes(source["input"]))
			if err := ValidateImageUploadCapabilities(source); err != nil {
				t.Fatal(err)
			}
			if err := ValidateRequestCapabilities(source); err == nil {
				t.Fatal("inline document bypassed authenticated upload")
			}
			prepared, err := PrepareResponsesBodyForAttachmentUpload(source, DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, raw := range prepared["input"].([]any) {
				item := objectValue(raw)
				for _, field := range []string{"content", "output"} {
					parts, _ := item[field].([]any)
					for _, rawPart := range parts {
						file := objectValue(rawPart)
						if file["type"] == "input_file" {
							delete(file, "file_data")
							file["file_id"] = "file-native_document"
							found = true
						}
					}
				}
			}
			if !found || string(JSONBytes(source["input"])) != before {
				t.Fatal("file preparation lost document or mutated original history")
			}
			if err := ValidateRequestCapabilities(prepared); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDocumentContentRejectsInvalidSourcesWithoutEchoingData(t *testing.T) {
	for name, part := range map[string]map[string]any{
		"missing":          {},
		"two sources":      {"file_data": "PRIVATE", "file_id": "file-PRIVATE"},
		"numeric id":       {"file_id": 42},
		"numeric data":     {"file_data": 42},
		"bad id":           {"file_id": "file-PRIVATE/path"},
		"short id":         {"file_id": "file-"},
		"unicode id":       {"file_id": "file-PRIVATE文件"},
		"remote URL":       {"file_url": "https://private.example/PRIVATE.pdf"},
		"local URL":        {"file_url": "file:///PRIVATE.pdf"},
		"path filename":    {"file_data": "PRIVATE", "filename": "../PRIVATE.pdf"},
		"windows filename": {"file_data": "PRIVATE", "filename": "C:" + string(rune(92)) + "PRIVATE.pdf"},
		"control filename": {"file_data": "PRIVATE", "filename": "PRIVATE" + string(rune(0)) + ".pdf"},
		"long filename":    {"file_data": "PRIVATE", "filename": strings.Repeat("a", 256)},
	} {
		part["type"] = "input_file"
		for _, kind := range []string{"message", "function_call_output", "custom_tool_call_output"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				err := ValidateImageUploadCapabilities(documentContentSource(part, kind))
				if err == nil || !strings.Contains(err.Error(), "input[0].") || strings.Contains(err.Error(), "PRIVATE") {
					t.Fatalf("invalid source was accepted or echoed: %v", err)
				}
			})
		}
	}
}

func TestDocumentContentDoesNotInterpretToolArguments(t *testing.T) {
	for _, kind := range []string{"function_call", "custom_tool_call"} {
		source := map[string]any{"input": []any{map[string]any{"type": kind, "arguments": string(JSONBytes(map[string]any{"type": "input_file", "file_url": "file:///local.pdf"})), "input": "read local Word document"}}}
		if err := ValidateRequestCapabilities(source); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDocumentFilenamesUseTheSameUnicodeSafetyRulesForNativeAndInline(t *testing.T) {
	for _, name := range []string{"a\u0085b.pdf", "a\u009fb.docx", "a\xffb.pdf"} {
		for _, sourceField := range []string{"file_id", "file_data"} {
			part := map[string]any{"type": "input_file", "filename": name, sourceField: "file-valid"}
			if err := ValidateImageUploadCapabilities(documentContentSource(part, "message")); err == nil {
				t.Errorf("unsafe Unicode filename accepted for %s", sourceField)
			}
		}
	}
	for _, name := range []string{"客户 O'Brien合同.pdf", "季度\u00a0报告.pdf", "季度\u200b报告.docx", "季度\"报告.pdf", "季度📄.pdf"} {
		part := map[string]any{"type": "input_file", "filename": name, "file_id": "file-valid"}
		if err := ValidateRequestCapabilities(documentContentSource(part, "message")); err != nil {
			t.Errorf("safe Unicode filename rejected: %v", err)
		}
	}
}
