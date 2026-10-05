package resolver

import (
	"path/filepath"
	"strings"
	"testing"
)

// jointFixture builds the standard tree plus two candidates whose zones
// reference each other: "web" rewrites example.test (v2) adding an alias
// app.example.test -> db.svc.example.test and the svc delegation; "svc"
// introduces the svc.example.test zone answering db.svc.example.test.
func jointFixture(t *testing.T) *Resolver {
	t.Helper()
	r := buildTree()
	if _, err := r.NewCandidate("web", []*Zone{
		NewZone("example.test", 2, 60, []Record{
			{Name: "www.example.test", Type: TypeA, Value: "10.0.0.1", TTL: 300},
			{Name: "api.example.test", Type: TypeCNAME, Value: "www.example.test", TTL: 300},
			{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
			{Name: "svc.example.test", Type: TypeNS, Value: "ns1.svc.example.test", TTL: 3600},
			{Name: "app.example.test", Type: TypeCNAME, Value: "db.svc.example.test", TTL: 300},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.NewCandidate("svc", []*Zone{
		NewZone("svc.example.test", 1, 60, []Record{
			{Name: "db.svc.example.test", Type: TypeA, Value: "10.2.2.2", TTL: 300},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestJointCheckCrossCandidateReference(t *testing.T) {
	r := jointFixture(t)
	if _, err := r.NewJointReview("rel1", []string{"web", "svc"}); err != nil {
		t.Fatal(err)
	}
	rep, err := r.CheckJoint("rel1")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("joint check should pass: %+v", rep)
	}
	// The alias only resolves because the fellow member provides the
	// target zone: a cross-candidate dependency must be reported.
	if len(rep.CrossDeps) != 1 || rep.CrossDeps[0].From != "web" ||
		rep.CrossDeps[0].ProvidedBy != "svc" || rep.CrossDeps[0].Name != "db.svc.example.test" {
		t.Fatalf("cross deps wrong: %+v", rep.CrossDeps)
	}
	// Version attribution covers member zones and the pinned live set.
	attr := map[string]ZoneAttribution{}
	for _, z := range rep.Zones {
		attr[z.Zone] = z
	}
	if attr["example.test"].Source != "candidate:web" || attr["example.test"].Version != 2 {
		t.Fatalf("example.test attribution wrong: %+v", attr["example.test"])
	}
	if attr["svc.example.test"].Source != "candidate:svc" || attr["svc.example.test"].Version != 1 {
		t.Fatalf("svc.example.test attribution wrong: %+v", attr["svc.example.test"])
	}
	if attr["shop.example.test"].Source != "live" || attr["shop.example.test"].Version != 1 {
		t.Fatalf("shop.example.test attribution wrong: %+v", attr["shop.example.test"])
	}
	// Resolvable inside the set does not mean adoptable alone: the "web"
	// candidate by itself cannot resolve its own alias target.
	if _, err := r.ResolveCandidate("web", "app.example.test"); err == nil {
		t.Fatal("web candidate alone should not resolve app.example.test")
	}
}

func TestJointCheckReportsLoopAndMissing(t *testing.T) {
	r := buildTree()
	if _, err := r.NewCandidate("loopy", []*Zone{
		NewZone("example.test", 2, 60, []Record{
			{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
			{Name: "x.example.test", Type: TypeCNAME, Value: "y.example.test", TTL: 300},
			{Name: "y.example.test", Type: TypeCNAME, Value: "x.example.test", TTL: 300},
			{Name: "broken.example.test", Type: TypeCNAME, Value: "ghost.example.test", TTL: 300},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.NewJointReview("bad", []string{"loopy"}); err != nil {
		t.Fatal(err)
	}
	rep, err := r.CheckJoint("bad")
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatal("report should not be OK")
	}
	if len(rep.Loops) == 0 || !strings.Contains(rep.Loops[0].Detail, "x.example.test -> y.example.test") {
		t.Fatalf("loop with full path not reported: %+v", rep.Loops)
	}
	foundMissing := false
	for _, m := range rep.Missing {
		if m.Target == "ghost.example.test" {
			foundMissing = true
		}
	}
	if !foundMissing {
		t.Fatalf("missing target not reported: %+v", rep.Missing)
	}
	// A set that does not check out cannot be adopted; live stays as-is.
	if _, err := r.AdoptJoint("bad"); err == nil {
		t.Fatal("adoption of a failing set must be refused")
	}
	if got := r.ZoneSnapshot("example.test").Version; got != 1 {
		t.Fatalf("failed adoption changed live zone: v%d", got)
	}
}

func TestJointAdoptAtomicAndRecorded(t *testing.T) {
	r := jointFixture(t)
	if _, err := r.NewJointReview("rel1", []string{"web", "svc"}); err != nil {
		t.Fatal(err)
	}
	ad, err := r.AdoptJoint("rel1")
	if err != nil {
		t.Fatal(err)
	}
	if ad.Zones["example.test"] != 2 || ad.Zones["svc.example.test"] != 1 {
		t.Fatalf("adopted zones wrong: %+v", ad.Zones)
	}
	if ad.Basis["example.test"] != 1 || ad.Basis["shop.example.test"] != 1 {
		t.Fatalf("basis wrong: %+v", ad.Basis)
	}
	// Both zones went live together; the cross-zone alias now resolves.
	res, err := r.Resolve("app.example.test")
	if err != nil || res.Records[0].Value != "10.2.2.2" {
		t.Fatalf("post-adoption resolve: %+v %v", res, err)
	}
	// Members are consumed; the review is gone; the adoption is logged.
	if r.Candidate("web") != nil || r.Candidate("svc") != nil {
		t.Fatal("member candidates should be consumed by adoption")
	}
	if r.JointReview("rel1") != nil {
		t.Fatal("adopted review should be gone")
	}
	if len(r.JointAdoptions()) != 1 {
		t.Fatalf("adoption log: %+v", r.JointAdoptions())
	}
}

func TestJointAdoptFailureLeavesLiveUntouched(t *testing.T) {
	r := jointFixture(t)
	if _, err := r.NewJointReview("rel1", []string{"web", "svc"}); err != nil {
		t.Fatal(err)
	}
	// Move a live zone the review rests on: the decision is now stale.
	if err := r.UpdateZone(NewZone("example.test", 2, 60, []Record{
		{Name: "www.example.test", Type: TypeA, Value: "10.0.0.5", TTL: 300},
		{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
	})); err != nil {
		t.Fatal(err)
	}
	stale, err := r.JointStale("rel1")
	if err != nil || len(stale) != 1 || stale[0].Zone != "example.test" {
		t.Fatalf("stale deps: %+v %v", stale, err)
	}
	if _, err := r.AdoptJoint("rel1"); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale adoption must be refused: %v", err)
	}
	// No partial replacement: svc.example.test must not have gone live.
	if r.ZoneSnapshot("svc.example.test") != nil {
		t.Fatal("failed adoption partially published svc.example.test")
	}
	res, err := r.Resolve("www.example.test")
	if err != nil || res.Records[0].Value != "10.0.0.5" {
		t.Fatalf("live view should serve the last good version: %+v %v", res, err)
	}
	// Recheck re-pins the base; adoption is still refused atomically by
	// the batch version check (candidate v2 no longer advances live v2),
	// and again nothing is published.
	if _, err := r.RecheckJoint("rel1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AdoptJoint("rel1"); err == nil {
		t.Fatal("adoption against moved live versions must fail")
	}
	if r.ZoneSnapshot("svc.example.test") != nil {
		t.Fatal("failed adoption partially published svc.example.test")
	}
}

func TestJointRecheckAndNarrow(t *testing.T) {
	r := jointFixture(t)
	if _, err := r.NewJointReview("rel1", []string{"web", "svc"}); err != nil {
		t.Fatal(err)
	}
	// An unrelated live zone moves; recheck reports it and re-pins.
	if err := r.UpdateZone(NewZone("shop.example.test", 2, 60, []Record{
		{Name: "web.shop.example.test", Type: TypeA, Value: "10.1.1.9", TTL: 300},
	})); err != nil {
		t.Fatal(err)
	}
	stale, err := r.RecheckJoint("rel1", nil)
	if err != nil || len(stale) != 1 || stale[0].Zone != "shop.example.test" {
		t.Fatalf("recheck stale: %+v %v", stale, err)
	}
	if s, _ := r.JointStale("rel1"); len(s) != 0 {
		t.Fatalf("recheck should clear staleness: %+v", s)
	}
	// Narrow the set to the self-contained member and re-check.
	if _, err := r.RecheckJoint("rel1", []string{"svc"}); err != nil {
		t.Fatal(err)
	}
	rep, err := r.CheckJoint("rel1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Members) != 1 || rep.Members[0] != "svc" || !rep.OK() || len(rep.CrossDeps) != 0 {
		t.Fatalf("narrowed report wrong: %+v", rep)
	}
	// Narrowing can only drop members.
	if _, err := r.RecheckJoint("rel1", []string{"svc", "web"}); err == nil {
		t.Fatal("widening via recheck must be refused")
	}
	// The narrowed set adopts cleanly.
	if _, err := r.AdoptJoint("rel1"); err != nil {
		t.Fatal(err)
	}
	if r.ZoneSnapshot("svc.example.test") == nil {
		t.Fatal("narrowed set did not go live")
	}
	if r.Candidate("web") == nil {
		t.Fatal("dropped member candidate must survive the adoption")
	}
}

func TestJointAbandonKeepsCandidates(t *testing.T) {
	r := jointFixture(t)
	if _, err := r.NewJointReview("rel1", []string{"web", "svc"}); err != nil {
		t.Fatal(err)
	}
	if err := r.AbandonJoint("rel1"); err != nil {
		t.Fatal(err)
	}
	if r.JointReview("rel1") != nil {
		t.Fatal("abandoned review should be gone")
	}
	if r.Candidate("web") == nil || r.Candidate("svc") == nil {
		t.Fatal("abandoning a set must not delete its candidates")
	}
	if _, err := r.NewJointReview("rel2", []string{"web", "svc"}); err != nil {
		t.Fatalf("candidates should be reusable in a new set: %v", err)
	}
}

func TestJointRejectsOverlapAndUnknown(t *testing.T) {
	r := jointFixture(t)
	if _, err := r.NewJointReview("bad", []string{"web", "ghost"}); err == nil {
		t.Fatal("unknown member must be rejected")
	}
	if _, err := r.NewCandidate("web2", []*Zone{
		NewZone("example.test", 3, 60, []Record{
			{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.NewJointReview("dup", []string{"web", "web2"}); err == nil ||
		!strings.Contains(err.Error(), "example.test") {
		t.Fatalf("overlapping members must be rejected: %v", err)
	}
}

func TestJointSurvivesRestart(t *testing.T) {
	r := jointFixture(t)
	if _, err := r.NewJointReview("rel1", []string{"web", "svc"}); err != nil {
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
	j := r2.JointReview("rel1")
	if j == nil || len(j.Members) != 2 || j.Base["example.test"] != 1 {
		t.Fatalf("joint review not restored: %+v", j)
	}
	rep, err := r2.CheckJoint("rel1")
	if err != nil || !rep.OK() || len(rep.CrossDeps) != 1 {
		t.Fatalf("restored joint check wrong: %+v %v", rep, err)
	}
	// The pending set can still be adopted after the restart.
	if _, err := r2.AdoptJoint("rel1"); err != nil {
		t.Fatal(err)
	}
	if err := r2.SaveSnapshot(path); err != nil {
		t.Fatal(err)
	}
	r3, err := LoadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(r3.JointAdoptions()) != 1 || r3.JointAdoptions()[0].Basis["example.test"] != 1 {
		t.Fatalf("joint adoption not restored: %+v", r3.JointAdoptions())
	}
	res, err := r3.Resolve("app.example.test")
	if err != nil || res.Records[0].Value != "10.2.2.2" {
		t.Fatalf("restored resolver does not serve adopted set: %+v %v", res, err)
	}
}
