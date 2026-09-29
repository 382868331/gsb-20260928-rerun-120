package resolver

import (
	"encoding/json"
	"os"
)

// Snapshot is the durable state of the resolver: zone versions plus the
// live cache. Written atomically (temp file + rename).
type Snapshot struct {
	Zones []Zone        `json:"zones"`
	Cache []CacheEntry  `json:"cache"`
}

// SaveSnapshot persists zones and live cache entries to path.
func (r *Resolver) SaveSnapshot(path string) error {
	snap := Snapshot{Cache: r.cache.Snapshot()}
	for _, name := range r.ZoneNames() {
		snap.Zones = append(snap.Zones, *r.ZoneSnapshot(name))
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadSnapshot rebuilds a resolver from a snapshot file. Zones are
// registered at their saved versions; still-valid cache entries are
// restored so a restart serves identical answers.
func LoadSnapshot(path string) (*Resolver, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	cache := NewCache()
	r := NewResolver(cache)
	for i := range snap.Zones {
		z := snap.Zones[i]
		r.AddZone(&z)
	}
	cache.Restore(snap.Cache)
	return r, nil
}

