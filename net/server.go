// Package netadapt exposes the resolution core over UDP and TCP. The wire
// format is one JSON request/response object; the first version talks only
// to the local fictional zones and never forwards queries to the public
// internet.
package netadapt

import (
	"encoding/json"
	"errors"
	"net"
	"time"

	"localdns/resolver"
)

// Request is a client query on the wire.
type Request struct {
	Name string        `json:"name"`
	Kind resolver.Kind `json:"kind"`
}

// Response wraps a core result plus adapter metadata. No upstream field is
// ever present: public resolution is intentionally unsupported in v1.
type Response struct {
	*resolver.Result
	ServerTime int64 `json:"server_time"`
}

// Serve serves both UDP and TCP listeners sharing one resolver core. It
// blocks until both listeners stop; call Close to shut them down.
type Server struct {
	sys *resolver.System
	udp *net.UDPConn
	tcp net.Listener
}

// NewServer wraps a core without binding sockets; useful for testing the
// protocol over net.Pipe / in-memory packet handlers.
func NewServer(sys *resolver.System) *Server { return &Server{sys: sys} }

// Listen binds UDP and TCP on the given host:port ("127.0.0.1:5354").
func Listen(sys *resolver.System, addr string) (*Server, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	uc, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, err
	}
	tl, err := net.Listen("tcp", ua.String())
	if err != nil {
		uc.Close()
		return nil, err
	}
	s := NewServer(sys)
	s.udp = uc
	s.tcp = tl
	go s.serveUDP()
	go s.serveTCP()
	return s, nil
}

// Addr reports the concrete TCP address (useful with port :0 in tests).
func (s *Server) Addr() net.Addr { return s.tcp.Addr() }

// UDPAddr reports the bound UDP address.
func (s *Server) UDPAddr() *net.UDPAddr { return s.udp.LocalAddr().(*net.UDPAddr) }

// Close stops both listeners.
func (s *Server) Close() error {
	s.udp.Close()
	return s.tcp.Close()
}

func (s *Server) serveUDP() {
	buf := make([]byte, 65536)
	for {
		n, peer, err := s.udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		out := s.HandlePacket(buf[:n])
		s.udp.WriteToUDP(out, peer)
	}
}

// HandlePacket runs one UDP request payload through the core and returns
// the response bytes (no sockets involved).
// HandlePacket runs one UDP request payload through the core and returns
// the response bytes (no sockets involved).
func (s *Server) HandlePacket(payload []byte) []byte {
	req, perr := decode(payload)
	if perr != nil {
		out, _ := encodeError("", "bad request: "+perr.Error())
		return out
	}
	res := s.sys.Resolver.Resolve(req.Name, req.Kind)
	out, _ := json.Marshal(Response{Result: res, ServerTime: time.Now().Unix()})
	return out
}

func (s *Server) serveTCP() {
	for {
		conn, err := s.tcp.Accept()
		if err != nil {
			return
		}
		go s.ServeConn(conn)
	}
}

// ServeConn processes JSON requests/responses on one TCP-like connection.
func (s *Server) ServeConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		res := s.sys.Resolver.Resolve(req.Name, req.Kind)
		if err := enc.Encode(Response{Result: res, ServerTime: time.Now().Unix()}); err != nil {
			return
		}
	}
}

func decode(data []byte) (Request, error) {
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		return req, err
	}
	if req.Kind == "" {
		req.Kind = resolver.A
	}
	if req.Name == "" {
		return req, errors.New("empty name")
	}
	return req, nil
}

func encodeError(name, msg string) ([]byte, error) {
	return json.Marshal(Response{Result: &resolver.Result{
		Status: "BAD_REQUEST", Question: name, Err: msg,
	}})
}

// QueryUDP sends one JSON request over UDP and returns the response.
func QueryUDP(addr, name string, kind resolver.Kind) (*resolver.Result, error) {
	ra, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	c, err := net.DialUDP("udp", nil, ra)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	payload, _ := json.Marshal(Request{Name: name, Kind: kind})
	if _, err := c.Write(payload); err != nil {
		return nil, err
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 65536)
	n, err := c.Read(buf)
	if err != nil {
		return nil, err
	}
	var resp Response
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		return nil, err
	}
	return resp.Result, nil
}

// QueryTCP sends one JSON request over a TCP stream and returns the response.
func QueryTCP(addr, name string, kind resolver.Kind) (*resolver.Result, error) {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	enc := json.NewEncoder(c)
	dec := json.NewDecoder(c)
	if err := enc.Encode(Request{Name: name, Kind: kind}); err != nil {
		return nil, err
	}
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return nil, err
	}
	return resp.Result, nil
}
