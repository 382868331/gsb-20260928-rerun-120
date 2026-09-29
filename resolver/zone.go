package resolver

import (
	"strings"
	"sync/atomic"
)

// Zone is an immutable set of authoritative records for one zone cut.
// Updates replace the whole Zone atomically (see ZoneStore), so an
// in-flight query never observes a mix of two versions.
type Zone struct {
	Name    string `json:"name"`
	Version uint64 `json:"version"`
	// Records keyed by owner name (absolute, lower-case, no trailing dot).
	Records map[string][]Record `json:"records"`
	// NegativeTTL is used for NXDOMAIN/NODATA caching from this zone.
	NegativeTTL int64 `json:"negative_ttl"`
}

// NewZone builds an immutable zone from a record list.
func NewZone(name string, version uint64, negativeTTL int64, records []Record) *Zone {
	z := &Zone{
		Name:        NormalizeName(name),
		Version:     version,
		Records:     make(map[string][]Record),
		NegativeTTL: negativeTTL,
	}
	for _, r := range records {
		key := NormalizeName(r.Name)
		r.Name = key
		z.Records[key] = append(z.Records[key], r)
	}
	return z
}

// lookup returns records owned by name, or nil.
func (z *Zone) lookup(name string) []Record { return z.Records[NormalizeName(name)] }

// ZoneStore holds the current version of a zone behind an atomic pointer.
type ZoneStore struct {
	ptr atomic.Pointer[Zone]
}

func NewZoneStore(z *Zone) *ZoneStore {
	s := &ZoneStore{}
	s.ptr.Store(z)
	return s
}

// Current returns the zone version visible to this call. All records used
// to answer a single query come from this one snapshot.
func (s *ZoneStore) Current() *Zone { return s.ptr.Load() }

// Update atomically publishes a new zone version. Concurrent queries see
// either the whole old zone or the whole new zone, never a splice.
func (s *ZoneStore) Update(z *Zone) { s.ptr.Store(z) }

// NormalizeName canonicalizes a name: lower-case, no trailing dot.
func NormalizeName(n string) string {
	n = strings.ToLower(strings.TrimSpace(n))
	n = strings.TrimSuffix(n, ".")
	return n
}

// ParentOf returns the parent name of n, or "" for the root.
func ParentOf(n string) string {
	n = NormalizeName(n)
	if n == "" {
		return ""
	}
	if i := strings.Index(n, "."); i >= 0 {
		return n[i+1:]
	}
	return ""
}

// IsSubName reports whether name is at or below zone.
func IsSubName(name, zone string) bool {
	name, zone = NormalizeName(name), NormalizeName(zone)
	if zone == "" {
		return true
	}
	return name == zone || strings.HasSuffix(name, "."+zone)
}

