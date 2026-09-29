// Command localdns builds the fictional zone tree, serves it over the UDP
// adapter, and can snapshot/restore its state. Version 1 never queries the
// public internet; only locally configured zones are served.
package main

import (
	"flag"
	"fmt"
	"log"

	"localdns/resolver"
	"localdns/server"
)

// buildResolver creates the fictional zone tree:
//
//	"" (root)        delegates example.test -> example.test zone
//	example.test     hosts www (A), api (CNAME -> www), shop (NS -> shop.example.test)
//	shop.example.test  child zone with its own records
func buildResolver() *resolver.Resolver {
	r := resolver.NewResolver(resolver.NewCache())

	r.AddZone(resolver.NewZone("", 1, 60, []resolver.Record{
		{Name: "example.test", Type: resolver.TypeNS, Value: "ns1.example.test", TTL: 3600},
	}))
	r.AddZone(resolver.NewZone("example.test", 1, 60, []resolver.Record{
		{Name: "www.example.test", Type: resolver.TypeA, Value: "10.0.0.1", TTL: 300},
		{Name: "api.example.test", Type: resolver.TypeCNAME, Value: "www.example.test", TTL: 300},
		{Name: "shop.example.test", Type: resolver.TypeNS, Value: "ns1.shop.example.test", TTL: 3600},
		// A stale/invalid record below the delegation cut: the child zone
		// is authoritative for web.shop.example.test, so this parent-side
		// record must never be used.
		{Name: "web.shop.example.test", Type: resolver.TypeA, Value: "10.9.9.9", TTL: 300},
	}))
	r.AddZone(resolver.NewZone("shop.example.test", 1, 60, []resolver.Record{
		{Name: "web.shop.example.test", Type: resolver.TypeA, Value: "10.1.1.1", TTL: 300},
	}))
	return r
}

func main() {
	listen := flag.String("listen", "127.0.0.1:5353", "UDP listen address")
	snapshot := flag.String("snapshot", "", "snapshot file to load/save")
	flag.Parse()

	var core *resolver.Resolver
	if *snapshot != "" {
		if loaded, err := resolver.LoadSnapshot(*snapshot); err == nil {
			core = loaded
			log.Printf("restored state from %s", *snapshot)
		} else {
			core = buildResolver()
			log.Printf("no usable snapshot (%v), built fresh zones", err)
		}
	} else {
		core = buildResolver()
	}
	if *snapshot != "" {
		defer func() {
			if err := core.SaveSnapshot(*snapshot); err != nil {
				log.Printf("snapshot save failed: %v", err)
			}
		}()
	}

	srv, err := server.New(core, *listen)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("localdns v1 (no public-internet lookups) on %s\n", srv.Addr())
	log.Fatal(srv.Serve())
}
