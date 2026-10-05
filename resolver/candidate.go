package resolver

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Candidate is an isolated trial view: a set of zone versions a maintainer
// wants to try before officially changing the live zones. The same query
// can be run against the live view and any number of candidates; each
// candidate has its own cache, so nothing it learns — in particular
// negative answers — ever leaks into the live cache.
//
// The candidate pins the live zone versions (Base) captured at creation
// (or last recheck). Every query inside the candidate is answered from
// exactly one consistent snapshot — candidate zones overlaid on the
// pinned base — so a parent/child delegation chain is never spliced from
// two different points in time.
type Candidate struct {
	ID    string            `json:"id"`
	Zones map[string]*Zone  `json:"zones"` // trial zone versions, by zone name
	Base  map[string]uint64 `json:"base"`  // live zone -> version this view rests on
	cache *Cache            `json:"-"`     // view-private cache
}

// StaleDep describes one live zone that moved after the candidate's base
// was captured: dependencies of the trial view that are now outdated.
type StaleDep struct {
	Zone           string `json:"zone"`
	BaseVersion    uint64 `json:"base_version"`
	CurrentVersion uint64 `json:"current_version"` // 0 when Removed
	Removed        bool   `json:"removed"`
}

// Adoption records that a candidate went live, together with the exact
// zone set (Basis) its trial answers were validated against.
type Adoption struct {
	ID    string            `json:"id"`
	Basis map[string]uint64 `json:"basis"` // live zone -> version the candidate rested on
	Zones map[string]uint64 `json:"zones"` // adopted zone -> version
	At    time.Time         `json:"at"`
}

// Abandonment records a dropped candidate and the diff basis that was
// observed, so the trial remains explainable afterwards.
type Abandonment struct {
	ID    string                `json:"id"`
	Basis map[string]uint64     `json:"basis"`
	Diffs map[string]RecordDiff `json:"diffs"` // candidate zone -> diff vs basis
	At    time.Time             `json:"at"`
}

// NewCandidate registers an isolated trial view under id. zones are the
// candidate zone versions (they must advance the current live versions,
// like a batch). The current live zone set is captured as the base.
func (r *Resolver) NewCandidate(id string, zones []*Zone) (*Candidate, error) {
	if id == "" {
		return nil, fmt.Errorf("candidate id must not be empty")
	}
	if err := r.validateBatch(zones); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.cands[id]; ok {
		return nil, fmt.Errorf("candidate %q already exists", id)
	}
	c := &Candidate{
		ID:    id,
		Zones: make(map[string]*Zone),
		Base:  r.liveVersionsLocked(),
		cache: NewCache(),
	}
	for _, z := range zones {
		c.Zones[z.Name] = z
	}
	r.cands[id] = c
	return c, nil
}

// liveVersionsLocked snapshots the current live zone versions.
// Callers must hold r.mu.
func (r *Resolver) liveVersionsLocked() map[string]uint64 {
	base := make(map[string]uint64, len(r.zones))
	for name, s := range r.zones {
		base[name] = s.Current().Version
	}
	return base
}

// Candidate returns the registered candidate, or nil.
func (r *Resolver) Candidate(id string) *Candidate {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cands[id]
}

// view builds the candidate's consistent snapshot: candidate zones where
// defined, otherwise the base-pinned historical version of each live
// zone. Live zones that changed since the base was captured are still
// seen at their base version here — never spliced with the candidate
// zones from a different point in time.
func (r *Resolver) candidateView(c *Candidate) zoneView {
	return r.overlayView(c.Zones, c.Base)
}

// overlayView builds a consistent snapshot: the given overlay zones
// where defined, otherwise the base-pinned historical version of each
// live zone. Live zones that changed since the base was captured are
// still seen at their base version here — never spliced with the
// overlay zones from a different point in time.
func (r *Resolver) overlayView(overlay map[string]*Zone, base map[string]uint64) zoneView {
	return zoneView{
		get: func(name string) *Zone {
			if z, ok := overlay[name]; ok {
				return z
			}
			if v, ok := base[name]; ok {
				return r.ZoneVersion(name, v)
			}
			return nil
		},
		// The candidate's zone set is its own: zones it carries plus every
		// zone the pinned base knows about. A zone that exists only in the
		// candidate (not yet in the live catalog) is authoritative inside
		// this view, so queries for it resolve here instead of NOZONE.
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

// ResolveCandidate answers a query inside the candidate view, using the
// candidate's private cache. Negative (and positive) outcomes are cached
// only within this view and tagged with its id.
func (r *Resolver) ResolveCandidate(id, name string) (*Result, error) {
	c := r.Candidate(id)
	if c == nil {
		return nil, fmt.Errorf("unknown candidate %q", id)
	}
	return r.resolve(name, nil, r.candidateView(c), c.cache, "candidate:"+id)
}

// StaleDeps lists the candidate's dependencies that are outdated: live
// zones whose version moved (or that were removed) since the base was
// captured. A non-empty result means trial answers may no longer reflect
// what adopting the candidate would do; call RecheckCandidate.
func (r *Resolver) StaleDeps(id string) ([]StaleDep, error) {
	c := r.Candidate(id)
	if c == nil {
		return nil, fmt.Errorf("unknown candidate %q", id)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var stale []StaleDep
	for zone, baseV := range c.Base {
		s, ok := r.zones[zone]
		if !ok {
			stale = append(stale, StaleDep{Zone: zone, BaseVersion: baseV, Removed: true})
			continue
		}
		if cur := s.Current().Version; cur != baseV {
			stale = append(stale, StaleDep{Zone: zone, BaseVersion: baseV, CurrentVersion: cur})
		}
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].Zone < stale[j].Zone })
	return stale, nil
}

// RecheckCandidate re-bases the candidate on the current live zone set
// after dependencies went stale, and drops everything the candidate
// cached so far (those answers rested on the old base). Returns the
// stale dependencies that were found, so the maintainer sees what moved.
func (r *Resolver) RecheckCandidate(id string) ([]StaleDep, error) {
	stale, err := r.StaleDeps(id)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	c := r.cands[id]
	c.Base = r.liveVersionsLocked()
	c.cache = NewCache()
	r.mu.Unlock()
	return stale, nil
}

// AdoptCandidate publishes the candidate's zones as one atomic batch and
// records the exact zone set (Basis) the trial was based on. Adoption is
// refused while dependencies are stale — recheck first, so the recorded
// basis always matches what was actually observed.
func (r *Resolver) AdoptCandidate(id string) (*Adoption, error) {
	stale, err := r.StaleDeps(id)
	if err != nil {
		return nil, err
	}
	if len(stale) > 0 {
		var names []string
		for _, d := range stale {
			names = append(names, d.Zone)
		}
		return nil, fmt.Errorf("candidate %q has stale dependencies (%s); recheck before adopting",
			id, strings.Join(names, ", "))
	}
	r.mu.RLock()
	c := r.cands[id]
	r.mu.RUnlock()
	zones := make([]*Zone, 0, len(c.Zones))
	adopted := make(map[string]uint64, len(c.Zones))
	for name, z := range c.Zones {
		zones = append(zones, z)
		adopted[name] = z.Version
	}
	if err := r.ApplyBatch(zones); err != nil {
		return nil, err
	}
	basis := make(map[string]uint64, len(c.Base))
	for k, v := range c.Base {
		basis[k] = v
	}
	ad := Adoption{ID: id, Basis: basis, Zones: adopted, At: time.Now()}
	r.mu.Lock()
	r.adopted = append(r.adopted, ad)
	delete(r.cands, id)
	r.mu.Unlock()
	return &ad, nil
}

// AbandonCandidate drops the candidate but keeps the diff basis: for
// every candidate zone, the record diff against the base version it was
// trialled against. The candidate's private cache dies with it.
func (r *Resolver) AbandonCandidate(id string) (*Abandonment, error) {
	r.mu.RLock()
	c := r.cands[id]
	r.mu.RUnlock()
	if c == nil {
		return nil, fmt.Errorf("unknown candidate %q", id)
	}
	diffs := make(map[string]RecordDiff, len(c.Zones))
	for name, z := range c.Zones {
		var old *Zone
		if v, ok := c.Base[name]; ok {
			old = r.ZoneVersion(name, v)
		}
		diffs[name] = diffZones(old, z)
	}
	basis := make(map[string]uint64, len(c.Base))
	for k, v := range c.Base {
		basis[k] = v
	}
	ab := Abandonment{ID: id, Basis: basis, Diffs: diffs, At: time.Now()}
	r.mu.Lock()
	r.abandoned = append(r.abandoned, ab)
	delete(r.cands, id)
	r.mu.Unlock()
	return &ab, nil
}

// Adoptions returns the log of adopted candidates and their basis.
func (r *Resolver) Adoptions() []Adoption {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Adoption(nil), r.adopted...)
}

// Abandonments returns the log of abandoned candidates and their diffs.
func (r *Resolver) Abandonments() []Abandonment {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Abandonment(nil), r.abandoned...)
}

// Overlaps reports the zone names both candidates carry their own
// version of — trial views whose effects cannot be evaluated (or
// adopted) independently of each other.
func (r *Resolver) Overlaps(id1, id2 string) ([]string, error) {
	c1, c2 := r.Candidate(id1), r.Candidate(id2)
	if c1 == nil || c2 == nil {
		return nil, fmt.Errorf("unknown candidate")
	}
	var shared []string
	for name := range c1.Zones {
		if _, ok := c2.Zones[name]; ok {
			shared = append(shared, name)
		}
	}
	sort.Strings(shared)
	return shared, nil
}

// Comparison holds the two outcomes of running the same query against
// the live view and a candidate view.
type Comparison struct {
	Name      string
	Live      *Result
	LiveErr   string
	Candidate *Result
	CandErr   string
}

// Changed reports whether the candidate would answer differently.
func (c *Comparison) Changed() bool {
	if (c.LiveErr != "") != (c.CandErr != "") || c.LiveErr != c.CandErr {
		return true
	}
	if c.Live == nil || c.Candidate == nil {
		return c.Live != c.Candidate
	}
	if len(c.Live.Records) != len(c.Candidate.Records) {
		return true
	}
	for i := range c.Live.Records {
		if c.Live.Records[i] != c.Candidate.Records[i] {
			return true
		}
	}
	return false
}

// Compare runs the same fictional query against the live view and the
// candidate view, so the maintainer can see exactly which queries would
// get different answers. Each side uses its own view and its own cache.
func (r *Resolver) Compare(id, name string) *Comparison {
	cmp := &Comparison{Name: NormalizeName(name)}
	if res, err := r.Resolve(name); err != nil {
		cmp.LiveErr = err.Error()
	} else {
		cmp.Live = res
	}
	if res, err := r.ResolveCandidate(id, name); err != nil {
		cmp.CandErr = err.Error()
	} else {
		cmp.Candidate = res
	}
	return cmp
}
