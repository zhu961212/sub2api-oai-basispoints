package transport

import (
	"strings"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// File validation/upload must run even when both conversion toggles are off.
// Inspect protocol content blocks only; never inspect tool arguments or text.
func hasInputFiles(source map[string]any) bool {
	input, _ := source["input"].([]any)
	for _, value := range input {
		item, _ := value.(map[string]any)
		for _, field := range []string{"content", "output"} {
			parts, _ := item[field].([]any)
			for _, raw := range parts {
				part, _ := raw.(map[string]any)
				if strings.EqualFold(protocol.StringValue(part["type"]), "input_file") {
					return true
				}
			}
		}
	}
	return false
}
