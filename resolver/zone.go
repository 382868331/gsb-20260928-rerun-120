package resolver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Snapshot is an immutable view of a single fictional zone.
// Zone data is never mutated in place: an update builds a fresh Snapshot
// and publishes it atomically, so an in-flight resolution always sees one
// coherent version instead of a mix of two versions.
type Snapshot struct {
	Origin   string   `json:"origin"`
	Serial   uint64   `json:"serial"`
	Records  []Record `json:"records"`
	byName   map[string][]Record
	children map[string]bool // immediate subzone origins
}

func buildSnapshot(origin string, serial uint64, records []Record) (*Snapshot, error) {
	origin = NormalizeName(origin)
	if origin == "" {
		return nil, fmt.Errorf("zone has empty origin")
	}
	kinds := make(map[string]map[Kind]bool)
	for _, r := range records {
		if err := validateRecord(r); err != nil {
			return nil, err
		}
		rn := NormalizeName(r.Name)
		if !HasSuffixName(rn, origin) {
			return nil, fmt.Errorf("record %q is outside zone %q", rn, origin)
		}
		if kinds[rn] == nil {
			kinds[rn] = map[Kind]bool{}
		}
		if r.Kind == CNAME && len(kinds[rn]) > 0 || kinds[rn][CNAME] {
			return nil, fmt.Errorf("name %q cannot mix CNAME with other records", rn)
		}
		kinds[rn][r.Kind] = true
	}
	s := &Snapshot{
		Origin:   origin,
		Serial:   serial,
		Records:  records,
		byName:   make(map[string][]Record),
		children: make(map[string]bool),
	}
	for _, r := range records {
		rn := NormalizeName(r.Name)
		s.byName[rn] = append(s.byName[rn], r)
	}
	return s, nil
}

// Lookup returns the records for a name within this snapshot.
func (s *Snapshot) Lookup(name string) []Record {
	return s.byName[NormalizeName(name)]
}

// ZoneStore keeps the current immutable snapshot of every zone.
// All zones advance together as one committed generation: UpdateZones
// publishes the full new set in a single pointer swap.
type ZoneStore struct {
	mu      sync.RWMutex
	gen     *generation
	file    string
	negTTL  int
	discard bool // true: persistence disabled (ephemeral store)
}

type generation struct {
	zones   map[string]*Snapshot // origin -> snapshot
	origins []string
	version uint64
}

func newGeneration(z map[string]*Snapshot, version uint64) *generation {
	g := &generation{zones: z, version: version}
	for origin := range z {
		g.origins = append(g.origins, origin)
	}
	sort.Slice(g.origins, func(i, j int) bool { return len(g.origins[i]) > len(g.origins[j]) })
	return g
}

// NewZoneStore creates an empty persistent zone store backed by file.
// Pass an empty path for an ephemeral in-memory store.
func NewZoneStore(file string, negativeTTL int) *ZoneStore {
	if negativeTTL <= 0 {
		negativeTTL = 30
	}
	return &ZoneStore{gen: newGeneration(map[string]*Snapshot{}, 0), file: file, negTTL: negativeTTL}
}

// Load reads a previously persisted zone set.
func (z *ZoneStore) Load() error {
	if z.file == "" {
		return nil
	}
	data, err := os.ReadFile(z.file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var dump persistedFile
	if err := json.Unmarshal(data, &dump); err != nil {
		return fmt.Errorf("parse zones file: %w", err)
	}
	g, err := buildGeneration(dump.Version, dump.Zones)
	if err != nil {
		return err
	}
	z.mu.Lock()
	z.gen = g
	z.mu.Unlock()
	return nil
}

type persistedZone struct {
	Origin  string   `json:"origin"`
	Serial  uint64   `json:"serial"`
	Records []Record `json:"records"`
}

type persistedFile struct {
	Version uint64          `json:"version"`
	Zones   []persistedZone `json:"zones"`
}

func buildGeneration(version uint64, pz []persistedZone) (*generation, error) {
	m := make(map[string]*Snapshot, len(pz))
	for _, p := range pz {
		s, err := buildSnapshot(p.Origin, p.Serial, p.Records)
		if err != nil {
			return nil, err
		}
		m[s.Origin] = s
	}
	// Fill delegation edges from the fully known zone set.
	for origin, s := range m {
		for other := range m {
			if other != origin && HasSuffixName(other, origin) {
				s.children[other] = true
			}
		}
	}
	return newGeneration(m, version), nil
}

// ZoneInput is a full replacement definition of one zone.
type ZoneInput struct {
	Origin    string
	Records   []Record
	serial    uint64
	serialSet bool
}

// UpdateZones atomically replaces the whole zone set. Each input may set an
// explicit Serial; omitted serials advance by one over the previous
// snapshot. The entire new set becomes visible at once and is persisted to
// disk as a single file rename so a restart reads the complete version.
func (z *ZoneStore) UpdateZones(inputs []ZoneInput) error {
	z.mu.Lock()
	defer z.mu.Unlock()

	pz := make([]persistedZone, 0, len(inputs))
	for _, in := range inputs {
		origin := NormalizeName(in.Origin)
		serial := uint64(0)
		if old := z.gen.zones[origin]; old != nil {
			serial = old.Serial + 1
		}
		if in.serialSet {
			serial = in.serial
		}
		pz = append(pz, persistedZone{Origin: origin, Serial: serial, Records: in.Records})
	}
	g, err := buildGeneration(z.gen.version+1, pz)
	if err != nil {
		return err
	}
	if z.file != "" && !z.discard {
		if err := persist(z.file, persistedFile{Version: g.version, Zones: pz}); err != nil {
			return err
		}
	}
	z.gen = g
	return nil
}

// UpdateZone replaces or creates a single zone while keeping the others.
func (z *ZoneStore) UpdateZone(origin string, records []Record) error {
	return z.UpdateZoneSerial(origin, records, 0, false)
}

// UpdateZoneSerial is UpdateZone with an explicit serial.
func (z *ZoneStore) UpdateZoneSerial(origin string, records []Record, serial uint64, set bool) error {
	origin = NormalizeName(origin)
	z.mu.RLock()
	inputs := make([]ZoneInput, 0, len(z.gen.zones)+1)
	for o, s := range z.gen.zones {
		if o == origin {
			continue
		}
		inputs = append(inputs, ZoneInput{Origin: o, Records: cloneRecords(s.Records), serial: s.Serial, serialSet: true})
	}
	z.mu.RUnlock()
	inputs = append(inputs, ZoneInput{Origin: origin, Records: records, serial: serial, serialSet: set})
	return z.UpdateZones(inputs)
}

// DeleteZone atomically removes a zone.
func (z *ZoneStore) DeleteZone(origin string) error {
	origin = NormalizeName(origin)
	z.mu.RLock()
	var inputs []ZoneInput
	for o, s := range z.gen.zones {
		if o == origin {
			continue
		}
		inputs = append(inputs, ZoneInput{Origin: o, Records: cloneRecords(s.Records), serial: s.Serial, serialSet: true})
	}
	z.mu.RUnlock()
	return z.UpdateZones(inputs)
}

func cloneRecords(rs []Record) []Record {
	out := make([]Record, len(rs))
	copy(out, rs)
	return out
}

// snapshotGeneration returns the current immutable generation; safe to use
// after the lock is released because generations are never mutated.
func (z *ZoneStore) snapshotGeneration() *generation {
	z.mu.RLock()
	g := z.gen
	z.mu.RUnlock()
	return g
}

// CurrentVersion reports the committed generation number.
func (z *ZoneStore) CurrentVersion() uint64 {
	z.mu.RLock()
	v := z.gen.version
	z.mu.RUnlock()
	return v
}

// ZoneSnapshot returns a copy-level view of one zone (the snapshot itself is immutable).
func (z *ZoneStore) ZoneSnapshot(origin string) (*Snapshot, bool) {
	z.mu.RLock()
	defer z.mu.RUnlock()
	s, ok := z.gen.zones[NormalizeName(origin)]
	return s, ok
}

func persist(file string, dump persistedFile) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		return err
	}
	tf, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tf.Name()
	if _, err := tf.Write(data); err != nil {
		tf.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tf.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, file)
}
