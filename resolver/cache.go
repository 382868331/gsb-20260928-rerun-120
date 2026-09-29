package resolver

import (
	"sync"
	"time"
)

type cacheEntry struct {
	result    *Result
	negative  bool
	expiresAt int64
	// sourceVersion is the zone generation that produced the entry. An entry
	// delivered late (after the zones advanced) must not overwrite fresher
	// state, and entries from an old generation are discarded on read.
	sourceVersion uint64
}

func cacheKey(name string, k Kind) string { return string(k) + " " + name }

// Cache is a TTL cache. Positive and negative results live in separate maps
// so expiry and invalidation policy can differ: negative answers carry a
// dedicated negative TTL and are tracked independently.
type Cache struct {
	mu       sync.RWMutex
	positive map[string]*cacheEntry
	negative map[string]*cacheEntry
	nowFunc  func() int64
	curGen   func() uint64
}

// NewCache builds a cache. now may be nil to use wall-clock seconds.
func NewCache(now func() int64) *Cache {
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	return &Cache{
		positive: map[string]*cacheEntry{},
		negative: map[string]*cacheEntry{},
		nowFunc:  now,
		curGen:   func() uint64 { return 0 },
	}
}

func (c *Cache) bindGeneration(g func() uint64) {
	c.curGen = g
}

// lookup returns a live, current-generation entry.
func (c *Cache) lookup(name string, k Kind) (*Result, bool) {
	key := cacheKey(NormalizeName(name), k)
	c.mu.RLock()
	e := c.positive[key]
	if e == nil {
		e = c.negative[key]
	}
	c.mu.RUnlock()
	if e == nil {
		return nil, false
	}
	// Source check: a result computed against an older generation can never
	// be served once the zones advanced, and cannot overwrite newer entries.
	if e.sourceVersion != c.curGen() {
		c.evict(key, e.negative)
		return nil, false
	}
	if c.nowFunc() >= e.expiresAt {
		c.evict(key, e.negative)
		return nil, false
	}
	return e.result, true
}

func (c *Cache) evict(key string, negative bool) {
	c.mu.Lock()
	if negative {
		delete(c.negative, key)
	} else {
		delete(c.positive, key)
	}
	c.mu.Unlock()
}

// store caches a result produced from the given generation. A late entry
// from an older generation is rejected rather than clobbering newer data.
func (c *Cache) store(genVersion uint64, res *Result, now int64) {
	if res == nil {
		return
	}
	switch res.Status {
	case Success, NXDomain, NoData:
	default:
		// Loop / error / no-zone results are never cached.
		return
	}
	ttl := res.TTL
	if ttl <= 0 {
		ttl = 1
	}
	e := &cacheEntry{
		result:        res,
		negative:      res.Negative(),
		expiresAt:     now + int64(ttl),
		sourceVersion: genVersion,
	}
	key := cacheKey(res.Question, res.QKind)
	c.mu.Lock()
	defer c.mu.Unlock()
	// Reject stale, late-arriving writes; newer generation always wins.
	if existing := c.positive[key]; existing != nil && e.sourceVersion < existing.sourceVersion {
		return
	}
	if existing := c.negative[key]; existing != nil && e.sourceVersion < existing.sourceVersion {
		return
	}
	// Positive and negative maps are separate; clear the opposite class so a
	// name cannot be simultaneously "found" and "not found".
	if e.negative {
		delete(c.positive, key)
		c.negative[key] = e
	} else {
		delete(c.negative, key)
		c.positive[key] = e
	}
}

// Len reports positive and negative live entry counts (after expiry scan).
func (c *Cache) Len() (pos, neg int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.nowFunc()
	for k, e := range c.positive {
		if now >= e.expiresAt || e.sourceVersion != c.curGen() {
			delete(c.positive, k)
			continue
		}
		pos++
	}
	for k, e := range c.negative {
		if now >= e.expiresAt || e.sourceVersion != c.curGen() {
			delete(c.negative, k)
			continue
		}
		neg++
	}
	return pos, neg
}

// Flush removes all cached entries (used after zone updates / on restart).
func (c *Cache) Flush() {
	c.mu.Lock()
	c.positive = map[string]*cacheEntry{}
	c.negative = map[string]*cacheEntry{}
	c.mu.Unlock()
}
