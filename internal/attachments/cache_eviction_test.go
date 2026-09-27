package attachments

import (
	"context"
	"encoding/binary"
	"testing"
	"time"
)

func cacheTestKey(index int) (key [32]byte) {
	binary.LittleEndian.PutUint64(key[:], uint64(index))
	return key
}

func TestCacheExpirationDoesNotExtendOnHit(t *testing.T) {
	u := New()
	now := time.Unix(100, 0)
	u.now = func() time.Time { return now }
	key := cacheTestKey(1)
	calls := 0
	upload := func() (string, error) {
		calls++
		return "file-test", nil
	}
	get := func() {
		t.Helper()
		if id, err := u.getOrUpload(context.Background(), key, true, upload); err != nil || id != "file-test" {
			t.Fatalf("cache lookup: id=%q err=%v", id, err)
		}
	}
	get()
	now = now.Add(cacheTTL - time.Nanosecond)
	get()
	if calls != 1 {
		t.Fatalf("unexpired entry reuploaded: %d", calls)
	}
	now = now.Add(time.Nanosecond)
	get()
	if calls != 2 {
		t.Fatalf("hit extended TTL or expiry boundary was accepted: %d", calls)
	}
}

func TestCacheHitProtectsLeastRecentlyUsedEntry(t *testing.T) {
	u := New()
	upload := func() (string, error) { return "file-test", nil }
	for i := 0; i < maxCacheEntries; i++ {
		if _, err := u.getOrUpload(context.Background(), cacheTestKey(i), true, upload); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := u.getOrUpload(context.Background(), cacheTestKey(0), true, func() (string, error) {
		t.Fatal("cache hit attempted another upload")
		return "", nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.getOrUpload(context.Background(), cacheTestKey(maxCacheEntries), true, upload); err != nil {
		t.Fatal(err)
	}
	if len(u.cache) != maxCacheEntries || u.cached(cacheTestKey(0)) == "" || u.cached(cacheTestKey(1)) != "" {
		t.Fatal("full cache did not evict only the least recently used entry")
	}
}

func TestCachePrunesExpiredEntriesBeforeEvictingLiveEntries(t *testing.T) {
	u := New()
	now := time.Unix(100, 0)
	u.now = func() time.Time { return now }
	upload := func() (string, error) { return "file-test", nil }
	for i := 0; i < maxCacheEntries; i++ {
		if i == maxCacheEntries/2 {
			now = now.Add(cacheTTL / 2)
		}
		if _, err := u.getOrUpload(context.Background(), cacheTestKey(i), true, upload); err != nil {
			t.Fatal(err)
		}
	}
	// Expiring entries are more recently used than the still-live entries.
	for i := 0; i < maxCacheEntries/2; i++ {
		if u.cached(cacheTestKey(i)) == "" {
			t.Fatal("entry expired before its deadline")
		}
	}
	now = now.Add(cacheTTL / 2)
	if _, err := u.getOrUpload(context.Background(), cacheTestKey(maxCacheEntries), true, upload); err != nil {
		t.Fatal(err)
	}
	if len(u.cache) != maxCacheEntries/2+1 {
		t.Fatalf("expired metadata not reclaimed at capacity: %d", len(u.cache))
	}
	for i := 0; i < maxCacheEntries; i++ {
		_, ok := u.cache[cacheTestKey(i)]
		if ok != (i >= maxCacheEntries/2) {
			t.Fatalf("wrong eviction for key %d: present=%v", i, ok)
		}
	}
}

func BenchmarkAttachmentCacheMetadata(b *testing.B) {
	for _, scenario := range []struct {
		name string
		size int
		hit  bool
	}{
		{"warm_hit", maxCacheEntries, true},
		{"insert_below_capacity", 64, false},
		{"insert_at_capacity", maxCacheEntries, false},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			u := New()
			now := time.Now()
			u.now = func() time.Time { return now }
			upload := func() (string, error) { return "file-benchmark", nil }
			ctx := context.Background()
			for i := 0; i < scenario.size; i++ {
				if _, err := u.getOrUpload(ctx, cacheTestKey(i), true, upload); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				key := cacheTestKey(i + scenario.size)
				if scenario.hit {
					key = cacheTestKey(i % scenario.size)
				}
				if _, err := u.getOrUpload(ctx, key, true, upload); err != nil {
					b.Fatal(err)
				}
				if !scenario.hit && scenario.size < maxCacheEntries {
					delete(u.cache, key)
				}
			}
		})
	}
}
