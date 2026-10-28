package dnsrouter

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestForwardRetriesTruncatedUDPOverTCP(t *testing.T) {
	var udpRequests, tcpRequests atomic.Int32
	address := startDNSServer(t, dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(request)
		if writer.LocalAddr().Network() == "udp" {
			udpRequests.Add(1)
			response.Truncated = true
		} else {
			tcpRequests.Add(1)
			response.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
				A:   net.ParseIP("192.0.2.8").To4(),
			}}
		}
		_ = writer.WriteMsg(response)
	}))
	router := newRouter(t, baseConfig())
	group := &runtimeGroup{name: "default", forward: []string{address}}
	request := new(dns.Msg)
	request.SetQuestion("test.example.", dns.TypeA)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := router.resolveForward(ctx, group, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Answer) != 1 || udpRequests.Load() != 1 || tcpRequests.Load() != 1 {
		t.Fatalf("response=%v, udp=%d, tcp=%d", response.Answer, udpRequests.Load(), tcpRequests.Load())
	}
}

func startDNSServer(t *testing.T, handler dns.Handler) string {
	t.Helper()
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := udp.LocalAddr().(*net.UDPAddr).Port
	tcp, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		_ = udp.Close()
		t.Fatal(err)
	}
	udpServer := &dns.Server{PacketConn: udp, Handler: handler}
	tcpServer := &dns.Server{Listener: tcp, Handler: handler}
	go func() { _ = udpServer.ActivateAndServe() }()
	go func() { _ = tcpServer.ActivateAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = udpServer.ShutdownContext(ctx)
		_ = tcpServer.ShutdownContext(ctx)
	})
	return fmt.Sprintf("127.0.0.1:%d", port)
}
