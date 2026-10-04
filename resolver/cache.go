package resolver

import (
	"sort"
	"sync"
	"time"
)

// CacheEntry is one cached outcome. Positive and negative results are
// distinct entry kinds with independent lifetimes. View tags which query
// view the entry belongs to: "" is the live (formal) view, any other
// value namespaces the entry to an isolated candidate view, so candidate
// outcomes — including negative ones — can never leak into live queries.
type CacheEntry struct {
	Name     string     `json:"name"`
	Type     RecordType `json:"type"`
	Records  []Record   `json:"records,omitempty"`
	Negative bool       `json:"negative"` // true: cached NXDOMAIN/NODATA
	Source   Source     `json:"source"`
	View     string     `json:"view,omitempty"`
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

func cacheKey(view, name string, rt RecordType) string {
	if view == "" {
		return string(rt) + " " + NormalizeName(name)
	}
	return view + "|" + string(rt) + " " + NormalizeName(name)
}

// Get returns a live-view entry, or nil if absent or expired.
func (c *Cache) Get(name string, rt RecordType) *CacheEntry {
	return c.GetView("", name, rt)
}

// GetView returns an entry of the given view, or nil if absent or
// expired. Expired entries are removed lazily so expiry and negativity
// stay separate concerns.
func (c *Cache) GetView(view, name string, rt RecordType) *CacheEntry {
	c.mu.RLock()
	e := c.entries[cacheKey(view, name, rt)]
	c.mu.RUnlock()
	if e == nil {
		return nil
	}
	if e.Expired(c.now()) {
		c.mu.Lock()
		// Re-check: a fresher update may have landed meanwhile.
		if cur := c.entries[cacheKey(view, name, rt)]; cur == e {
			delete(c.entries, cacheKey(view, name, rt))
		}
		c.mu.Unlock()
		return nil
	}
	return e
}

// Put inserts or replaces an entry in the live view.
func (c *Cache) Put(e *CacheEntry) bool {
	return c.PutView("", e)
}

// PutView inserts or replaces an entry in the given view. A late update
// (older source version than the stored entry from the same zone) is
// rejected, so stale data arriving out of order cannot clobber fresher
// cache content.
func (c *Cache) PutView(view string, e *CacheEntry) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.View = view
	k := cacheKey(view, e.Name, e.Type)
	if old, ok := c.entries[k]; ok && old.Source.Zone == e.Source.Zone && e.Source.Version < old.Source.Version {
		return false
	}
	cp := *e
	c.entries[k] = &cp
	return true
}

// staleKeys lists keys of entries whose source zone moved past their
// recorded version: the answer (positive or negative) was valid for the
// zone version it came from, and must not be served once the zone has
// changed. With allVersions=false, entries from version >= fromVersion
// are still valid; with allVersions=true every entry from the zone is
// stale (zone removed).
func (c *Cache) staleKeys(zone string, fromVersion uint64, allVersions bool) []string {
	var keys []string
	for k, e := range c.entries {
		if e.View != "" {
			continue // candidate-view entries are managed per candidate
		}
		if e.Source.Zone != zone {
			continue
		}
		if allVersions || e.Source.Version < fromVersion {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// InvalidateView drops every entry belonging to a non-live view (used
// when a candidate is rebased, adopted, or abandoned).
func (c *Cache) InvalidateView(view string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var keys []string
	for k, e := range c.entries {
		if e.View == view {
			keys = append(keys, k)
			delete(c.entries, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// StaleKeys reports which live entries would be invalidated if zone
// advanced to fromVersion (used by batch preview; does not mutate).
func (c *Cache) StaleKeys(zone string, fromVersion uint64) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.staleKeys(zone, fromVersion, false)
}

// InvalidateZone drops every entry sourced from zone at a version older
// than fromVersion. Called when a zone version is published so superseded
// answers — including negative ones — stop being returned immediately,
// instead of lingering until their TTL expires. Returns the removed keys.
func (c *Cache) InvalidateZone(zone string, fromVersion uint64) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := c.staleKeys(zone, fromVersion, false)
	for _, k := range keys {
		delete(c.entries, k)
	}
	return keys
}

// InvalidateZoneAll drops every entry sourced from zone (zone removed).
func (c *Cache) InvalidateZoneAll(zone string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := c.staleKeys(zone, 0, true)
	for _, k := range keys {
		delete(c.entries, k)
	}
	return keys
}

// Snapshot returns all live entries of the live view (for persistence).
// Candidate-view entries are ephemeral and never persisted.
func (c *Cache) Snapshot() []CacheEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := c.now()
	out := make([]CacheEntry, 0, len(c.entries))
	for _, e := range c.entries {
		if e.View != "" {
			continue
		}
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
			c.entries[cacheKey(e.View, e.Name, e.Type)] = &cp
		}
	}
}
