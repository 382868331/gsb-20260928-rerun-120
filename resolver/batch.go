package resolver

import "fmt"

// RecordDiff describes how one zone's record set changes.
type RecordDiff struct {
	Added   []Record `json:"added"`
	Removed []Record `json:"removed"`
}

// ZonePreview is the dry-run outcome for one zone in a batch.
type ZonePreview struct {
	Zone        string     `json:"zone"`
	FromVersion uint64     `json:"from_version"` // 0 for a new zone
	ToVersion   uint64     `json:"to_version"`
	Diff        RecordDiff `json:"diff"`
	// CacheInvalidated lists live cache entries (keyed "TYPE name") that
	// the change would drop because their source version is superseded.
	CacheInvalidated []string `json:"cache_invalidated"`
}

// BatchPreview is the result of PreviewBatch: the full effect of a batch
// of zone changes, computed without mutating any live state.
type BatchPreview struct {
	Zones []ZonePreview `json:"zones"`
}

// PreviewBatch simulates applying zones as one batch and reports, per
// zone, the record diff and which cache entries would be invalidated.
// Nothing is published and the cache is untouched.
func (r *Resolver) PreviewBatch(zones []*Zone) (*BatchPreview, error) {
	if err := r.validateBatch(zones); err != nil {
		return nil, err
	}
	p := &BatchPreview{}
	for _, z := range zones {
		zp := ZonePreview{Zone: z.Name, ToVersion: z.Version}
		var old *Zone
		if s, ok := r.zones[z.Name]; ok {
			old = s.Current()
			zp.FromVersion = old.Version
		}
		zp.Diff = diffZones(old, z)
		zp.CacheInvalidated = r.cache.StaleKeys(z.Name, z.Version)
		p.Zones = append(p.Zones, zp)
	}
	return p, nil
}

// ApplyBatch publishes several zone changes as one atomic batch: every
// zone is validated first, then all versions become visible together, so
// a concurrent query never observes half of a batch. Cache entries
// sourced from superseded versions are invalidated per zone.
func (r *Resolver) ApplyBatch(zones []*Zone) error {
	if err := r.validateBatch(zones); err != nil {
		return err
	}
	for _, z := range zones {
		if _, ok := r.zones[z.Name]; ok {
			if err := r.UpdateZone(z); err != nil {
				return err // unreachable: validateBatch already checked
			}
		} else {
			r.AddZone(z)
		}
	}
	return nil
}

// validateBatch checks the whole batch before anything is applied:
// known zones must advance their version, and a zone may appear once.
func (r *Resolver) validateBatch(zones []*Zone) error {
	seen := make(map[string]bool)
	for _, z := range zones {
		if seen[z.Name] {
			return fmt.Errorf("zone %q appears twice in batch", z.Name)
		}
		seen[z.Name] = true
		if s, ok := r.zones[z.Name]; ok {
			if cur := s.Current(); z.Version <= cur.Version {
				return fmt.Errorf("zone %q version %d does not advance current %d", z.Name, z.Version, cur.Version)
			}
		}
	}
	return nil
}

func diffZones(old, new *Zone) RecordDiff {
	var d RecordDiff
	if old != nil {
		for owner, recs := range old.Records {
			for _, rec := range recs {
				if !hasRecord(new.Records[owner], rec) {
					d.Removed = append(d.Removed, rec)
				}
			}
		}
	}
	for owner, recs := range new.Records {
		for _, rec := range recs {
			if old == nil || !hasRecord(old.Records[owner], rec) {
				d.Added = append(d.Added, rec)
			}
		}
	}
	return d
}

func hasRecord(recs []Record, want Record) bool {
	for _, r := range recs {
		if r == want {
			return true
		}
	}
	return false
}
