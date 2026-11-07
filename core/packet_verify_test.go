package main

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"
)

func stunFixture(request []byte, ip netip.Addr) []byte {
	b := make([]byte, 32)
	binary.BigEndian.PutUint16(b, 0x0101)
	binary.BigEndian.PutUint16(b[2:], 12)
	copy(b[4:20], request[4:20])
	binary.BigEndian.PutUint16(b[20:], 0x0020)
	binary.BigEndian.PutUint16(b[22:], 8)
	b[25] = 1
	a := ip.As4()
	for i := range a {
		b[28+i] = a[i] ^ request[4+i]
	}
	return b
}

func TestEffectiveDNSRejectsRemainingISPOrScopedResolvers(t *testing.T) {
	if !effectiveDNSOwned("resolver #1\n nameserver[0] : 198.18.0.2\nresolver #2\n domain : local\n") {
		t.Fatal("owned DNS rejected")
	}
	for _, output := range []string{"", "nameserver[0] : 192.0.2.53", "nameserver[0] : 198.18.0.2\n nameserver[1] : 192.0.2.53", "nameserver[0] : 198.18.0.2\n nameserver[0] : 2001:db8::53"} {
		if effectiveDNSOwned(output) {
			t.Fatal("unowned resolver accepted")
		}
	}
}

func TestSTUNRejectsSpoofedTruncatedAndMismatchedResponses(t *testing.T) {
	request := make([]byte, 20)
	binary.BigEndian.PutUint32(request[4:8], 0x2112a442)
	copy(request[8:], "fixture-nonc")
	want := netip.MustParseAddr("198.51.100.42")
	response := stunFixture(request, want)
	got, err := parseSTUNAddress(response, request)
	if err != nil || got != want {
		t.Fatalf("valid response: %v %v", got, err)
	}
	for _, change := range []func([]byte) []byte{
		func(b []byte) []byte { b[8] ^= 1; return b },
		func(b []byte) []byte { b[4] ^= 1; return b },
		func(b []byte) []byte { b[0] = 0; return b },
		func(b []byte) []byte { b[23] = 99; return b },
		func(b []byte) []byte { return b[:len(b)-1] },
	} {
		if _, err := parseSTUNAddress(change(append([]byte(nil), response...)), request); err == nil {
			t.Fatal("invalid STUN response accepted")
		}
	}
}

func TestUDPExitCheckUsesDatagramPathAndRejectsDifferentExit(t *testing.T) {
	for _, expected := range []string{"198.51.100.42", "198.51.100.43"} {
		t.Run(expected, func(t *testing.T) {
			dial := func(ctx context.Context, network, address string) (net.Conn, error) {
				if network != "udp" || address != "192.0.2.1:3478" {
					t.Error("unexpected datagram path")
				}
				a, b := net.Pipe()
				go func() {
					defer b.Close()
					request := make([]byte, 20)
					n, err := b.Read(request)
					if err == nil && n == 20 {
						b.Write(stunFixture(request, netip.MustParseAddr("198.51.100.42")))
					}
				}()
				return a, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := verifyUDPExit(ctx, dial, "192.0.2.1:3478", expected)
			if (err == nil) != (expected == "198.51.100.42") {
				t.Fatalf("exit matching failed: %v", err)
			}
		})
	}
}

func TestDNSDataPathChecksBothUDPAndTCPAndRejectsWrongQuery(t *testing.T) {
	for _, network := range []string{"udp", "tcp"} {
		upstream := &packetTestTunnel{}
		dial := func(ctx context.Context, kind, address string) (net.Conn, error) {
			if kind != network || address != "192.0.2.254:53" {
				t.Error("unexpected DNS path")
			}
			c, err := upstream.DialContext(ctx, "tcp", "1.1.1.1:53")
			if kind == "udp" && err == nil {
				return &datagramStream{Conn: c}, nil
			}
			return c, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := verifyDNSPath(ctx, dial, probeDNSAddress, network)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	q, _ := dnsProbeQuery("example.com.")
	other, _ := dnsProbeQuery("different.example.")
	if _, err := dnsProbeAnswers(q, other); err == nil {
		t.Fatal("mismatched DNS answer accepted")
	}
}
