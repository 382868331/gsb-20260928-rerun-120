package resolver

import (
	"encoding/json"
	"os"
	"sort"
)

// Snapshot is the durable state of the resolver: current zone versions,
// the full version history (so historical answers stay explainable after
// a restart), and the live cache. Written atomically (temp file + rename).
type Snapshot struct {
	Zones      []Zone       `json:"zones"`
	History    []Zone       `json:"history,omitempty"`
	Cache      []CacheEntry `json:"cache"`
	Candidates []Candidate  `json:"candidates,omitempty"`
}

// SaveSnapshot persists zones and live cache entries to path.
func (r *Resolver) SaveSnapshot(path string) error {
	snap := Snapshot{Cache: r.cache.Snapshot()}
	for _, name := range r.ZoneNames() {
		snap.Zones = append(snap.Zones, *r.ZoneSnapshot(name))
	}
	r.mu.RLock()
	for _, versions := range r.hist {
		for _, z := range versions {
			snap.History = append(snap.History, *z)
		}
	}
	for _, c := range r.candidates {
		snap.Candidates = append(snap.Candidates, *c)
	}
	r.mu.RUnlock()
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
	// Restore version history so ResolveAt can explain past answers.
	// Snapshots written before history existed fall back to the current
	// versions already registered by AddZone.
	if len(snap.History) > 0 {
		r.mu.Lock()
		r.hist = make(map[string][]*Zone)
		for i := range snap.History {
			z := snap.History[i]
			r.hist[z.Name] = append(r.hist[z.Name], &z)
		}
		for name := range r.hist {
			sort.Slice(r.hist[name], func(i, j int) bool {
				return r.hist[name][i].Version < r.hist[name][j].Version
			})
		}
		r.mu.Unlock()
	}
	cache.Restore(snap.Cache)
	for i := range snap.Candidates {
		c := snap.Candidates[i]
		r.candidates[c.ID] = &c
	}
	return r, nil
}
