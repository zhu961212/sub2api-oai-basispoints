package attachments

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func filePart(data, filename string) map[string]any {
	part := map[string]any{"type": "input_file", "file_data": data}
	if filename != "" {
		part["filename"] = filename
	}
	return part
}

func testDOCX(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, item := range []struct{ name, content string }{
		{"[Content_Types].xml", "<Types xmlns=\"http://schemas.openxmlformats.org/package/2006/content-types\"/>"},
		{"word/document.xml", "<w:document xmlns:w=\"http://schemas.openxmlformats.org/wordprocessingml/2006/main\"><w:body/></w:document>"},
	} {
		part, err := writer.Create(item.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, item.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func documentFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestFilesPreserveMultipartBytesAndTypedOutputs(t *testing.T) {
	cases := []struct {
		name, mime, filename string
		data                 []byte
		raw                  bool
	}{
		{"pdf", pdfMIME, "attachment.pdf", []byte("%PDF-1.7\n1 0 obj <<>> endobj\n%%EOF\n"), false},
		{"raw_pdf", pdfMIME, "report.PDF", []byte("%PDF-1.7\n%%EOF\n"), true},
		{"nbsp_filename", pdfMIME, "季度\u00a0报告.pdf", []byte("%PDF-1.7\n%%EOF\n"), false},
		{"zero_width_filename", pdfMIME, "季度\u200b报告.pdf", []byte("%PDF-1.7\n%%EOF\n"), false},
		{"quoted_filename", pdfMIME, "季度\"报告.pdf", []byte("%PDF-1.7\n%%EOF\n"), false},
		{"real_pdf", pdfMIME, "真实合同📄.pdf", documentFixture(t, "sample.pdf"), false},
		{"real_docx", docxMIME, "客户 O'Brien\u00a0合同.docx", documentFixture(t, "sample.docx"), true},
		{"docx", docxMIME, "季度报告.docx", testDOCX(t), false},
		{"doc", docMIME, "report.doc", append([]byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}, make([]byte, 32)...), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var uploads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				uploads.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/basispoints/api/attachments" || r.Header.Get("Authorization") != testHeaders().Get("Authorization") {
					t.Error("wrong native upload route or identity")
				}
				if r.ContentLength <= int64(len(tc.data)) || len(r.TransferEncoding) != 0 {
					t.Error("missing multipart content length")
				}
				reader, err := r.MultipartReader()
				if err != nil {
					t.Error(err)
					return
				}
				part, err := reader.NextPart()
				if err != nil {
					t.Error(err)
					return
				}
				got, err := io.ReadAll(part)
				if err != nil || !bytes.Equal(got, tc.data) || part.FormName() != "file" || part.FileName() != tc.filename || part.Header.Get("Content-Type") != tc.mime {
					t.Error("file multipart metadata or bytes changed")
				}
				if _, err := reader.NextPart(); err != io.EOF {
					t.Error("unexpected extra multipart fields")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": "file-document"})
			}))
			defer server.Close()
			data, name := dataURL(tc.data, tc.mime), tc.filename
			if tc.raw {
				data = base64.StdEncoding.EncodeToString(tc.data)
			}
			if tc.name == "pdf" {
				name = ""
			}
			u := New()
			for attempt := 0; attempt < 2; attempt++ {
				parts := []map[string]any{filePart(data, name), filePart(data, name), filePart(data, name)}
				source := message(parts[0])
				source["input"] = append(source["input"].([]any), toolImageInput("function_call_output", parts[1]), toolImageInput("custom_tool_call_output", parts[2]))
				changed, err := u.RewriteFiles(context.Background(), server.Client(), server.URL+"/basispoints/api/responses", testHeaders(), source, "scope")
				if !changed || err != nil || uploads.Load() != 1 {
					t.Fatalf("changed=%v uploads=%d err=%v", changed, uploads.Load(), err)
				}
				for _, part := range parts {
					if part["file_id"] != "file-document" || part["file_data"] != nil || part["filename"] != nil {
						t.Fatal("typed file was not converted to native ID")
					}
				}
			}
		})
	}
}

func TestFilesRejectInvalidBatchBeforeUpload(t *testing.T) {
	good := dataURL([]byte("%PDF-1.7\n%%EOF\n"), pdfMIME)
	cases := map[string]map[string]any{
		"bad_base64":       filePart("data:application/pdf;base64,PRIVATE%%%%", "secret.pdf"),
		"interior_padding": filePart("data:application/pdf;base64,AAAA====AAAA", "secret.pdf"),
		"newlines":         filePart(good+"\n", "secret.pdf"),
		"wrong_magic":      filePart(dataURL([]byte("PRIVATE not a pdf"), pdfMIME), "secret.pdf"),
		"wrong_mime":       filePart(good, "secret.docx"),
		"unknown_mime":     filePart(dataURL([]byte("PRIVATE"), "application/zip"), "secret.docx"),
		"raw_without_name": filePart(base64.StdEncoding.EncodeToString([]byte("%PDF-1.7")), ""),
		"url":              {"type": "input_file", "file_url": "http://127.0.0.1/PRIVATE"},
		"both_sources":     {"type": "input_file", "file_id": "file-existing", "file_data": good},
		"path":             filePart(good, "../secret.pdf"),
		"windows_path":     filePart(good, "C:\\secret.pdf"),
		"control":          filePart(good, "secret\r\n.pdf"),
		"invalid_utf8":     filePart(good, "secret\xff.pdf"),
		"long_filename":    filePart(good, strings.Repeat("名", 85)+".pdf"),
		"missing":          {"type": "input_file"},
	}
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { uploads.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			source := message(filePart(good, "first.pdf"), bad)
			before, _ := json.Marshal(source)
			changed, err := New().RewriteFiles(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), source, "scope")
			var typed *Error
			if changed || !errors.As(err, &typed) || typed.StatusCode() != 400 || !strings.Contains(err.Error(), "path=input[0].content[1]") || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			after, _ := json.Marshal(source)
			if uploads.Load() != 0 || !bytes.Equal(before, after) {
				t.Fatal("invalid batch uploaded or mutated source")
			}
		})
	}
}

func TestFileCacheSeparatesFilenameAndIdentity(t *testing.T) {
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := uploads.Add(1)
		fmt.Fprintf(w, "{\"openai_file_id\":\"file-%d\"}", n)
	}))
	defer server.Close()
	u := New()
	raw := dataURL([]byte("%PDF-1.7\n%%EOF"), pdfMIME)
	for _, tc := range []struct {
		name, scope, token string
		want               int32
	}{
		{"a.pdf", "scope", "token-a", 1}, {"a.pdf", "scope", "token-a", 1}, {"b.pdf", "scope", "token-a", 2},
		{"a.pdf", "scope", "token-b", 3}, {"a.pdf", "other", "token-b", 4}, {"a.pdf", "", "token-b", 5}, {"a.pdf", "", "token-b", 6},
	} {
		headers := testHeaders()
		headers.Set("Authorization", tc.token)
		_, err := u.RewriteFiles(context.Background(), server.Client(), server.URL+"/responses", headers, message(filePart(raw, tc.name)), tc.scope)
		if err != nil || uploads.Load() != tc.want {
			t.Fatalf("name=%s want=%d got=%d err=%v", tc.name, tc.want, uploads.Load(), err)
		}
	}
}

func TestFilesEnforceResourceLimitsBeforeUploads(t *testing.T) {
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { uploads.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	for _, test := range []struct {
		name         string
		bytes, count int
		want         string
	}{
		{"file_size", maxFileBytes + 1, 1, "20 MiB"}, {"total_size", 17 << 20, 2, "32 MiB"}, {"count", 16, 21, "20 inline files"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := make([]byte, test.bytes)
			copy(data, []byte("%PDF-1.7"))
			raw := dataURL(data, pdfMIME)
			parts := make([]map[string]any, test.count)
			for i := range parts {
				parts[i] = filePart(raw, "document.pdf")
			}
			changed, err := New().RewriteFiles(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), message(parts...), "scope")
			if changed || err == nil || !strings.Contains(err.Error(), test.want) || uploads.Load() != 0 {
				t.Fatalf("changed=%v uploads=%d err=%v", changed, uploads.Load(), err)
			}
		})
	}
}

func TestFilesFailAtomicallyAndRejectRedirects(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusTemporaryRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var uploads, redirects atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirects.Add(1) }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if uploads.Add(1) == 1 {
					io.WriteString(w, "{\"openai_file_id\":\"file-first\"}")
					return
				}
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				io.WriteString(w, "PRIVATE upstream body")
			}))
			defer server.Close()
			raw := dataURL([]byte("%PDF-1.7\n%%EOF"), pdfMIME)
			source := message(filePart(raw, "first.pdf"), filePart(raw, "second.pdf"))
			before, _ := json.Marshal(source)
			changed, err := New().RewriteFiles(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), source, "scope")
			after, _ := json.Marshal(source)
			if changed || err == nil || strings.Contains(err.Error(), "PRIVATE") || redirects.Load() != 0 || !bytes.Equal(before, after) {
				t.Fatalf("changed=%v redirects=%d err=%v", changed, redirects.Load(), err)
			}
		})
	}
}

func TestFilesPreserveNativeIDsAndIgnoreOrdinaryStrings(t *testing.T) {
	native := map[string]any{"type": "input_file", "file_id": "file-native"}
	source := message(native, map[string]any{"type": "input_text", "text": "data:application/pdf;base64,PRIVATE"})
	source["input"] = append(source["input"].([]any), map[string]any{"type": "function_call", "arguments": "PRIVATE file_data"})
	before, _ := json.Marshal(source)
	changed, err := (*Uploader)(nil).RewriteFiles(context.Background(), nil, "INVALID", nil, source, "")
	after, _ := json.Marshal(source)
	if changed || err != nil || !bytes.Equal(before, after) {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inline := filePart(dataURL([]byte("%PDF-1.7"), pdfMIME), "test.pdf")
	changed, err = New().RewriteFiles(ctx, http.DefaultClient, "http://localhost/responses", nil, message(inline), "")
	if changed || !errors.Is(err, context.Canceled) || inline["file_data"] == nil {
		t.Fatalf("cancellation changed=%v err=%v", changed, err)
	}
}
