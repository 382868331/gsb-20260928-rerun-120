package resolver

import (
	"path/filepath"
	"strings"
	"testing"
)

// candidateOnTree builds the standard tree plus a candidate that changes
// www.example.test to 10.0.0.99 and adds a new name.
func candidateOnTree(t *testing.T, id string) *Resolver {
	t.Helper()
	r := buildTree()
	_, err := r.NewCandidate(id, []*Zone{
		NewZone("example.test", 2, 60, []Record{
			{Name: "www.example.test", Type: TypeA, Value: "10.0.0.99", TTL: 300},
			{Name: "api.example.test", Type: TypeCNAME, Value: "www.example.test", TTL: 300},
			{Name: "new.example.test", Type: TypeA, Value: "10.0.0.77", TTL: 300},
			{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCandidateViewsAreIsolated(t *testing.T) {
	r := candidateOnTree(t, "v2")

	live, err := r.Resolve("www.example.test")
	if err != nil || live.Records[0].Value != "10.0.0.1" || live.View != "live" {
		t.Fatalf("live: %+v %v", live, err)
	}
	cand, err := r.ResolveCandidate("v2", "www.example.test")
	if err != nil || cand.Records[0].Value != "10.0.0.99" || cand.View != "candidate:v2" {
		t.Fatalf("candidate: %+v %v", cand, err)
	}
	// Same query, both views, different answers — and the live answer is
	// unchanged after the candidate ran.
	live2, _ := r.Resolve("www.example.test")
	if live2.Records[0].Value != "10.0.0.1" {
		t.Fatalf("candidate query polluted live view: %+v", live2)
	}
	// Candidate-only name is invisible to the live view.
	if _, err := r.Resolve("new.example.test"); err == nil {
		t.Fatal("candidate-only name leaked into live view")
	}
	if _, err := r.ResolveCandidate("v2", "new.example.test"); err != nil {
		t.Fatalf("candidate-only name missing in candidate: %v", err)
	}
}

func TestCandidateCacheTaggedAndNegativeIsolated(t *testing.T) {
	r := candidateOnTree(t, "v2")

	// Negative answer inside the candidate (name absent from candidate zones).
	if _, err := r.ResolveCandidate("v2", "ghost.example.test"); err == nil {
		t.Fatal("want NXDOMAIN in candidate")
	}
	found := false
	for _, e := range r.Candidate("v2").cache.Snapshot() {
		if e.View != "candidate:v2" {
			t.Fatalf("candidate cache entry not tagged with view: %+v", e)
		}
		if e.Name == "ghost.example.test" && e.Negative {
			found = true
		}
	}
	if !found {
		t.Fatal("candidate negative answer not in candidate cache")
	}
	// The live cache must hold nothing from the candidate view.
	for _, e := range r.Cache().Snapshot() {
		if e.View != "live" {
			t.Fatalf("non-live entry in live cache: %+v", e)
		}
		if e.Name == "ghost.example.test" {
			t.Fatal("candidate negative answer leaked into live cache")
		}
	}
	// Live view re-derives the negative answer itself (not served from
	// the candidate's cache).
	if _, err := r.Resolve("ghost.example.test"); err == nil {
		t.Fatal("want NXDOMAIN live")
	}
	liveNeg := false
	for _, e := range r.Cache().Snapshot() {
		if e.Name == "ghost.example.test" && e.View == "live" && e.Negative {
			liveNeg = true
		}
	}
	if !liveNeg {
		t.Fatal("live negative answer not cached under live view")
	}
}

func TestStaleDepsRecheckAndNoSplice(t *testing.T) {
	r := buildTree()
	// Candidate only overrides the child zone.
	if _, err := r.NewCandidate("shop-v2", []*Zone{
		NewZone("shop.example.test", 2, 60, []Record{
			{Name: "web.shop.example.test", Type: TypeA, Value: "10.1.1.2", TTL: 300},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	stale, err := r.StaleDeps("shop-v2")
	if err != nil || len(stale) != 0 {
		t.Fatalf("fresh candidate should have no stale deps: %v %v", stale, err)
	}

	// Live parent zone changes while the candidate is being observed.
	if err := r.UpdateZone(NewZone("example.test", 2, 60, []Record{
		{Name: "www.example.test", Type: TypeA, Value: "10.0.0.5", TTL: 300},
		{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
	})); err != nil {
		t.Fatal(err)
	}
	stale, err = r.StaleDeps("shop-v2")
	if err != nil || len(stale) != 1 || stale[0].Zone != "example.test" ||
		stale[0].BaseVersion != 1 || stale[0].CurrentVersion != 2 {
		t.Fatalf("stale deps wrong: %+v %v", stale, err)
	}

	// Before recheck the candidate still answers from its pinned base:
	// parent@1 delegation + candidate child — one consistent snapshot,
	// never parent@2 spliced with anything.
	res, err := r.ResolveCandidate("shop-v2", "web.shop.example.test")
	if err != nil || res.Records[0].Value != "10.1.1.2" || res.Source.String() != "shop.example.test@v2" {
		t.Fatalf("candidate answer: %+v %v", res, err)
	}
	res, err = r.ResolveCandidate("shop-v2", "www.example.test")
	if err != nil || res.Records[0].Value != "10.0.0.1" { // base example.test@1, not live @2
		t.Fatalf("candidate must use pinned base, got %+v %v", res, err)
	}

	// Adoption refused while stale.
	if _, err := r.AdoptCandidate("shop-v2"); err == nil ||
		!strings.Contains(err.Error(), "stale") {
		t.Fatalf("adopt with stale deps must fail: %v", err)
	}

	// Recheck re-bases; now the candidate sees the new parent.
	moved, err := r.RecheckCandidate("shop-v2")
	if err != nil || len(moved) != 1 || moved[0].Zone != "example.test" {
		t.Fatalf("recheck: %+v %v", moved, err)
	}
	if stale, _ := r.StaleDeps("shop-v2"); len(stale) != 0 {
		t.Fatalf("still stale after recheck: %+v", stale)
	}
	res, err = r.ResolveCandidate("shop-v2", "www.example.test")
	if err != nil || res.Records[0].Value != "10.0.0.5" {
		t.Fatalf("after recheck candidate must see new base: %+v %v", res, err)
	}

	// Adoption now records the basis it actually rested on.
	ad, err := r.AdoptCandidate("shop-v2")
	if err != nil {
		t.Fatal(err)
	}
	if ad.Basis["example.test"] != 2 || ad.Basis[""] != 1 || ad.Zones["shop.example.test"] != 2 {
		t.Fatalf("adoption basis wrong: %+v", ad)
	}
	// Candidate gone; live view now serves the adopted zone.
	if r.Candidate("shop-v2") != nil {
		t.Fatal("candidate still registered after adoption")
	}
	res, err = r.Resolve("web.shop.example.test")
	if err != nil || res.Records[0].Value != "10.1.1.2" {
		t.Fatalf("adopted zone not live: %+v %v", res, err)
	}
	if got := r.Adoptions(); len(got) != 1 || got[0].ID != "shop-v2" {
		t.Fatalf("adoption log: %+v", got)
	}
}

func TestAbandonKeepsDiffBasis(t *testing.T) {
	r := candidateOnTree(t, "v2")
	ab, err := r.AbandonCandidate("v2")
	if err != nil {
		t.Fatal(err)
	}
	d := ab.Diffs["example.test"]
	if len(d.Added) == 0 || len(d.Removed) == 0 {
		t.Fatalf("abandonment diff empty: %+v", d)
	}
	if ab.Basis["example.test"] != 1 {
		t.Fatalf("abandonment basis wrong: %+v", ab.Basis)
	}
	if r.Candidate("v2") != nil {
		t.Fatal("candidate still registered after abandonment")
	}
	if got := r.Abandonments(); len(got) != 1 || got[0].ID != "v2" {
		t.Fatalf("abandonment log: %+v", got)
	}
	// Live view untouched by the abandoned trial.
	res, err := r.Resolve("www.example.test")
	if err != nil || res.Records[0].Value != "10.0.0.1" {
		t.Fatalf("live changed after abandonment: %+v %v", res, err)
	}
}

func TestCrossZoneAliasInCandidate(t *testing.T) {
	r := buildTree()
	// Candidate makes api.example.test point across the delegation cut
	// into the child zone.
	if _, err := r.NewCandidate("xalias", []*Zone{
		NewZone("example.test", 2, 60, []Record{
			{Name: "www.example.test", Type: TypeA, Value: "10.0.0.1", TTL: 300},
			{Name: "api.example.test", Type: TypeCNAME, Value: "web.shop.example.test", TTL: 300},
			{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	res, err := r.ResolveCandidate("xalias", "api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].Value != "10.1.1.1" || len(res.Chain) != 1 ||
		res.Chain[0].Value != "web.shop.example.test" {
		t.Fatalf("cross-zone alias in candidate wrong: %+v", res)
	}
	// Live view still resolves api to the old target.
	live, err := r.Resolve("api.example.test")
	if err != nil || live.Records[0].Value != "10.0.0.1" {
		t.Fatalf("live alias changed: %+v %v", live, err)
	}
}

func TestOverlappingCandidates(t *testing.T) {
	r := candidateOnTree(t, "a")
	if _, err := r.NewCandidate("b", []*Zone{
		NewZone("example.test", 2, 60, []Record{
			{Name: "www.example.test", Type: TypeA, Value: "10.0.0.55", TTL: 300},
			{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.NewCandidate("c", []*Zone{
		NewZone("shop.example.test", 2, 60, []Record{
			{Name: "web.shop.example.test", Type: TypeA, Value: "10.1.1.9", TTL: 300},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	ov, err := r.Overlaps("a", "b")
	if err != nil || len(ov) != 1 || ov[0] != "example.test" {
		t.Fatalf("overlap a/b: %v %v", ov, err)
	}
	if ov, _ := r.Overlaps("a", "c"); len(ov) != 0 {
		t.Fatalf("a and c must not overlap: %v", ov)
	}
	// Each overlapping candidate still resolves independently.
	ra, _ := r.ResolveCandidate("a", "www.example.test")
	rb, _ := r.ResolveCandidate("b", "www.example.test")
	if ra.Records[0].Value != "10.0.0.99" || rb.Records[0].Value != "10.0.0.55" {
		t.Fatalf("overlapping candidates not independent: %v / %v", ra, rb)
	}
}

func TestCompareAndReopen(t *testing.T) {
	r := candidateOnTree(t, "v2")
	cmp := r.Compare("v2", "www.example.test")
	if !cmp.Changed() || cmp.Live.Records[0].Value != "10.0.0.1" ||
		cmp.Candidate.Records[0].Value != "10.0.0.99" {
		t.Fatalf("compare: %+v", cmp)
	}
	if cmp := r.Compare("v2", "web.shop.example.test"); cmp.Changed() {
		t.Fatalf("unchanged query reported changed: %+v", cmp)
	}

	// Snapshot, "restart", and the same comparison must still work.
	path := t.TempDir() + "/snap.json"
	if err := r.SaveSnapshot(path); err != nil {
		t.Fatal(err)
	}
	r2, err := LoadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	cmp2 := r2.Compare("v2", "www.example.test")
	if !cmp2.Changed() || cmp2.Candidate.Records[0].Value != "10.0.0.99" {
		t.Fatalf("compare after reopen: %+v", cmp2)
	}
	// Historical answers still explainable per their original view.
	res, err := r2.ResolveAt("www.example.test", map[string]uint64{"example.test": 1})
	if err != nil || res.Records[0].Value != "10.0.0.1" || res.Source.String() != "example.test@v1" {
		t.Fatalf("historical view after reopen: %+v %v", res, err)
	}
}

func TestCandidateValidation(t *testing.T) {
	r := buildTree()
	if _, err := r.NewCandidate("bad", []*Zone{
		NewZone("example.test", 1, 60, nil), // does not advance live v1
	}); err == nil {
		t.Fatal("non-advancing candidate version accepted")
	}
	if _, err := r.NewCandidate("", []*Zone{NewZone("example.test", 2, 60, nil)}); err == nil {
		t.Fatal("empty id accepted")
	}
	if _, err := r.NewCandidate("ok", []*Zone{NewZone("example.test", 2, 60, nil)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.NewCandidate("ok", []*Zone{NewZone("example.test", 3, 60, nil)}); err == nil {
		t.Fatal("duplicate candidate id accepted")
	}
	if _, err := r.ResolveCandidate("nope", "www.example.test"); err == nil {
		t.Fatal("unknown candidate resolved")
	}
}

// A candidate may introduce a zone that does not exist in the live
// catalog at all. Queries for it must resolve inside the candidate view
// (not NOZONE), cross-zone aliases and negative results must stay inside
// that view, and the conclusions must survive a snapshot/restart.
func TestCandidateWithZoneUnknownToLiveCatalog(t *testing.T) {
	// No root zone here: the candidate's new zone is outside every live
	// zone, so the live view has no authoritative source for it at all.
	r := NewResolver(NewCache())
	r.AddZone(NewZone("example.test", 1, 60, []Record{
		{Name: "www.example.test", Type: TypeA, Value: "10.0.0.1", TTL: 300},
	}))
	if _, err := r.NewCandidate("lab", []*Zone{
		NewZone("example.test", 2, 60, []Record{
			{Name: "www.example.test", Type: TypeA, Value: "10.0.0.1", TTL: 300},
			{Name: "alias.example.test", Type: TypeCNAME, Value: "host.lab.test", TTL: 300},
		}),
		NewZone("lab.test", 1, 60, []Record{
			{Name: "host.lab.test", Type: TypeA, Value: "10.7.7.7", TTL: 300},
		}),
	}); err != nil {
		t.Fatal(err)
	}

	// The new zone resolves inside the candidate view only.
	res, err := r.ResolveCandidate("lab", "host.lab.test")
	if err != nil || res.Records[0].Value != "10.7.7.7" ||
		res.View != "candidate:lab" || res.Source.String() != "lab.test@v1" {
		t.Fatalf("candidate new-zone answer: %+v %v", res, err)
	}
	if _, err := r.Resolve("host.lab.test"); err == nil ||
		err.(*ResolveError).Code != ErrNoZone {
		t.Fatalf("live view must not see candidate-only zone: %v", err)
	}

	// Cross-zone alias: candidate example.test -> candidate lab.test.
	res, err = r.ResolveCandidate("lab", "alias.example.test")
	if err != nil || res.Records[0].Value != "10.7.7.7" || len(res.Chain) != 1 {
		t.Fatalf("cross-zone alias in candidate: %+v %v", res, err)
	}

	// Negative answer stays in the candidate's private cache.
	if _, err := r.ResolveCandidate("lab", "ghost.lab.test"); err == nil ||
		err.(*ResolveError).Code != ErrNXDOMAIN {
		t.Fatalf("want NXDOMAIN in candidate, got %v", err)
	}
	found := false
	for _, e := range r.Candidate("lab").cache.Snapshot() {
		if e.Name == "ghost.lab.test" && e.Negative && e.View == "candidate:lab" {
			found = true
		}
	}
	if !found {
		t.Fatal("candidate negative answer not in candidate cache")
	}
	for _, e := range r.Cache().Snapshot() {
		if e.Name == "ghost.lab.test" {
			t.Fatal("candidate negative answer leaked into live cache")
		}
	}

	// After a restart the same conclusions hold.
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := r.SaveSnapshot(path); err != nil {
		t.Fatal(err)
	}
	r2, err := LoadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	res, err = r2.ResolveCandidate("lab", "host.lab.test")
	if err != nil || res.Records[0].Value != "10.7.7.7" {
		t.Fatalf("candidate new-zone answer after reopen: %+v %v", res, err)
	}
	if _, err := r2.Resolve("host.lab.test"); err == nil {
		t.Fatal("live view sees candidate-only zone after reopen")
	}
}

// When a live zone changes, StaleDeps must report the moved base even for
// a zone the candidate overrides — adoption-time version conflicts must
// not be the first notice.
func TestStaleDepsReportOverriddenZone(t *testing.T) {
	r := buildTree()
	if _, err := r.NewCandidate("cand", []*Zone{
		NewZone("example.test", 3, 60, []Record{
			{Name: "www.example.test", Type: TypeA, Value: "10.0.0.9", TTL: 300},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	if stale, _ := r.StaleDeps("cand"); len(stale) != 0 {
		t.Fatalf("fresh candidate should have no stale deps: %+v", stale)
	}

	// Live moves the very zone the candidate overrides.
	if err := r.UpdateZone(NewZone("example.test", 2, 60, []Record{
		{Name: "www.example.test", Type: TypeA, Value: "10.0.0.5", TTL: 300},
	})); err != nil {
		t.Fatal(err)
	}
	stale, err := r.StaleDeps("cand")
	if err != nil || len(stale) != 1 || stale[0].Zone != "example.test" ||
		stale[0].BaseVersion != 1 || stale[0].CurrentVersion != 2 {
		t.Fatalf("overridden zone must be reported stale: %+v %v", stale, err)
	}
	if _, err := r.AdoptCandidate("cand"); err == nil ||
		!strings.Contains(err.Error(), "stale") {
		t.Fatalf("adopt with stale deps must fail: %v", err)
	}

	// The stale conclusion survives a restart.
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := r.SaveSnapshot(path); err != nil {
		t.Fatal(err)
	}
	r2, err := LoadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	stale, err = r2.StaleDeps("cand")
	if err != nil || len(stale) != 1 || stale[0].Zone != "example.test" {
		t.Fatalf("stale deps after reopen: %+v %v", stale, err)
	}

	// Recheck re-pins the base; adoption then succeeds and records the
	// basis it actually rested on.
	moved, err := r2.RecheckCandidate("cand")
	if err != nil || len(moved) != 1 || moved[0].Zone != "example.test" {
		t.Fatalf("recheck: %+v %v", moved, err)
	}
	ad, err := r2.AdoptCandidate("cand")
	if err != nil {
		t.Fatal(err)
	}
	if ad.Basis["example.test"] != 2 || ad.Zones["example.test"] != 3 {
		t.Fatalf("adoption basis wrong: %+v", ad)
	}
	res, err := r2.Resolve("www.example.test")
	if err != nil || res.Records[0].Value != "10.0.0.9" {
		t.Fatalf("adopted answer: %+v %v", res, err)
	}
}
