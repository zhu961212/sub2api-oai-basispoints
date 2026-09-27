package protocol

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Inline documents require the same authenticated attachment upload as images.
// File URLs are deliberately not fetched by this proxy. Native file IDs already
// belong to the selected upstream account and are forwarded without rewriting.
func validateCapabilityFile(part map[string]any, prepareAttachments bool) error {
	if part["type"] != "input_file" {
		return fmt.Errorf("file type must be input_file")
	}
	fields := make(map[string]string, 3)
	for _, key := range []string{"file_id", "file_data", "file_url"} {
		if value := part[key]; value != nil {
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("input_file source fields must be strings")
			}
			if text != "" {
				fields[key] = text
			}
		}
	}
	if len(fields) != 1 {
		return fmt.Errorf("input_file requires exactly one of file_id or file_data")
	}
	if fields["file_url"] != "" {
		return fmt.Errorf("document file_url is unavailable; attach PDF or Word bytes using file_data, or use a native file_id from the selected account")
	}
	if value := part["filename"]; value != nil {
		name, ok := value.(string)
		if !ok || len(name) > 255 || !utf8.ValidString(name) || strings.TrimSpace(name) != name || strings.IndexFunc(name, func(r rune) bool { return unicode.IsControl(r) || r == '/' || r == 92 }) >= 0 {
			return fmt.Errorf("input_file filename must be a basename of at most 255 bytes without control characters")
		}
	}
	if id := fields["file_id"]; id != "" {
		if !strings.HasPrefix(id, "file-") || len(id) < 6 || len(id) > 256 {
			return fmt.Errorf("input_file requires a valid native file_id from the selected account")
		}
		for _, r := range id {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				return fmt.Errorf("input_file requires a valid native file_id from the selected account")
			}
		}
		return nil
	}
	if !prepareAttachments {
		return fmt.Errorf("inline document was not uploaded as a native attachment")
	}
	return nil
}
