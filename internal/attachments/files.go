package attachments

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxFileBytes    = 20 << 20
	maxRequestFiles = 20
	pdfMIME         = "application/pdf"
	docMIME         = "application/msword"
	docxMIME        = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

type inlineFile struct {
	mime     string
	filename string
	payload  string
	size     int64
	key      [32]byte
}

type fileEdit struct {
	part map[string]any
	file *inlineFile
}

// RewriteFiles converts typed inline PDF/Word files into native file IDs in
// messages and tool results. It never downloads remote URLs or interprets text
// and tool arguments as files. Every file validates before the first upload;
// source maps only change after all uploads succeed. File bytes are streamed
// from the request and never retained in the persistent metadata cache.
func (u *Uploader) RewriteFiles(ctx context.Context, client *http.Client, responsesURL string, headers http.Header, source map[string]any, scope string) (bool, error) {
	var edits []fileEdit
	var order []*inlineFile
	unique := make(map[[32]byte]*inlineFile)
	type encodedFile struct{ data, filename string }
	encoded := make(map[encodedFile]*inlineFile)
	var total int64
	endpoint := ""
	items, _ := source["input"].([]any)
	for i, rawItem := range items {
		item, _ := rawItem.(map[string]any)
		field := ""
		switch item["type"] {
		case nil, "", "message":
			field = "content"
		case "function_call_output", "custom_tool_call_output":
			field = "output"
		default:
			continue
		}
		parts, _ := item[field].([]any)
		for j, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if part["type"] != "input_file" {
				continue
			}
			fieldPath := fmt.Sprintf("input[%d].%s[%d]", i, field, j)
			invalid := func(message string) error { return fail(400, "invalid_file", message+" (path="+fieldPath+")") }
			if value, exists := part["file_url"]; exists && value != nil && value != "" {
				return false, invalid("Remote file_url attachments are not supported; provide file_data or a native file_id")
			}
			if value, exists := part["file_id"]; exists && value != nil && value != "" {
				if data, exists := part["file_data"]; exists && data != nil && data != "" {
					return false, invalid("input_file cannot contain both file_data and file_id")
				}
				continue
			}
			raw, ok := part["file_data"].(string)
			if !ok || raw == "" {
				return false, invalid("input_file requires nonempty file_data or a native file_id")
			}
			filename := ""
			if value := part["filename"]; value != nil {
				var ok bool
				filename, ok = value.(string)
				if !ok {
					return false, invalid("input_file filename must be a string")
				}
			}
			if len(edits) >= maxRequestFiles {
				return false, invalid("At most 20 inline files are allowed per request; conversation history, tool outputs, and repeated files count toward this limit")
			}
			if err := ctx.Err(); err != nil {
				return false, canceled(err)
			}
			if u == nil || client == nil {
				return false, fail(503, "attachment_unavailable", "Basis Points attachment transport is unavailable")
			}
			if endpoint == "" {
				var err error
				endpoint, err = attachmentURL(responsesURL)
				if err != nil {
					return false, err
				}
			}
			encodedKey := encodedFile{raw, filename}
			file := encoded[encodedKey]
			if file == nil {
				var err error
				file, err = parseFileMetadata(raw, filename)
				if err != nil {
					return false, invalid(err.Error())
				}
				// Domain separation and the filename prevent document/image cache
				// aliases and preserve filename-dependent attachment semantics.
				file.key, err = u.imageKey(ctx, endpoint, scope, headers, &inlineImage{mime: "document\x00" + file.mime + "\x00" + file.filename, payload: file.payload})
				if err != nil {
					return false, err
				}
				if existing := unique[file.key]; existing != nil {
					file = existing
				} else {
					if scope == "" || u.cached(file.key) == "" {
						if err := validateFile(ctx, file); err != nil {
							if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
								return false, canceled(err)
							}
							return false, invalid(err.Error())
						}
					}
					unique[file.key] = file
					order = append(order, file)
				}
				encoded[encodedKey] = file
			}
			total += file.size
			if total > maxRequestBytes {
				return false, invalid("Inline files exceed the 32 MiB request limit; conversation history, tool outputs, and repeated files count toward this limit")
			}
			edits = append(edits, fileEdit{part, file})
		}
	}
	if len(edits) == 0 {
		return false, nil
	}
	ids := make(map[[32]byte]string, len(order))
	for _, file := range order {
		id, err := u.getOrUpload(ctx, file.key, scope != "", func() (string, error) {
			return u.uploadAttachment(ctx, client, endpoint, headers, file.filename, file.mime, file.payload, file.size)
		})
		if err != nil {
			return false, err
		}
		ids[file.key] = id
	}
	if err := ctx.Err(); err != nil {
		return false, canceled(err)
	}
	for _, edit := range edits {
		delete(edit.part, "file_data")
		delete(edit.part, "filename")
		edit.part["file_id"] = ids[edit.file.key]
	}
	return true, nil
}

func documentMIME(extension string) string {
	switch strings.ToLower(extension) {
	case ".pdf":
		return pdfMIME
	case ".doc":
		return docMIME
	case ".docx":
		return docxMIME
	default:
		return ""
	}
}

func parseFileMetadata(raw, filename string) (*inlineFile, error) {
	if filename != "" {
		if len(filename) > 255 || !utf8.ValidString(filename) || strings.TrimSpace(filename) == "" || strings.ContainsAny(filename, "/\\") {
			return nil, errors.New("File filename must be a safe name of at most 255 UTF-8 bytes without path separators")
		}
		for _, ch := range filename {
			if unicode.IsControl(ch) {
				return nil, errors.New("File filename must not contain control characters")
			}
		}
	}
	mime, payload := "", raw
	if len(raw) >= 5 && strings.EqualFold(raw[:5], "data:") {
		comma := strings.IndexByte(raw[:min(len(raw), 128)], ',')
		if comma < 0 {
			return nil, errors.New("Inline file requires a base64 PDF or Word data URL")
		}
		header := strings.ToLower(raw[5:comma])
		if !strings.HasSuffix(header, ";base64") {
			return nil, errors.New("Inline file requires base64 encoding")
		}
		mime, payload = strings.TrimSuffix(header, ";base64"), raw[comma+1:]
		switch mime {
		case pdfMIME:
			if filename == "" {
				filename = "attachment.pdf"
			}
		case docMIME:
			if filename == "" {
				filename = "attachment.doc"
			}
		case docxMIME:
			if filename == "" {
				filename = "attachment.docx"
			}
		default:
			return nil, errors.New("Inline files support PDF, DOC, and DOCX documents only")
		}
	} else if filename == "" {
		return nil, errors.New("Raw base64 file_data requires a PDF, DOC, or DOCX filename")
	}
	fromName := documentMIME(path.Ext(filename))
	if fromName == "" {
		return nil, errors.New("Inline files require a PDF, DOC, or DOCX filename")
	}
	if mime == "" {
		mime = fromName
	} else if mime != fromName {
		return nil, errors.New("Inline file MIME type does not match its filename")
	}
	if len(payload) > base64.StdEncoding.EncodedLen(maxFileBytes) {
		return nil, errors.New("Inline file exceeds the 20 MiB limit")
	}
	if len(payload) < 4 || len(payload)%4 != 0 {
		return nil, errors.New("Inline file contains invalid base64 data")
	}
	var tail [3]byte
	n, err := base64.StdEncoding.Strict().Decode(tail[:], []byte(payload[len(payload)-4:]))
	if err != nil {
		return nil, errors.New("Inline file contains invalid base64 data")
	}
	size := int64(len(payload)/4-1)*3 + int64(n)
	if size == 0 || size > maxFileBytes {
		return nil, errors.New("Inline file is empty or exceeds the 20 MiB limit")
	}
	return &inlineFile{mime: mime, filename: filename, payload: payload, size: size}, nil
}

func validateFile(ctx context.Context, file *inlineFile) error {
	if err := validateBase64Shape(file.payload); err != nil {
		return errors.New("Inline file contains invalid base64 data")
	}
	decoder := contextReader{ctx, base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(file.payload))}
	var header [16]byte
	n, err := io.ReadFull(decoder, header[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Inline file contains invalid base64 data")
	}
	buffer := scratch.Get().(*[32 << 10]byte)
	defer release(buffer)
	rest, err := io.CopyBuffer(struct{ io.Writer }{io.Discard}, decoder, buffer[:])
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || int64(n)+rest != file.size {
		return errors.New("Inline file contains invalid base64 data")
	}
	var magic []byte
	switch file.mime {
	case pdfMIME:
		magic = []byte("%PDF-")
	case docMIME:
		magic = []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}
	case docxMIME:
		magic = []byte{0x50, 0x4b, 0x03, 0x04}
	default:
		return errors.New("Inline file type is unsupported")
	}
	if n < len(magic) || !bytes.Equal(header[:len(magic)], magic) {
		return errors.New("Inline file contents do not match its PDF or Word MIME type")
	}
	return nil
}
