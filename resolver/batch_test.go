package resolver

import (
	"strings"
	"testing"
)

// TestBatchPreviewDoesNotMutate: preview reports the diff and the cache
// entries that would be invalidated, without changing answers or cache.
func TestBatchPreviewDoesNotMutate(t *testing.T) {
	r := buildTree()
	if _, err := r.Resolve("www.example.test"); err != nil {
		t.Fatal(err)
	}
	newParent := NewZone("example.test", 2, 60, []Record{
		{Name: "www.example.test", Type: TypeA, Value: "10.0.0.2", TTL: 300},
		{Name: "api.example.test", Type: TypeCNAME, Value: "www.example.test", TTL: 300},
		{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
	})
	p, err := r.PreviewBatch([]*Zone{newParent})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Zones) != 1 || p.Zones[0].FromVersion != 1 || p.Zones[0].ToVersion != 2 {
		t.Fatalf("bad preview versions: %+v", p.Zones)
	}
	zp := p.Zones[0]
	if len(zp.Diff.Added) != 1 || zp.Diff.Added[0].Value != "10.0.0.2" {
		t.Fatalf("bad added diff: %+v", zp.Diff.Added)
	}
	// removed: old www record + the stale below-cut record
	if len(zp.Diff.Removed) != 2 {
		t.Fatalf("bad removed diff: %+v", zp.Diff.Removed)
	}
	if len(zp.CacheInvalidated) != 1 || !strings.Contains(zp.CacheInvalidated[0], "www.example.test") {
		t.Fatalf("preview should list cached www entry: %v", zp.CacheInvalidated)
	}
	// Nothing applied: answer and cache still reflect version 1.
	res, err := r.Resolve("www.example.test")
	if err != nil || res.Records[0].Value != "10.0.0.1" || !res.FromCache {
		t.Fatalf("preview mutated state: %+v %v", res, err)
	}
	if r.ZoneSnapshot("example.test").Version != 1 {
		t.Fatal("preview bumped zone version")
	}
}

// TestBatchAtomicValidation: one bad zone rejects the whole batch.
func TestBatchAtomicValidation(t *testing.T) {
	r := buildTree()
	good := NewZone("example.test", 2, 60, []Record{
		{Name: "www.example.test", Type: TypeA, Value: "10.0.0.2", TTL: 300},
	})
	bad := NewZone("shop.example.test", 1, 60, nil) // version does not advance
	if err := r.ApplyBatch([]*Zone{good, bad}); err == nil {
		t.Fatal("batch with stale version accepted")
	}
	if v := r.ZoneSnapshot("example.test").Version; v != 1 {
		t.Fatalf("partial batch applied, example.test at v%d", v)
	}
	dup := NewZone("example.test", 3, 60, nil)
	if err := r.ApplyBatch([]*Zone{good, dup}); err == nil {
		t.Fatal("batch with duplicate zone accepted")
	}
}

// TestNegativeCacheInvalidatedOnUpdate: a cached NXDOMAIN must stop being
// served as soon as the zone version that justified it is superseded.
func TestNegativeCacheInvalidatedOnUpdate(t *testing.T) {
	r := buildTree()
	if _, err := r.Resolve("new.example.test"); err == nil {
		t.Fatal("want NXDOMAIN before update")
	}
	if e := r.Cache().Get("new.example.test", TypeA); e == nil || !e.Negative {
		t.Fatal("negative answer not cached")
	}
	// Zone update adds the previously missing name.
	if err := r.UpdateZone(NewZone("example.test", 2, 60, []Record{
		{Name: "www.example.test", Type: TypeA, Value: "10.0.0.1", TTL: 300},
		{Name: "api.example.test", Type: TypeCNAME, Value: "www.example.test", TTL: 300},
		{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
		{Name: "new.example.test", Type: TypeA, Value: "10.0.0.7", TTL: 300},
	})); err != nil {
		t.Fatal(err)
	}
	if e := r.Cache().Get("new.example.test", TypeA); e != nil {
		t.Fatalf("stale negative entry survived zone update: %+v", e)
	}
	res, err := r.Resolve("new.example.test")
	if err != nil || res.Records[0].Value != "10.0.0.7" || res.Source.Version != 2 {
		t.Fatalf("update not visible: %+v %v", res, err)
	}
}

// TestCrossZoneAlias: a CNAME may point into another zone; the answer's
// source is the zone that owns the final A record.
func TestCrossZoneAlias(t *testing.T) {
	r := buildTree()
	r.AddZone(NewZone("cdn.test", 1, 60, []Record{
		{Name: "edge.cdn.test", Type: TypeA, Value: "192.0.2.5", TTL: 300},
	}))
	// Root delegates the new zone as well.
	if err := r.UpdateZone(NewZone("", 2, 60, []Record{
		{Name: "example.test", Type: TypeNS, Value: "ns1.example.test", TTL: 3600},
		{Name: "cdn.test", Type: TypeNS, Value: "ns1.cdn.test", TTL: 3600},
	})); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateZone(NewZone("example.test", 2, 60, []Record{
		{Name: "www.example.test", Type: TypeA, Value: "10.0.0.1", TTL: 300},
		{Name: "api.example.test", Type: TypeCNAME, Value: "www.example.test", TTL: 300},
		{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
		{Name: "img.example.test", Type: TypeCNAME, Value: "edge.cdn.test", TTL: 300},
	})); err != nil {
		t.Fatal(err)
	}
	res, err := r.Resolve("img.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].Value != "192.0.2.5" || res.Source.Zone != "cdn.test" {
		t.Fatalf("cross-zone alias wrong: %+v", res)
	}
	if len(res.Chain) != 1 || res.Chain[0].Value != "edge.cdn.test" {
		t.Fatalf("alias path lost: %+v", res.Chain)
	}
}

// TestDelegationRevoked: removing the NS delegation (and the child zone)
// makes the parent authoritative again; cached child answers are dropped.
func TestDelegationRevoked(t *testing.T) {
	r := buildTree()
	res, err := r.Resolve("web.shop.example.test")
	if err != nil || res.Records[0].Value != "10.1.1.1" {
		t.Fatalf("precondition: %+v %v", res, err)
	}
	// Revoke the delegation: parent v2 has no NS for shop.example.test
	// and serves web.shop.example.test itself.
	if err := r.UpdateZone(NewZone("example.test", 2, 60, []Record{
		{Name: "www.example.test", Type: TypeA, Value: "10.0.0.1", TTL: 300},
		{Name: "web.shop.example.test", Type: TypeA, Value: "10.9.9.9", TTL: 300},
	})); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveZone("shop.example.test"); err != nil {
		t.Fatal(err)
	}
	if e := r.Cache().Get("web.shop.example.test", TypeA); e != nil {
		t.Fatalf("cache kept answer from removed zone: %+v", e)
	}
	res, err = r.Resolve("web.shop.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].Value != "10.9.9.9" || res.Source.Zone != "example.test" {
		t.Fatalf("parent not authoritative after revocation: %+v", res)
	}
	// History view still reproduces and explains the old answer.
	past, err := r.ResolveAt("web.shop.example.test", map[string]uint64{
		"example.test": 1, "shop.example.test": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if past.Records[0].Value != "10.1.1.1" || past.Source.Zone != "shop.example.test" || past.FromCache {
		t.Fatalf("historical view wrong: %+v", past)
	}
}

// TestZoneRollback: rollback re-publishes old content under a new version
// and invalidates answers cached from the rolled-back-from version.
func TestZoneRollback(t *testing.T) {
	r := buildTree()
	if err := r.UpdateZone(NewZone("example.test", 2, 60, []Record{
		{Name: "www.example.test", Type: TypeA, Value: "10.0.0.2", TTL: 300},
		{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
	})); err != nil {
		t.Fatal(err)
	}
	res, err := r.Resolve("www.example.test")
	if err != nil || res.Records[0].Value != "10.0.0.2" {
		t.Fatalf("precondition: %+v %v", res, err)
	}
	rb, err := r.RollbackZone("example.test", 1)
	if err != nil {
		t.Fatal(err)
	}
	if rb.Version != 3 {
		t.Fatalf("rollback should get fresh version, got %d", rb.Version)
	}
	res, err = r.Resolve("www.example.test")
	if err != nil || res.Records[0].Value != "10.0.0.1" || res.Source.Version != 3 {
		t.Fatalf("rollback content not served: %+v %v", res, err)
	}
	// The rolled-forward v2 answer is still explainable via history.
	past, err := r.ResolveAt("www.example.test", map[string]uint64{"example.test": 2})
	if err != nil || past.Records[0].Value != "10.0.0.2" {
		t.Fatalf("v2 not reproducible: %+v %v", past, err)
	}
	if _, err := r.RollbackZone("example.test", 99); err == nil {
		t.Fatal("rollback to unknown version accepted")
	}
}

// TestRestartExplainsHistory: after save+reload, ResolveAt still pins old
// versions and attributes answers to the zone set that produced them.
func TestRestartExplainsHistory(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/snap.json"

	r := buildTree()
	if _, err := r.Resolve("www.example.test"); err != nil { // cache v1 answer
		t.Fatal(err)
	}
	if err := r.UpdateZone(NewZone("example.test", 2, 60, []Record{
		{Name: "www.example.test", Type: TypeA, Value: "10.0.0.2", TTL: 300},
		{Name: "shop.example.test", Type: TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
	})); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveSnapshot(path); err != nil {
		t.Fatal(err)
	}

	r2, err := LoadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	// History survived the restart.
	if got := r2.ZoneHistory("example.test"); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("history lost across restart: %v", got)
	}
	past, err := r2.ResolveAt("www.example.test", map[string]uint64{"example.test": 1})
	if err != nil {
		t.Fatal(err)
	}
	if past.Records[0].Value != "10.0.0.1" || past.Source.Zone != "example.test" || past.Source.Version != 1 {
		t.Fatalf("cannot explain historical answer after restart: %+v", past)
	}
	// Live view serves the new version.
	cur, err := r2.Resolve("www.example.test")
	if err != nil || cur.Records[0].Value != "10.0.0.2" {
		t.Fatalf("current answer wrong after restart: %+v %v", cur, err)
	}
	// Unknown pinned version is rejected, not silently resolved.
	if _, err := r2.ResolveAt("www.example.test", map[string]uint64{"example.test": 7}); err == nil {
		t.Fatal("unknown pinned version accepted")
	}
}
