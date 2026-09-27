package protocol

import (
	"strings"
	"testing"
)

// Local PDF and Word work uses the client executor. These fixtures preserve
// multiline scripts, Unicode, and Windows paths through both relay JSON layers.
func TestDocumentCustomToolRoundTripPreservesScript(t *testing.T) {
	for _, test := range []struct{ name, script string }{
		{"read_pdf", `from pathlib import Path
import pdfplumber
source = Path(r"E:\文档\客户 O'Brien\合同.pdf")
with pdfplumber.open(source) as document:
    for page_number, page in enumerate(document.pages, 1):
        print(f"--- 第 {page_number} 页 ---")
        print(page.extract_text() or "")
`},
		{"read_write_word", `from pathlib import Path
from docx import Document
source = Path(r"E:\文档\客户 O'Brien\合同.docx")
document = Document(source)
print("\n".join(paragraph.text for paragraph in document.paragraphs))
document.add_paragraph('修订说明：保留 "引号"、反斜杠 \\ 和换行\n下一行。')
document.save(source.with_name("合同（修订）.docx"))
`},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := "@'\r\n" + strings.ReplaceAll(test.script, "\n", "\r\n") + "'@ | python -"
			input := "const result = await tools.exec_command(" + string(JSONBytes(map[string]any{
				"cmd": command, "workdir": `E:\文档\客户 O'Brien`,
			})) + ");\ntext(result.output);\n"
			source := map[string]any{
				"session_id": t.Name(),
				"tools":      []any{map[string]any{"type": "custom", "name": "functions.exec"}},
				"input":      []any{messageItem("user", "读取 PDF 并更新 Word 文档")},
			}
			if _, err := PrepareResponsesBody(source, DefaultConfig()); err != nil {
				t.Fatal(err)
			}
			native := relayCompatNative(map[string]any{"tool": "functions.exec", "args": input})
			response := map[string]any{"id": "resp_document", "status": "completed", "output": []any{native}}
			_, transformed, changed, err := TransformResponseBody(JSONBytes(response), source)
			if err != nil || !changed {
				t.Fatalf("document script translation failed: changed=%t err=%v", changed, err)
			}
			call := objectValue(transformed["output"].([]any)[0])
			if call["type"] != "custom_tool_call" || call["name"] != "functions.exec" || call["input"] != input {
				t.Fatal("document script changed across the relay JSON layers")
			}
			streamed, err := ParseFinalStreamResponse(SyntheticStream(transformed))
			if err != nil || !jsonValuesEqual(streamed, transformed) {
				t.Fatalf("document script changed in SSE delivery: %v", err)
			}
			replayed := translateInputItemsInNamespace([]any{call}, clientToolSpecs(source), nativeCallNamespace(source))
			if len(replayed) != 1 || !jsonValuesEqual(replayed[0], native) {
				t.Fatal("document script or call identity changed on replay")
			}
		})
	}
}

func TestDocumentFunctionToolRoundTripPreservesPathsAndResults(t *testing.T) {
	source := map[string]any{
		"session_id": t.Name(),
		"tools": []any{map[string]any{
			"type": "function", "name": "exec_command",
			"parameters": map[string]any{
				"type": "object", "required": []any{"cmd", "workdir"},
				"properties": map[string]any{
					"cmd": map[string]any{"type": "string"}, "workdir": map[string]any{"type": "string"},
				},
				"additionalProperties": false,
			},
		}},
	}
	want := map[string]any{
		"cmd":     `python inspect_document.py --input "E:\文档\客户 O'Brien\合同.docx" --output "E:\文档\修订版本\合同.docx"`,
		"workdir": `E:\文档\客户 O'Brien`,
	}
	native := relayCompatNative(map[string]any{"tool": "exec_command", "args": want})
	call, ok := extractNativeClientToolCallFromItem(native, source, true)
	if !ok || !jsonValuesEqual(parseArguments(call["arguments"]), want) {
		t.Fatal("document function arguments lost paths or quotes")
	}
	// MCP resource links are serialized as tool text by the client.
	resultText := string(JSONBytes(map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "共读取 12 段。\n已保存修订版本，保留表格与中文标点。"},
		map[string]any{"type": "resource_link", "name": "合同（修订）.docx",
			"uri":      "file:///E:/documents/revised-contract.docx",
			"mimeType": "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
	}, "structuredContent": map[string]any{"path": `E:\文档\修订版本\合同.docx`}}))
	for _, result := range []any{resultText, []any{map[string]any{"type": "input_text", "text": resultText}}} {
		source["input"] = []any{call, map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": result}}
		body, err := PrepareResponsesBody(source, DefaultConfig())
		if err != nil {
			t.Fatalf("document tool continuation rejected: %v", err)
		}
		var gotOutput any
		for _, raw := range body["input"].([]any) {
			item := objectValue(raw)
			if item["type"] == "function_call_output" && item["call_id"] == call["call_id"] {
				gotOutput = item["output"]
			}
		}
		if !jsonValuesEqual(gotOutput, result) {
			t.Fatal("document continuation lost text, path, or resource link")
		}
	}
}

func TestDocumentNestedHelperCannotBecomeUndeclaredRelayTarget(t *testing.T) {
	source := map[string]any{"tools": []any{map[string]any{
		"type": "custom", "name": "functions.exec",
		"description": "Run JavaScript. Use tools.exec_command to read PDFs or generate Word documents with python-docx.",
	}}}
	for _, name := range []string{"exec_command", "functions.exec_command", "tools.exec_command"} {
		native := relayCompatNative(map[string]any{"tool": name, "args": map[string]any{"cmd": "python update_document.py"}})
		call, reason := decodeNativeClientToolCallFromItem(native, source, true)
		if call != nil || reason != unknownClientToolMessage {
			t.Fatalf("undeclared document helper became callable: %s", name)
		}
	}
}
