package server

import (
	"strings"
	"testing"

	"localdns/resolver"
)

func TestUDPAdapterUsesSharedCore(t *testing.T) {
	core := resolver.NewResolver(resolver.NewCache())
	core.AddZone(resolver.NewZone("example.test", 1, 60, []resolver.Record{
		{Name: "www.example.test", Type: resolver.TypeA, Value: "10.0.0.1", TTL: 300},
		{Name: "api.example.test", Type: resolver.TypeCNAME, Value: "www.example.test", TTL: 300},
	}))
	// Handler-level test: the sandbox forbids binding UDP sockets, so the
	// adapter is exercised through the same code path Serve() uses.
	srv := &Server{core: core}

	if got := srv.handle("QUERY www.example.test"); !strings.Contains(got, "OK www.example.test A 10.0.0.1") {
		t.Fatalf("bad A response: %s", got)
	}
	got := srv.handle("QUERY api.example.test")
	if !strings.Contains(got, "CNAME api.example.test www.example.test") || !strings.Contains(got, "A 10.0.0.1") {
		t.Fatalf("bad alias response: %s", got)
	}
	if got := srv.handle("QUERY missing.example.test"); !strings.Contains(got, "ERR NXDOMAIN") {
		t.Fatalf("bad negative response: %s", got)
	}
	if got := srv.handle("garbage"); !strings.Contains(got, "ERR BADREQUEST") {
		t.Fatalf("bad request handling: %s", got)
	}
}

func TestQueryAtHistoricalView(t *testing.T) {
	core := resolver.NewResolver(resolver.NewCache())
	core.AddZone(resolver.NewZone("example.test", 1, 60, []resolver.Record{
		{Name: "www.example.test", Type: resolver.TypeA, Value: "10.0.0.1", TTL: 300},
	}))
	if err := core.UpdateZone(resolver.NewZone("example.test", 2, 60, []resolver.Record{
		{Name: "www.example.test", Type: resolver.TypeA, Value: "10.0.0.2", TTL: 300},
	})); err != nil {
		t.Fatal(err)
	}
	srv := &Server{core: core}

	if got := srv.handle("QUERY www.example.test"); !strings.Contains(got, "A 10.0.0.2") {
		t.Fatalf("live query wrong: %s", got)
	}
	got := srv.handle("QUERYAT www.example.test example.test@1")
	if !strings.Contains(got, "A 10.0.0.1") || !strings.Contains(got, "source=example.test@v1") {
		t.Fatalf("historical query wrong: %s", got)
	}
	if got := srv.handle("QUERYAT www.example.test example.test@9"); !strings.Contains(got, "ERR") {
		t.Fatalf("unknown version should fail: %s", got)
	}
	if got := srv.handle("QUERYAT www.example.test bogus"); !strings.Contains(got, "ERR BADREQUEST") {
		t.Fatalf("bad pin should fail: %s", got)
	}
}

func TestCandidateQueryAndCompare(t *testing.T) {
	core := resolver.NewResolver(resolver.NewCache())
	core.AddZone(resolver.NewZone("example.test", 1, 60, []resolver.Record{
		{Name: "www.example.test", Type: resolver.TypeA, Value: "10.0.0.1", TTL: 300},
	}))
	if _, err := core.OpenCandidate("c1", []*resolver.Zone{
		resolver.NewZone("example.test", 2, 60, []resolver.Record{
			{Name: "www.example.test", Type: resolver.TypeA, Value: "10.0.0.2", TTL: 300},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{core: core}

	got := srv.handle("CQUERY c1 www.example.test")
	if !strings.Contains(got, "A 10.0.0.2") || !strings.Contains(got, "view=candidate:c1") {
		t.Fatalf("bad candidate response: %s", got)
	}
	got = srv.handle("CCMP c1 www.example.test")
	if !strings.Contains(got, "LIVE OK www.example.test A 10.0.0.1") ||
		!strings.Contains(got, "CANDIDATE OK www.example.test A 10.0.0.2") ||
		!strings.Contains(got, "DIFFERS true") {
		t.Fatalf("bad comparison response: %s", got)
	}
	// The candidate view did not disturb the live answer.
	if got := srv.handle("QUERY www.example.test"); !strings.Contains(got, "A 10.0.0.1") {
		t.Fatalf("live answer disturbed: %s", got)
	}
	if got := srv.handle("CQUERY nope www.example.test"); !strings.Contains(got, "ERR") {
		t.Fatalf("unknown candidate should fail: %s", got)
	}
}
