package resolver

import (
	"strings"
	"sync"
	"testing"
)

// TestBatchAtomicAcrossZones: a batch that moves a delegation (parent NS
// removed, parent-side record added, child zone removed) must be observed
// as all-old or all-new. Concurrent queries may see either answer, but
// never the intermediate state (child zone gone, parent not yet updated)
// which would surface as NXDOMAIN.
func TestBatchAtomicAcrossZones(t *testing.T) {
	r := NewResolver(NewCache())
	mkParentV1 := func() *Zone {
		return NewZone("example.test", 1, 60, []Record{
			{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
		})
	}
	mkParentV2 := func() *Zone {
		return NewZone("example.test", 2, 60, []Record{
			{Name: "web.shop.example.test", Type: TypeA, Value: "10.9.9.9", TTL: 300},
		})
	}
	mkChild := func() *Zone {
		return NewZone("shop.example.test", 1, 60, []Record{
			{Name: "web.shop.example.test", Type: TypeA, Value: "10.1.1.1", TTL: 300},
		})
	}
	r.AddZone(mkParentV1())
	r.AddZone(mkChild())

	delegate := Batch{Upsert: []*Zone{mkParentV1(), mkChild()}, Note: "delegate shop"}
	absorb := Batch{Upsert: []*Zone{mkParentV2()}, Remove: []string{"shop.example.test"}, Note: "absorb shop"}

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
				res, err := r.Resolve("web.shop.example.test")
				if err != nil {
					t.Errorf("intermediate state leaked: %v", err)
					return
				}
				got := res.Records[0].Value
				if got != "10.1.1.1" && got != "10.9.9.9" {
					t.Errorf("spliced answer: %s", got)
					return
				}
			}
		}()
	}
	for i := 0; i < 500; i++ {
		if _, err := r.ApplyBatch(absorb); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ApplyBatch(delegate); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestPreviewBatchDoesNotMutate: dry-run reports zone diffs and doomed
// cache entries (including negative ones) without changing live state.
func TestPreviewBatchDoesNotMutate(t *testing.T) {
	r := buildTree()
	if _, err := r.Resolve("www.example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("nope.example.test"); err == nil {
		t.Fatal("want NXDOMAIN")
	}
	viewBefore := r.CurrentViewID()

	p, err := r.PreviewBatch(Batch{
		Upsert: []*Zone{NewZone("example.test", 2, 60, []Record{
			{Name: "www.example.test", Type: TypeA, Value: "10.0.0.2", TTL: 300},
		})},
		Note: "readdress www",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Changes) != 1 || p.Changes[0].Kind != ChangeUpdated ||
		p.Changes[0].Zone != "example.test" || p.Changes[0].OldVersion != 1 || p.Changes[0].NewVersion != 2 {
		t.Fatalf("bad changes: %+v", p.Changes)
	}
	var doomedPos, doomedNeg bool
	for _, e := range p.Invalidate {
		if e.Name == "www.example.test" && !e.Negative {
			doomedPos = true
		}
		if e.Name == "nope.example.test" && e.Negative {
			doomedNeg = true
		}
	}
	if !doomedPos || !doomedNeg {
		t.Fatalf("preview missed doomed entries: %+v", p.Invalidate)
	}
	// Nothing applied: view unchanged, cache intact, old answer still served.
	if r.CurrentViewID() != viewBefore {
		t.Fatal("preview changed the view")
	}
	if r.Cache().Get("nope.example.test", TypeA) == nil {
		t.Fatal("preview invalidated the cache")
	}
	res, err := r.Resolve("www.example.test")
	if err != nil || res.Records[0].Value != "10.0.0.1" {
		t.Fatalf("preview mutated state: %+v %v", res, err)
	}
}

// TestNegativeCacheInvalidatedOnChange: a cached NXDOMAIN must not survive
// a rule change that adds the name.
func TestNegativeCacheInvalidatedOnChange(t *testing.T) {
	r := buildTree()
	if _, err := r.Resolve("nope.example.test"); err == nil {
		t.Fatal("want NXDOMAIN")
	}
	if r.Cache().Get("nope.example.test", TypeA) == nil {
		t.Fatal("negative answer not cached")
	}
	if _, err := r.ApplyBatch(Batch{Upsert: []*Zone{NewZone("example.test", 2, 60, []Record{
		{Name: "nope.example.test", Type: TypeA, Value: "10.5.5.5", TTL: 300},
	})}, Note: "add nope"}); err != nil {
		t.Fatal(err)
	}
	if r.Cache().Get("nope.example.test", TypeA) != nil {
		t.Fatal("stale negative entry survived the zone change")
	}
	res, err := r.Resolve("nope.example.test")
	if err != nil || res.Records[0].Value != "10.5.5.5" {
		t.Fatalf("stale negative answer still served: %+v %v", res, err)
	}
}

// TestRevokeDelegationAndRollback: removing a zone invalidates its cache
// entries; rolling back to an older zone version (version numbers decrease)
// invalidates just the same.
func TestRevokeDelegationAndRollback(t *testing.T) {
	r := buildTree()
	res, err := r.Resolve("web.shop.example.test")
	if err != nil || res.Records[0].Value != "10.1.1.1" {
		t.Fatalf("setup: %+v %v", res, err)
	}

	// Revoke the delegation properly: parent drops the NS and serves the
	// name itself; the child zone is removed in the same batch.
	if _, err := r.ApplyBatch(Batch{
		Upsert: []*Zone{NewZone("example.test", 2, 60, []Record{
			{Name: "www.example.test", Type: TypeA, Value: "10.0.0.1", TTL: 300},
			{Name: "web.shop.example.test", Type: TypeA, Value: "10.9.9.9", TTL: 300},
		})},
		Remove: []string{"shop.example.test"},
		Note:   "revoke shop delegation",
	}); err != nil {
		t.Fatal(err)
	}
	if e := r.Cache().Get("web.shop.example.test", TypeA); e != nil {
		t.Fatalf("cache entry from removed zone survived: %+v", e)
	}
	res, err = r.Resolve("web.shop.example.test")
	if err != nil || res.Records[0].Value != "10.9.9.9" || res.Source.Zone != "example.test" {
		t.Fatalf("revoked delegation not in effect: %+v %v", res, err)
	}

	// Roll back the parent to version 1 content (older version number):
	// the v2-sourced cache entry must be invalidated by inequality.
	if _, err := r.ApplyBatch(Batch{Upsert: []*Zone{
		NewZone("example.test", 1, 60, []Record{
			{Name: "www.example.test", Type: TypeA, Value: "10.0.0.1", TTL: 300},
			{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
		}),
		NewZone("shop.example.test", 1, 60, []Record{
			{Name: "web.shop.example.test", Type: TypeA, Value: "10.1.1.1", TTL: 300},
		}),
	}, Note: "rollback to v1"}); err != nil {
		t.Fatal(err)
	}
	res, err = r.Resolve("web.shop.example.test")
	if err != nil || res.Records[0].Value != "10.1.1.1" || res.Source.Zone != "shop.example.test" {
		t.Fatalf("rollback not in effect: %+v %v", res, err)
	}
}

// TestCrossZoneAlias: a CNAME in the parent zone may point into a child
// zone; the chain is followed within one consistent view.
func TestCrossZoneAlias(t *testing.T) {
	r := NewResolver(NewCache())
	r.AddZone(NewZone("example.test", 1, 60, []Record{
		{Name: "mirror.example.test", Type: TypeCNAME, Value: "web.shop.example.test", TTL: 300},
		{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
	}))
	r.AddZone(NewZone("shop.example.test", 1, 60, []Record{
		{Name: "web.shop.example.test", Type: TypeA, Value: "10.1.1.1", TTL: 300},
	}))
	res, err := r.Resolve("mirror.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].Value != "10.1.1.1" || res.Source.Zone != "shop.example.test" {
		t.Fatalf("cross-zone alias wrong: %+v", res)
	}
	if len(res.Chain) != 1 || res.Chain[0].Name != "mirror.example.test" {
		t.Fatalf("alias path not reported: %+v", res.Chain)
	}
}

// TestHistoryViewQueryAndRestart: every batch is recorded; past views stay
// queryable in memory; after save/restart the history still explains which
// zone set produced a past answer.
func TestHistoryViewQueryAndRestart(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/snap.json"

	r := buildTree()
	base := r.CurrentViewID()
	res1, err := r.Resolve("www.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if res1.View != base {
		t.Fatalf("answer not attributed to current view: %d vs %d", res1.View, base)
	}

	rec, err := r.ApplyBatch(Batch{Upsert: []*Zone{NewZone("example.test", 2, 60, []Record{
		{Name: "www.example.test", Type: TypeA, Value: "10.0.0.2", TTL: 300},
	})}, Note: "readdress www"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Zones["example.test"] != 2 || rec.Zones["shop.example.test"] != 1 {
		t.Fatalf("history record missing zone versions: %+v", rec.Zones)
	}

	// Historical view query reproduces the old answer from the old zones.
	old, err := r.ResolveInView(base, "www.example.test")
	if err != nil || old.Records[0].Value != "10.0.0.1" || old.View != base {
		t.Fatalf("historical view query wrong: %+v %v", old, err)
	}
	cur, err := r.Resolve("www.example.test")
	if err != nil || cur.Records[0].Value != "10.0.0.2" {
		t.Fatalf("current answer wrong: %+v %v", cur, err)
	}

	if err := r.SaveSnapshot(path); err != nil {
		t.Fatal(err)
	}
	r2, err := LoadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	h := r2.History()
	if len(h) == 0 {
		t.Fatal("history lost across restart")
	}
	var found bool
	for _, v := range h {
		if v.ID == rec.ID && v.Note == "readdress www" && v.Zones["example.test"] == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("restarted history cannot explain view %d: %+v", rec.ID, h)
	}
	if r2.CurrentViewID() != rec.ID {
		t.Fatalf("view ID not restored: %d vs %d", r2.CurrentViewID(), rec.ID)
	}
	// The restarted resolver keeps resolving from the restored zone set.
	res, err := r2.Resolve("www.example.test")
	if err != nil || res.Records[0].Value != "10.0.0.2" {
		t.Fatalf("post-restart answer wrong: %+v %v", res, err)
	}
	if !strings.Contains(h[len(h)-1].String(), "example.test@v2") {
		t.Fatalf("history record not human-explainable: %s", h[len(h)-1])
	}
}
