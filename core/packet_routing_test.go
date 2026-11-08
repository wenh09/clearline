package main

import (
	"context"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"
	"golang.org/x/net/dns/dnsmessage"
)

func splitFixture(t *testing.T) (*packetRouting, *packetTestTunnel, *packetTestTunnel) {
	t.Helper()
	policy, err := NewRoutingPolicy("ai", []string{"ai.example"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := openFakeIPCache(filepath.Join(t.TempDir(), "dns-map.json"))
	if err != nil {
		t.Fatal(err)
	}
	remote, direct := &packetTestTunnel{}, &packetTestTunnel{}
	return &packetRouting{policy: policy, direct: direct, names: cache}, remote, direct
}
func splitQuestion(t *testing.T, host string, kind dnsmessage.Type) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 321, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(host + "."), Type: kind, Class: dnsmessage.ClassINET})
	q, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return q
}
func TestAIStrictDNSUDPAndIPv6ShareServerWhileOrdinaryTCPIsDirect(t *testing.T) {
	r, remote, direct := splitFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, kind := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		answer, err := r.dns(ctx, remote, splitQuestion(t, "sub.ai.example", kind))
		if err != nil {
			t.Fatal(err)
		}
		var m dnsmessage.Message
		if err = m.Unpack(answer); err != nil || len(m.Answers) != 1 {
			t.Fatal("missing protected answer", err)
		}
		var ip netip.Addr
		switch body := m.Answers[0].Body.(type) {
		case *dnsmessage.AResource:
			ip = netip.AddrFrom4(body.A)
		case *dnsmessage.AAAAResource:
			ip = netip.AddrFrom16(body.AAAA)
		}
		if host, fake := r.names.lookup(ip); !fake || host != "sub.ai.example" {
			t.Fatal("AI DNS did not preserve hostname")
		}
		for _, network := range []string{"tcp", "udp"} {
			c, err := r.dial(ctx, remote, network, netip.AddrPortFrom(ip, 443).String())
			if err != nil {
				t.Fatal(err)
			}
			c.Close()
		}
	}
	if len(direct.calls) != 0 {
		t.Fatalf("AI escaped to local resolver/data: %v", direct.calls)
	}
	q := splitQuestion(t, "ordinary.example", dnsmessage.TypeA)
	if _, err := r.dns(ctx, remote, q); err != nil {
		t.Fatal(err)
	}
	for _, network := range []string{"tcp", "udp"} {
		c, err := r.dial(ctx, remote, network, "198.51.100.23:443")
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	direct.mu.Lock()
	calls := append([]string(nil), direct.calls...)
	direct.mu.Unlock()
	if len(calls) != 1 || calls[0] != "tcp 198.51.100.23:443" {
		t.Fatalf("ordinary traffic did not stay local: %v", calls)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	for _, call := range remote.calls {
		if call != "tcp 1.1.1.1:53" && call != "tcp sub.ai.example:443" && call != "udp sub.ai.example:443" && call != "udp 198.51.100.23:443" {
			t.Fatalf("unexpected remote call: %s", call)
		}
	}
}
func TestAIStrictDNSNeverFallsBackToLocalForAnyDomain(t *testing.T) {
	r, remote, direct := splitFixture(t)
	remote.fail = true
	for _, host := range []string{"ai.example", "ordinary.example"} {
		if _, err := r.dns(context.Background(), remote, splitQuestion(t, host, dnsmessage.TypeA)); err == nil {
			t.Fatal("offline DNS resolved", host)
		}
	}
	if len(direct.calls) != 0 {
		t.Fatal("DNS fell back to local resolver")
	}
	remote.fail = false
	direct.fail = true
	if _, err := r.dns(context.Background(), remote, splitQuestion(t, "ordinary.example", dnsmessage.TypeA)); err != nil {
		t.Fatal("DNS depended on local resolver", err)
	}
}
func TestProxyHostnameResolutionAlsoUsesTunnelDNS(t *testing.T) {
	remote := &packetTestTunnel{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := resolverThroughTunnel(remote).LookupIPAddr(ctx, "ordinary.example")
	if err != nil || len(ips) != 2 {
		t.Fatal("tunneled hostname resolution", err)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	for _, call := range remote.calls {
		if call != "tcp 1.1.1.1:53" {
			t.Fatal("resolver escaped tunnel", call)
		}
	}
}
func TestIPv6AndEncryptedDNSCannotUsePhysicalFallback(t *testing.T) {
	r, remote, direct := splitFixture(t)
	for _, addr := range []string{"[2001:db8::23]:443", "198.51.100.23:853", "1.1.1.1:443", "8.8.8.8:443"} {
		c, err := r.dial(context.Background(), remote, "tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	if len(direct.calls) != 0 {
		t.Fatal("IPv6 or encrypted DNS escaped")
	}
	remote.fail = true
	if _, err := r.dial(context.Background(), remote, "tcp", "[2001:db8::23]:443"); err == nil {
		t.Fatal("offline IPv6 succeeded")
	}
	if len(direct.calls) != 0 {
		t.Fatal("IPv6 fell back to physical network")
	}
}
func TestFakeIPCacheSurvivesReconnectAndRejectsUnknownAddresses(t *testing.T) {
	r, remote, direct := splitFixture(t)
	a, err := r.names.address("first.ai.example", false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.names.address("second.ai.example", true)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := openFakeIPCache(r.names.path)
	if err != nil {
		t.Fatal(err)
	}
	for ip, host := range map[netip.Addr]string{a: "first.ai.example", b: "second.ai.example"} {
		got, fake := loaded.lookup(ip)
		if !fake || got != host {
			t.Fatal("mapping changed across reconnect")
		}
	}
	info, err := os.Stat(r.names.path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private DNS cache permissions", err)
	}
	if _, err = r.dial(context.Background(), remote, "tcp", "198.19.254.253:443"); err == nil {
		t.Fatal("unknown stale fake IP accepted")
	}
	if len(direct.calls) != 0 || len(remote.calls) != 0 {
		t.Fatal("unknown fake address escaped")
	}
	if err = os.WriteFile(r.names.path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = openFakeIPCache(r.names.path); err == nil {
		t.Fatal("corrupt mapping accepted")
	}
}
func TestSplitUnknownUDPAndSTUNKeepProtectedExit(t *testing.T) {
	r, remote, direct := splitFixture(t)
	for _, addr := range []string{"198.51.100.99:9999", "198.51.100.23:3478"} {
		c, err := r.dial(context.Background(), remote, "udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	if len(remote.calls) != 2 || len(direct.calls) != 0 {
		t.Fatal("unattributed UDP leaked")
	}
}
func TestAIHTTPSHintsCannotEscapeSyntheticAddressRouting(t *testing.T) {
	r, remote, direct := splitFixture(t)
	q := splitQuestion(t, "ai.example", 65)
	reply, err := r.dns(context.Background(), remote, q)
	if err != nil {
		t.Fatal(err)
	}
	var m dnsmessage.Message
	if m.Unpack(reply) != nil || m.Header.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 || len(m.Additionals) != 0 {
		t.Fatal("HTTPS address hint escaped split routing")
	}
	if len(direct.calls) != 0 {
		t.Fatal("HTTPS query escaped to local DNS")
	}
}
func TestAllDNSAndPreviouslyCachedAIAddressesUseTunnel(t *testing.T) {
	r, remote, direct := splitFixture(t)
	r.policy.Mode = "all"
	if _, err := r.dns(context.Background(), remote, splitQuestion(t, "ordinary.example", dnsmessage.TypeA)); err != nil {
		t.Fatal(err)
	}
	ip, err := r.names.address("ai.example", false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.dial(context.Background(), remote, "tcp", netip.AddrPortFrom(ip, 443).String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if len(direct.calls) != 0 || remote.calls[0] != "tcp 1.1.1.1:53" || remote.calls[1] != "tcp ai.example:443" {
		t.Fatal("all-mode cached routing mismatch")
	}
}
func TestSplitPacketStackDNSAndBothFamiliesTCPUDP(t *testing.T) {
	r, remote, direct := splitFixture(t)
	clientDevice, client, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("2001:db8::2")}, []netip.Addr{netip.MustParseAddr("198.18.0.2")}, 1280)
	if err != nil {
		t.Fatal(err)
	}
	device := &channelDevice{incoming: make(chan []byte, 128), outgoing: make(chan []byte, 128), done: make(chan struct{})}
	server, err := newPacketStack(context.Background(), device, remote, r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close(); clientDevice.Close() })
	go func() {
		for {
			b := make([]byte, 65535)
			sizes := []int{0}
			if _, e := clientDevice.Read([][]byte{b}, sizes, 0); e != nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ips, err := client.LookupContextHost(ctx, "ai.example")
	if err != nil || len(ips) != 2 {
		t.Fatal("split DNS through virtual stack", err)
	}
	for _, ip := range ips {
		for _, network := range []string{"tcp", "udp"} {
			addr := netip.MustParseAddr(ip)
			c, err := client.DialContext(ctx, network, netip.AddrPortFrom(addr, 443).String())
			if err != nil {
				t.Fatal(err)
			}
			c.SetDeadline(time.Now().Add(3 * time.Second))
			want := []byte("split-protected")
			if _, err = c.Write(want); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(want))
			_, err = io.ReadFull(c, got)
			c.Close()
			if err != nil || string(got) != string(want) {
				t.Fatal("split packet data", err)
			}
		}
	}
	if len(direct.calls) != 0 {
		t.Fatal("AI packets used physical fallback")
	}
}

func TestAIProxyCommandsCannotBypassProtectedIPv6OrDNS(t *testing.T) {
	r, remote, direct := splitFixture(t)
	p, err := newProxy(remote, r.policy, nil, direct)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, addr := range []string{"[2001:db8::23]:443", "198.51.100.23:853", "1.1.1.1:443"} {
		c, err := p.dial(context.Background(), addr)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	if len(direct.calls) != 0 || len(remote.calls) != 3 {
		t.Fatal("explicit proxy command escaped AI protection")
	}
}
