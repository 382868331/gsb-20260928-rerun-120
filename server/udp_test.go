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

