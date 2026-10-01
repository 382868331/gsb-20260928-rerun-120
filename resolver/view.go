package resolver

import (
	"fmt"
	"sort"
	"time"
)

// zoneView is an immutable snapshot of every zone the resolver serves.
// A single query reads exactly one view, so a batch change that touches a
// parent delegation and child zone content together is observed as either
// the complete old view or the complete new view — never a splice.
type zoneView struct {
	ID    uint64
	Zones map[string]*Zone
}

func (v *zoneView) zone(name string) *Zone { return v.Zones[NormalizeName(name)] }

// Batch describes a multi-zone change applied (or previewed) atomically.
// Upsert adds or replaces zones; Remove drops zones entirely (e.g. a
// revoked delegation). Rollback is just an upsert of older zone content.
type Batch struct {
	Upsert []*Zone
	Remove []string
	Note   string // human-readable reason, kept in the view history
}

// Zone change kinds recorded in previews and history.
const (
	ChangeAdded   = "added"
	ChangeUpdated = "updated"
	ChangeRemoved = "removed"
)

// ZoneChange describes one zone-level diff between two views.
type ZoneChange struct {
	Zone       string `json:"zone"`
	Kind       string `json:"kind"`
	OldVersion uint64 `json:"old_version,omitempty"`
	NewVersion uint64 `json:"new_version,omitempty"`
}

// Preview is the dry-run outcome of a Batch: the zone diffs it would make
// and the cache entries it would invalidate, without changing live state.
type Preview struct {
	Changes    []ZoneChange
	Invalidate []CacheEntry
}

// ViewRecord is a history entry explaining which set of zone versions
// formed one view. Persisted in snapshots, so after a restart a past
// answer can still be attributed to the zone set that produced it.
type ViewRecord struct {
	ID      uint64            `json:"id"`
	Time    time.Time         `json:"time"`
	Note    string            `json:"note"`
	Zones   map[string]uint64 `json:"zones"` // zone name -> zone version
	Changes []ZoneChange      `json:"changes,omitempty"`
}

func (r ViewRecord) String() string {
	names := make([]string, 0, len(r.Zones))
	for z := range r.Zones {
		names = append(names, z)
	}
	sort.Strings(names)
	s := ""
	for i, z := range names {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf("%s@v%d", z, r.Zones[z])
	}
	return fmt.Sprintf("view %d (%s) zones=%s", r.ID, r.Note, s)
}

// diffZones computes the zone-level changes from old to new.
func diffZones(old, new map[string]*Zone) []ZoneChange {
	var out []ZoneChange
	for name, nz := range new {
		if oz, ok := old[name]; ok {
			if oz.Version != nz.Version {
				out = append(out, ZoneChange{Zone: name, Kind: ChangeUpdated, OldVersion: oz.Version, NewVersion: nz.Version})
			}
		} else {
			out = append(out, ZoneChange{Zone: name, Kind: ChangeAdded, NewVersion: nz.Version})
		}
	}
	for name, oz := range old {
		if _, ok := new[name]; !ok {
			out = append(out, ZoneChange{Zone: name, Kind: ChangeRemoved, OldVersion: oz.Version})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Zone < out[j].Zone })
	return out
}
