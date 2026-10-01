package resolver

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	View      uint64 // view the answer was resolved from
	FromCache bool
}

// maxRetainedViews bounds how many past views stay queryable in memory.
const maxRetainedViews = 64

// Resolver is the single resolution core shared by every adapter.
//
// All zones live behind one atomic view pointer: a query loads the view
// once and uses only that view's zones, so a batch change to a parent
// delegation plus child content is seen as all-old or all-new.
type Resolver struct {
	view  atomic.Pointer[zoneView]
	cache *Cache

	mu      sync.Mutex // serializes batch application and history
	now     func() time.Time
	history []ViewRecord
	views   map[uint64]*zoneView // retained past views for historical queries
}

func NewResolver(cache *Cache) *Resolver {
	r := &Resolver{
		cache: cache,
		now:   time.Now,
		views: make(map[uint64]*zoneView),
	}
	empty := &zoneView{ID: 0, Zones: map[string]*Zone{}}
	r.view.Store(empty)
	r.views[0] = empty
	return r
}

func (r *Resolver) Cache() *Cache { return r.cache }

// SetClock overrides the wall clock used for view history timestamps.
func (r *Resolver) SetClock(now func() time.Time) { r.now = now }

// AddZone registers a zone (first version) as a one-zone batch.
func (r *Resolver) AddZone(z *Zone) {
	if _, err := r.ApplyBatch(Batch{Upsert: []*Zone{z}, Note: "add zone " + z.Name}); err != nil {
		panic(err) // AddZone cannot fail: upserts never reference unknown zones
	}
}

// UpdateZone atomically publishes a new version of an existing zone.
func (r *Resolver) UpdateZone(z *Zone) error {
	if r.view.Load().zone(z.Name) == nil {
		return fmt.Errorf("unknown zone %q", z.Name)
	}
	_, err := r.ApplyBatch(Batch{Upsert: []*Zone{z}, Note: "update zone " + z.Name})
	return err
}

// ApplyBatch validates and atomically publishes a multi-zone change.
// Cache entries whose source zone version no longer matches the new view
// (updated, removed, or rolled-back zones) are invalidated immediately,
// so stale positive or negative answers are never served after a change.
func (r *Resolver) ApplyBatch(b Batch) (*ViewRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.view.Load()
	next, changes, err := applyTo(old, b)
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return nil, fmt.Errorf("batch is a no-op")
	}
	next.ID = old.ID + 1
	r.view.Store(next)

	// Invalidate cache entries sourced from zones whose version changed
	// (or that vanished) in this view. Comparison is by inequality, so
	// rollback to an older version invalidates just like an upgrade.
	r.cache.InvalidateWhere(func(e *CacheEntry) bool {
		z := next.zone(e.Source.Zone)
		return z == nil || z.Version != e.Source.Version
	})

	rec := ViewRecord{
		ID:      next.ID,
		Time:    r.now(),
		Note:    b.Note,
		Zones:   zoneVersions(next),
		Changes: changes,
	}
	r.history = append(r.history, rec)
	r.views[next.ID] = next
	if len(r.views) > maxRetainedViews {
		// Drop the oldest retained views (never the current one).
		for id := range r.views {
			if id != next.ID && len(r.views) > maxRetainedViews {
				delete(r.views, id)
			}
		}
	}
	return &rec, nil
}

// PreviewBatch dry-runs a Batch against the current view: it reports the
// zone-level changes and the cache entries that would be invalidated,
// without touching live state.
func (r *Resolver) PreviewBatch(b Batch) (*Preview, error) {
	cur := r.view.Load()
	next, changes, err := applyTo(cur, b)
	if err != nil {
		return nil, err
	}
	p := &Preview{Changes: changes}
	for _, e := range r.cache.Entries() {
		entry := e
		if z := next.zone(entry.Source.Zone); z == nil || z.Version != entry.Source.Version {
			p.Invalidate = append(p.Invalidate, entry)
		}
	}
	return p, nil
}

// applyTo builds the successor view for b and validates it.
func applyTo(cur *zoneView, b Batch) (*zoneView, []ZoneChange, error) {
	zones := make(map[string]*Zone, len(cur.Zones))
	for name, z := range cur.Zones {
		zones[name] = z
	}
	for _, name := range b.Remove {
		name = NormalizeName(name)
		if _, ok := zones[name]; !ok {
			return nil, nil, fmt.Errorf("cannot remove unknown zone %q", name)
		}
		delete(zones, name)
	}
	for _, z := range b.Upsert {
		if z == nil {
			return nil, nil, fmt.Errorf("nil zone in batch")
		}
		zones[z.Name] = z
	}
	return &zoneView{ID: cur.ID, Zones: zones}, diffZones(cur.Zones, zones), nil
}

func zoneVersions(v *zoneView) map[string]uint64 {
	out := make(map[string]uint64, len(v.Zones))
	for name, z := range v.Zones {
		out[name] = z.Version
	}
	return out
}

// History returns the recorded view changes, oldest first. View 0 is the
// empty bootstrap view and is not listed.
func (r *Resolver) History() []ViewRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ViewRecord, len(r.history))
	copy(out, r.history)
	return out
}

// CurrentViewID returns the ID of the view queries currently resolve from.
func (r *Resolver) CurrentViewID() uint64 { return r.view.Load().ID }

// ZoneNames lists configured zones (sorted), used by persistence.
func (r *Resolver) ZoneNames() []string {
	v := r.view.Load()
	names := make([]string, 0, len(v.Zones))
	for n := range v.Zones {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ZoneSnapshot returns the current immutable zone for persistence.
func (r *Resolver) ZoneSnapshot(name string) *Zone {
	return r.view.Load().zone(name)
}

// authoritativeZone finds, within one view, the deepest zone authoritative
// for name, walking delegations: an NS record at or above name cuts over
// to the child zone. Records below a delegation cut in the parent zone
// are never consulted.
func (r *Resolver) authoritativeZone(v *zoneView, qname string) (*Zone, error) {
	qname = NormalizeName(qname)
	var matches []*Zone
	for zn, z := range v.Zones {
		if IsSubName(qname, zn) {
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
		best := ""
		for owner, recs := range zone.Records {
			if owner == zone.Name || !IsSubName(qname, owner) {
				continue
			}
			for _, rec := range recs {
				if rec.Type == TypeNS && len(owner) > len(best) {
					if v.zone(owner) != nil {
						best = owner
					}
				}
			}
		}
		if best == "" || best == zone.Name {
			return zone, nil
		}
		zone = v.zone(best)
	}
}

// Resolve answers an A-type query for name against the current view,
// chasing aliases.
func (r *Resolver) Resolve(name string) (*Result, error) {
	return r.resolveIn(r.view.Load(), name, nil)
}

// ResolveInView answers a query against a retained historical view, so a
// past answer can be reproduced and attributed to its zone set. The cache
// is bypassed: historical answers must come from that view's zones.
func (r *Resolver) ResolveInView(viewID uint64, name string) (*Result, error) {
	r.mu.Lock()
	v, ok := r.views[viewID]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("view %d not retained", viewID)
	}
	return r.resolveIn(v, name, nil)
}

func (r *Resolver) resolveIn(v *zoneView, name string, chain []Record) (*Result, error) {
	name = NormalizeName(name)
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

	// Only the current view uses the shared cache; historical views always
	// resolve straight from their own zone data.
	if v.ID == r.view.Load().ID {
		if e := r.cache.Get(name, TypeA); e != nil {
			if e.Negative {
				return nil, &ResolveError{Code: ErrNXDOMAIN, Path: path}
			}
			return &Result{Name: name, Records: e.Records, Source: e.Source, View: v.ID, FromCache: true}, nil
		}
	}

	zone, err := r.authoritativeZone(v, name)
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
		if v.ID == r.view.Load().ID {
			r.cache.Put(&CacheEntry{
				Name: name, Type: TypeA, Records: aRecs, Source: src,
				Expires: r.cache.now().Add(time.Duration(minTTL(aRecs)) * time.Second),
			})
		}
		return &Result{Name: name, Records: aRecs, Source: src, View: v.ID}, nil

	case len(cnames) > 0:
		cn := cnames[0]
		sub, err := r.resolveIn(v, NormalizeName(cn.Value), append(chain, cn))
		if err != nil {
			return nil, err
		}
		sub.Chain = append(append([]Record{}, chain...), append([]Record{cn}, sub.Chain...)...)
		return sub, nil

	default:
		code := ErrNXDOMAIN
		if zone.Name == name || len(recs) > 0 {
			code = ErrNODATA
		}
		if v.ID == r.view.Load().ID {
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
