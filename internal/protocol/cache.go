package protocol

import (
	"bytes"
	"container/list"
	"encoding/json"
	"sync"
	"time"
)

const (
	protocolCacheIdleTTL    = 2 * time.Hour
	protocolCacheEntryBytes = 4 << 20
)

// Entries own immutable JSON trees. A hit takes only an immutable snapshot
// under the mutex; cloning happens afterwards, preserving independent callers
// while avoiding JSON encoding/decoding and long global critical sections.
// Weight conservatively accounts for keys, strings and JSON container metadata;
// it bounds retained cache data, not total process RSS or in-flight requests.
type protocolCache struct {
	mu            sync.Mutex
	items         map[string]*list.Element
	recent        list.List
	bytes         int
	maxEntries    int
	maxBytes      int
	maxEntryBytes int
	idleTTL       time.Duration
	now           func() time.Time
}

type protocolCacheEntry struct {
	key    string
	value  any
	weight int
	used   time.Time
}

func newProtocolCache(entries, budget, entryBudget int, ttl time.Duration) *protocolCache {
	return &protocolCache{items: make(map[string]*list.Element), maxEntries: entries, maxBytes: budget, maxEntryBytes: entryBudget, idleTTL: ttl, now: time.Now}
}

var (
	nativeCallCache      = newProtocolCache(512, 32<<20, protocolCacheEntryBytes, protocolCacheIdleTTL)
	toolCatalogCache     = newProtocolCache(256, 32<<20, protocolCacheEntryBytes, protocolCacheIdleTTL)
	responseContextCache = newProtocolCache(512, 1<<20, 16<<10, protocolCacheIdleTTL)
	catalogWriteLocks    [64]sync.Mutex
)

func catalogWriteLock(key string) *sync.Mutex {
	// Fixed stripes cannot grow with untrusted session IDs. They serialize
	// same-session read/merge/write, without locking readers or other stripes.
	hash := uint32(2166136261)
	for index := range len(key) {
		hash = (hash ^ uint32(key[index])) * 16777619
	}
	return &catalogWriteLocks[hash%uint32(len(catalogWriteLocks))]
}

func (c *protocolCache) removeLocked(element *list.Element) {
	entry := element.Value.(*protocolCacheEntry)
	delete(c.items, entry.key)
	c.bytes -= entry.weight
	c.recent.Remove(element)
}

func (c *protocolCache) expireLocked(now time.Time) {
	for element := c.recent.Back(); element != nil; element = c.recent.Back() {
		if now.Sub(element.Value.(*protocolCacheEntry).used) < c.idleTTL {
			break
		}
		c.removeLocked(element)
	}
}

func (c *protocolCache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.expireLocked(now)
	element := c.items[key]
	if element == nil {
		return nil, false
	}
	entry := element.Value.(*protocolCacheEntry)
	entry.used = now
	c.recent.MoveToFront(element)
	return entry.value, true
}

func (c *protocolCache) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.items[key]; element != nil {
		c.removeLocked(element)
	}
}

// put takes ownership of value. A false result means it was too large; an
// existing stale value is removed rather than retained after a rejected update.
// A non-replacing put preserves any existing value, including an explicit nil.
func (c *protocolCache) put(key string, value any, weight int, replace bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.expireLocked(now)
	element := c.items[key]
	if element != nil && !replace {
		return true
	}
	weight += len(key) + 128
	if element != nil {
		c.removeLocked(element)
	}
	if weight > c.maxEntryBytes || weight > c.maxBytes {
		return false
	}
	for len(c.items) >= c.maxEntries || c.bytes+weight > c.maxBytes {
		c.removeLocked(c.recent.Back())
	}
	entry := &protocolCacheEntry{key: key, value: value, weight: weight, used: now}
	c.items[key] = c.recent.PushFront(entry)
	c.bytes += weight
	return true
}

// Canonicalize once on admission. Number decoding is exact, unlike a generic
// json.Unmarshal into map[string]any, which rounds integer metadata to float64.
func protocolCacheSnapshot(value any) (any, int, bool) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > protocolCacheEntryBytes {
		return nil, 0, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var owned any
	if decoder.Decode(&owned) != nil {
		return nil, 0, false
	}
	return owned, protocolCacheWeight(owned), true
}

func protocolCacheWeight(value any) int {
	switch typed := value.(type) {
	case map[string]any:
		weight := 64
		for key, value := range typed {
			weight += 80 + len(key) + protocolCacheWeight(value)
		}
		return weight
	case []any:
		weight := 24 + 16*len(typed)
		for _, value := range typed {
			weight += protocolCacheWeight(value)
		}
		return weight
	case string:
		return 16 + len(typed)
	case json.Number:
		return 16 + len(typed)
	default:
		return 16
	}
}

// Only canonical cache trees enter this helper; their leaves are immutable.
// Reusing string storage avoids duplicating large descriptions and arguments.
func cloneProtocolCacheValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copy := make(map[string]any, len(typed))
		for key, value := range typed {
			copy[key] = cloneProtocolCacheValue(value)
		}
		return copy
	case []any:
		copy := make([]any, len(typed))
		for index, value := range typed {
			copy[index] = cloneProtocolCacheValue(value)
		}
		return copy
	default:
		return value
	}
}

func storeCatalogSnapshot(key string, tools any, replace bool) {
	owned, weight, ok := protocolCacheSnapshot(tools)
	if !ok || !toolCatalogCache.put(key, owned, weight, replace) {
		// An oversized explicit update must not leave an older catalog callable
		// or let an older in-flight completion resurrect that older catalog.
		toolCatalogCache.put(key, nil, 16, replace)
	}
}
