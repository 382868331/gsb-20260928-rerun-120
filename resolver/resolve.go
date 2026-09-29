package resolver

import (
	"fmt"
	"strings"
)

// RCode is the outcome of a resolution, modelled after DNS status names.
type RCode string

const (
	// Success: at least one answer record exists.
	Success RCode = "NOERROR"
	// NXDomain: the queried name does not exist in the authoritative zone.
	NXDomain RCode = "NXDOMAIN"
	// NoData: the name exists but has no record of the requested kind.
	NoData RCode = "NODATA"
	// AliasLoop: following CNAME records revisited a name.
	AliasLoop RCode = "ALIAS_LOOP"
	// TooManyHops: the alias chain exceeded the safety limit.
	TooManyHops RCode = "TOO_MANY_HOPS"
	// NoZone: no configured fictional zone is authoritative for the name.
	NoZone RCode = "NO_ZONE"
)

// AliasStep records one CNAME hop for diagnostics.
type AliasStep struct {
	From string `json:"from"`
	To   string `json:"to"`
	TTL  int    `json:"ttl"`
}

// Answer describes one fully resolved record.
type Answer struct {
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	Data string `json:"data"`
	TTL  int    `json:"ttl"`
}

// Result is what the resolution core returns. Positive answers, negative
// answers (NXDOMAIN / NODATA) and errors are distinct so callers and the
// cache can treat them separately.
type Result struct {
	Status   RCode       `json:"status"`
	Question string      `json:"question"`
	QKind    Kind        `json:"qkind"`
	Answers  []Answer    `json:"answers,omitempty"`
	Aliases  []AliasStep `json:"aliases,omitempty"`
	// Authority is the zone origin that produced the result.
	Authority string `json:"authority,omitempty"`
	// TTL is the cache lifetime for a negative result.
	TTL int `json:"ttl,omitempty"`
	// LoopPath is set when Status == AliasLoop and shows the full path,
	// including the repeated name at the end.
	LoopPath []string `json:"loop_path,omitempty"`
	Err      string   `json:"error,omitempty"`
}

// Found reports whether the result is a positive answer.
func (r *Result) Found() bool { return r.Status == Success }

// Negative reports whether the result is authoritative "no answer".
func (r *Result) Negative() bool { return r.Status == NXDomain || r.Status == NoData }

// MaxAliasHops bounds CNAME chain length to prevent runaway work.
const MaxAliasHops = 16

// Resolver is the resolution core. It never performs network I/O: the same
// core is used directly in-process and behind the UDP/TCP adapter.
type Resolver struct {
	zones   *ZoneStore
	cache   *Cache
	nowFunc func() int64
}

// New builds a resolver around a zone store and cache.
func New(z *ZoneStore, c *Cache) *Resolver {
	return &Resolver{zones: z, cache: c, nowFunc: c.nowFunc}
}

// Resolve answers a local query. gen is captured once so a whole query sees
// a single zone generation even if an update lands mid-resolution.
func (r *Resolver) Resolve(qname string, qkind Kind) *Result {
	qname = NormalizeName(qname)
	if key, hit := r.cache.lookup(qname, qkind); hit {
		return key
	}
	gen := r.zones.snapshotGeneration()
	res := r.resolveGen(gen, qname, qkind)
	r.cache.store(gen.version, res, r.nowFunc())
	return res
}

// resolveGen performs lookup and alias chasing against one frozen generation.
func (r *Resolver) resolveGen(gen *generation, qname string, qkind Kind) *Result {
	zone, inZone := authoritativeZone(gen, qname)
	if !inZone {
		return &Result{Status: NoZone, Question: qname, QKind: qkind,
			Err: "no configured fictional zone is authoritative for " + qname}
	}

	// Child delegation: a subzone owns its own names. The parent's records
	// for the subzone's namespace are never consulted, so a stale or invalid
	// parent record cannot shadow the child's authoritative answer.
	current := qname
	visited := map[string]bool{}
	var path []string
	var aliases []AliasStep

	for hops := 0; ; hops++ {
		if visited[current] {
			lp := append(append([]string{}, path...), current)
			return &Result{
				Status: AliasLoop, Question: qname, QKind: qkind,
				Authority: zone.Origin, Aliases: aliases, LoopPath: lp,
				Err: "alias loop detected: " + strings.Join(lp, " -> "),
			}
		}
		if hops > MaxAliasHops {
			return &Result{Status: TooManyHops, Question: qname, QKind: qkind,
				Authority: zone.Origin, Aliases: aliases,
				Err: fmt.Sprintf("alias chain exceeded %d hops", MaxAliasHops)}
		}
		visited[current] = true
		path = append(path, current)

		// Re-check authority at every hop: aliases may cross zones.
		z, ok := authoritativeZone(gen, current)
		if !ok {
			return &Result{Status: NoZone, Question: qname, QKind: qkind, Aliases: aliases,
				Err: "alias target " + current + " is outside configured zones"}
		}
		zone = z
		// If the current name belongs to a delegated child, follow into the
		// child rather than reading any parent-side record.
		records := zone.Lookup(current)

		// CNAME chase takes precedence (a CNAME name has no other records).
		var cname *Record
		for i := range records {
			if records[i].Kind == CNAME {
				cname = &records[i]
				break
			}
		}
		if cname != nil {
			target := NormalizeName(cname.Data)
			aliases = append(aliases, AliasStep{From: current, To: target, TTL: cname.TTL})
			current = target
			continue
		}

		// Terminal name: collect records of the requested kind.
		var answers []Answer
		minTTL := 0
		for _, rec := range records {
			if rec.Kind == qkind {
				answers = append(answers, Answer{Name: current, Kind: rec.Kind, Data: rec.Data, TTL: rec.TTL})
				if minTTL == 0 || rec.TTL < minTTL {
					minTTL = rec.TTL
				}
			}
		}
		if len(answers) > 0 {
			return &Result{Status: Success, Question: qname, QKind: qkind,
				Answers: answers, Aliases: aliases, Authority: zone.Origin, TTL: minTTL}
		}
		// Records exist under the name but not of this kind -> NODATA;
		// the name is absent entirely -> NXDOMAIN.
		if len(records) > 0 {
			return &Result{Status: NoData, Question: qname, QKind: qkind,
				Aliases: aliases, Authority: zone.Origin, TTL: r.zones.negTTL}
		}
		return &Result{Status: NXDomain, Question: qname, QKind: qkind,
			Aliases: aliases, Authority: zone.Origin, TTL: r.zones.negTTL}
	}
}

// authoritativeZone finds the most specific configured zone that owns name.
// Origins are ordered longest-first, so a subzone wins over its parent.
func authoritativeZone(gen *generation, name string) (*Snapshot, bool) {
	for _, origin := range gen.origins {
		s := gen.zones[origin]
		if HasSuffixName(name, origin) {
			return s, true
		}
	}
	return nil, false
}
