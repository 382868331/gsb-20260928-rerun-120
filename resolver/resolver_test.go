package resolver

import (
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

var nowMillis int64 = 1_000_000

func testSystem(t *testing.T) *System {
	t.Helper()
	sys := NewSystemClock("", 5, func() int64 { return atomic.LoadInt64(&nowMillis) })
	err := sys.Commit([]ZoneInput{
		{
			Origin: "example.test",
			Records: []Record{
				{Name: "example.test", Kind: A, Data: "10.0.0.1", TTL: 60},
				{Name: "www.example.test", Kind: CNAME, Data: "example.test", TTL: 60},
				{Name: "multi.example.test", Kind: A, Data: "10.0.0.2", TTL: 20},
				{Name: "multi.example.test", Kind: A, Data: "10.0.0.3", TTL: 10},
				{Name: "loop1.example.test", Kind: CNAME, Data: "loop2.example.test", TTL: 60},
				{Name: "loop2.example.test", Kind: CNAME, Data: "loop1.example.test", TTL: 60},
				// Invalid-looking parent-side record that must NOT serve the child.
				{Name: "gw.city.example.test", Kind: A, Data: "9.9.9.9", TTL: 60},
				{Name: "onlycname.example.test", Kind: CNAME, Data: "multi.example.test", TTL: 60},
			},
		},
		{
			Origin: "city.example.test",
			Records: []Record{
				{Name: "gw.city.example.test", Kind: A, Data: "10.1.0.1", TTL: 30},
			},
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return sys
}

func TestAliasResolution(t *testing.T) {
	sys := testSystem(t)
	r := sys.Resolver.Resolve("www.example.test", A)
	if r.Status != Success {
		t.Fatalf("status=%s err=%s", r.Status, r.Err)
	}
	if len(r.Answers) != 1 || r.Answers[0].Data != "10.0.0.1" {
		t.Fatalf("answers=%+v", r.Answers)
	}
	if len(r.Aliases) != 1 || r.Aliases[0].From != "www.example.test" || r.Aliases[0].To != "example.test" {
		t.Fatalf("aliases=%+v", r.Aliases)
	}
}

func TestAliasLoopShowsPath(t *testing.T) {
	sys := testSystem(t)
	r := sys.Resolver.Resolve("loop1.example.test", A)
	if r.Status != AliasLoop {
		t.Fatalf("status=%s", r.Status)
	}
	want := []string{"loop1.example.test", "loop2.example.test", "loop1.example.test"}
	if !reflect.DeepEqual(r.LoopPath, want) {
		t.Fatalf("loop path=%v want %v", r.LoopPath, want)
	}
	if !strings.Contains(r.Err, "loop1.example.test -> loop2.example.test -> loop1.example.test") {
		t.Fatalf("err=%q does not show path", r.Err)
	}
}

func TestDelegatedSubzoneIgnoresParentRecord(t *testing.T) {
	sys := testSystem(t)
	r := sys.Resolver.Resolve("gw.city.example.test", A)
	if r.Status != Success {
		t.Fatalf("status=%s", r.Status)
	}
	if r.Answers[0].Data != "10.1.0.1" {
		t.Fatalf("got %s, child subzone record must win over parent's 9.9.9.9", r.Answers[0].Data)
	}
	if r.Authority != "city.example.test" {
		t.Fatalf("authority=%s", r.Authority)
	}
}

func TestNXDOMAINAndNODATAAreDistinct(t *testing.T) {
	sys := testSystem(t)
	nx := sys.Resolver.Resolve("missing.example.test", A)
	if nx.Status != NXDomain {
		t.Fatalf("got %s want NXDOMAIN", nx.Status)
	}
	nd := sys.Resolver.Resolve("example.test", CNAME) // name exists, no CNAME
	if nd.Status != NoData {
		t.Fatalf("got %s want NODATA", nd.Status)
	}
}

func TestPositiveAndNegativeCachesExpireSeparately(t *testing.T) {
	sys := testSystem(t)
	// Positive result (TTL 10) cached.
	pos := sys.Resolver.Resolve("multi.example.test", A)
	if pos.Status != Success || pos.TTL != 10 {
		t.Fatalf("pos=%+v", pos)
	}
	// Negative result cached with dedicated negative TTL (5).
	neg := sys.Resolver.Resolve("nope.example.test", A)
	if neg.Status != NXDomain || neg.TTL != 5 {
		t.Fatalf("neg=%+v", neg)
	}
	p, n := sys.Cache.Len()
	if p != 1 || n != 1 {
		t.Fatalf("cache pos=%d neg=%d, want 1/1", p, n)
	}
	// Advance past negative TTL only; positive remains live.
	atomic.AddInt64(&nowMillis, 6)
	p, n = sys.Cache.Len()
	if p != 1 || n != 0 {
		t.Fatalf("after neg expiry pos=%d neg=%d, want 1/0", p, n)
	}
	r := sys.Resolver.Resolve("nope.example.test", A)
	if r.Status != NXDomain {
		t.Fatalf("re-resolve negative: %s", r.Status)
	}
	// Past positive TTL as well.
	atomic.AddInt64(&nowMillis, 10)
	p, _ = sys.Cache.Len()
	if p != 0 {
		t.Fatalf("positive entry should expire, pos=%d", p)
	}
}

func TestUpdateIsAtomicNoMixedVersions(t *testing.T) {
	sys := testSystem(t)
	// Warm cache from generation 2 (seed is version 1; make it 2).
	sys.CommitZone("example.test", []Record{
		{Name: "www.example.test", Kind: CNAME, Data: "example.test", TTL: 60},
		{Name: "example.test", Kind: A, Data: "10.0.0.1", TTL: 60},
	})
	before := sys.Resolver.Resolve("www.example.test", A)
	if before.Answers[0].Data != "10.0.0.1" {
		t.Fatalf("baseline %+v", before.Answers)
	}
	// Concurrent readers during a full replacement must never see a partial
	// set: either old coherent version or new coherent version answers only.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				r := sys.Resolver.Resolve("www.example.test", A)
				switch {
				case r.Status == Success && r.Answers[0].Data == "10.0.0.1":
				case r.Status == Success && r.Answers[0].Data == "10.0.0.9":
				case r.Status == NXDomain, r.Status == NoData:
				default:
					t.Errorf("torn read: status=%s answers=%+v aliases=%+v", r.Status, r.Answers, r.Aliases)
					return
				}
			}
		}()
	}
	err := sys.Commit([]ZoneInput{{
		Origin: "example.test",
		Records: []Record{
			{Name: "www.example.test", Kind: CNAME, Data: "example.test", TTL: 60},
			{Name: "example.test", Kind: A, Data: "10.0.0.9", TTL: 60},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
	after := sys.Resolver.Resolve("www.example.test", A)
	if after.Answers[0].Data != "10.0.0.9" {
		t.Fatalf("post-update answer=%s want 10.0.0.9", after.Answers[0].Data)
	}
}

func TestLateCacheEntryFromOldGenerationRejected(t *testing.T) {
	sys := testSystem(t)
	genV := sys.Zones.CurrentVersion()

	// Simulate a query whose result arrives late: computed against the old
	// generation, but zones have since advanced.
	oldRes := &Result{Status: Success, Question: "late.example.test", QKind: A,
		Answers: []Answer{{Name: "late.example.test", Kind: A, Data: "1.1.1.1", TTL: 60}}, TTL: 60}
	sys.CommitZone("example.test", []Record{
		{Name: "late.example.test", Kind: A, Data: "2.2.2.2", TTL: 60},
	})
	// Attempt to inject stale entry; source version is behind current.
	sys.Cache.store(genV, oldRes, atomic.LoadInt64(&nowMillis))
	p, _ := sys.Cache.Len()
	if p != 0 {
		t.Fatalf("stale late entry must not populate cache, got pos=%d", p)
	}
	r := sys.Resolver.Resolve("late.example.test", A)
	if r.Answers[0].Data != "2.2.2.2" {
		t.Fatalf("late stale answer leaked: %s", r.Answers[0].Data)
	}
	snap, ok := sys.Zones.ZoneSnapshot("example.test")
	if !ok {
		t.Fatal("zone missing")
	}
	_ = snap
}

func TestPersistenceAndRestart(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "zones.json")
	sys := NewSystem(file, 7)
	if err := sys.Commit([]ZoneInput{
		{Origin: "example.test", Records: []Record{
			{Name: "example.test", Kind: A, Data: "10.0.0.1", TTL: 60},
			{Name: "www.example.test", Kind: CNAME, Data: "example.test", TTL: 60},
		}},
		{Origin: "city.example.test", Records: []Record{
			{Name: "gw.city.example.test", Kind: A, Data: "10.1.0.1", TTL: 30},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// Restart: new process-equivalent store reading the same file.
	restarted := NewSystem(file, 7)
	if err := restarted.Load(); err != nil {
		t.Fatalf("restart load: %v", err)
	}
	cases := map[string]string{
		"www.example.test":     "10.0.0.1",
		"gw.city.example.test": "10.1.0.1",
	}
	for q, want := range cases {
		r := restarted.Resolver.Resolve(q, A)
		if r.Status != Success || r.Answers[0].Data != want {
			t.Fatalf("after restart %s: status=%s answers=%+v", q, r.Status, r.Answers)
		}
	}
	// Parent still must not shadow child after restart.
	r := restarted.Resolver.Resolve("gw.city.example.test", A)
	if r.Authority != "city.example.test" {
		t.Fatalf("authority after restart=%s", r.Authority)
	}
	if v := restarted.Zones.CurrentVersion(); v != 1 {
		t.Fatalf("version after restart=%d want 1", v)
	}
}

func TestAliasCrossingZones(t *testing.T) {
	sys := testSystem(t)
	r := sys.Resolver.Resolve("onlycname.example.test", A)
	if r.Status != Success {
		t.Fatalf("status=%s err=%s", r.Status, r.Err)
	}
	if r.Answers[0].Data != "10.0.0.2" {
		t.Fatalf("cross-name alias answer=%+v", r.Answers)
	}
}
