package resolver

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// CandidateSet is a joint review view: several candidates grouped as one
// pending adoption set. Cross-zone references are resolved against the
// set's actual content — every member's zones overlaid on the pinned
// live base — so a name introduced by one member can be the alias target
// of another. A candidate that resolves inside the set is NOT thereby
// cleared for adoption on its own: the set view is the only thing the
// joint review vouches for.
//
// The set pins the live zone versions (Base) at creation (or last
// recheck), exactly like a single candidate, so one review sees one
// consistent snapshot. When a live zone moves afterwards, StaleSetDeps
// names the affected dependencies; only decisions resting on those are
// invalidated — answers already served keep their recorded basis.
type CandidateSet struct {
	ID      string            `json:"id"`
	Members []string          `json:"members"` // candidate ids, sorted
	Base    map[string]uint64 `json:"base"`    // live zone -> version this review rests on
	cache   *Cache            `json:"-"`       // view-private cache
}

// ZoneAttribution states where one zone of the joint view comes from:
// which member candidate supplies it, or that it is the pinned live
// version, plus the exact version seen by the review.
type ZoneAttribution struct {
	Zone    string `json:"zone"`
	Version uint64 `json:"version"`
	Origin  string `json:"origin"` // "candidate:<id>" or "live"
}

// MissingTarget is an alias inside the set whose target cannot be
// resolved from the set's content.
type MissingTarget struct {
	Owner  string `json:"owner"`  // name holding the CNAME
	Target string `json:"target"` // unresolvable alias target
	Code   string `json:"code"`   // NXDOMAIN or NOZONE
}

// SetReview is the outcome of ReviewSet: the joint check of every member
// against the set's actual content.
type SetReview struct {
	ID      string            `json:"id"`
	Members []string          `json:"members"`
	Basis   map[string]uint64 `json:"basis"` // live zone -> version at review time
	Zones   []ZoneAttribution `json:"zones"` // version attribution per zone
	Loops   [][]string        `json:"loops"` // alias cycles (full paths)
	Missing []MissingTarget   `json:"missing"`
	Stale   []StaleDep        `json:"stale"`
	// Ready is true when nothing is stale, no alias loops and no missing
	// targets were found: the set may be confirmed for adoption.
	Ready bool `json:"ready"`
}

// SetAdoption records that a set went live atomically, with the exact
// basis the joint review rested on.
type SetAdoption struct {
	ID      string            `json:"id"`
	Members []string          `json:"members"`
	Basis   map[string]uint64 `json:"basis"`
	Zones   map[string]uint64 `json:"zones"` // adopted zone -> version
	At      time.Time         `json:"at"`
}

// NewCandidateSet groups existing candidates into one pending adoption
// set. Members must exist and must not carry the same zone twice (two
// members supplying different versions of one zone cannot be reviewed
// jointly — narrow the selection first). The current live zone set is
// pinned as the review base.
func (r *Resolver) NewCandidateSet(id string, memberIDs []string) (*CandidateSet, error) {
	if id == "" {
		return nil, fmt.Errorf("set id must not be empty")
	}
	if len(memberIDs) == 0 {
		return nil, fmt.Errorf("set %q needs at least one member candidate", id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sets[id]; ok {
		return nil, fmt.Errorf("set %q already exists", id)
	}
	owner := make(map[string]string) // zone -> member id carrying it
	members := append([]string(nil), memberIDs...)
	sort.Strings(members)
	for _, m := range members {
		c, ok := r.cands[m]
		if !ok {
			return nil, fmt.Errorf("unknown candidate %q", m)
		}
		for zn := range c.Zones {
			if prev, dup := owner[zn]; dup {
				return nil, fmt.Errorf("zone %q carried by both %q and %q; narrow the set", zn, prev, m)
			}
			owner[zn] = m
		}
	}
	s := &CandidateSet{
		ID:      id,
		Members: members,
		Base:    r.liveVersionsLocked(),
		cache:   NewCache(),
	}
	r.sets[id] = s
	return s, nil
}

// CandidateSet returns the registered set, or nil.
func (r *Resolver) CandidateSet(id string) *CandidateSet {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sets[id]
}

// setView builds the joint snapshot: every member's zones overlaid on
// the pinned live base. References between members (a name one member
// adds, aliased from another member's zone) resolve against this
// combined content — never against the live catalog alone.
func (r *Resolver) setView(s *CandidateSet) zoneView {
	r.mu.RLock()
	overlay := make(map[string]*Zone)
	for _, m := range s.Members {
		if c, ok := r.cands[m]; ok {
			for zn, z := range c.Zones {
				overlay[zn] = z
			}
		}
	}
	r.mu.RUnlock()
	return zoneView{
		get: func(name string) *Zone {
			if z, ok := overlay[name]; ok {
				return z
			}
			if v, ok := s.Base[name]; ok {
				return r.ZoneVersion(name, v)
			}
			return nil
		},
		names: func() []string {
			names := r.histZoneNames()
			seen := make(map[string]bool, len(names))
			for _, n := range names {
				seen[n] = true
			}
			for zn := range overlay {
				if !seen[zn] {
					names = append(names, zn)
				}
			}
			return names
		},
	}
}

// ResolveSet answers a query inside the joint set view, using the set's
// private cache; nothing learned here enters the live cache.
func (r *Resolver) ResolveSet(id, name string) (*Result, error) {
	s := r.CandidateSet(id)
	if s == nil {
		return nil, fmt.Errorf("unknown set %q", id)
	}
	return r.resolve(name, nil, r.setView(s), s.cache, "set:"+id)
}

// StaleSetDeps lists the review's dependencies that moved since the base
// was pinned: only decisions resting on those zones are invalidated by
// the change; everything already answered keeps its recorded basis.
func (r *Resolver) StaleSetDeps(id string) ([]StaleDep, error) {
	s := r.CandidateSet(id)
	if s == nil {
		return nil, fmt.Errorf("unknown set %q", id)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var stale []StaleDep
	for zone, baseV := range s.Base {
		store, ok := r.zones[zone]
		if !ok {
			stale = append(stale, StaleDep{Zone: zone, BaseVersion: baseV, Removed: true})
			continue
		}
		if cur := store.Current().Version; cur != baseV {
			stale = append(stale, StaleDep{Zone: zone, BaseVersion: baseV, CurrentVersion: cur})
		}
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].Zone < stale[j].Zone })
	return stale, nil
}

// RecheckSet re-pins the review base to the current live zone set and
// drops the set's private cache (those answers rested on the old base).
// Returns the stale dependencies that were found. Narrowing the set is
// done by abandoning it and grouping a smaller selection.
func (r *Resolver) RecheckSet(id string) ([]StaleDep, error) {
	stale, err := r.StaleSetDeps(id)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	s := r.sets[id]
	s.Base = r.liveVersionsLocked()
	s.cache = NewCache()
	r.mu.Unlock()
	return stale, nil
}

// ReviewSet jointly checks the set against its actual content: it
// resolves every alias introduced by any member inside the joint view
// and reports circular aliases (full loop paths), missing targets, the
// version attribution of every zone in the view, and any stale
// dependencies. Nothing is mutated; the set's cache is untouched.
func (r *Resolver) ReviewSet(id string) (*SetReview, error) {
	s := r.CandidateSet(id)
	if s == nil {
		return nil, fmt.Errorf("unknown set %q", id)
	}
	stale, err := r.StaleSetDeps(id)
	if err != nil {
		return nil, err
	}
	view := r.setView(s)
	rev := &SetReview{
		ID:      s.ID,
		Members: append([]string(nil), s.Members...),
		Basis:   make(map[string]uint64, len(s.Base)),
		Stale:   stale,
	}
	for k, v := range s.Base {
		rev.Basis[k] = v
	}

	// Version attribution: every zone the joint view can serve, tagged
	// with the member that supplies it or marked as the pinned live one.
	r.mu.RLock()
	owner := make(map[string]string)
	for _, m := range s.Members {
		if c, ok := r.cands[m]; ok {
			for zn := range c.Zones {
				owner[zn] = m
			}
		}
	}
	r.mu.RUnlock()
	for _, zn := range view.names() {
		z := view.get(zn)
		if z == nil {
			continue
		}
		attr := ZoneAttribution{Zone: zn, Version: z.Version, Origin: "live"}
		if m, ok := owner[zn]; ok {
			attr.Origin = "candidate:" + m
		}
		rev.Zones = append(rev.Zones, attr)
	}
	sort.Slice(rev.Zones, func(i, j int) bool { return rev.Zones[i].Zone < rev.Zones[j].Zone })

	// Alias checks: every CNAME carried by a member zone is resolved
	// against the set's combined content. Loops and unresolvable targets
	// are reported; the live cache is bypassed and nothing is cached.
	seenLoop := make(map[string]bool)
	seenMissing := make(map[string]bool)
	for _, zn := range view.names() {
		if _, ok := owner[zn]; !ok {
			continue // only aliases introduced by members are reviewed
		}
		z := view.get(zn)
		for _, recs := range z.Records {
			for _, rec := range recs {
				if rec.Type != TypeCNAME {
					continue
				}
				_, rerr := r.resolve(rec.Name, nil, view, nil, "set-review:"+id)
				re, ok := rerr.(*ResolveError)
				if !ok {
					continue
				}
				switch re.Code {
				case ErrLoop:
					key := loopKey(re.Path)
					if !seenLoop[key] {
						seenLoop[key] = true
						rev.Loops = append(rev.Loops, re.Path)
					}
				case ErrNXDOMAIN, ErrNoZone:
					target := NormalizeName(rec.Value)
					key := rec.Name + "->" + target
					if !seenMissing[key] {
						seenMissing[key] = true
						rev.Missing = append(rev.Missing, MissingTarget{
							Owner: rec.Name, Target: target, Code: re.Code,
						})
					}
				}
			}
		}
	}
	sort.Slice(rev.Loops, func(i, j int) bool {
		return strings.Join(rev.Loops[i], ">") < strings.Join(rev.Loops[j], ">")
	})
	sort.Slice(rev.Missing, func(i, j int) bool { return rev.Missing[i].Owner < rev.Missing[j].Owner })
	rev.Ready = len(rev.Stale) == 0 && len(rev.Loops) == 0 && len(rev.Missing) == 0
	return rev, nil
}

// loopKey normalizes a cycle path so the same loop found from different
// entry points is reported once.
func loopKey(path []string) string {
	cycle := append([]string(nil), path[:len(path)-1]...) // drop repeated tail
	best := 0
	for i := range cycle {
		if cycle[i] < cycle[best] {
			best = i
		}
	}
	rot := append(append([]string(nil), cycle[best:]...), cycle[:best]...)
	return strings.Join(rot, ">")
}

// AdoptSet confirms the joint review and publishes every member's zones
// as one atomic batch. Adoption is refused while dependencies are stale
// (recheck first) or while the review still reports loops or missing
// targets. On any failure nothing is published: the live zones keep
// serving their previous versions, never a half-replaced mix. On success
// the member candidates are consumed and the exact basis is recorded.
func (r *Resolver) AdoptSet(id string) (*SetAdoption, error) {
	rev, err := r.ReviewSet(id)
	if err != nil {
		return nil, err
	}
	if len(rev.Stale) > 0 {
		var names []string
		for _, d := range rev.Stale {
			names = append(names, d.Zone)
		}
		return nil, fmt.Errorf("set %q has stale dependencies (%s); recheck before adopting",
			id, strings.Join(names, ", "))
	}
	if !rev.Ready {
		return nil, fmt.Errorf("set %q review not clean: %d alias loops, %d missing targets",
			id, len(rev.Loops), len(rev.Missing))
	}
	r.mu.RLock()
	s := r.sets[id]
	var zones []*Zone
	adopted := make(map[string]uint64)
	for _, m := range s.Members {
		c := r.cands[m]
		for zn, z := range c.Zones {
			zones = append(zones, z)
			adopted[zn] = z.Version
		}
	}
	r.mu.RUnlock()
	// ApplyBatch validates everything before publishing anything, so a
	// failure leaves the live zones exactly as they were.
	if err := r.ApplyBatch(zones); err != nil {
		return nil, err
	}
	basis := make(map[string]uint64, len(s.Base))
	for k, v := range s.Base {
		basis[k] = v
	}
	ad := SetAdoption{
		ID:      id,
		Members: append([]string(nil), s.Members...),
		Basis:   basis,
		Zones:   adopted,
		At:      time.Now(),
	}
	r.mu.Lock()
	r.setAdopted = append(r.setAdopted, ad)
	for _, m := range s.Members {
		delete(r.cands, m)
	}
	delete(r.sets, id)
	r.mu.Unlock()
	return &ad, nil
}

// AbandonSet dissolves the pending set without adopting it. The member
// candidates themselves are NOT deleted: they remain registered and can
// be regrouped into a narrower set or reviewed on their own.
func (r *Resolver) AbandonSet(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sets[id]; !ok {
		return fmt.Errorf("unknown set %q", id)
	}
	delete(r.sets, id)
	return nil
}

// SetAdoptions returns the log of adopted sets and their basis.
func (r *Resolver) SetAdoptions() []SetAdoption {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]SetAdoption(nil), r.setAdopted...)
}
