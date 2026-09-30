package transport

import (
	"crypto/sha256"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

const (
	bpsRateBackoffMaxWait    = 24 * time.Hour
	bpsRateBackoffMaxEntries = 4096
)

type bpsRateBackoffKey [sha256.Size]byte

// Only opaque identity hashes and expiry times are retained. This per-instance
// optimization never changes host quota, model selection or account health.
type bpsRateLimitBackoff struct {
	mu    sync.Mutex
	until map[bpsRateBackoffKey]time.Time
}

func bpsRateLimitScope(accountID int64, endpoint, model, proxyURL string, headers http.Header) bpsRateBackoffKey {
	if accountID <= 0 || !isBearerToken(headers.Get("Authorization")) {
		return bpsRateBackoffKey{}
	}
	return sha256.Sum256(protocol.JSONBytes([]any{accountID, endpoint, model, proxyURL,
		headers.Get("Authorization"), headers.Get("ChatGPT-Account-ID"),
		headers.Get("X-OpenAI-Account-ID"), headers.Get("X-Basispoints-Auth-Mode")}))
}

// Missing, conflicting, invalid, expired or excessively long instructions do
// not invent a fallback pause. In particular, never clamp a longer valid wait
// to 24 hours and then issue an earlier retry. The upstream failure still wins.
func bpsRetryAfterDelay(headers http.Header, now time.Time) (time.Duration, bool) {
	values := headers.Values("Retry-After")
	if len(values) != 1 {
		return 0, false
	}
	raw := strings.TrimSpace(values[0])
	if raw == "" || len(raw) > 128 {
		return 0, false
	}
	digits := true
	for _, c := range raw {
		if c < '0' || c > '9' {
			digits = false
			break
		}
	}
	if digits {
		seconds, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || seconds == 0 || seconds > uint64(bpsRateBackoffMaxWait/time.Second) {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	until, err := http.ParseTime(raw)
	if err != nil {
		return 0, false
	}
	delay := until.Sub(now)
	return delay, delay > 0 && delay <= bpsRateBackoffMaxWait
}

func (s *bpsRateLimitBackoff) record(key bpsRateBackoffKey, headers http.Header, now time.Time) {
	if key == (bpsRateBackoffKey{}) {
		return
	}
	delay, valid := bpsRetryAfterDelay(headers, now)
	if !valid {
		return
	}
	next := now.Add(delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.until == nil {
		s.until = make(map[bpsRateBackoffKey]time.Time)
	}
	if previous, exists := s.until[key]; exists {
		if next.After(previous) {
			s.until[key] = next
		}
		return
	}
	if len(s.until) >= bpsRateBackoffMaxEntries {
		for k, expiry := range s.until {
			if !expiry.After(now) {
				delete(s.until, k)
			}
		}
		if len(s.until) >= bpsRateBackoffMaxEntries {
			return
		}
	}
	s.until[key] = next
}

func (s *bpsRateLimitBackoff) remainingSeconds(key bpsRateBackoffKey, now time.Time) int64 {
	if key == (bpsRateBackoffKey{}) {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, exists := s.until[key]
	if !exists {
		return 0
	}
	if !expiry.After(now) {
		delete(s.until, key)
		return 0
	}
	return int64((expiry.Sub(now) + time.Second - 1) / time.Second)
}

type bpsRateBackoffTransport struct {
	base                         http.RoundTripper
	state                        *bpsRateLimitBackoff
	key                          bpsRateBackoffKey
	responsesURL, attachmentsURL string
}

func (t *bpsRateBackoffTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err == nil && response != nil && response.StatusCode == http.StatusTooManyRequests &&
		request.Method == http.MethodPost && request.URL != nil &&
		(request.URL.String() == t.responsesURL || request.URL.String() == t.attachmentsURL) {
		t.state.record(t.key, response.Header, time.Now())
	}
	return response, err
}

// Copy only the client wrapper; retain the exact shared transport/proxy pool,
// timeout, redirect policy and request context. Nothing is closed or retried.
func withBPSRateBackoff(client *http.Client, state *bpsRateLimitBackoff, key bpsRateBackoffKey, endpoint string) *http.Client {
	if client == nil || key == (bpsRateBackoffKey{}) {
		return client
	}
	attachmentsURL := ""
	if parsed, err := url.Parse(strings.TrimRight(endpoint, "/")); err == nil {
		attachmentsURL = parsed.ResolveReference(&url.URL{Path: "attachments"}).String()
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copied := *client
	copied.Transport = &bpsRateBackoffTransport{base: base, state: state, key: key, responsesURL: endpoint, attachmentsURL: attachmentsURL}
	return &copied
}
