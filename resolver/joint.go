package resolver

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// JointReview is a combined review view over several candidates whose
// zones may reference names introduced by each other. The joint view
// resolves cross-zone references against the set's actual content:
// member candidate zones overlaid on the live zone versions pinned when
// the review was created (Base). A name that resolves inside the set
// does not imply any single member is adoptable on its own — the report
// lists the cross-candidate dependencies that make it resolve.
type JointReview struct {
	ID      string            `json:"id"`
	Members []string          `json:"members"` // candidate ids forming the pending set
	Base    map[string]uint64 `json:"base"`    // live zone -> version the review rests on
	At      time.Time         `json:"at"`
}

// ZoneAttribution states which source supplies one zone in the joint
// view: a member candidate's trial version, or a live zone pinned at
// the version the review was created against.
type ZoneAttribution struct {
	Zone    string `json:"zone"`
	Source  string `json:"source"` // "candidate:<id>" or "live"
	Version uint64 `json:"version"`
}

// RefProblem is one broken cross-zone reference found by the joint
// check: an alias loop (with the full path) or a missing target.
type RefProblem struct {
	From   string `json:"from"`   // owner name of the alias record
	Target string `json:"target"` // where it points
	Kind   string `json:"kind"`   // "loop" or "missing"
	Detail string `json:"detail"`
}

// CrossDep records that a reference from one member's zones only
// resolves because another member of the set provides the target zone.
// Adopting the referring candidate alone would leave it dangling.
type CrossDep struct {
	From       string `json:"from"`        // candidate id holding the reference
	Name       string `json:"name"`        // referenced name
	ProvidedBy string `json:"provided_by"` // candidate id whose zones answer it
}

// JointReport is the outcome of checking a joint review set.
type JointReport struct {
	ID        string            `json:"id"`
	Members   []string          `json:"members"`
	Zones     []ZoneAttribution `json:"zones"`      // version attribution per zone in the view
	Loops     []RefProblem      `json:"loops"`      // alias cycles found inside the set
	Missing   []RefProblem      `json:"missing"`    // alias targets the set cannot resolve
	CrossDeps []CrossDep        `json:"cross_deps"` // references only satisfied by fellow members
}

// OK reports whether the set is clear to publish: no loops, no missing
// targets. Cross-deps do not block publishing the set as a whole.
func (rep *JointReport) OK() bool { return len(rep.Loops) == 0 && len(rep.Missing) == 0 }

// JointAdoption records that a joint set went live atomically, with the
// exact live zone set (Basis) the review was validated against.
type JointAdoption struct {
	ID      string            `json:"id"`
	Members []string          `json:"members"`
	Basis   map[string]uint64 `json:"basis"`
	Zones   map[string]uint64 `json:"zones"` // adopted zone -> version
	At      time.Time         `json:"at"`
}

// NewJointReview registers a joint review over existing candidates.
// Members must be registered candidates and must not overlap (two
// members carrying the same zone cannot be reviewed as one set). The
// current live zone set is pinned as the review's base.
func (r *Resolver) NewJointReview(id string, members []string) (*JointReview, error) {
	if id == "" {
		return nil, fmt.Errorf("joint review id must not be empty")
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("joint review %q has no members", id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.joints[id]; ok {
		return nil, fmt.Errorf("joint review %q already exists", id)
	}
	if err := r.validateJointMembersLocked(members); err != nil {
		return nil, err
	}
	j := &JointReview{
		ID:      id,
		Members: append([]string(nil), members...),
		Base:    r.liveVersionsLocked(),
		At:      time.Now(),
	}
	r.joints[id] = j
	return j, nil
}

// validateJointMembersLocked checks that every member is a registered
// candidate and no zone is carried by two members. Callers hold r.mu.
func (r *Resolver) validateJointMembersLocked(members []string) error {
	seenCand := make(map[string]bool)
	owner := make(map[string]string) // zone -> member id
	for _, m := range members {
		if seenCand[m] {
			return fmt.Errorf("candidate %q listed twice", m)
		}
		seenCand[m] = true
		c, ok := r.cands[m]
		if !ok {
			return fmt.Errorf("unknown candidate %q", m)
		}
		for zn := range c.Zones {
			if prev, dup := owner[zn]; dup {
				return fmt.Errorf("zone %q carried by both %q and %q; overlapping candidates cannot form one set", zn, prev, m)
			}
			owner[zn] = m
		}
	}
	return nil
}

// JointReview returns the registered joint review, or nil.
func (r *Resolver) JointReview(id string) *JointReview {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.joints[id]
}

// jointZonesLocked collects the zone overlay of the set: the zone
// versions themselves plus the owning member id per zone.
// Callers hold r.mu.
func (r *Resolver) jointZonesLocked(j *JointReview) (map[string]*Zone, map[string]string) {
	zones := make(map[string]*Zone)
	owner := make(map[string]string)
	for _, m := range j.Members {
		c := r.cands[m]
		if c == nil {
			continue
		}
		for zn, z := range c.Zones {
			zones[zn] = z
			owner[zn] = m
		}
	}
	return zones, owner
}

// jointView builds the review's consistent snapshot: member candidate
// zones overlaid on the base-pinned live versions.
func (r *Resolver) jointView(j *JointReview) zoneView {
	r.mu.RLock()
	zones, _ := r.jointZonesLocked(j)
	r.mu.RUnlock()
	return r.overlayView(zones, j.Base)
}

// CheckJoint resolves every alias introduced by the set's zones against
// the joint view and reports loops, missing targets, cross-candidate
// dependencies, and the version attribution of every zone in the view.
// Nothing is mutated and nothing is cached.
func (r *Resolver) CheckJoint(id string) (*JointReport, error) {
	r.mu.RLock()
	j := r.joints[id]
	if j == nil {
		r.mu.RUnlock()
		return nil, fmt.Errorf("unknown joint review %q", id)
	}
	zones, owner := r.jointZonesLocked(j)
	base := make(map[string]uint64, len(j.Base))
	for k, v := range j.Base {
		base[k] = v
	}
	members := append([]string(nil), j.Members...)
	r.mu.RUnlock()

	view := r.overlayView(zones, base)
	rep := &JointReport{ID: id, Members: members}

	// Version attribution: member zones first, then the pinned live set.
	seen := make(map[string]bool)
	var names []string
	for zn := range zones {
		seen[zn] = true
		names = append(names, zn)
	}
	for zn := range base {
		if !seen[zn] {
			names = append(names, zn)
		}
	}
	sort.Strings(names)
	for _, zn := range names {
		if m, ok := owner[zn]; ok {
			rep.Zones = append(rep.Zones, ZoneAttribution{Zone: zn, Source: "candidate:" + m, Version: zones[zn].Version})
		} else {
			rep.Zones = append(rep.Zones, ZoneAttribution{Zone: zn, Source: "live", Version: base[zn]})
		}
	}

	// Follow every alias the set's zones introduce, inside the joint view.
	for zn, z := range zones {
		for _, recs := range z.Records {
			for _, rec := range recs {
				if rec.Type != TypeCNAME {
					continue
				}
				target := NormalizeName(rec.Value)
				res, err := r.resolve(target, nil, view, nil, "joint:"+id)
				if err != nil {
					if re, ok := err.(*ResolveError); ok && re.Code == ErrLoop {
						rep.Loops = append(rep.Loops, RefProblem{
							From: rec.Name, Target: target, Kind: "loop", Detail: re.Error(),
						})
					} else {
						rep.Missing = append(rep.Missing, RefProblem{
							From: rec.Name, Target: target, Kind: "missing", Detail: err.Error(),
						})
					}
					continue
				}
				// Resolved: if the answering zone belongs to a different
				// member, the reference is a cross-candidate dependency.
				if provider, ok := owner[res.Source.Zone]; ok && provider != owner[zn] {
					rep.CrossDeps = append(rep.CrossDeps, CrossDep{
						From: owner[zn], Name: target, ProvidedBy: provider,
					})
				}
			}
		}
	}
	sort.Slice(rep.Loops, func(i, j int) bool { return rep.Loops[i].From < rep.Loops[j].From })
	sort.Slice(rep.Missing, func(i, j int) bool { return rep.Missing[i].From < rep.Missing[j].From })
	sort.Slice(rep.CrossDeps, func(i, j int) bool {
		if rep.CrossDeps[i].From != rep.CrossDeps[j].From {
			return rep.CrossDeps[i].From < rep.CrossDeps[j].From
		}
		return rep.CrossDeps[i].Name < rep.CrossDeps[j].Name
	})
	return rep, nil
}

// JointStale lists the review's base dependencies that moved since the
// review was created (or last rechecked). A live zone change invalidates
// only the reviews whose base pins that zone; every other decision —
// and every answer already given — keeps the basis it was made on.
func (r *Resolver) JointStale(id string) ([]StaleDep, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	j := r.joints[id]
	if j == nil {
		return nil, fmt.Errorf("unknown joint review %q", id)
	}
	var stale []StaleDep
	for zone, baseV := range j.Base {
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

// RecheckJoint re-pins the review on the current live zone set and
// optionally narrows the member set (members must be a subset of the
// current members). Returns the stale dependencies that were found, so
// the operator sees what moved before re-checking the narrowed set.
func (r *Resolver) RecheckJoint(id string, members []string) ([]StaleDep, error) {
	stale, err := r.JointStale(id)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.joints[id]
	if members != nil {
		current := make(map[string]bool, len(j.Members))
		for _, m := range j.Members {
			current[m] = true
		}
		for _, m := range members {
			if !current[m] {
				return nil, fmt.Errorf("candidate %q is not a member of joint review %q; narrowing can only drop members", m, id)
			}
		}
		if err := r.validateJointMembersLocked(members); err != nil {
			return nil, err
		}
		j.Members = append([]string(nil), members...)
	}
	j.Base = r.liveVersionsLocked()
	j.At = time.Now()
	return stale, nil
}

// AdoptJoint publishes every member's zones as one atomic batch after a
// final staleness and consistency check. Adoption is refused while base
// dependencies are stale (recheck first) or while the joint check still
// finds alias loops or missing targets. On any failure nothing is
// published: the live zones keep serving their previous versions, never
// a half-adopted mix. On success the member candidates are consumed and
// the adoption is recorded with the exact basis it rested on.
func (r *Resolver) AdoptJoint(id string) (*JointAdoption, error) {
	stale, err := r.JointStale(id)
	if err != nil {
		return nil, err
	}
	if len(stale) > 0 {
		var names []string
		for _, d := range stale {
			names = append(names, d.Zone)
		}
		return nil, fmt.Errorf("joint review %q has stale dependencies (%s); recheck before adopting",
			id, strings.Join(names, ", "))
	}
	rep, err := r.CheckJoint(id)
	if err != nil {
		return nil, err
	}
	if !rep.OK() {
		var probs []string
		for _, p := range rep.Loops {
			probs = append(probs, p.Detail)
		}
		for _, p := range rep.Missing {
			probs = append(probs, fmt.Sprintf("%s -> %s: %s", p.From, p.Target, p.Detail))
		}
		return nil, fmt.Errorf("joint review %q does not check out: %s", id, strings.Join(probs, "; "))
	}
	r.mu.RLock()
	j := r.joints[id]
	zones, _ := r.jointZonesLocked(j)
	r.mu.RUnlock()

	batch := make([]*Zone, 0, len(zones))
	adopted := make(map[string]uint64, len(zones))
	for name, z := range zones {
		batch = append(batch, z)
		adopted[name] = z.Version
	}
	// ApplyBatch validates the whole set before publishing anything, so
	// a failure here leaves the live zones exactly as they were.
	if err := r.ApplyBatch(batch); err != nil {
		return nil, err
	}
	basis := make(map[string]uint64, len(j.Base))
	for k, v := range j.Base {
		basis[k] = v
	}
	ad := JointAdoption{
		ID:      id,
		Members: append([]string(nil), j.Members...),
		Basis:   basis,
		Zones:   adopted,
		At:      time.Now(),
	}
	r.mu.Lock()
	r.jointAdopted = append(r.jointAdopted, ad)
	for _, m := range j.Members {
		delete(r.cands, m)
	}
	delete(r.joints, id)
	r.mu.Unlock()
	return &ad, nil
}

// AbandonJoint drops the joint review. The member candidates themselves
// are kept: abandoning an unadopted set never deletes its candidates,
// which can still be reviewed, combined, or adopted on their own.
func (r *Resolver) AbandonJoint(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.joints[id]; !ok {
		return fmt.Errorf("unknown joint review %q", id)
	}
	delete(r.joints, id)
	return nil
}

// JointAdoptions returns the log of adopted joint sets and their basis.
func (r *Resolver) JointAdoptions() []JointAdoption {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]JointAdoption(nil), r.jointAdopted...)
}
