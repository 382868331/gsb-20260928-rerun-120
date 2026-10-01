package server

import (
	"strconv"
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

func TestUDPAdapterHistoryCommands(t *testing.T) {
	core := resolver.NewResolver(resolver.NewCache())
	core.AddZone(resolver.NewZone("example.test", 1, 60, []resolver.Record{
		{Name: "www.example.test", Type: resolver.TypeA, Value: "10.0.0.1", TTL: 300},
	}))
	base := core.CurrentViewID()
	if _, err := core.ApplyBatch(resolver.Batch{Upsert: []*resolver.Zone{
		resolver.NewZone("example.test", 2, 60, []resolver.Record{
			{Name: "www.example.test", Type: resolver.TypeA, Value: "10.0.0.2", TTL: 300},
		}),
	}, Note: "readdress www"}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{core: core}

	if got := srv.handle("QUERY www.example.test"); !strings.Contains(got, "A 10.0.0.2") {
		t.Fatalf("current view answer wrong: %s", got)
	}
	got := srv.handle("QUERYAT " + strconv.FormatUint(base, 10) + " www.example.test")
	if !strings.Contains(got, "A 10.0.0.1") || !strings.Contains(got, "view=") {
		t.Fatalf("historical view answer wrong: %s", got)
	}
	if got := srv.handle("VIEWS"); !strings.Contains(got, "readdress www") || !strings.Contains(got, "example.test@v2") {
		t.Fatalf("history listing wrong: %s", got)
	}
	if got := srv.handle("QUERYAT xyz www.example.test"); !strings.Contains(got, "ERR BADREQUEST") {
		t.Fatalf("bad view id handling: %s", got)
	}
}
