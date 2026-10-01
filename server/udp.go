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
//	QUERY <name>            resolve against the current view
//	QUERYAT <view> <name>   resolve against a retained historical view
//	VIEWS                   list the recorded view history
//
//	RESPONSE: "OK <name> A <value> ..." | "CNAME <target> ..." | "ERR <code> <detail>"
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
	if len(parts) == 0 {
		return "ERR BADREQUEST want: QUERY <name> | QUERYAT <view> <name> | VIEWS"
	}
	switch {
	case len(parts) == 1 && strings.EqualFold(parts[0], "VIEWS"):
		var b strings.Builder
		for _, v := range s.core.History() {
			fmt.Fprintf(&b, "VIEW %s; ", v)
		}
		fmt.Fprintf(&b, "current=%d", s.core.CurrentViewID())
		return b.String()

	case len(parts) == 2 && strings.EqualFold(parts[0], "QUERY"):
		return s.answer(parts[1], func(name string) (*resolver.Result, error) {
			return s.core.Resolve(name)
		})

	case len(parts) == 3 && strings.EqualFold(parts[0], "QUERYAT"):
		id, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			return "ERR BADREQUEST view must be a number"
		}
		return s.answer(parts[2], func(name string) (*resolver.Result, error) {
			return s.core.ResolveInView(id, name)
		})
	}
	return "ERR BADREQUEST want: QUERY <name> | QUERYAT <view> <name> | VIEWS"
}

func (s *Server) answer(name string, resolve func(string) (*resolver.Result, error)) string {
	res, err := resolve(name)
	if err != nil {
		if re, ok := err.(*resolver.ResolveError); ok {
			return "ERR " + re.Code + " " + re.Error()
		}
		return "ERR INTERNAL " + err.Error()
	}
	var b strings.Builder
	for _, c := range res.Chain {
		fmt.Fprintf(&b, "CNAME %s %s; ", c.Name, c.Value)
	}
	for _, a := range res.Records {
		fmt.Fprintf(&b, "OK %s A %s; ", a.Name, a.Value)
	}
	fmt.Fprintf(&b, "source=%s view=%d cache=%t", res.Source, res.View, res.FromCache)
	return b.String()
}
