package transport

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type nativeTimezoneRoundTripper func(*http.Request) (*http.Response, error)

func (f nativeTimezoneRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func nativeTimezoneResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func nativeTimezoneTestClock(resolver *nativeTimezoneResolver) *atomic.Int64 {
	clock := &atomic.Int64{}
	clock.Store(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	resolver.now = func() time.Time { return time.Unix(0, clock.Load()) }
	return clock
}

func TestNativeTimezoneLookupUsesIsolatedBoundedRequest(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, _ := url.Parse(nativeTimezoneLookupURL)
	jar.SetCookies(endpoint, []*http.Cookie{{Name: "private-session", Value: "fixture-cookie"}})
	var calls atomic.Int32
	client := &http.Client{Jar: jar, Timeout: time.Minute, Transport: nativeTimezoneRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.Method != http.MethodGet || request.URL.String() != nativeTimezoneLookupURL || request.Body != nil {
			t.Error("timezone query changed its fixed independent GET")
		}
		if len(request.Header) != 0 || request.URL.User != nil {
			t.Error("timezone query inherited headers or credentials")
		}
		deadline, bounded := request.Context().Deadline()
		if !bounded || time.Until(deadline) > nativeTimezoneLookupTimeout {
			t.Error("timezone query is not bounded to two seconds")
		}
		return nativeTimezoneResponse("America/New_York\n"), nil
	})}
	resolver := newNativeTimezoneResolver()
	location, ok := resolver.resolve(context.Background(), 7, "http://fixture:secret@proxy.invalid:8080", client)
	if !ok || location.String() != "America/New_York" || calls.Load() != 1 {
		t.Fatal("valid independent timezone query did not resolve")
	}
	if client.Jar != jar || client.Timeout != time.Minute {
		t.Fatal("timezone query mutated the account client")
	}
}

func TestNativeTimezoneLookupRejectsRedirectWithoutFollowing(t *testing.T) {
	var calls, followed atomic.Int32
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { followed.Add(1); return nil },
		Transport: nativeTimezoneRoundTripper(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			response := nativeTimezoneResponse("UTC")
			response.StatusCode = http.StatusFound
			response.Header.Set("Location", "https://outside.invalid/timezone")
			return response, nil
		})}
	if _, ok := newNativeTimezoneResolver().resolve(context.Background(), 7, "", client); ok {
		t.Fatal("redirect produced a timezone")
	}
	if calls.Load() != 1 || followed.Load() != 0 {
		t.Fatal("timezone query followed a redirect or reused client redirect policy")
	}
}

func TestNativeTimezoneLookupHonorsShorterClientTimeout(t *testing.T) {
	client := &http.Client{Timeout: 20 * time.Millisecond, Transport: nativeTimezoneRoundTripper(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	started := time.Now()
	if _, ok := newNativeTimezoneResolver().resolve(context.Background(), 7, "", client); ok {
		t.Fatal("timed-out query produced a timezone")
	}
	if time.Since(started) > time.Second || client.Timeout != 20*time.Millisecond {
		t.Fatal("timezone lookup lengthened or mutated the shorter client timeout")
	}
}

func TestNativeTimezoneLookupValidatesIANAAndRejectsHTTPFailures(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		want       bool
	}{
		{"iana", "Asia/Shanghai", 200, true},
		{"utc", "UTC", 200, true},
		{"iana offset zone", "Etc/GMT+8", 200, true},
		{"empty", "", 200, false},
		{"local", "Local", 200, false},
		{"offset", "+08:00", 200, false},
		{"prefixed offset", "UTC+08:00", 200, false},
		{"unknown", "Invalid/Timezone", 200, false},
		{"multiple lines", "UTC\nAsia/Shanghai", 200, false},
		{"path traversal", "../UTC", 200, false},
		{"too long", strings.Repeat("x", nativeTimezoneMaxResponseSize+1), 200, false},
		{"denied", "UTC", 403, false},
		{"rate limited", "UTC", 429, false},
		{"server error", "UTC", 500, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: nativeTimezoneRoundTripper(func(*http.Request) (*http.Response, error) {
				response := nativeTimezoneResponse(test.body)
				response.StatusCode = test.status
				return response, nil
			})}
			location, ok := newNativeTimezoneResolver().resolve(context.Background(), 7, "", client)
			if ok != test.want || (location != nil) != test.want {
				t.Fatalf("timezone validation: got success=%t; want %t", ok, test.want)
			}
		})
	}
}

func TestNativeTimezoneCachePartitionsAccountAndProxyAndExpiresSuccess(t *testing.T) {
	resolver := newNativeTimezoneResolver()
	clock := nativeTimezoneTestClock(resolver)
	var calls atomic.Int32
	client := &http.Client{Transport: nativeTimezoneRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nativeTimezoneResponse("UTC"), nil
	})}
	const proxy = "http://synthetic-user:synthetic-secret@proxy.invalid:8080"
	for _, pair := range []struct {
		id    int64
		proxy string
	}{{7, proxy}, {7, proxy}, {8, proxy}, {7, proxy + "/changed"}} {
		if _, ok := resolver.resolve(context.Background(), pair.id, pair.proxy, client); !ok {
			t.Fatal("cache query did not resolve")
		}
	}
	if calls.Load() != 3 || len(resolver.entries) != 3 {
		t.Fatal("cache reused a different account or proxy entry")
	}
	key := nativeTimezoneKey{accountID: 7, proxyHash: sha256.Sum256([]byte(proxy))}
	if resolver.entries[key] == nil {
		t.Fatal("cache did not key by account and proxy hash")
	}
	clock.Add(int64(nativeTimezoneSuccessTTL - time.Nanosecond))
	_, _ = resolver.resolve(context.Background(), 7, proxy, client)
	if calls.Load() != 3 {
		t.Fatal("successful timezone expired early")
	}
	clock.Add(1)
	_, _ = resolver.resolve(context.Background(), 7, proxy, client)
	if calls.Load() != 4 {
		t.Fatal("six-hour success TTL did not expire")
	}
}

func TestNativeTimezoneCacheExpiresFailuresAfterFiveMinutes(t *testing.T) {
	resolver := newNativeTimezoneResolver()
	clock := nativeTimezoneTestClock(resolver)
	var calls atomic.Int32
	client := &http.Client{Transport: nativeTimezoneRoundTripper(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("synthetic lookup failure")
		}
		return nativeTimezoneResponse("UTC"), nil
	})}
	if _, ok := resolver.resolve(context.Background(), 7, "", client); ok {
		t.Fatal("failed lookup produced a timezone")
	}
	clock.Add(int64(nativeTimezoneFailureTTL - time.Nanosecond))
	if _, ok := resolver.resolve(context.Background(), 7, "", client); ok || calls.Load() != 1 {
		t.Fatal("negative cache did not suppress repeated failures")
	}
	clock.Add(1)
	if _, ok := resolver.resolve(context.Background(), 7, "", client); !ok || calls.Load() != 2 {
		t.Fatal("five-minute failure TTL did not permit recovery")
	}
}

func TestNativeTimezoneCanceledContextNeverQueriesOrReturnsCache(t *testing.T) {
	resolver := newNativeTimezoneResolver()
	var calls atomic.Int32
	client := &http.Client{Transport: nativeTimezoneRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nativeTimezoneResponse("UTC"), nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if location, ok := resolver.resolve(ctx, 7, "", client); location != nil || ok || calls.Load() != 0 {
		t.Fatal("pre-canceled context issued a query")
	}
	if _, ok := resolver.resolve(context.Background(), 7, "", client); !ok {
		t.Fatal("cache warmup failed")
	}
	if location, ok := resolver.resolve(ctx, 7, "", client); location != nil || ok || calls.Load() != 1 {
		t.Fatal("pre-canceled context received a cached timezone")
	}
}

func TestNativeTimezoneCanceledWaiterDoesNotCancelSharedLookup(t *testing.T) {
	resolver := newNativeTimezoneResolver()
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	var lookupContext context.Context
	client := &http.Client{Transport: nativeTimezoneRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		lookupContext = request.Context()
		close(started)
		select {
		case <-release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		return nativeTimezoneResponse("UTC"), nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan bool, 1)
	go func() { _, ok := resolver.resolve(ctx, 7, "", client); first <- ok }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("lookup did not start")
	}
	cancel()
	select {
	case ok := <-first:
		if ok {
			t.Fatal("canceled waiter received a timezone")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not return promptly")
	}
	if lookupContext.Err() != nil {
		t.Fatal("one caller canceled the shared lookup")
	}
	// A separate short-lived waiter shares the same lookup instead of
	// negatively caching or starting another request after the first cancels.
	second, cancelSecond := context.WithCancel(context.Background())
	secondDone := make(chan bool, 1)
	go func() { _, ok := resolver.resolve(second, 7, "", client); secondDone <- ok }()
	cancelSecond()
	select {
	case ok := <-secondDone:
		if ok {
			t.Fatal("canceled follower received a timezone")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled follower did not return promptly")
	}
	if calls.Load() != 1 {
		t.Fatal("cancellation started a duplicate lookup")
	}
}

func TestNativeTimezoneSameKeySingleflightAndIndependentKeys(t *testing.T) {
	resolver := newNativeTimezoneResolver()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int32
	client := &http.Client{Transport: nativeTimezoneRoundTripper(func(request *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		}
		return nativeTimezoneResponse("UTC"), nil
	})}
	var wg sync.WaitGroup
	var successful atomic.Int32
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := resolver.resolve(context.Background(), 7, "same-proxy", client); ok {
				successful.Add(1)
			}
		}()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shared lookup did not start")
	}
	independent, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, ok := resolver.resolve(independent, 8, "same-proxy", client); !ok {
		t.Fatal("slow lookup blocked an unrelated account")
	}
	once.Do(func() { close(release) })
	wg.Wait()
	if calls.Load() != 2 || successful.Load() != 32 {
		t.Fatalf("singleflight: calls=%d successful=%d", calls.Load(), successful.Load())
	}
}

func TestNativeTimezoneCacheEvictsLeastRecentlyUsedWithoutGrowing(t *testing.T) {
	resolver := newNativeTimezoneResolver()
	nativeTimezoneTestClock(resolver)
	client := &http.Client{Transport: nativeTimezoneRoundTripper(func(*http.Request) (*http.Response, error) {
		return nativeTimezoneResponse("UTC"), nil
	})}
	for id := int64(1); id <= nativeTimezoneMaxEntries; id++ {
		if _, ok := resolver.resolve(context.Background(), id, "", client); !ok {
			t.Fatal("cache fill failed")
		}
	}
	_, _ = resolver.resolve(context.Background(), 1, "", client)
	_, _ = resolver.resolve(context.Background(), nativeTimezoneMaxEntries+1, "", client)
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	first := nativeTimezoneKey{accountID: 1, proxyHash: sha256.Sum256(nil)}
	second := nativeTimezoneKey{accountID: 2, proxyHash: sha256.Sum256(nil)}
	if len(resolver.entries) != nativeTimezoneMaxEntries || resolver.recent.Len() != nativeTimezoneMaxEntries ||
		resolver.entries[first] == nil || resolver.entries[second] != nil || resolver.active != 0 {
		t.Fatal("bounded LRU accounting or eviction order is incorrect")
	}
}

func TestNativeTimezoneConcurrentLookupLimitFailsOpenWithoutQueueing(t *testing.T) {
	resolver := newNativeTimezoneResolver()
	started := make(chan struct{}, nativeTimezoneMaxConcurrent+1)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int32
	client := &http.Client{Transport: nativeTimezoneRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		return nativeTimezoneResponse("UTC"), nil
	})}
	var wg sync.WaitGroup
	for id := int64(1); id <= nativeTimezoneMaxConcurrent; id++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = resolver.resolve(context.Background(), id, "", client) }()
	}
	for range nativeTimezoneMaxConcurrent {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("bounded lookup pool did not start")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, ok := resolver.resolve(ctx, 9999, "", client); ok || ctx.Err() != nil || calls.Load() != nativeTimezoneMaxConcurrent {
		t.Fatal("full lookup pool queued work or exceeded its bound")
	}
	once.Do(func() { close(release) })
	wg.Wait()
	if _, ok := resolver.resolve(context.Background(), 9999, "", client); !ok {
		t.Fatal("full-pool rejection incorrectly poisoned the cache")
	}
}

type nativeTimezoneCountingBody struct {
	reads  int
	closed bool
	fail   bool
}

func (body *nativeTimezoneCountingBody) Read(buffer []byte) (int, error) {
	if body.fail {
		return 0, io.ErrUnexpectedEOF
	}
	for i := range buffer {
		buffer[i] = 'x'
	}
	body.reads += len(buffer)
	return len(buffer), nil
}
func (body *nativeTimezoneCountingBody) Close() error { body.closed = true; return nil }

func TestNativeTimezoneResponseReadIsBoundedAndClosed(t *testing.T) {
	for _, fail := range []bool{false, true} {
		body := &nativeTimezoneCountingBody{fail: fail}
		client := &http.Client{Transport: nativeTimezoneRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
		})}
		if _, ok := newNativeTimezoneResolver().resolve(context.Background(), 7, "", client); ok {
			t.Fatal("invalid or endless body produced a timezone")
		}
		if !body.closed || body.reads > nativeTimezoneMaxResponseSize+1 {
			t.Fatal("lookup did not bound or close the response body")
		}
	}
}
