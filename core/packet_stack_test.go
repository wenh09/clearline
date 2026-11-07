package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"
	"golang.org/x/net/dns/dnsmessage"
)

type packetTestTunnel struct {
	mu    sync.Mutex
	calls []string
	fail  bool
	large bool
}

func (t *packetTestTunnel) Close() {}
func (t *packetTestTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	t.mu.Lock()
	t.calls = append(t.calls, network+" "+address)
	large := t.large
	t.mu.Unlock()
	if t.fail {
		return nil, errors.New("offline")
	}
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		if strings.HasSuffix(address, ":53") {
			query, e := readDatagram(b)
			if e != nil {
				return
			}
			var p dnsmessage.Parser
			h, e := p.Start(query)
			if e != nil {
				return
			}
			q, e := p.Question()
			if e != nil {
				return
			}
			builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, RecursionDesired: true, RecursionAvailable: true})
			builder.StartQuestions()
			builder.Question(q)
			builder.StartAnswers()
			count := 1
			if large {
				count = 100
			}
			for i := 0; i < count; i++ {
				if q.Type == dnsmessage.TypeA {
					builder.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 30}, dnsmessage.AResource{A: [4]byte{198, 51, 100, 23}})
				} else if q.Type == dnsmessage.TypeAAAA {
					builder.AAAAResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 30}, dnsmessage.AAAAResource{AAAA: netip.MustParseAddr("2001:db8::23").As16()})
				}
			}
			reply, _ := builder.Finish()
			writeDatagram(b, reply)
			return
		}
		buffer := make([]byte, 65535)
		for {
			n, e := b.Read(buffer)
			if e != nil {
				return
			}
			if _, e = b.Write(buffer[:n]); e != nil {
				return
			}
		}
	}()
	return a, nil
}

type channelDevice struct {
	incoming, outgoing chan []byte
	done               chan struct{}
	once               sync.Once
}

func (d *channelDevice) Read(b []byte) (int, error) {
	select {
	case packet := <-d.incoming:
		return copy(b, packet), nil
	case <-d.done:
		return 0, io.EOF
	}
}
func (d *channelDevice) Write(b []byte) (int, error) {
	select {
	case d.outgoing <- append([]byte(nil), b...):
		return len(b), nil
	case <-d.done:
		return 0, io.EOF
	}
}
func (d *channelDevice) Close() error { d.once.Do(func() { close(d.done) }); return nil }

func TestPacketStackForwardsIPv4IPv6TCPUDPAndSystemDNS(t *testing.T) {
	clientDevice, client, e := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("2001:db8::2")}, []netip.Addr{netip.MustParseAddr("198.18.0.2")}, 1280)
	if e != nil {
		t.Fatal(e)
	}
	device := &channelDevice{incoming: make(chan []byte, 128), outgoing: make(chan []byte, 128), done: make(chan struct{})}
	upstream := &packetTestTunnel{}
	server, e := newPacketStack(context.Background(), device, upstream)
	if e != nil {
		t.Fatal(e)
	}
	go func() {
		for {
			b := make([]byte, 65535)
			sizes := []int{0}
			_, e := clientDevice.Read([][]byte{b}, sizes, 0)
			if e != nil {
				return
			}
			select {
			case device.incoming <- b[:sizes[0]]:
			case <-device.done:
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case b := <-device.outgoing:
				clientDevice.Write([][]byte{b}, 0)
			case <-device.done:
				return
			}
		}
	}()
	t.Cleanup(func() { server.Close(); clientDevice.Close() })
	for _, network := range []string{"tcp", "udp"} {
		for _, target := range []string{"198.51.100.5:23456", "[2001:db8::5]:23456"} {
			t.Run(network+target, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				c, e := client.DialContext(ctx, network, target)
				if e != nil {
					t.Fatal(e)
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				want := []byte("packet-through-tunnel")
				if _, e = c.Write(want); e != nil {
					t.Fatal(e)
				}
				got := make([]byte, len(want))
				if _, e = io.ReadFull(c, got); e != nil {
					t.Fatal(e)
				}
				if string(got) != string(want) {
					t.Fatalf("payload %q", got)
				}
			})
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, e := client.LookupContextHost(ctx, "dns-test.example")
	if e != nil {
		t.Fatal(e)
	}
	if len(ips) != 2 {
		t.Fatalf("DNS responses: %v", ips)
	}
	upstream.mu.Lock()
	upstream.large = true
	upstream.mu.Unlock()
	ips, e = client.LookupContextHost(ctx, "dns-big.example")
	if e != nil || len(ips) < 100 {
		t.Fatalf("large UDP DNS did not retry over TCP: count=%d error=%v", len(ips), e)
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	for _, call := range upstream.calls {
		if strings.HasSuffix(call, ":53") && call != "tcp 1.1.1.1:53" {
			t.Fatalf("DNS escaped configured tunnel resolver: %s", call)
		}
	}
}

func TestDNSFailsClosedWhenTunnelUnavailable(t *testing.T) {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 123, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName("private-test.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	query, _ := b.Finish()
	tunnel := &packetTestTunnel{fail: true}
	if _, e := dnsThroughTunnel(context.Background(), tunnel, query); e == nil {
		t.Fatal("offline DNS unexpectedly succeeded")
	}
	if len(tunnel.calls) != 2 || tunnel.calls[0] != "tcp 1.1.1.1:53" || tunnel.calls[1] != "tcp 9.9.9.9:53" {
		t.Fatalf("unexpected DNS path: %v", tunnel.calls)
	}
}
func TestAuthenticatedWorkerPreservesUDPPackets(t *testing.T) {
	upstream := &packetTestTunnel{}
	policy, _ := NewRoutingPolicy("all", nil, nil)
	p, e := NewProxy(upstream, policy, nil, "test-worker-token")
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	_, port := p.ports()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c, e := dialWorkerUDP(ctx, address, "wrong-token", "198.51.100.5:23456"); e == nil {
		c.Close()
		t.Fatal("accepted wrong token")
	}
	c, e := dialWorkerUDP(ctx, address, "test-worker-token", "198.51.100.5:23456")
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	for _, want := range []string{"one", "second packet", "third"} {
		if _, e = c.Write([]byte(want)); e != nil {
			t.Fatal(e)
		}
		buf := make([]byte, 100)
		n, e := c.Read(buf)
		if e != nil || string(buf[:n]) != want {
			t.Fatalf("packet %q: %q %v", want, buf[:n], e)
		}
	}
}

func TestDatagramFramingRejectsOversizeAndConsumesWholePacket(t *testing.T) {
	if e := writeDatagram(io.Discard, make([]byte, 65536)); e == nil {
		t.Fatal("oversize packet accepted")
	}
	var h [2]byte
	binary.BigEndian.PutUint16(h[:], 4)
	if _, e := readDatagram(io.MultiReader(strings.NewReader(string(h[:])), strings.NewReader("ab"))); !errors.Is(e, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated packet: %v", e)
	}
}

func TestPacketEntryPinPreservesTLSIdentityAndCredential(t *testing.T) {
	profile := Profile{Kind: "hysteria2", Config: "hy2://fixture-user:fixture-password@192.0.2.10:8443?sni=original.example&obfs=salamander&obfs-password=fixture"}
	pinned, endpoint, e := pinPacketEndpoint(context.Background(), Request{Profile: profile})
	if e != nil {
		t.Fatal(e)
	}
	a, e := ParseHY(pinned.Profile.Config)
	if e != nil {
		t.Fatal(e)
	}
	if endpoint != "192.0.2.10" || a.SNI != "original.example" || a.Auth != "fixture-user:fixture-password" || a.Server != "192.0.2.10:8443" {
		t.Fatal("entry pin changed identity or credential")
	}
}

func TestRealHysteriaUDPThroughAuthenticatedWorker(t *testing.T) {
	echo, e := net.ListenPacket("udp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer echo.Close()
	go func() {
		b := make([]byte, 65535)
		for {
			n, addr, e := echo.ReadFrom(b)
			if e != nil {
				return
			}
			echo.WriteTo(b[:n], addr)
		}
	}()
	tunnel := fastOpenFixture(t, nil)
	proxy, e := NewProxy(tunnel, RoutingPolicy{Mode: "all"}, nil, "udp-real-fixture")
	if e != nil {
		t.Fatal(e)
	}
	defer proxy.Close()
	_, port := proxy.ports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, e := dialWorkerUDP(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), "udp-real-fixture", echo.LocalAddr().String())
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	for _, size := range []int{8, 3000, 17} {
		want := strings.Repeat("x", size)
		if _, e = c.Write([]byte(want)); e != nil {
			t.Fatal(e)
		}
		b := make([]byte, 65535)
		n, e := c.Read(b)
		if e != nil || string(b[:n]) != want {
			t.Fatalf("Hysteria UDP length %d, got %d: %v", size, n, e)
		}
	}
}
