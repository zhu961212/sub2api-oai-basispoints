package transport

import "github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"

// normalizeRelayFailure translates upstream terminal aliases to Responses
// events. A clean [DONE] does not help a client that never recognized the
// preceding terminal event, and every failure needs a readable cause.
func normalizeRelayFailure(event string, payload map[string]any) string {
	redactImageErrorValues(payload)
	return protocol.NormalizeResponseFailure(payload, protocol.ClassifyResponseTerminal(event, payload))
}
