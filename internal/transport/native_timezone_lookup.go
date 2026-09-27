package transport

import (
	"container/list"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // Minimal hosts may not have a system zoneinfo database.
)

const (
	nativeTimezoneLookupURL       = "https://ipapi.co/timezone/"
	nativeTimezoneLookupTimeout   = 2 * time.Second
	nativeTimezoneSuccessTTL      = 6 * time.Hour
	nativeTimezoneFailureTTL      = 5 * time.Minute
	nativeTimezoneMaxEntries      = 1024
	nativeTimezoneMaxConcurrent   = 8
	nativeTimezoneMaxResponseSize = 256
)

type nativeTimezoneKey struct {
	accountID int64
	proxyHash [sha256.Size]byte
}

type nativeTimezoneEntry struct {
	key      nativeTimezoneKey
	ready    chan struct{}
	location *time.Location
	expires  time.Time
	pending  bool
	element  *list.Element
}

// Entries include active requests, so both cache memory and lookup concurrency
// stay bounded. Proxy credentials are never retained in cache keys or logs.
type nativeTimezoneResolver struct {
	mu      sync.Mutex
	entries map[nativeTimezoneKey]*nativeTimezoneEntry
	recent  list.List
	active  int
	now     func() time.Time
}

func newNativeTimezoneResolver() *nativeTimezoneResolver {
	return &nativeTimezoneResolver{entries: make(map[nativeTimezoneKey]*nativeTimezoneEntry), now: time.Now}
}

func (r *nativeTimezoneResolver) resolve(ctx context.Context, accountID int64, proxyURL string, client *http.Client) (*time.Location, bool) {
	if r == nil || ctx == nil || ctx.Err() != nil || client == nil {
		return nil, false
	}
	key := nativeTimezoneKey{accountID: accountID, proxyHash: sha256.Sum256([]byte(proxyURL))}
	r.mu.Lock()
	if ctx.Err() != nil {
		r.mu.Unlock()
		return nil, false
	}
	if r.entries == nil {
		r.entries = make(map[nativeTimezoneKey]*nativeTimezoneEntry)
	}
	if r.now == nil {
		r.now = time.Now
	}
	if entry := r.entries[key]; entry != nil {
		if entry.pending || r.now().Before(entry.expires) {
			r.recent.MoveToFront(entry.element)
			r.mu.Unlock()
			return awaitNativeTimezone(ctx, entry)
		}
		r.removeLocked(entry)
	}
	// Optional metadata must not create an unbounded backlog or delay the
	// actual native request when the small lookup pool is already occupied.
	if r.active >= nativeTimezoneMaxConcurrent {
		r.mu.Unlock()
		return nil, false
	}
	if len(r.entries) >= nativeTimezoneMaxEntries {
		for oldest := r.recent.Back(); oldest != nil; oldest = oldest.Prev() {
			if entry := oldest.Value.(*nativeTimezoneEntry); !entry.pending {
				r.removeLocked(entry)
				break
			}
		}
		if len(r.entries) >= nativeTimezoneMaxEntries {
			r.mu.Unlock()
			return nil, false
		}
	}
	entry := &nativeTimezoneEntry{key: key, ready: make(chan struct{}), pending: true}
	entry.element = r.recent.PushFront(entry)
	r.entries[key] = entry
	r.active++
	isolated := *client
	isolated.Jar = nil
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if isolated.Timeout <= 0 || isolated.Timeout > nativeTimezoneLookupTimeout {
		isolated.Timeout = nativeTimezoneLookupTimeout
	}
	r.mu.Unlock()
	go r.lookup(entry, &isolated)
	return awaitNativeTimezone(ctx, entry)
}

func (r *nativeTimezoneResolver) removeLocked(entry *nativeTimezoneEntry) {
	delete(r.entries, entry.key)
	r.recent.Remove(entry.element)
}

func awaitNativeTimezone(ctx context.Context, entry *nativeTimezoneEntry) (*time.Location, bool) {
	select {
	case <-ctx.Done():
		return nil, false
	case <-entry.ready:
		if ctx.Err() != nil || entry.location == nil {
			return nil, false
		}
		return entry.location, true
	}
}

func (r *nativeTimezoneResolver) lookup(entry *nativeTimezoneEntry, client *http.Client) {
	// This bounded shared request outlives any one waiter, while each waiter
	// returns immediately on its own cancellation. No account context, cookie,
	// token, or original request header enters this independent GET.
	ctx, cancel := context.WithTimeout(context.Background(), nativeTimezoneLookupTimeout)
	defer cancel()
	location := queryNativeTimezone(ctx, client)
	ttl := nativeTimezoneSuccessTTL
	if location == nil {
		ttl = nativeTimezoneFailureTTL
	}
	r.mu.Lock()
	entry.location, entry.expires, entry.pending = location, r.now().Add(ttl), false
	r.active--
	close(entry.ready)
	r.mu.Unlock()
}

func queryNativeTimezone(ctx context.Context, client *http.Client) *time.Location {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nativeTimezoneLookupURL, nil)
	if err != nil {
		return nil
	}
	response, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, nativeTimezoneMaxResponseSize+1))
	if err != nil || len(body) > nativeTimezoneMaxResponseSize {
		return nil
	}
	name := strings.TrimSpace(string(body))
	if name == "" || name == "Local" || len(name) > 128 ||
		!(name[0] >= 'A' && name[0] <= 'Z' || name[0] >= 'a' && name[0] <= 'z') {
		return nil
	}
	for _, ch := range name {
		if !(ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' ||
			ch == '/' || ch == '_' || ch == '-' || ch == '+') {
			return nil
		}
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil
	}
	return location
}
