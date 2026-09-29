package resolver

// System wires the zone store, cache and resolution core together and
// enforces one rule: a committed zone update both publishes a fresh
// generation and discards every cache entry sourced from older generations
// (positive and negative alike). It is the entry point used by the network
// adapter and the CLI; the core itself performs no network access.
type System struct {
	Zones    *ZoneStore
	Cache    *Cache
	Resolver *Resolver
}

// NewSystem creates the core with an optional persistence file and a
// negative-result TTL (seconds; <=0 uses the default).
func NewSystem(zoneFile string, negativeTTL int) *System {
	z := NewZoneStore(zoneFile, negativeTTL)
	c := NewCache(nil)
	c.bindGeneration(z.CurrentVersion)
	r := New(z, c)
	return &System{Zones: z, Cache: c, Resolver: r}
}

// NewSystemClock is NewSystem with an injectable clock for tests.
func NewSystemClock(zoneFile string, negativeTTL int, now func() int64) *System {
	z := NewZoneStore(zoneFile, negativeTTL)
	c := NewCache(now)
	c.bindGeneration(z.CurrentVersion)
	r := New(z, c)
	return &System{Zones: z, Cache: c, Resolver: r}
}

// Load restores zones persisted on disk (used after restart).
func (s *System) Load() error {
	if err := s.Zones.Load(); err != nil {
		return err
	}
	s.Cache.Flush()
	return nil
}

// Commit replaces the whole zone set atomically and invalidates the cache.
func (s *System) Commit(inputs []ZoneInput) error {
	if err := s.Zones.UpdateZones(inputs); err != nil {
		return err
	}
	s.Cache.Flush()
	return nil
}

// CommitZone replaces one zone and invalidates the cache.
func (s *System) CommitZone(origin string, records []Record) error {
	if err := s.Zones.UpdateZone(origin, records); err != nil {
		return err
	}
	s.Cache.Flush()
	return nil
}

// DeleteZone removes a zone and invalidates the cache.
func (s *System) DeleteZone(origin string) error {
	if err := s.Zones.DeleteZone(origin); err != nil {
		return err
	}
	s.Cache.Flush()
	return nil
}
