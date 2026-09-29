package resolver

import (
	"fmt"
	"sort"
	"strings"
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
	Name    string
	Records []Record // final A records
	Chain   []Record // CNAME chain followed, in order
	Source  Source
	FromCache bool
}

// Resolver is the single resolution core shared by every adapter.
type Resolver struct {
	zones map[string]*ZoneStore // zone name -> store
	cache *Cache
}

func NewResolver(cache *Cache) *Resolver {
	return &Resolver{zones: make(map[string]*ZoneStore), cache: cache}
}

func (r *Resolver) Cache() *Cache { return r.cache }

// AddZone registers a zone (first version).
func (r *Resolver) AddZone(z *Zone) { r.zones[z.Name] = NewZoneStore(z) }

// UpdateZone atomically publishes a new version of an existing zone.
func (r *Resolver) UpdateZone(z *Zone) error {
	s, ok := r.zones[z.Name]
	if !ok {
		return fmt.Errorf("unknown zone %q", z.Name)
	}
	s.Update(z)
	return nil
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

// authoritativeZone finds the deepest zone that is authoritative for name,
// walking delegations: starting from the shallowest matching zone, an NS
// record at or above name cuts over to the child zone. Records for names
// below a delegation point in the parent zone are never consulted.
func (r *Resolver) authoritativeZone(qname string) (*Zone, error) {
	qname = NormalizeName(qname)
	// candidate zones, shallowest first
	var matches []*Zone
	for zn, store := range r.zones {
		if IsSubName(qname, zn) {
			matches = append(matches, store.Current())
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
					if _, ok := r.zones[owner]; ok {
						best = owner
					}
				}
			}
		}
		if best == "" || best == zone.Name {
			return zone, nil
		}
		zone = r.zones[best].Current()
	}
}

// Resolve answers an A-type query for name, chasing aliases. All records
// used for one delegation step come from a single zone snapshot.
func (r *Resolver) Resolve(name string) (*Result, error) {
	return r.resolve(name, nil)
}

func (r *Resolver) resolve(name string, chain []Record) (*Result, error) {
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

	if e := r.cache.Get(name, TypeA); e != nil {
		if e.Negative {
			return nil, &ResolveError{Code: ErrNXDOMAIN, Path: path}
		}
		return &Result{Name: name, Records: e.Records, Source: e.Source, FromCache: true}, nil
	}

	zone, err := r.authoritativeZone(name)
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
		r.cache.Put(&CacheEntry{
			Name: name, Type: TypeA, Records: aRecs, Source: src,
			Expires: r.cache.now().Add(time.Duration(minTTL(aRecs)) * time.Second),
		})
		return &Result{Name: name, Records: aRecs, Source: src}, nil

	case len(cnames) > 0:
		cn := cnames[0]
		sub, err := r.resolve(NormalizeName(cn.Value), append(chain, cn))
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
		r.cache.Put(&CacheEntry{
			Name: name, Type: TypeA, Negative: true, Source: src,
			Expires: r.cache.now().Add(time.Duration(zone.NegativeTTL) * time.Second),
		})
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
