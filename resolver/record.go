package resolver

import "fmt"

// RecordType enumerates supported record kinds.
type RecordType string

const (
	TypeA     RecordType = "A"
	TypeCNAME RecordType = "CNAME"
	TypeNS    RecordType = "NS"
)

// Record is a single resource record inside a zone.
type Record struct {
	Name  string     `json:"name"`
	Type  RecordType `json:"type"`
	Value string     `json:"value"`
	TTL   int64      `json:"ttl"` // seconds
}

func (r Record) String() string {
	return fmt.Sprintf("%s %s %s (ttl=%d)", r.Name, r.Type, r.Value, r.TTL)
}

// Source identifies where an answer (or cache entry) came from, so that
// late/stale updates can be checked against the current zone version.
type Source struct {
	Zone    string `json:"zone"`
	Version uint64 `json:"version"`
}

func (s Source) String() string { return fmt.Sprintf("%s@v%d", s.Zone, s.Version) }

