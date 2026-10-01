package resolver

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Error codes distinguishing failure modes.
const (
	ErrNXDOMAIN = "NXDOMAIN" // name does not exist
	ErrNODATA   = "NODATA"   // name exists, type does not
	ErrNoZone   = "NOZONE"   // no authoritative zone configured
	ErrLoop     = "LOOP"     // alias cycle detected
)

// ResolveError carries a code plus, for loops, the alias path taken.
type ResolveError struct {
	Code string
	Path []string // alias chain when Code == ErrLoop
}

func (e *ResolveError) Error() string {
	if e.Code == ErrLoop {
		return fmt.Sprintf("alias loop detected: %s", strings.Join(e.Path, " -> "))
	}
	return e.Code
}

// Result is the outcome of a successful resolution.
type Result struct {
	Name      string
	Records   []Record // final A records
	Chain     []Record // CNAME chain followed, in order
	Source    Source
	FromCache bool
}

// Resolver is the single resolution core shared by every adapter.
//
// Besides the live zone versions it retains every published version
// (history), so a query can be answered "as of" an older zone set and
// any answer — including a cached one — can be explained by the exact
// zone@version it came from.
type Resolver struct {
	zones map[string]*ZoneStore // zone name -> store
	cache *Cache

	mu   sync.RWMutex
	hist map[string][]*Zone // zone name -> versions, ascending
}

func NewResolver(cache *Cache) *Resolver {
	return &Resolver{
		zones: make(map[string]*ZoneStore),
		cache: cache,
		hist:  make(map[string][]*Zone),
	}
}

func (r *Resolver) Cache() *Cache { return r.cache }

// AddZone registers a zone (first version).
func (r *Resolver) AddZone(z *Zone) {
	r.zones[z.Name] = NewZoneStore(z)
	r.mu.Lock()
	r.hist[z.Name] = []*Zone{z}
	r.mu.Unlock()
}

// UpdateZone atomically publishes a new version of an existing zone.
// Versions must increase strictly; the published version is appended to
// the zone history and cache entries sourced from older versions of this
// zone are invalidated immediately.
func (r *Resolver) UpdateZone(z *Zone) error {
	s, ok := r.zones[z.Name]
	if !ok {
		return fmt.Errorf("unknown zone %q", z.Name)
	}
	if cur := s.Current(); z.Version <= cur.Version {
		return fmt.Errorf("zone %q version %d does not advance current %d", z.Name, z.Version, cur.Version)
	}
	s.Update(z)
	r.mu.Lock()
	r.hist[z.Name] = append(r.hist[z.Name], z)
	r.mu.Unlock()
	r.cache.InvalidateZone(z.Name, z.Version)
	return nil
}

// RemoveZone deletes a zone entirely (e.g. a revoked delegation whose
// child zone is gone). Cached answers sourced from it are dropped; its
// history is retained so past answers remain explainable.
func (r *Resolver) RemoveZone(name string) error {
	name = NormalizeName(name)
	if _, ok := r.zones[name]; !ok {
		return fmt.Errorf("unknown zone %q", name)
	}
	delete(r.zones, name)
	r.cache.InvalidateZoneAll(name)
	return nil
}

// RollbackZone re-publishes the record set of an earlier version as a
// new, higher version. Versions stay monotonic, so late-update rejection
// and cache invalidation keep working; the rollback itself becomes part
// of the history.
func (r *Resolver) RollbackZone(name string, toVersion uint64) (*Zone, error) {
	name = NormalizeName(name)
	s, ok := r.zones[name]
	if !ok {
		return nil, fmt.Errorf("unknown zone %q", name)
	}
	r.mu.RLock()
	var old *Zone
	for _, z := range r.hist[name] {
		if z.Version == toVersion {
			old = z
		}
	}
	r.mu.RUnlock()
	if old == nil {
		return nil, fmt.Errorf("zone %q has no version %d in history", name, toVersion)
	}
	cur := s.Current()
	rb := &Zone{
		Name:        old.Name,
		Version:     cur.Version + 1,
		Records:     old.Records, // zones are immutable; safe to share
		NegativeTTL: old.NegativeTTL,
	}
	if err := r.UpdateZone(rb); err != nil {
		return nil, err
	}
	return rb, nil
}

// ZoneVersion returns the recorded snapshot of one zone version, or nil.
func (r *Resolver) ZoneVersion(name string, version uint64) *Zone {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, z := range r.hist[NormalizeName(name)] {
		if z.Version == version {
			return z
		}
	}
	return nil
}

// ZoneHistory lists the known versions of a zone, ascending.
func (r *Resolver) ZoneHistory(name string) []uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []uint64
	for _, z := range r.hist[NormalizeName(name)] {
		out = append(out, z.Version)
	}
	return out
}

// ZoneNames lists configured zones (deepest first), used by persistence.
func (r *Resolver) ZoneNames() []string {
	names := make([]string, 0, len(r.zones))
	for n := range r.zones {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ZoneSnapshot returns the current immutable zone for persistence.
func (r *Resolver) ZoneSnapshot(name string) *Zone {
	if s, ok := r.zones[NormalizeName(name)]; ok {
		return s.Current()
	}
	return nil
}

// zoneView resolves a zone name to the immutable snapshot to use.
// The live view returns current versions; a pinned view (ResolveAt)
// returns historical versions so a past answer can be reproduced and
// explained.
type zoneView func(name string) *Zone

func (r *Resolver) liveView() zoneView {
	return func(name string) *Zone {
		if s, ok := r.zones[name]; ok {
			return s.Current()
		}
		return nil
	}
}

// pinnedView returns a view of the zone set at the given versions.
// Zones absent from pin are seen at their current version.
func (r *Resolver) pinnedView(pin map[string]uint64) zoneView {
	return func(name string) *Zone {
		if v, ok := pin[name]; ok {
			return r.ZoneVersion(name, v)
		}
		return r.liveView()(name)
	}
}

// authoritativeZone finds the deepest zone that is authoritative for name,
// walking delegations: starting from the shallowest matching zone, an NS
// record at or above name cuts over to the child zone. Records for names
// below a delegation point in the parent zone are never consulted.
func (r *Resolver) authoritativeZone(qname string, view zoneView) (*Zone, error) {
	qname = NormalizeName(qname)
	// candidate zones, shallowest first
	var matches []*Zone
	r.mu.RLock()
	names := make([]string, 0, len(r.hist))
	for zn := range r.hist { // history covers live and removed zones
		names = append(names, zn)
	}
	r.mu.RUnlock()
	for _, zn := range names {
		if z := view(zn); z != nil && IsSubName(qname, zn) {
			matches = append(matches, z)
		}
	}
	if len(matches) == 0 {
		return nil, &ResolveError{Code: ErrNoZone}
	}
	sort.Slice(matches, func(i, j int) bool {
		return len(strings.Split(matches[i].Name, ".")) < len(strings.Split(matches[j].Name, "."))
	})
	zone := matches[0]
	for {
		// Look for the deepest NS delegation in this zone that covers qname
		// and points at a zone we actually serve.
		best := ""
		for owner, recs := range zone.Records {
			if owner == zone.Name || !IsSubName(qname, owner) {
				continue
			}
			for _, rec := range recs {
				if rec.Type == TypeNS && len(owner) > len(best) {
					if view(owner) != nil {
						best = owner
					}
				}
			}
		}
		if best == "" || best == zone.Name {
			return zone, nil
		}
		zone = view(best)
	}
}

// Resolve answers an A-type query for name, chasing aliases. All records
// used for one delegation step come from a single zone snapshot.
func (r *Resolver) Resolve(name string) (*Result, error) {
	return r.resolve(name, nil, r.liveView(), true)
}

// ResolveAt answers a query against a historical view: each zone named in
// pin is used at the pinned version, others at their current version. The
// live cache is bypassed (a past answer must be recomputed from the past
// zone set, not from today's cache), and nothing is cached. The result's
// Source identifies exactly which zone@version produced the answer.
func (r *Resolver) ResolveAt(name string, pin map[string]uint64) (*Result, error) {
	for zn, v := range pin {
		if r.ZoneVersion(zn, v) == nil {
			return nil, &ResolveError{Code: ErrNoZone}
		}
	}
	return r.resolve(name, nil, r.pinnedView(pin), false)
}

func (r *Resolver) resolve(name string, chain []Record, view zoneView, useCache bool) (*Result, error) {
	name = NormalizeName(name)
	// Loop detection: name already visited in this chain. Build the full
	// alias path first so the error reports the entire cycle.
	path := make([]string, 0, len(chain)+1)
	looped := false
	for _, c := range chain {
		path = append(path, c.Name)
		if c.Name == name {
			looped = true
		}
	}
	if looped {
		path = append(path, name)
		return nil, &ResolveError{Code: ErrLoop, Path: path}
	}

	if useCache {
		if e := r.cache.Get(name, TypeA); e != nil {
			if e.Negative {
				return nil, &ResolveError{Code: ErrNXDOMAIN, Path: path}
			}
			return &Result{Name: name, Records: e.Records, Source: e.Source, FromCache: true}, nil
		}
	}

	zone, err := r.authoritativeZone(name, view)
	if err != nil {
		return nil, err
	}
	src := Source{Zone: zone.Name, Version: zone.Version}
	recs := zone.lookup(name)

	var aRecs, cnames []Record
	for _, rec := range recs {
		switch rec.Type {
		case TypeA:
			aRecs = append(aRecs, rec)
		case TypeCNAME:
			cnames = append(cnames, rec)
		}
	}

	switch {
	case len(aRecs) > 0:
		if useCache {
			r.cache.Put(&CacheEntry{
				Name: name, Type: TypeA, Records: aRecs, Source: src,
				Expires: r.cache.now().Add(time.Duration(minTTL(aRecs)) * time.Second),
			})
		}
		return &Result{Name: name, Records: aRecs, Source: src}, nil

	case len(cnames) > 0:
		cn := cnames[0]
		sub, err := r.resolve(NormalizeName(cn.Value), append(chain, cn), view, useCache)
		if err != nil {
			return nil, err
		}
		sub.Chain = append(append([]Record{}, chain...), append([]Record{cn}, sub.Chain...)...)
		return sub, nil

	default:
		// Negative answer: cache it with the zone's negative TTL, kept
		// strictly separate from positive entries.
		code := ErrNXDOMAIN
		if zone.Name == name || len(recs) > 0 {
			code = ErrNODATA
		}
		if useCache {
			r.cache.Put(&CacheEntry{
				Name: name, Type: TypeA, Negative: true, Source: src,
				Expires: r.cache.now().Add(time.Duration(zone.NegativeTTL) * time.Second),
			})
		}
		return nil, &ResolveError{Code: code}
	}
}

func minTTL(recs []Record) int64 {
	m := recs[0].TTL
	for _, r := range recs[1:] {
		if r.TTL < m {
			m = r.TTL
		}
	}
	return m
}
