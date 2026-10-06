// Package server is a network adapter over the shared resolution core.
// Version 1 serves only the locally configured fictional zones and never
// forwards queries to the public internet.
package server

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"localdns/resolver"
)

// Server answers line-based queries over UDP:
//
//	REQUEST:  "QUERY <name>" | "QUERYAT <name> <zone@version,...>"
//	          | "CANDQUERY <candidate-id> <name>" | "COMPARE <candidate-id> <name>"
//	RESPONSE: "OK <name> A <value> ..." | "CNAME <target> ..." | "ERR <code> <detail>"
//
// QUERYAT resolves against a historical view: each listed zone is pinned
// to the given version, so a past answer can be reproduced and attributed
// to the zone set that produced it.
//
// CANDQUERY resolves inside an isolated candidate view; COMPARE runs the
// same query against the live view and the candidate and reports both
// outcomes side by side. SETQUERY resolves inside a joint review set
// (several candidates as one pending adoption set); SETREVIEW reports
// the set's joint check: readiness, per-zone version attribution, alias
// loops, missing targets and stale dependencies.
type Server struct {
	core *resolver.Resolver
	conn *net.UDPConn
}

func New(core *resolver.Resolver, addr string) (*Server, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, err
	}
	return &Server{core: core, conn: conn}, nil
}

func (s *Server) Addr() string { return s.conn.LocalAddr().String() }

func (s *Server) Close() error { return s.conn.Close() }

// Serve handles datagrams until the connection is closed.
func (s *Server) Serve() error {
	buf := make([]byte, 4096)
	for {
		n, peer, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return err
		}
		resp := s.handle(strings.TrimSpace(string(buf[:n])))
		s.conn.WriteToUDP([]byte(resp), peer)
	}
}

func (s *Server) handle(q string) string {
	parts := strings.Fields(q)
	if len(parts) == 2 && strings.EqualFold(parts[0], "QUERY") {
		return s.answer(parts[1], nil)
	}
	if len(parts) == 3 && strings.EqualFold(parts[0], "QUERYAT") {
		pin, err := parsePin(parts[2])
		if err != nil {
			return "ERR BADREQUEST " + err.Error()
		}
		return s.answer(parts[1], pin)
	}
	if len(parts) == 3 && strings.EqualFold(parts[0], "CANDQUERY") {
		res, err := s.core.ResolveCandidate(parts[1], parts[2])
		if err != nil {
			if re, ok := err.(*resolver.ResolveError); ok {
				return "ERR " + re.Code + " " + re.Error()
			}
			return "ERR INTERNAL " + err.Error()
		}
		return formatResult(res)
	}
	if len(parts) == 3 && strings.EqualFold(parts[0], "SETQUERY") {
		res, err := s.core.ResolveSet(parts[1], parts[2])
		if err != nil {
			if re, ok := err.(*resolver.ResolveError); ok {
				return "ERR " + re.Code + " " + re.Error()
			}
			return "ERR INTERNAL " + err.Error()
		}
		return formatResult(res)
	}
	if len(parts) == 2 && strings.EqualFold(parts[0], "SETREVIEW") {
		rev, err := s.core.ReviewSet(parts[1])
		if err != nil {
			return "ERR INTERNAL " + err.Error()
		}
		var b strings.Builder
		fmt.Fprintf(&b, "ready=%t members=%v", rev.Ready, rev.Members)
		for _, z := range rev.Zones {
			fmt.Fprintf(&b, " zone=%s@%d(%s)", z.Zone, z.Version, z.Origin)
		}
		for _, l := range rev.Loops {
			fmt.Fprintf(&b, " loop=%s", strings.Join(l, "->"))
		}
		for _, m := range rev.Missing {
			fmt.Fprintf(&b, " missing=%s->%s(%s)", m.Owner, m.Target, m.Code)
		}
		for _, d := range rev.Stale {
			fmt.Fprintf(&b, " stale=%s", d.Zone)
		}
		return b.String()
	}
	if len(parts) == 3 && strings.EqualFold(parts[0], "COMPARE") {
		cmp := s.core.Compare(parts[1], parts[2])
		return fmt.Sprintf("live=[%s] candidate=[%s] changed=%t",
			outcome(cmp.Live, cmp.LiveErr), outcome(cmp.Candidate, cmp.CandErr), cmp.Changed())
	}
	return "ERR BADREQUEST want: QUERY <name> | QUERYAT <name> <zone@version,...> | CANDQUERY <id> <name> | COMPARE <id> <name> | SETQUERY <set-id> <name> | SETREVIEW <set-id>"
}

func (s *Server) answer(name string, pin map[string]uint64) string {
	var res *resolver.Result
	var err error
	if pin == nil {
		res, err = s.core.Resolve(name)
	} else {
		res, err = s.core.ResolveAt(name, pin)
	}
	if err != nil {
		if re, ok := err.(*resolver.ResolveError); ok {
			return "ERR " + re.Code + " " + re.Error()
		}
		return "ERR INTERNAL " + err.Error()
	}
	return formatResult(res)
}

func formatResult(res *resolver.Result) string {
	var b strings.Builder
	for _, c := range res.Chain {
		fmt.Fprintf(&b, "CNAME %s %s; ", c.Name, c.Value)
	}
	for _, a := range res.Records {
		fmt.Fprintf(&b, "OK %s A %s; ", a.Name, a.Value)
	}
	fmt.Fprintf(&b, "view=%s source=%s cache=%t", res.View, res.Source, res.FromCache)
	return b.String()
}

func outcome(res *resolver.Result, errStr string) string {
	if errStr != "" {
		return "ERR " + errStr
	}
	return formatResult(res)
}

// parsePin parses "zone@version,zone@version" into a pin map.
func parsePin(s string) (map[string]uint64, error) {
	pin := make(map[string]uint64)
	for _, part := range strings.Split(s, ",") {
		zn, vs, ok := strings.Cut(part, "@")
		if !ok {
			return nil, fmt.Errorf("bad pin %q, want zone@version", part)
		}
		v, err := strconv.ParseUint(vs, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad version in %q", part)
		}
		pin[resolver.NormalizeName(zn)] = v
	}
	return pin, nil
}
