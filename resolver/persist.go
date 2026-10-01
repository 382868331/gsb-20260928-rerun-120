package resolver

import (
	"encoding/json"
	"os"
)

// Snapshot is the durable state of the resolver: zone versions plus the
// live cache. Written atomically (temp file + rename).
type Snapshot struct {
	ViewID  uint64       `json:"view_id"`
	Zones   []Zone       `json:"zones"`
	Cache   []CacheEntry `json:"cache"`
	History []ViewRecord `json:"history,omitempty"`
}

// SaveSnapshot persists zones and live cache entries to path.
func (r *Resolver) SaveSnapshot(path string) error {
	snap := Snapshot{ViewID: r.CurrentViewID(), Cache: r.cache.Snapshot(), History: r.History()}
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
	zones := make(map[string]*Zone, len(snap.Zones))
	for i := range snap.Zones {
		z := snap.Zones[i]
		zones[z.Name] = &z
	}
	// Restore the exact saved view (same ID) without synthesizing history;
	// the persisted history already explains which zone set answered what.
	v := &zoneView{ID: snap.ViewID, Zones: zones}
	r.view.Store(v)
	r.views[v.ID] = v
	r.history = append(r.history, snap.History...)
	cache.Restore(snap.Cache)
	return r, nil
}
