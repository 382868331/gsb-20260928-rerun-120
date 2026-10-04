package resolver

import (
	"strings"
	"testing"
)

// openShopCandidate opens a candidate that re-points www and adds a name.
func openShopCandidate(t *testing.T, r *Resolver, id string) *Candidate {
	t.Helper()
	c, err := r.OpenCandidate(id, []*Zone{
		NewZone("example.test", 2, 60, []Record{
			{Name: "www.example.test", Type: TypeA, Value: "10.0.0.2", TTL: 300},
			{Name: "api.example.test", Type: TypeCNAME, Value: "www.example.test", TTL: 300},
			{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
			{Name: "new.example.test", Type: TypeA, Value: "10.0.0.7", TTL: 300},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestCandidateIsolationAndCompare: the same query runs against the live
// view and the candidate view; the candidate's cache is namespaced and a
// candidate negative result never leaks into the formal cache.
func TestCandidateIsolationAndCompare(t *testing.T) {
	r := buildTree()
	openShopCandidate(t, r, "c1")

	cmp, err := r.CompareQuery("c1", "www.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if !cmp.Differs || cmp.Live.Records[0].Value != "10.0.0.1" ||
		cmp.Candidate.Records[0].Value != "10.0.0.2" {
		t.Fatalf("bad comparison: %+v", cmp)
	}
	if cmp.Candidate.View != "candidate:c1" || cmp.Live.View != "" {
		t.Fatalf("results not tagged with their view: %+v", cmp)
	}
	// Same answer for an unchanged name.
	cmp, err = r.CompareQuery("c1", "web.shop.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Differs || cmp.Candidate.Records[0].Value != "10.1.1.1" {
		t.Fatalf("unchanged name should not differ: %+v", cmp)
	}

	// Candidate negative result: new name exists only in the candidate.
	cmp, err = r.CompareQuery("c1", "new.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if !cmp.Differs || cmp.LiveErr != ErrNXDOMAIN || cmp.Candidate.Records[0].Value != "10.0.0.7" {
		t.Fatalf("bad negative comparison: %+v", cmp)
	}
	// A name missing in the candidate view caches negatively there only.
	if _, err := r.ResolveCandidate("c1", "ghost.example.test"); err == nil {
		t.Fatal("want NXDOMAIN in candidate view")
	}
	e := r.Cache().GetView("candidate:c1", "ghost.example.test", TypeA)
	if e == nil || !e.Negative || e.View != "candidate:c1" {
		t.Fatalf("candidate negative not cached under its view: %+v", e)
	}
	if e := r.Cache().Get("ghost.example.test", TypeA); e != nil {
		t.Fatalf("candidate negative leaked into live cache: %+v", e)
	}
	// And the live view still answers from the formal zones.
	if _, err := r.Resolve("ghost.example.test"); err == nil {
		t.Fatal("live view should still NXDOMAIN")
	}
}

// TestCandidateViewConsistency: while a candidate is being observed the
// formal zones move on; the candidate view still resolves from its pinned
// base and never splices parent/child delegations from two time points.
func TestCandidateViewConsistency(t *testing.T) {
	r := buildTree()
	c := openShopCandidate(t, r, "c1")

	// Formal changes during observation: the child zone advances, and a
	// new sibling zone is delegated from the root and appears.
	if err := r.UpdateZone(NewZone("shop.example.test", 2, 60, []Record{
		{Name: "web.shop.example.test", Type: TypeA, Value: "10.1.1.9", TTL: 300},
	})); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateZone(NewZone("", 2, 60, []Record{
		{Name: "example.test", Type: TypeNS, Value: "ns1.example.test", TTL: 3600},
		{Name: "shop2.example.test", Type: TypeNS, Value: "ns1.shop2.example.test", TTL: 3600},
	})); err != nil {
		t.Fatal(err)
	}
	r.AddZone(NewZone("shop2.example.test", 1, 60, []Record{
		{Name: "web.shop2.example.test", Type: TypeA, Value: "10.2.2.2", TTL: 300},
	}))

	stale, err := r.StaleDependencies("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 3 {
		t.Fatalf("want root+shop changed and shop2 appeared, got %+v", stale)
	}
	var sawChanged, sawNew bool
	for _, s := range stale {
		if s.Zone == "shop.example.test" && s.BaseVersion == 1 && s.CurrentVersion == 2 {
			sawChanged = true
		}
		if s.Zone == "shop2.example.test" && s.BaseVersion == 0 && s.CurrentVersion == 1 {
			sawNew = true
		}
	}
	if !sawChanged || !sawNew {
		t.Fatalf("stale deps wrong: %+v", stale)
	}

	// The candidate view still sees the child at v1: a single pinned
	// snapshot, not the candidate's parent spliced with the formal
	// child v2.
	res, err := r.ResolveCandidate("c1", "web.shop.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].Value != "10.1.1.1" || res.Source.Zone != "shop.example.test" || res.Source.Version != 1 {
		t.Fatalf("candidate view spliced versions: %+v", res)
	}
	// The new formal zone is invisible inside the old-based candidate.
	if _, err := r.ResolveCandidate("c1", "web.shop2.example.test"); err == nil {
		t.Fatal("candidate view should not see zones newer than its base")
	}

	// Rebase re-anchors on the current formal set and clears the
	// candidate's cache; afterwards the new zone is visible.
	if _, err := r.RebaseCandidate("c1"); err != nil {
		t.Fatal(err)
	}
	if e := r.Cache().GetView("candidate:c1", "web.shop.example.test", TypeA); e != nil {
		t.Fatalf("rebase kept candidate cache: %+v", e)
	}
	if c.Base["shop.example.test"] != 2 || c.Base["shop2.example.test"] != 1 {
		t.Fatalf("base not re-anchored: %+v", c.Base)
	}
	if stale, err := r.StaleDependencies("c1"); err != nil || len(stale) != 0 {
		t.Fatalf("still stale after rebase: %+v %v", stale, err)
	}
	if _, err := r.ResolveCandidate("c1", "web.shop2.example.test"); err != nil {
		t.Fatalf("rebased candidate should see new zone: %v", err)
	}
	// And the child is now seen at its current formal version.
	res, err = r.ResolveCandidate("c1", "web.shop.example.test")
	if err != nil || res.Records[0].Value != "10.1.1.9" || res.Source.Version != 2 {
		t.Fatalf("rebased candidate view wrong: %+v %v", res, err)
	}
}

// TestCandidateAdoptRecordsBase: adoption is refused while dependencies
// are stale; after rebase, adoption publishes the overrides and records
// the exact zone set the candidate was based on.
func TestCandidateAdoptRecordsBase(t *testing.T) {
	r := buildTree()
	c := openShopCandidate(t, r, "c1")
	if err := r.UpdateZone(NewZone("shop.example.test", 2, 60, []Record{
		{Name: "web.shop.example.test", Type: TypeA, Value: "10.1.1.9", TTL: 300},
	})); err != nil {
		t.Fatal(err)
	}
	if err := r.AdoptCandidate("c1"); err == nil {
		t.Fatal("adoption with stale dependencies must be refused")
	}
	if _, err := r.RebaseCandidate("c1"); err != nil {
		t.Fatal(err)
	}
	if err := r.AdoptCandidate("c1"); err != nil {
		t.Fatal(err)
	}
	res, err := r.Resolve("www.example.test")
	if err != nil || res.Records[0].Value != "10.0.0.2" || res.Source.Version != 2 {
		t.Fatalf("adopted override not live: %+v %v", res, err)
	}
	if c.Status != CandidateAdopted {
		t.Fatalf("status = %q", c.Status)
	}
	// The recorded base is the set the candidate was actually based on:
	// example.test pinned at v1 at open, shop.example.test re-anchored to
	// v2 by the rebase.
	if c.AdoptedFrom["example.test"] != 1 || c.AdoptedFrom["shop.example.test"] != 2 {
		t.Fatalf("adopted-from base wrong: %+v", c.AdoptedFrom)
	}
	if _, err := r.ResolveCandidate("c1", "www.example.test"); err == nil {
		t.Fatal("adopted candidate must not accept queries")
	}
}

// TestCandidateAbandonKeepsBasis: abandoning keeps the diff basis for
// inspection and drops the candidate's cache.
func TestCandidateAbandonKeepsBasis(t *testing.T) {
	r := buildTree()
	c := openShopCandidate(t, r, "c1")
	if _, err := r.ResolveCandidate("c1", "www.example.test"); err != nil {
		t.Fatal(err)
	}
	if err := r.AbandonCandidate("c1"); err != nil {
		t.Fatal(err)
	}
	if c.Status != CandidateAbandoned {
		t.Fatalf("status = %q", c.Status)
	}
	if len(c.Basis) != 1 || c.Basis[0].Zone != "example.test" ||
		c.Basis[0].FromVersion != 1 || c.Basis[0].ToVersion != 2 {
		t.Fatalf("basis lost: %+v", c.Basis)
	}
	var addedWWW bool
	for _, rec := range c.Basis[0].Diff.Added {
		if rec.Name == "www.example.test" && rec.Value == "10.0.0.2" {
			addedWWW = true
		}
	}
	if !addedWWW {
		t.Fatalf("basis diff missing www change: %+v", c.Basis[0].Diff)
	}
	if e := r.Cache().GetView("candidate:c1", "www.example.test", TypeA); e != nil {
		t.Fatalf("abandoned candidate kept cache: %+v", e)
	}
	if _, err := r.ResolveCandidate("c1", "www.example.test"); err == nil {
		t.Fatal("abandoned candidate must not accept queries")
	}
}

// TestOverlappingCandidates: two candidates may change the same zone;
// each view is isolated from the other and from the live view.
func TestOverlappingCandidates(t *testing.T) {
	r := buildTree()
	mk := func(id, value string, version uint64) {
		t.Helper()
		if _, err := r.OpenCandidate(id, []*Zone{
			NewZone("example.test", version, 60, []Record{
				{Name: "www.example.test", Type: TypeA, Value: value, TTL: 300},
				{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
			}),
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("c1", "10.0.0.2", 2)
	mk("c2", "10.0.0.3", 3)

	for id, want := range map[string]string{"c1": "10.0.0.2", "c2": "10.0.0.3"} {
		res, err := r.ResolveCandidate(id, "www.example.test")
		if err != nil || res.Records[0].Value != want {
			t.Fatalf("%s: got %+v %v", id, res, err)
		}
	}
	res, err := r.Resolve("www.example.test")
	if err != nil || res.Records[0].Value != "10.0.0.1" {
		t.Fatalf("live view disturbed by candidates: %+v %v", res, err)
	}
}

// TestCandidateCrossZoneAlias: a candidate may introduce an alias pointing
// into another zone; the chain and final source are reported.
func TestCandidateCrossZoneAlias(t *testing.T) {
	r := buildTree()
	r.AddZone(NewZone("cdn.test", 1, 60, []Record{
		{Name: "edge.cdn.test", Type: TypeA, Value: "192.0.2.5", TTL: 300},
	}))
	// The root formally delegates the CDN zone.
	if err := r.UpdateZone(NewZone("", 2, 60, []Record{
		{Name: "example.test", Type: TypeNS, Value: "ns1.example.test", TTL: 3600},
		{Name: "cdn.test", Type: TypeNS, Value: "ns1.cdn.test", TTL: 3600},
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := r.OpenCandidate("c1", []*Zone{
		NewZone("example.test", 2, 60, []Record{
			{Name: "www.example.test", Type: TypeA, Value: "10.0.0.1", TTL: 300},
			{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
			{Name: "img.example.test", Type: TypeCNAME, Value: "edge.cdn.test", TTL: 300},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	res, err := r.ResolveCandidate("c1", "img.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].Value != "192.0.2.5" || res.Source.Zone != "cdn.test" {
		t.Fatalf("cross-zone alias in candidate wrong: %+v", res)
	}
	if len(res.Chain) != 1 || res.Chain[0].Value != "edge.cdn.test" {
		t.Fatalf("alias path lost: %+v", res.Chain)
	}
	if _, err := r.Resolve("img.example.test"); err == nil {
		t.Fatal("candidate alias leaked into live view")
	}
}

// TestCandidateSurvivesRestart: candidates persist across snapshot
// save/load, so the live-vs-candidate comparison still works after a
// reopen, and historical answers stay explainable per their view.
func TestCandidateSurvivesRestart(t *testing.T) {
	r := buildTree()
	openShopCandidate(t, r, "c1")
	path := t.TempDir() + "/snap.json"
	if err := r.SaveSnapshot(path); err != nil {
		t.Fatal(err)
	}
	r2, err := LoadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	cmp, err := r2.CompareQuery("c1", "www.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if !cmp.Differs || cmp.Candidate.Records[0].Value != "10.0.0.2" {
		t.Fatalf("comparison broken after reopen: %+v", cmp)
	}
	// Historical answers are still interpreted against their own view.
	past, err := r2.ResolveAt("www.example.test", map[string]uint64{"example.test": 1})
	if err != nil || past.Records[0].Value != "10.0.0.1" || past.Source.Version != 1 {
		t.Fatalf("historical view broken after reopen: %+v %v", past, err)
	}
}

// TestCandidateNewZone: a candidate may introduce a zone that does not
// exist formally; the delegation is followed inside the candidate view.
func TestCandidateNewZone(t *testing.T) {
	r := buildTree()
	if _, err := r.OpenCandidate("c1", []*Zone{
		NewZone("example.test", 2, 60, []Record{
			{Name: "www.example.test", Type: TypeA, Value: "10.0.0.1", TTL: 300},
			{Name: "lab.example.test", Type: TypeNS, Value: "ns1.lab.example.test", TTL: 3600},
		}),
		NewZone("lab.example.test", 1, 60, []Record{
			{Name: "app.lab.example.test", Type: TypeA, Value: "10.7.7.7", TTL: 300},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	res, err := r.ResolveCandidate("c1", "app.lab.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].Value != "10.7.7.7" || res.Source.Zone != "lab.example.test" {
		t.Fatalf("candidate-introduced zone wrong: %+v", res)
	}
	if _, err := r.Resolve("app.lab.example.test"); err == nil {
		t.Fatal("candidate zone leaked into live view")
	}
	if !strings.Contains(r.Candidate("c1").Status, "open") {
		t.Fatal("candidate should still be open")
	}
}
