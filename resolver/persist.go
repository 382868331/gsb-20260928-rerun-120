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
	Zones      []Zone              `json:"zones"`
	History    []Zone              `json:"history,omitempty"`
	Cache      []CacheEntry        `json:"cache"`
	Candidates []CandidateSnapshot `json:"candidates,omitempty"`
	Adopted    []Adoption          `json:"adopted,omitempty"`
	Abandoned  []Abandonment       `json:"abandoned,omitempty"`
}

// CandidateSnapshot is the durable form of a candidate view. The
// candidate's private cache is not persisted: after a restart the view
// re-derives its answers from the same pinned zone set.
type CandidateSnapshot struct {
	ID    string            `json:"id"`
	Zones []Zone            `json:"zones"`
	Base  map[string]uint64 `json:"base"`
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
	for _, c := range r.cands {
		cs := CandidateSnapshot{ID: c.ID, Base: c.Base}
		for _, z := range c.Zones {
			cs.Zones = append(cs.Zones, *z)
		}
		snap.Candidates = append(snap.Candidates, cs)
	}
	snap.Adopted = r.adopted
	snap.Abandoned = r.abandoned
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
		cs := snap.Candidates[i]
		c := &Candidate{
			ID:    cs.ID,
			Zones: make(map[string]*Zone),
			Base:  cs.Base,
			cache: NewCache(),
		}
		for j := range cs.Zones {
			z := cs.Zones[j]
			c.Zones[z.Name] = &z
		}
		r.cands[cs.ID] = c
	}
	r.adopted = snap.Adopted
	r.abandoned = snap.Abandoned
	return r, nil
}
