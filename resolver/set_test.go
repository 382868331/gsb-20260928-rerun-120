package resolver

import (
	"path/filepath"
	"strings"
	"testing"
)

// twoMemberTree builds the standard tree plus two candidates whose zones
// reference each other's new names: cand-a rewrites example.test with an
// alias into the brand-new zone carried by cand-b.
func twoMemberTree(t *testing.T) *Resolver {
	t.Helper()
	r := buildTree()
	_, err := r.NewCandidate("cand-a", []*Zone{
		NewZone("example.test", 2, 60, []Record{
			{Name: "www.example.test", Type: TypeA, Value: "10.0.0.1", TTL: 300},
			{Name: "api.example.test", Type: TypeCNAME, Value: "www.example.test", TTL: 300},
			{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
			{Name: "news.example.test", Type: TypeCNAME, Value: "web.blog.example.test", TTL: 300},
			{Name: "blog.example.test", Type: TypeNS, Value: "ns1.blog.example.test", TTL: 3600},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.NewCandidate("cand-b", []*Zone{
		NewZone("blog.example.test", 1, 60, []Record{
			{Name: "web.blog.example.test", Type: TypeA, Value: "10.5.5.5", TTL: 300},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSetResolvesCrossMemberReferences(t *testing.T) {
	r := twoMemberTree(t)
	if _, err := r.NewCandidateSet("rel1", []string{"cand-a", "cand-b"}); err != nil {
		t.Fatal(err)
	}
	// Neither member alone can resolve the alias: cand-a's target zone
	// exists only in cand-b.
	if _, err := r.ResolveCandidate("cand-a", "news.example.test"); err == nil {
		t.Fatal("candidate alone should not resolve cross-member target")
	}
	res, err := r.ResolveSet("rel1", "news.example.test")
	if err != nil {
		t.Fatalf("set query: %v", err)
	}
	if res.Records[0].Value != "10.5.5.5" || res.View != "set:rel1" {
		t.Fatalf("set answer: %+v", res)
	}
	// Live view untouched by the joint review.
	if _, err := r.Resolve("news.example.test"); err == nil {
		t.Fatal("set-only name leaked into live view")
	}
}

func TestSetReviewAttribution(t *testing.T) {
	r := twoMemberTree(t)
	if _, err := r.NewCandidateSet("rel1", []string{"cand-a", "cand-b"}); err != nil {
		t.Fatal(err)
	}
	rev, err := r.ReviewSet("rel1")
	if err != nil {
		t.Fatal(err)
	}
	if !rev.Ready {
		t.Fatalf("review should be ready: %+v", rev)
	}
	origins := map[string]ZoneAttribution{}
	for _, z := range rev.Zones {
		origins[z.Zone] = z
	}
	if o := origins["example.test"]; o.Origin != "candidate:cand-a" || o.Version != 2 {
		t.Fatalf("example.test attribution: %+v", o)
	}
	if o := origins["blog.example.test"]; o.Origin != "candidate:cand-b" || o.Version != 1 {
		t.Fatalf("blog.example.test attribution: %+v", o)
	}
	if o := origins["shop.example.test"]; o.Origin != "live" || o.Version != 1 {
		t.Fatalf("shop.example.test attribution: %+v", o)
	}
	if rev.Basis["example.test"] != 1 {
		t.Fatalf("basis should pin live example.test@1: %+v", rev.Basis)
	}
}

func TestSetReviewReportsLoopAndMissing(t *testing.T) {
	r := buildTree()
	mk := func(id string, zones ...*Zone) {
		t.Helper()
		if _, err := r.NewCandidate(id, zones); err != nil {
			t.Fatal(err)
		}
	}
	mk("parent", NewZone("example.test", 2, 60, []Record{
		{Name: "www.example.test", Type: TypeA, Value: "10.0.0.1", TTL: 300},
		{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
		{Name: "loop.example.test", Type: TypeNS, Value: "ns1.loop.example.test", TTL: 3600},
		{Name: "gap.example.test", Type: TypeNS, Value: "ns1.gap.example.test", TTL: 3600},
	}))
	mk("loop", NewZone("loop.example.test", 1, 60, []Record{
		{Name: "a.loop.example.test", Type: TypeCNAME, Value: "b.loop.example.test", TTL: 300},
		{Name: "b.loop.example.test", Type: TypeCNAME, Value: "a.loop.example.test", TTL: 300},
	}))
	mk("gap", NewZone("gap.example.test", 1, 60, []Record{
		{Name: "x.gap.example.test", Type: TypeCNAME, Value: "nowhere.example.test", TTL: 300},
	}))
	if _, err := r.NewCandidateSet("bad", []string{"parent", "loop", "gap"}); err != nil {
		t.Fatal(err)
	}
	rev, err := r.ReviewSet("bad")
	if err != nil {
		t.Fatal(err)
	}
	if rev.Ready {
		t.Fatal("review with loop and missing target must not be ready")
	}
	if len(rev.Loops) != 1 {
		t.Fatalf("want exactly 1 deduplicated loop, got %+v", rev.Loops)
	}
	joined := strings.Join(rev.Loops[0], " -> ")
	if !strings.Contains(joined, "a.loop.example.test") || !strings.Contains(joined, "b.loop.example.test") {
		t.Fatalf("loop path incomplete: %s", joined)
	}
	if len(rev.Missing) != 1 || rev.Missing[0].Target != "nowhere.example.test" {
		t.Fatalf("missing targets: %+v", rev.Missing)
	}
	// Adoption refused and live zones untouched.
	if _, err := r.AdoptSet("bad"); err == nil {
		t.Fatal("adoption of a failing review must be refused")
	}
	if _, err := r.Resolve("x.gap.example.test"); err == nil {
		t.Fatal("failed adoption leaked zones into live view")
	}
}

func TestSetAdoptionAtomicAndRecorded(t *testing.T) {
	r := twoMemberTree(t)
	if _, err := r.NewCandidateSet("rel1", []string{"cand-a", "cand-b"}); err != nil {
		t.Fatal(err)
	}
	ad, err := r.AdoptSet("rel1")
	if err != nil {
		t.Fatal(err)
	}
	if ad.Zones["example.test"] != 2 || ad.Zones["blog.example.test"] != 1 {
		t.Fatalf("adopted zones: %+v", ad.Zones)
	}
	if ad.Basis["example.test"] != 1 {
		t.Fatalf("basis: %+v", ad.Basis)
	}
	// Both members went live together; the joint name now resolves live.
	res, err := r.Resolve("news.example.test")
	if err != nil || res.Records[0].Value != "10.5.5.5" {
		t.Fatalf("live after adoption: %+v %v", res, err)
	}
	// Members and the set itself are consumed.
	if r.Candidate("cand-a") != nil || r.Candidate("cand-b") != nil || r.CandidateSet("rel1") != nil {
		t.Fatal("adopted set and members should be gone")
	}
	if len(r.SetAdoptions()) != 1 {
		t.Fatal("set adoption not logged")
	}
}

func TestSetStaleDepsAndRecheck(t *testing.T) {
	r := twoMemberTree(t)
	if _, err := r.NewCandidateSet("rel1", []string{"cand-a", "cand-b"}); err != nil {
		t.Fatal(err)
	}
	// An answer served before the change keeps its basis afterwards.
	before, err := r.Resolve("www.example.test")
	if err != nil {
		t.Fatal(err)
	}
	// Live zone moves after the review: only that dependency is stale.
	if err := r.UpdateZone(NewZone("shop.example.test", 2, 60, []Record{
		{Name: "web.shop.example.test", Type: TypeA, Value: "10.1.1.2", TTL: 300},
	})); err != nil {
		t.Fatal(err)
	}
	stale, err := r.StaleSetDeps("rel1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].Zone != "shop.example.test" || stale[0].BaseVersion != 1 || stale[0].CurrentVersion != 2 {
		t.Fatalf("stale deps: %+v", stale)
	}
	if _, err := r.AdoptSet("rel1"); err == nil {
		t.Fatal("adoption with stale deps must be refused")
	}
	// The old answer is still explainable at its original versions.
	replay, err := r.ResolveAt("www.example.test", map[string]uint64{"example.test": before.Source.Version})
	if err != nil || replay.Records[0].Value != "10.0.0.1" {
		t.Fatalf("old answer not preserved: %+v %v", replay, err)
	}
	// Recheck narrows nothing but re-pins the base; adoption then works.
	found, err := r.RecheckSet("rel1")
	if err != nil || len(found) != 1 {
		t.Fatalf("recheck: %+v %v", found, err)
	}
	if _, err := r.AdoptSet("rel1"); err != nil {
		t.Fatalf("adopt after recheck: %v", err)
	}
}

func TestAbandonSetKeepsMembers(t *testing.T) {
	r := twoMemberTree(t)
	if _, err := r.NewCandidateSet("rel1", []string{"cand-a", "cand-b"}); err != nil {
		t.Fatal(err)
	}
	if err := r.AbandonSet("rel1"); err != nil {
		t.Fatal(err)
	}
	if r.CandidateSet("rel1") != nil {
		t.Fatal("set should be dissolved")
	}
	if r.Candidate("cand-a") == nil || r.Candidate("cand-b") == nil {
		t.Fatal("abandoning a set must not delete its member candidates")
	}
	// Narrowing: regroup a smaller selection and review it alone.
	if _, err := r.NewCandidateSet("rel2", []string{"cand-a"}); err != nil {
		t.Fatal(err)
	}
	rev, err := r.ReviewSet("rel2")
	if err != nil {
		t.Fatal(err)
	}
	// cand-a alone aliases into cand-b's zone: the target is now missing.
	if rev.Ready || len(rev.Missing) != 1 {
		t.Fatalf("narrowed review should report the missing member: %+v", rev)
	}
}

func TestSetRejectsConflictingMembers(t *testing.T) {
	r := twoMemberTree(t)
	if _, err := r.NewCandidate("cand-c", []*Zone{
		NewZone("example.test", 2, 60, []Record{
			{Name: "www.example.test", Type: TypeA, Value: "10.0.0.2", TTL: 300},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.NewCandidateSet("clash", []string{"cand-a", "cand-c"}); err == nil {
		t.Fatal("two members carrying the same zone must be rejected")
	}
	if _, err := r.NewCandidateSet("ghost", []string{"cand-a", "nope"}); err == nil {
		t.Fatal("unknown member must be rejected")
	}
}

func TestSetSurvivesRestart(t *testing.T) {
	r := twoMemberTree(t)
	if _, err := r.NewCandidateSet("rel1", []string{"cand-a", "cand-b"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := r.SaveSnapshot(path); err != nil {
		t.Fatal(err)
	}
	r2, err := LoadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	// Pending joint review resumes: same members, same pinned base.
	s := r2.CandidateSet("rel1")
	if s == nil || len(s.Members) != 2 || s.Base["example.test"] != 1 {
		t.Fatalf("restored set: %+v", s)
	}
	res, err := r2.ResolveSet("rel1", "news.example.test")
	if err != nil || res.Records[0].Value != "10.5.5.5" {
		t.Fatalf("set query after restart: %+v %v", res, err)
	}
	rev, err := r2.ReviewSet("rel1")
	if err != nil || !rev.Ready {
		t.Fatalf("review after restart: %+v %v", rev, err)
	}
	// The pending review can still be confirmed after the restart.
	if _, err := r2.AdoptSet("rel1"); err != nil {
		t.Fatalf("adopt after restart: %v", err)
	}
	// Adoption records survive a second restart too.
	if err := r2.SaveSnapshot(path); err != nil {
		t.Fatal(err)
	}
	r3, err := LoadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	ads := r3.SetAdoptions()
	if len(ads) != 1 || ads[0].ID != "rel1" || ads[0].Zones["blog.example.test"] != 1 {
		t.Fatalf("set adoption log after restart: %+v", ads)
	}
}
