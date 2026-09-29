package netadapt

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"localdns/resolver"
)

func protoSystem(t *testing.T) *resolver.System {
	t.Helper()
	sys := resolver.NewSystem("", 30)
	if err := sys.Commit([]resolver.ZoneInput{
		{
			Origin: "example.test",
			Records: []resolver.Record{
				{Name: "example.test", Kind: resolver.A, Data: "10.0.0.1", TTL: 60},
				{Name: "www.example.test", Kind: resolver.CNAME, Data: "example.test", TTL: 60},
			},
		},
		{
			Origin: "city.example.test",
			Records: []resolver.Record{
				{Name: "gw.city.example.test", Kind: resolver.A, Data: "10.1.0.1", TTL: 30},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return sys
}

func TestUDPPacketPathUsesSameCore(t *testing.T) {
	srv := NewServer(protoSystem(t))
	payload, _ := json.Marshal(Request{Name: "www.example.test", Kind: resolver.A})
	var resp Response
	if err := json.Unmarshal(srv.HandlePacket(payload), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != resolver.Success || resp.Answers[0].Data != "10.0.0.1" {
		t.Fatalf("packet result=%+v", resp.Result)
	}
	// Delegation is enforced through the same core.
	p2, _ := json.Marshal(Request{Name: "gw.city.example.test", Kind: resolver.A})
	json.Unmarshal(srv.HandlePacket(p2), &resp)
	if resp.Authority != "city.example.test" || resp.Answers[0].Data != "10.1.0.1" {
		t.Fatalf("delegation=%+v", resp.Result)
	}
}

func TestTCPStreamPathOverPipe(t *testing.T) {
	srv := NewServer(protoSystem(t))
	a, b := net.Pipe()
	defer a.Close()
	go srv.ServeConn(b)
	a.SetDeadline(time.Now().Add(3 * time.Second))
	enc := json.NewEncoder(a)
	dec := json.NewDecoder(a)
	if err := enc.Encode(Request{Name: "ghost.example.test", Kind: resolver.A}); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != resolver.NXDomain {
		t.Fatalf("want NXDOMAIN got %+v", resp.Result)
	}
}
