package transport

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"unicode/utf16"
)

const bpsInstallationHeader = "X-Codex-Installation-Id"
const bpsTurnMetadataHeader = "X-Codex-Turn-Metadata"

var bpsDeviceFieldSeparators = strings.NewReplacer("-", "", "_", "")

// applyBPSDeviceHeaders converges device identity only. Session, thread, turn,
// window and cache identifiers retain their incoming values.
func applyBPSDeviceHeaders(h http.Header, installationID string) {
	installationID = strings.TrimSpace(installationID)
	if h == nil || installationID == "" {
		return
	}
	deviceKeys := make(map[string]struct{})
	var metadataKeys []string
	for key := range h {
		switch {
		case isBPSDeviceField(key):
			deviceKeys[http.CanonicalHeaderKey(key)] = struct{}{}
			delete(h, key)
		case isBPSTurnMetadataField(key):
			metadataKeys = append(metadataKeys, key)
		}
	}
	for key := range deviceKeys {
		h[key] = []string{installationID}
	}
	h[bpsInstallationHeader] = []string{installationID}
	if len(metadataKeys) == 0 {
		return
	}
	// Prefer the conventional header spelling, then a deterministic first
	// value. Duplicate metadata headers have no unambiguous merged semantics.
	sort.Slice(metadataKeys, func(i, j int) bool {
		leftCanonical, rightCanonical := metadataKeys[i] == bpsTurnMetadataHeader, metadataKeys[j] == bpsTurnMetadataHeader
		if leftCanonical != rightCanonical {
			return leftCanonical
		}
		return metadataKeys[i] < metadataKeys[j]
	})
	raw, selected := "", false
	for _, key := range metadataKeys {
		if !selected {
			for _, value := range h[key] {
				if strings.TrimSpace(value) != "" {
					raw, selected = value, true
					break
				}
			}
		}
		delete(h, key)
	}
	rewritten, _ := rewriteBPSTurnMetadata(raw, installationID)
	h[bpsTurnMetadataHeader] = []string{rewritten.(string)}
}

// applyBPSDeviceBody updates only existing identity carriers. It never adds
// metadata containers, traverses user input/tool arguments, or changes the
// BPS task/turn values produced by request preparation.
func applyBPSDeviceBody(body map[string]any, installationID string) bool {
	installationID = strings.TrimSpace(installationID)
	if body == nil || installationID == "" {
		return false
	}
	changed := rewriteBPSDeviceCarrier(body, installationID)
	for _, name := range []string{"metadata", "client_metadata"} {
		if carrier, ok := body[name].(map[string]any); ok {
			changed = rewriteBPSDeviceCarrier(carrier, installationID) || changed
		}
	}
	return changed
}

func rewriteBPSDeviceCarrier(carrier map[string]any, installationID string) bool {
	changed := false
	for key, value := range carrier {
		switch {
		case isBPSDeviceField(key):
			if current, ok := value.(string); !ok || current != installationID {
				carrier[key] = installationID
				changed = true
			}
		case isBPSTurnMetadataField(key):
			if rewritten, modified := rewriteBPSTurnMetadata(value, installationID); modified {
				carrier[key] = rewritten
				changed = true
			}
		}
	}
	return changed
}

func bpsDeviceFieldName(key string) string {
	return bpsDeviceFieldSeparators.Replace(strings.ToLower(key))
}

// This allowlist deliberately excludes generic IDs and similarly named
// session/window/request keys. CamelCase and header case variants are aliases.
func isBPSDeviceField(key string) bool {
	switch bpsDeviceFieldName(key) {
	case "xcodexinstallationid", "xdeviceid", "xopenaideviceid", "oaideviceid", "deviceid", "installationid":
		return true
	default:
		return false
	}
}

func isBPSTurnMetadataField(key string) bool {
	return bpsDeviceFieldName(key) == "xcodexturnmetadata"
}

func rewriteBPSTurnMetadata(value any, installationID string) (any, bool) {
	metadata, object := value.(map[string]any)
	raw, encoded := value.(string)
	if encoded {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&metadata) == nil {
			var trailing any
			object = metadata != nil && decoder.Decode(&trailing) == io.EOF
		}
	}
	changed := !object || metadata == nil
	if changed {
		metadata = make(map[string]any)
	}
	// Only the immediate embedded device aliases are interpreted. In
	// particular, unrelated nested objects remain unchanged.
	for key, previous := range metadata {
		if isBPSDeviceField(key) {
			if current, ok := previous.(string); !ok || current != installationID {
				metadata[key] = installationID
				changed = true
			}
		}
	}
	if current, ok := metadata["installation_id"].(string); !ok || current != installationID {
		metadata["installation_id"] = installationID
		changed = true
	}
	if !encoded {
		return metadata, changed
	}
	if !changed && bpsTurnMetadataIsHeaderSafeASCII(raw) {
		return raw, false
	}
	// Values came from a complete JSON object or the minimal replacement,
	// so Marshal cannot encounter unsupported Go values or invalid numbers.
	rebuilt, _ := marshalBPSTurnMetadata(metadata)
	return string(rebuilt), true
}

func bpsTurnMetadataIsHeaderSafeASCII(raw string) bool {
	for index := range len(raw) {
		if raw[index] >= 0x7f || raw[index] < 0x20 {
			return false
		}
	}
	return true
}

// Match the host's ASCII-safe turn metadata encoding. json.Marshal leaves DEL
// and UTF-8 literal; DEL is rejected by net/http, including when it originated
// as a legal JSON escape in the incoming header. Non-BMP runes need a UTF-16
// surrogate pair so decoding the header restores exactly the original value.
func marshalBPSTurnMetadata(metadata map[string]any) ([]byte, error) {
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	for index, value := range raw {
		if value < 0x7f {
			continue
		}
		output := make([]byte, 0, len(raw)+16)
		output = append(output, raw[:index]...)
		appendEscape := func(value rune) {
			const hex = "0123456789abcdef"
			output = append(output, 92, 'u', hex[value>>12&15], hex[value>>8&15], hex[value>>4&15], hex[value&15])
		}
		for _, value := range string(raw[index:]) {
			switch {
			case value < 0x7f:
				output = append(output, byte(value))
			case value <= 0xffff:
				appendEscape(value)
			default:
				high, low := utf16.EncodeRune(value)
				appendEscape(high)
				appendEscape(low)
			}
		}
		return output, nil
	}
	return raw, nil
}
