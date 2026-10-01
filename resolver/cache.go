package resolver

import (
	"sync"
	"time"
)

// CacheEntry is one cached outcome. Positive and negative results are
// distinct entry kinds with independent lifetimes.
type CacheEntry struct {
	Name     string     `json:"name"`
	Type     RecordType `json:"type"`
	Records  []Record   `json:"records,omitempty"`
	Negative bool       `json:"negative"` // true: cached NXDOMAIN/NODATA
	Source   Source     `json:"source"`
	Expires  time.Time  `json:"expires"`
}

// Expired reports whether the entry is past its TTL at t.
func (e *CacheEntry) Expired(t time.Time) bool { return !t.Before(e.Expires) }

// Cache is a TTL cache. Positive answers use record TTLs; negative answers
// use the zone's NegativeTTL. The two never share an entry.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]*CacheEntry
	now     func() time.Time
}

func NewCache() *Cache {
	return &Cache{entries: make(map[string]*CacheEntry), now: time.Now}
}

// SetClock overrides the clock (tests).
func (c *Cache) SetClock(now func() time.Time) { c.now = now }

func cacheKey(name string, rt RecordType) string { return string(rt) + " " + NormalizeName(name) }

// Get returns a live entry, or nil if absent or expired. Expired entries
// are removed lazily so expiry and negativity stay separate concerns.
func (c *Cache) Get(name string, rt RecordType) *CacheEntry {
	c.mu.RLock()
	e := c.entries[cacheKey(name, rt)]
	c.mu.RUnlock()
	if e == nil {
		return nil
	}
	if e.Expired(c.now()) {
		c.mu.Lock()
		// Re-check: a fresher update may have landed meanwhile.
		if cur := c.entries[cacheKey(name, rt)]; cur == e {
			delete(c.entries, cacheKey(name, rt))
		}
		c.mu.Unlock()
		return nil
	}
	return e
}

// Put inserts or replaces an entry. A late update (older source version
// than the stored entry from the same zone) is rejected, so stale data
// arriving out of order cannot clobber fresher cache content.
func (c *Cache) Put(e *CacheEntry) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := cacheKey(e.Name, e.Type)
	if old, ok := c.entries[k]; ok && old.Source.Zone == e.Source.Zone && e.Source.Version < old.Source.Version {
		return false
	}
	cp := *e
	c.entries[k] = &cp
	return true
}

// Snapshot returns all live entries (for persistence).
func (c *Cache) Snapshot() []CacheEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := c.now()
	out := make([]CacheEntry, 0, len(c.entries))
	for _, e := range c.entries {
		if !e.Expired(now) {
			out = append(out, *e)
		}
	}
	return out
}

// Restore loads persisted entries, dropping already-expired ones.
func (c *Cache) Restore(entries []CacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for i := range entries {
		e := entries[i]
		if !e.Expired(now) {
			cp := e
			c.entries[cacheKey(e.Name, e.Type)] = &cp
		}
	}
}

// InvalidateWhere removes every entry matching pred. Used when a new view
// is published: entries sourced from changed or removed zones (including
// negative results) must not outlive the rules that produced them.
func (c *Cache) InvalidateWhere(pred func(*CacheEntry) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if pred(e) {
			delete(c.entries, k)
		}
	}
}

// Entries returns a snapshot of all entries, live or expired (for
// dry-run inspection).
func (c *Cache) Entries() []CacheEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]CacheEntry, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, *e)
	}
	return out
}
