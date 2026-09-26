package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// bpsAccountDeviceID reads only host-owned account metadata, never client
// headers or request bodies. Retain neither the seed nor the raw snapshot.
func bpsAccountDeviceID(raw []byte) string {
	var snapshot struct{ Extra map[string]json.RawMessage }
	if json.Unmarshal(raw, &snapshot) != nil {
		return ""
	}
	var deviceID, seed string
	_ = json.Unmarshal(snapshot.Extra["openai_device_id"], &deviceID)
	_ = json.Unmarshal(snapshot.Extra["codex_fingerprint_seed"], &seed)
	if deviceID = strings.TrimSpace(deviceID); validBPSDeviceID(deviceID) {
		return deviceID
	}
	seed = strings.TrimSpace(seed)
	if !canonicalBPSFingerprintSeed(seed) {
		return ""
	}
	return deriveBPSDeviceUUID("sub2api:codex-install-id:v2:" + seed)
}

func validBPSDeviceID(value string) bool {
	if value == "" || len(value) > 512 {
		return false
	}
	for _, ch := range []byte(value) {
		if ch < 0x20 || ch == 0x7f {
			return false
		}
	}
	return true
}

func canonicalBPSFingerprintSeed(seed string) bool {
	if len(seed) != 36 || seed != strings.ToLower(seed) || seed[8] != '-' || seed[13] != '-' || seed[18] != '-' || seed[23] != '-' {
		return false
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(seed, "-", ""))
	if err != nil || len(decoded) != 16 {
		return false
	}
	for _, b := range decoded {
		if b != 0 {
			return true
		}
	}
	return false
}

// Same SHA-256 truncation and UUID version/variant bits as Sub2API.
func deriveBPSDeviceUUID(seed string) string {
	h := sha256.Sum256([]byte(seed))
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Shared by forwarding, attachments and probes. Credential resolution still
// owns token, account and proxy choice. Only BPS callers use this wrapper.
func (t *Transport) prepareBPSHeaders(ctx context.Context, start *pluginv1.ForwardRequestStart, host pluginv1.HostServiceClient, cfg protocol.Config) (http.Header, string, error) {
	h, proxyURL, err := prepareHeaders(ctx, start, host, cfg.AuthMode)
	if err != nil {
		return nil, "", err
	}
	// Use the request snapshot for headers and body, even if ApplyConfig runs
	// during identity lookup. Off does not perform a device metadata lookup.
	if !cfg.BPSDeviceConvergence {
		return h, proxyURL, nil
	}
	// Legacy hosts may lack readable metadata. The already resolved upstream
	// account (including shadow-parent identity) survives token refresh and
	// restart; never seed from client devices, tokens or a local numeric ID.
	deviceID := deriveBPSDeviceUUID("sub2api:bps-install-id:v1:" + h.Get("ChatGPT-Account-ID"))
	if host != nil {
		t.mu.RLock()
		bound := t.host == host && !t.closed
		t.mu.RUnlock()
		if bound {
			if trustedID := t.accountDeviceID(ctx, start.GetAccountId()); trustedID != "" {
				deviceID = trustedID
			}
			t.mu.RLock()
			stillBound := t.host == host && !t.closed
			t.mu.RUnlock()
			if !stillBound {
				return nil, "", unreplayable(fmt.Errorf("host identity changed while preparing BPS device"))
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	applyBPSDeviceHeaders(h, deviceID)
	return h, proxyURL, nil
}
