// Command localdns manages a fictional, local-only name resolution system.
// It never contacts the public internet: all answers come from configured
// local zones, in memory, from the cache, or over the local UDP/TCP API.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"localdns/net"
	"localdns/resolver"
)

const defaultFile = "zones.json"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "seed":
		err = cmdSeed(os.Args[2:])
	case "zone":
		err = cmdZone(os.Args[2:])
	case "query":
		err = cmdQuery(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `localdns - local fictional name resolver (no public internet)

usage:
  localdns seed  [-f zones.json]
  localdns zone  [-f zones.json] -origin example.test [-add name:A:data:ttl ...] [-del name ...]
  localdns query [-f zones.json] [-name host.example.test] [-kind A]
  localdns serve [-f zones.json] [-addr 127.0.0.1:5354]`)
}

func loadSystem(file string) *resolver.System {
	sys := resolver.NewSystem(file, 30)
	if err := sys.Load(); err != nil {
		fatal(err)
	}
	return sys
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// seed installs the built-in fictional region used in the first round:
// example.test with an alias, and a delegated subzone city.example.test.
func cmdSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	file := fs.String("f", defaultFile, "zone persistence file")
	fs.Parse(args)
	sys := resolver.NewSystem(*file, 30)
	if err := sys.Load(); err != nil {
		return err
	}
	return sys.Commit([]resolver.ZoneInput{
		{
			Origin: "example.test",
			Records: []resolver.Record{
				{Name: "example.test", Kind: resolver.A, Data: "10.0.0.1", TTL: 60},
				{Name: "www.example.test", Kind: resolver.CNAME, Data: "example.test", TTL: 60},
				{Name: "loop1.example.test", Kind: resolver.CNAME, Data: "loop2.example.test", TTL: 60},
				{Name: "loop2.example.test", Kind: resolver.CNAME, Data: "loop1.example.test", TTL: 60},
			},
		},
		{
			Origin: "city.example.test",
			Records: []resolver.Record{
				{Name: "gw.city.example.test", Kind: resolver.A, Data: "10.1.0.1", TTL: 30},
			},
		},
	})
}

func cmdZone(args []string) error {
	fs := flag.NewFlagSet("zone", flag.ExitOnError)
	file := fs.String("f", defaultFile, "zone persistence file")
	origin := fs.String("origin", "", "zone origin")
	add := fs.String("add", "", "comma-separated records name:KIND:data:ttl")
	del := fs.String("del", "", "comma-separated record names to remove")
	fs.Parse(args)
	if *origin == "" {
		return fmt.Errorf("-origin required")
	}
	sys := loadSystem(*file)
	var recs []resolver.Record
	if snap, ok := sys.Zones.ZoneSnapshot(*origin); ok {
		recs = append(recs, snap.Records...)
	}
	deleting := map[string]bool{}
	for _, n := range splitCSV(*del) {
		deleting[resolver.NormalizeName(n)] = true
	}
	filtered := recs[:0]
	for _, r := range recs {
		if !deleting[resolver.NormalizeName(r.Name)] {
			filtered = append(filtered, r)
		}
	}
	for _, spec := range splitCSV(*add) {
		r, err := parseRecord(spec)
		if err != nil {
			return err
		}
		filtered = append(filtered, r)
	}
	return sys.CommitZone(*origin, filtered)
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseRecord(spec string) (resolver.Record, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 4 {
		return resolver.Record{}, fmt.Errorf("record %q must be name:KIND:data:ttl", spec)
	}
	ttl := 0
	if _, err := fmt.Sscanf(parts[3], "%d", &ttl); err != nil {
		return resolver.Record{}, fmt.Errorf("bad ttl in %q", spec)
	}
	return resolver.Record{Name: parts[0], Kind: resolver.Kind(strings.ToUpper(parts[1])), Data: parts[2], TTL: ttl}, nil
}

func cmdQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	file := fs.String("f", defaultFile, "zone persistence file")
	name := fs.String("name", "", "name to resolve")
	kind := fs.String("kind", "A", "record kind")
	fs.Parse(args)
	if *name == "" {
		return fmt.Errorf("-name required")
	}
	sys := loadSystem(*file)
	res := sys.Resolver.Resolve(*name, resolver.Kind(strings.ToUpper(*kind)))
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	file := fs.String("f", defaultFile, "zone persistence file")
	addr := fs.String("addr", "127.0.0.1:5354", "local UDP/TCP listen address")
	fs.Parse(args)
	sys := loadSystem(*file)
	srv, err := netadapt.Listen(sys, *addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "localdns serving local zones on udp/tcp %s (no public forwarding)\n", srv.Addr())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	return srv.Close()
}
