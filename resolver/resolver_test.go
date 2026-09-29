package resolver

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// buildTree creates root -> example.test -> shop.example.test, including a
// deliberately stale parent-side record below the shop delegation cut.
func buildTree() *Resolver {
	r := NewResolver(NewCache())
	r.AddZone(NewZone("", 1, 60, []Record{
		{Name: "example.test", Type: TypeNS, Value: "ns1.example.test", TTL: 3600},
	}))
	r.AddZone(NewZone("example.test", 1, 60, []Record{
		{Name: "www.example.test", Type: TypeA, Value: "10.0.0.1", TTL: 300},
		{Name: "api.example.test", Type: TypeCNAME, Value: "www.example.test", TTL: 300},
		{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
		{Name: "web.shop.example.test", Type: TypeA, Value: "10.9.9.9", TTL: 300}, // stale, below cut
	}))
	r.AddZone(NewZone("shop.example.test", 1, 60, []Record{
		{Name: "web.shop.example.test", Type: TypeA, Value: "10.1.1.1", TTL: 300},
	}))
	return r
}

func TestBasicAndAliasResolution(t *testing.T) {
	r := buildTree()
	res, err := r.Resolve("www.example.test")
	if err != nil || res.Records[0].Value != "10.0.0.1" {
		t.Fatalf("A: %v %v", res, err)
	}
	res, err = r.Resolve("api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].Value != "10.0.0.1" || len(res.Chain) != 1 || res.Chain[0].Value != "www.example.test" {
		t.Fatalf("alias chain wrong: %+v", res)
	}
}

func TestAliasLoopReportsPath(t *testing.T) {
	r := NewResolver(NewCache())
	r.AddZone(NewZone("loop.test", 1, 60, []Record{
		{Name: "a.loop.test", Type: TypeCNAME, Value: "b.loop.test", TTL: 60},
		{Name: "b.loop.test", Type: TypeCNAME, Value: "c.loop.test", TTL: 60},
		{Name: "c.loop.test", Type: TypeCNAME, Value: "a.loop.test", TTL: 60},
	}))
	_, err := r.Resolve("a.loop.test")
	re, ok := err.(*ResolveError)
	if !ok || re.Code != ErrLoop {
		t.Fatalf("want loop error, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"a.loop.test", "b.loop.test", "c.loop.test"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("loop path %q missing %q", msg, want)
		}
	}
	// Path must end where it started to show the cycle.
	if re.Path[0] != re.Path[len(re.Path)-1] {
		t.Fatalf("path does not close the loop: %v", re.Path)
	}
	t.Logf("loop path: %s", msg)
}

func TestCacheExpiryVsNegative(t *testing.T) {
	now := time.Now()
	r := buildTree()
	r.Cache().SetClock(func() time.Time { return now })

	if _, err := r.Resolve("www.example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("nope.example.test"); err == nil {
		t.Fatal("want NXDOMAIN")
	}
	// Both cached now: drop the zones, answers must still come from cache.
	pos := r.Cache().Get("www.example.test", TypeA)
	neg := r.Cache().Get("nope.example.test", TypeA)
	if pos == nil || pos.Negative || neg == nil || !neg.Negative {
		t.Fatalf("cache entries wrong: pos=%v neg=%v", pos, neg)
	}
	// Negative TTL (60s) expires before positive TTL (300s).
	now = now.Add(61 * time.Second)
	if r.Cache().Get("nope.example.test", TypeA) != nil {
		t.Fatal("negative entry should have expired")
	}
	if r.Cache().Get("www.example.test", TypeA) == nil {
		t.Fatal("positive entry should still be live")
	}
	now = now.Add(300 * time.Second)
	if r.Cache().Get("www.example.test", TypeA) != nil {
		t.Fatal("positive entry should have expired")
	}
}

func TestSubzoneIgnoresParentStaleRecord(t *testing.T) {
	r := buildTree()
	res, err := r.Resolve("web.shop.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].Value != "10.1.1.1" {
		t.Fatalf("used parent stale record: %+v", res.Records[0])
	}
	if res.Source.Zone != "shop.example.test" {
		t.Fatalf("answer not from child zone: %v", res.Source)
	}
}

func TestAtomicZoneUpdate(t *testing.T) {
	r := NewResolver(NewCache())
	mk := func(v uint64, ip string) *Zone {
		return NewZone("ex.test", v, 60, []Record{
			{Name: "a.ex.test", Type: TypeA, Value: ip, TTL: 1},
			{Name: "b.ex.test", Type: TypeA, Value: ip, TTL: 1},
		})
	}
	r.AddZone(mk(1, "10.0.0.1"))

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
				z := r.zones["ex.test"].Current()
				a, b := z.lookup("a.ex.test")[0], z.lookup("b.ex.test")[0]
				if a.Value != b.Value {
					t.Errorf("spliced zone versions: a=%s b=%s", a.Value, b.Value)
					return
				}
			}
		}()
	}
	for v := uint64(2); v < 2000; v++ {
		r.UpdateZone(mk(v, strings.Repeat("1", int(v%9)+1)))
	}
	close(stop)
	wg.Wait()
}

func TestLateCacheUpdateRejected(t *testing.T) {
	c := NewCache()
	fresh := &CacheEntry{
		Name: "x.test", Type: TypeA, Negative: false,
		Records: []Record{{Name: "x.test", Type: TypeA, Value: "10.0.0.2", TTL: 300}},
		Source:  Source{Zone: "test", Version: 5},
		Expires: c.now().Add(time.Hour),
	}
	if !c.Put(fresh) {
		t.Fatal("fresh put rejected")
	}
	stale := &CacheEntry{
		Name: "x.test", Type: TypeA,
		Records: []Record{{Name: "x.test", Type: TypeA, Value: "10.0.0.1", TTL: 300}},
		Source:  Source{Zone: "test", Version: 3}, // late update from older version
		Expires: c.now().Add(time.Hour),
	}
	if c.Put(stale) {
		t.Fatal("late update accepted")
	}
	if got := c.Get("x.test", TypeA); got.Records[0].Value != "10.0.0.2" || got.Source.Version != 5 {
		t.Fatalf("cache clobbered by stale update: %+v", got)
	}
	// A newer version must still win.
	newer := *fresh
	newer.Source.Version = 6
	if !c.Put(&newer) {
		t.Fatal("newer update rejected")
	}
}

func TestParentChildSourceAndRestart(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/snap.json"

	r := buildTree()
	res, err := r.Resolve("web.shop.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Source.Zone != "shop.example.test" || res.Source.Version != 1 {
		t.Fatalf("wrong source: %v", res.Source)
	}
	if _, err := r.Resolve("api.example.test"); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveSnapshot(path); err != nil {
		t.Fatal(err)
	}

	// "Restart": brand-new resolver from the snapshot only.
	r2, err := LoadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := r2.Resolve("web.shop.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if res2.Records[0].Value != "10.1.1.1" {
		t.Fatalf("wrong answer after restart: %+v", res2.Records[0])
	}
	if !res2.FromCache {
		t.Fatal("expected restored cache hit after restart")
	}
	if res2.Source.Zone != "shop.example.test" {
		t.Fatalf("source lost across restart: %v", res2.Source)
	}
	// Zone data itself must also survive: clear the cache and re-resolve.
	r2.cache = NewCache()
	res3, err := r2.Resolve("web.shop.example.test")
	if err != nil || res3.Records[0].Value != "10.1.1.1" || res3.FromCache {
		t.Fatalf("zone restore wrong: %+v %v", res3, err)
	}
}

