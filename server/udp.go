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
//	REQUEST:  "QUERY <name>" | "QUERYAT <name> <zone@version,...>" |
//	          "CQUERY <candidate> <name>" | "CCMP <candidate> <name>"
//	RESPONSE: "OK <name> A <value> ..." | "CNAME <target> ..." | "ERR <code> <detail>"
//
// QUERYAT resolves against a historical view: each listed zone is pinned
// to the given version, so a past answer can be reproduced and attributed
// to the zone set that produced it.
//
// CQUERY resolves against an isolated candidate view; CCMP runs the same
// query against the live view and the candidate view and reports both
// outcomes side by side.
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
	if len(parts) == 3 && strings.EqualFold(parts[0], "CQUERY") {
		res, err := s.core.ResolveCandidate(parts[1], parts[2])
		if err != nil {
			return errString(err)
		}
		return okString(res)
	}
	if len(parts) == 3 && strings.EqualFold(parts[0], "CCMP") {
		cmp, err := s.core.CompareQuery(parts[1], parts[2])
		if err != nil {
			return errString(err)
		}
		return fmt.Sprintf("LIVE %s | CANDIDATE %s | DIFFERS %t",
			sideString(cmp.Live, cmp.LiveErr),
			sideString(cmp.Candidate, cmp.CandidateErr),
			cmp.Differs)
	}
	return "ERR BADREQUEST want: QUERY <name> | QUERYAT <name> <zone@version,...> | CQUERY <candidate> <name> | CCMP <candidate> <name>"
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
		return errString(err)
	}
	return okString(res)
}

func errString(err error) string {
	if re, ok := err.(*resolver.ResolveError); ok {
		return "ERR " + re.Code + " " + re.Error()
	}
	return "ERR INTERNAL " + err.Error()
}

func okString(res *resolver.Result) string {
	var b strings.Builder
	for _, c := range res.Chain {
		fmt.Fprintf(&b, "CNAME %s %s; ", c.Name, c.Value)
	}
	for _, a := range res.Records {
		fmt.Fprintf(&b, "OK %s A %s; ", a.Name, a.Value)
	}
	fmt.Fprintf(&b, "source=%s cache=%t", res.Source, res.FromCache)
	if res.View != "" {
		fmt.Fprintf(&b, " view=%s", res.View)
	}
	return b.String()
}

func sideString(res *resolver.Result, errCode string) string {
	if errCode != "" {
		return "ERR " + errCode
	}
	return okString(res)
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
