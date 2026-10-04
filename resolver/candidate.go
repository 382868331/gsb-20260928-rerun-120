package resolver

import (
	"fmt"
	"sort"
)

// Candidate lifecycle states.
const (
	CandidateOpen      = "open"
	CandidateAdopted   = "adopted"
	CandidateAbandoned = "abandoned"
)

// Candidate is an isolated query view trialling a set of zone changes
// before they are formally applied. The view pins every zone to a fixed
// version — the candidate's own override where one exists, otherwise the
// base version captured when the candidate was opened (or last rebased) —
// so one query never splices parent/child delegations from two different
// points in time. Candidates are isolated from each other: overlapping
// candidates may change the same zone without interfering.
type Candidate struct {
	ID string `json:"id"`
	// Base pins every formal zone to the version this view is based on.
	Base map[string]uint64 `json:"base"`
	// Zones holds the candidate's zone overrides by zone name.
	Zones  map[string]*Zone `json:"zones"`
	Status string           `json:"status"`
	// AdoptedFrom records the exact zone set the candidate was based on
	// when it was formally adopted.
	AdoptedFrom map[string]uint64 `json:"adopted_from,omitempty"`
	// Basis is the per-zone diff against Base. It is kept after the
	// candidate is abandoned so the reasons for the difference remain
	// inspectable.
	Basis []ZonePreview `json:"basis"`
}

// cacheView returns the cache namespace isolating this candidate's
// cached outcomes (including negative ones) from the live view.
func (c *Candidate) cacheView() string { return "candidate:" + c.ID }

// StaleDependency reports one formal zone that moved (or appeared, or
// disappeared) after the candidate's base was captured.
type StaleDependency struct {
	Zone           string `json:"zone"`
	BaseVersion    uint64 `json:"base_version"`    // 0: absent from the candidate view
	CurrentVersion uint64 `json:"current_version"` // 0: zone no longer exists
}

// Comparison holds the outcome of one fictional query executed against
// both the live view and a candidate view.
type Comparison struct {
	Query        string  `json:"query"`
	CandidateID  string  `json:"candidate_id"`
	Live         *Result `json:"live,omitempty"`
	LiveErr      string  `json:"live_err,omitempty"`
	Candidate    *Result `json:"candidate,omitempty"`
	CandidateErr string  `json:"candidate_err,omitempty"`
	Differs      bool    `json:"differs"`
}

// OpenCandidate creates an isolated candidate view trialling zones on top
// of the current formal zone set. The base versions of all formal zones
// are captured now; each override's version must advance the base version
// of its zone so a later adoption can publish it. Nothing formal changes:
// no zone is published and the live cache is untouched.
func (r *Resolver) OpenCandidate(id string, zones []*Zone) (*Candidate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.candidates[id]; ok {
		return nil, fmt.Errorf("candidate %q already exists", id)
	}
	c := &Candidate{
		ID:     id,
		Base:   make(map[string]uint64),
		Zones:  make(map[string]*Zone),
		Status: CandidateOpen,
	}
	for name, s := range r.zones {
		c.Base[name] = s.Current().Version
	}
	for _, z := range zones {
		if _, ok := c.Zones[z.Name]; ok {
			return nil, fmt.Errorf("zone %q appears twice in candidate", z.Name)
		}
		if z.Version <= c.Base[z.Name] {
			return nil, fmt.Errorf("zone %q version %d does not advance base %d", z.Name, z.Version, c.Base[z.Name])
		}
		c.Zones[z.Name] = z
	}
	c.Basis = r.candidateBasisLocked(c)
	r.candidates[id] = c
	return c, nil
}

// Candidate returns the stored candidate, or nil.
func (r *Resolver) Candidate(id string) *Candidate {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.candidates[id]
}

// Candidates lists all known candidates (any status), ordered by ID.
func (r *Resolver) Candidates() []*Candidate {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Candidate, 0, len(r.candidates))
	for _, c := range r.candidates {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// candidateBasisLocked computes the per-zone diff of the candidate's
// overrides against the zone versions pinned in Base.
func (r *Resolver) candidateBasisLocked(c *Candidate) []ZonePreview {
	var basis []ZonePreview
	names := make([]string, 0, len(c.Zones))
	for name := range c.Zones {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		z := c.Zones[name]
		zp := ZonePreview{Zone: name, FromVersion: c.Base[name], ToVersion: z.Version}
		var old *Zone
		if v, ok := c.Base[name]; ok {
			old = r.zoneVersionLocked(name, v)
		}
		zp.Diff = diffZones(old, z)
		basis = append(basis, zp)
	}
	return basis
}

func (r *Resolver) zoneVersionLocked(name string, version uint64) *Zone {
	for _, z := range r.hist[name] {
		if z.Version == version {
			return z
		}
	}
	return nil
}

// openCandidate returns the candidate if it may be queried.
func (r *Resolver) openCandidate(id string) (*Candidate, error) {
	r.mu.RLock()
	c, ok := r.candidates[id]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown candidate %q", id)
	}
	if c.Status != CandidateOpen {
		return nil, fmt.Errorf("candidate %q is %s", id, c.Status)
	}
	return c, nil
}

// candidateView pins every zone to a single fixed version: the override
// where the candidate defines one, otherwise the base version.
func (r *Resolver) candidateView(c *Candidate) (zoneView, []string) {
	view := func(name string) *Zone {
		if z, ok := c.Zones[name]; ok {
			return z
		}
		if v, ok := c.Base[name]; ok {
			return r.ZoneVersion(name, v)
		}
		return nil
	}
	names := r.zoneNamesAll()
	for name := range c.Zones {
		if r.ZoneVersion(name, c.Zones[name].Version) == nil {
			names = append(names, name) // zone introduced by the candidate
		}
	}
	return view, names
}

// ResolveCandidate answers a query against the candidate's isolated view.
// Results are cached under the candidate's own namespace; candidate
// negative results never enter the formal (live) cache.
func (r *Resolver) ResolveCandidate(id, name string) (*Result, error) {
	c, err := r.openCandidate(id)
	if err != nil {
		return nil, err
	}
	view, names := r.candidateView(c)
	return r.resolve(name, nil, view, names, c.cacheView(), true)
}

// CompareQuery executes the same fictional query against the live view
// and the candidate view and reports whether the outcomes differ.
func (r *Resolver) CompareQuery(id, name string) (*Comparison, error) {
	if _, err := r.openCandidate(id); err != nil {
		return nil, err
	}
	cmp := &Comparison{Query: NormalizeName(name), CandidateID: id}
	cmp.Live, cmp.LiveErr = outcome(r.Resolve(name))
	cmp.Candidate, cmp.CandidateErr = outcome(r.ResolveCandidate(id, name))
	cmp.Differs = cmp.LiveErr != cmp.CandidateErr ||
		recordsKey(cmp.Live) != recordsKey(cmp.Candidate)
	return cmp, nil
}

func outcome(res *Result, err error) (*Result, string) {
	if err == nil {
		return res, ""
	}
	if re, ok := err.(*ResolveError); ok {
		return nil, re.Code
	}
	return nil, err.Error()
}

func recordsKey(res *Result) string {
	if res == nil {
		return ""
	}
	parts := make([]string, 0, len(res.Records))
	for _, rec := range res.Records {
		parts = append(parts, rec.String())
	}
	sort.Strings(parts)
	return fmt.Sprintf("%v", parts)
}

// StaleDependencies lists formal zones that changed since the candidate's
// base was captured: versions that advanced, zones that were removed, and
// zones that appeared after the base was taken.
func (r *Resolver) StaleDependencies(id string) ([]StaleDependency, error) {
	r.mu.RLock()
	c, ok := r.candidates[id]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown candidate %q", id)
	}
	var stale []StaleDependency
	for name, baseVer := range c.Base {
		s, live := r.zones[name]
		switch {
		case !live:
			stale = append(stale, StaleDependency{Zone: name, BaseVersion: baseVer})
		case s.Current().Version != baseVer:
			stale = append(stale, StaleDependency{
				Zone: name, BaseVersion: baseVer, CurrentVersion: s.Current().Version,
			})
		}
	}
	for name, s := range r.zones {
		if _, ok := c.Base[name]; !ok {
			stale = append(stale, StaleDependency{Zone: name, CurrentVersion: s.Current().Version})
		}
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].Zone < stale[j].Zone })
	return stale, nil
}

// RebaseCandidate re-anchors the candidate on the current formal zone
// set after dependencies went stale during observation, and returns the
// stale dependencies that were found. The candidate's overrides are
// re-validated against the new base, the diff basis is recomputed, and
// the candidate's cache is dropped (old entries described the old base).
func (r *Resolver) RebaseCandidate(id string) ([]StaleDependency, error) {
	stale, err := r.StaleDependencies(id)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.candidates[id]
	if c.Status != CandidateOpen {
		return nil, fmt.Errorf("candidate %q is %s", id, c.Status)
	}
	base := make(map[string]uint64)
	for name, s := range r.zones {
		base[name] = s.Current().Version
	}
	for _, z := range c.Zones {
		if z.Version <= base[z.Name] {
			return nil, fmt.Errorf("zone %q version %d does not advance new base %d", z.Name, z.Version, base[z.Name])
		}
	}
	c.Base = base
	c.Basis = r.candidateBasisLocked(c)
	r.cache.InvalidateView(c.cacheView())
	return stale, nil
}

// AdoptCandidate formally publishes the candidate's zone overrides and
// records the exact zone set the candidate was based on. Adoption is
// refused while any dependency is stale — rebase first — so a formal
// change is never assembled from a view pinned to an older zone set.
func (r *Resolver) AdoptCandidate(id string) error {
	stale, err := r.StaleDependencies(id)
	if err != nil {
		return err
	}
	if len(stale) > 0 {
		return fmt.Errorf("candidate %q has %d stale dependencies; rebase before adopting", id, len(stale))
	}
	r.mu.Lock()
	c := r.candidates[id]
	if c.Status != CandidateOpen {
		r.mu.Unlock()
		return fmt.Errorf("candidate %q is %s", id, c.Status)
	}
	zones := make([]*Zone, 0, len(c.Zones))
	for _, z := range c.Zones {
		zones = append(zones, z)
	}
	c.AdoptedFrom = make(map[string]uint64, len(c.Base))
	for name, v := range c.Base {
		c.AdoptedFrom[name] = v
	}
	c.Status = CandidateAdopted
	r.mu.Unlock()
	// Publish outside the lock; versions were validated against the base,
	// which StaleDependencies just confirmed equals the live set.
	for _, z := range zones {
		if _, ok := r.zones[z.Name]; ok {
			if err := r.UpdateZone(z); err != nil {
				return err
			}
		} else {
			r.AddZone(z)
		}
	}
	r.cache.InvalidateView(c.cacheView())
	return nil
}

// AbandonCandidate shelves the candidate without applying it. The diff
// basis is retained so the difference that was observed stays
// explainable; the candidate's isolated cache is dropped.
func (r *Resolver) AbandonCandidate(id string) error {
	r.mu.Lock()
	c, ok := r.candidates[id]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("unknown candidate %q", id)
	}
	if c.Status != CandidateOpen {
		r.mu.Unlock()
		return fmt.Errorf("candidate %q is %s", id, c.Status)
	}
	c.Status = CandidateAbandoned
	r.mu.Unlock()
	r.cache.InvalidateView(c.cacheView())
	return nil
}
