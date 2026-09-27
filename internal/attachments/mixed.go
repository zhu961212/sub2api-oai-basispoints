package attachments

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ValidateMixedInputs preflights the original mixed image/document request
// before either rewrite can upload anything. It performs no network requests,
// cache operations, or source mutations. Repeated occurrences share the decoded
// byte budget even though their validation work is deduplicated locally.
func ValidateMixedInputs(ctx context.Context, source map[string]any) error {
	images := make(map[string]*inlineImage)
	type encodedFile struct{ data, filename string }
	files := make(map[encodedFile]*inlineFile)
	var imageCount, fileCount int
	var total int64
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
			fieldPath := fmt.Sprintf("input[%d].%s[%d]", i, field, j)
			invalid := func(code, message string) error { return fail(400, code, message+" (path="+fieldPath+")") }
			var validationErr error
			code := ""
			switch part["type"] {
			case "input_image":
				raw, _ := part["image_url"].(string)
				if len(raw) < 5 || !strings.EqualFold(raw[:5], "data:") {
					continue
				}
				if err := ctx.Err(); err != nil {
					return canceled(err)
				}
				code = "invalid_image"
				imageCount++
				if imageCount > maxRequestImages {
					return invalid(code, "At most 20 inline images are allowed per request; conversation history, tool screenshots, and repeated images count toward this limit")
				}
				if value, exists := part["file_id"]; exists && value != nil && value != "" {
					return invalid(code, "input_image cannot contain both image_url and file_id")
				}
				img := images[raw]
				if img == nil {
					img, validationErr = parseImageMetadata(raw)
					if validationErr == nil {
						validationErr = validateImage(ctx, img)
					}
					if validationErr == nil {
						images[raw] = img
					}
				}
				if validationErr == nil {
					total += img.size
				}
			case "input_file":
				code = "invalid_file"
				if value, exists := part["file_url"]; exists && value != nil && value != "" {
					return invalid(code, "Remote file_url attachments are not supported; provide file_data or a native file_id")
				}
				if value, exists := part["file_id"]; exists && value != nil && value != "" {
					if data, exists := part["file_data"]; exists && data != nil && data != "" {
						return invalid(code, "input_file cannot contain both file_data and file_id")
					}
					continue
				}
				if err := ctx.Err(); err != nil {
					return canceled(err)
				}
				raw, ok := part["file_data"].(string)
				if !ok || raw == "" {
					return invalid(code, "input_file requires nonempty file_data or a native file_id")
				}
				filename := ""
				if value := part["filename"]; value != nil {
					filename, ok = value.(string)
					if !ok {
						return invalid(code, "input_file filename must be a string")
					}
				}
				fileCount++
				if fileCount > maxRequestFiles {
					return invalid(code, "At most 20 inline files are allowed per request; conversation history, tool outputs, and repeated files count toward this limit")
				}
				key := encodedFile{raw, filename}
				file := files[key]
				if file == nil {
					file, validationErr = parseFileMetadata(raw, filename)
					if validationErr == nil {
						validationErr = validateFile(ctx, file)
					}
					if validationErr == nil {
						files[key] = file
					}
				}
				if validationErr == nil {
					total += file.size
				}
			default:
				continue
			}
			if validationErr != nil {
				if errors.Is(validationErr, context.Canceled) || errors.Is(validationErr, context.DeadlineExceeded) {
					return canceled(validationErr)
				}
				return invalid(code, validationErr.Error())
			}
			if total > maxRequestBytes {
				return invalid("invalid_attachment", "Inline images and files exceed the shared 32 MiB request limit; conversation history, tool screenshots, and repeated attachments count toward this limit")
			}
		}
	}
	return nil
}
