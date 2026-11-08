package main

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// One stalled DNS lookup must not block other transactions on the same socket.
type gatedDNSTunnel struct {
	entered      chan string
	release      chan struct{}
	finished     chan struct{}
	mu           sync.Mutex
	active, peak int
}

func (t *gatedDNSTunnel) Close() {}
func (t *gatedDNSTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if network != "tcp" || (address != "1.1.1.1:53" && address != "9.9.9.9:53") {
		panic("DNS escaped tunnel resolver")
	}
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		stop := context.AfterFunc(ctx, func() { b.Close() })
		defer stop()
		query, err := readDatagram(b)
		if err != nil {
			return
		}
		var message dnsmessage.Message
		if message.Unpack(query) != nil || len(message.Questions) != 1 {
			return
		}
		t.mu.Lock()
		t.active++
		t.peak = max(t.peak, t.active)
		t.mu.Unlock()
		defer func() {
			t.mu.Lock()
			t.active--
			t.mu.Unlock()
			if t.finished != nil {
				t.finished <- struct{}{}
			}
		}()
		select {
		case t.entered <- message.Questions[0].Name.String():
		case <-ctx.Done():
			return
		}
		if message.Questions[0].Name.String() == "slow.example." {
			select {
			case <-t.release:
			case <-ctx.Done():
				return
			}
		}
		message.Header.Response = true
		message.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: message.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 30}, Body: &dnsmessage.AResource{A: [4]byte{198, 51, 100, 23}}}}
		reply, _ := message.Pack()
		_ = writeDatagram(b, reply)
	}()
	return a, nil
}
func dnsConcurrencyQuery(t *testing.T, name string, id uint16) []byte {
	t.Helper()
	query, err := dnsProbeQuery(name)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint16(query, id)
	return query
}
func TestDNSUDPStalledQueryDoesNotBlockSameSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := &gatedDNSTunnel{entered: make(chan string, 2), release: make(chan struct{})}
	a, b := net.Pipe()
	client := &datagramStream{Conn: a}
	server := &datagramStream{Conn: b}
	p := &packetStack{ctx: ctx, tunnel: upstream, dnsSlots: make(chan struct{}, maxConcurrentDNSQueries)}
	done := make(chan struct{})
	go func() { defer close(done); p.serveDNS(server, "udp") }()
	t.Cleanup(func() {
		cancel()
		client.Close()
		server.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("DNS worker did not stop")
		}
	})
	client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write(dnsConcurrencyQuery(t, "slow.example.", 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-upstream.entered:
	case <-time.After(time.Second):
		t.Fatal("first DNS query not sent")
	}
	writeDone := make(chan error, 1)
	query := dnsConcurrencyQuery(t, "fast.example.", 2)
	go func() { _, err := client.Write(query); writeDone <- err }()
	select {
	case <-upstream.entered:
	case <-time.After(time.Second):
		t.Fatal("second query blocked behind stalled reply")
	}
	buf := make([]byte, 4096)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	var response dnsmessage.Message
	if response.Unpack(buf[:n]) != nil || response.Header.ID != 2 || response.Questions[0].Name.String() != "fast.example." {
		t.Fatal("concurrent reply mixed transaction IDs")
	}
	close(upstream.release)
	n, err = client.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if response.Unpack(buf[:n]) != nil || response.Header.ID != 1 {
		t.Fatal("stalled query lost its reply")
	}
	if err = <-writeDone; err != nil {
		t.Fatal(err)
	}
}

func TestDNSUDPCancelReleasesQueriesAndBothLimits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	upstream := &gatedDNSTunnel{entered: make(chan string, 1), release: make(chan struct{}), finished: make(chan struct{}, 1)}
	a, b := net.Pipe()
	client := &datagramStream{Conn: a}
	server := &datagramStream{Conn: b}
	p := &packetStack{ctx: ctx, tunnel: upstream, dnsSlots: make(chan struct{}, maxConcurrentDNSQueries)}
	done := make(chan struct{})
	go func() { defer close(done); p.serveDNS(server, "udp") }()
	t.Cleanup(func() { cancel(); client.Close(); server.Close() })
	if _, err := client.Write(dnsConcurrencyQuery(t, "slow.example.", 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-upstream.entered:
	case <-time.After(time.Second):
		t.Fatal("lookup not started")
	}
	cancel()
	client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled lookup leaked a worker")
	}
	if len(p.dnsSlots) != 0 {
		t.Fatal("cancelled lookup retained a concurrency slot")
	}
	select {
	case <-upstream.finished:
	case <-time.After(time.Second):
		t.Fatal("upstream lookup survived cancellation")
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.active != 0 {
		t.Fatal("upstream lookup survived cancellation")
	}
}

func TestDNSConcurrentRepliesRetainTruncationAndFailureSemantics(t *testing.T) {
	for _, large := range []bool{false, true} {
		upstream := &packetTestTunnel{large: large, fail: !large}
		p := &packetStack{ctx: context.Background(), tunnel: upstream}
		query := dnsConcurrencyQuery(t, "ordinary.example.", 7)
		reply, err := p.dnsReply(p.ctx, query, true)
		if err != nil {
			t.Fatal(err)
		}
		var message dnsmessage.Message
		if message.Unpack(reply) != nil || message.Header.ID != 7 || len(message.Questions) != 1 {
			t.Fatal("DNS transaction changed")
		}
		if large && (!message.Header.Truncated || len(reply) > 512 || len(message.Answers) != 0) {
			t.Fatal("UDP did not request TCP retry")
		}
		if !large && message.Header.RCode != dnsmessage.RCodeServerFailure {
			t.Fatal("upstream failure did not fail closed")
		}
		if large {
			reply, err = p.dnsReply(p.ctx, query, false)
			if err != nil {
				t.Fatal(err)
			}
			if message.Unpack(reply) != nil || message.Header.Truncated || len(message.Answers) != 100 {
				t.Fatal("TCP lost full answer")
			}
		}
	}
}

func TestDNSUDPSameSocketConcurrencyIsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	upstream := &gatedDNSTunnel{entered: make(chan string, maxDNSQueriesPerSocket+1), release: make(chan struct{})}
	a, b := net.Pipe()
	client := &datagramStream{Conn: a}
	server := &datagramStream{Conn: b}
	p := &packetStack{ctx: ctx, tunnel: upstream, dnsSlots: make(chan struct{}, maxConcurrentDNSQueries)}
	done := make(chan struct{})
	go func() { defer close(done); p.serveDNS(server, "udp") }()
	t.Cleanup(func() {
		cancel()
		client.Close()
		server.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("DNS workers did not stop")
		}
	})
	client.SetDeadline(time.Now().Add(3 * time.Second))
	for i := 0; i < maxDNSQueriesPerSocket; i++ {
		if _, err := client.Write(dnsConcurrencyQuery(t, "slow.example.", uint16(i+1))); err != nil {
			t.Fatal(err)
		}
		select {
		case <-upstream.entered:
		case <-time.After(time.Second):
			t.Fatal("query did not enter")
		}
	}
	if _, err := client.Write(dnsConcurrencyQuery(t, "slow.example.", uint16(maxDNSQueriesPerSocket+1))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-upstream.entered:
		t.Fatal("per-socket concurrency exceeded")
	case <-time.After(30 * time.Millisecond):
	}
	close(upstream.release)
	seen := map[uint16]bool{}
	buf := make([]byte, 4096)
	for i := 0; i < maxDNSQueriesPerSocket+1; i++ {
		n, err := client.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		var response dnsmessage.Message
		if response.Unpack(buf[:n]) != nil || seen[response.Header.ID] {
			t.Fatal("transaction ID repeated or corrupted")
		}
		seen[response.Header.ID] = true
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.peak > maxDNSQueriesPerSocket {
		t.Fatal("too many upstream queries")
	}
}

func TestDNSUDPConcurrencyLimitIsSharedAcrossSockets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := &gatedDNSTunnel{entered: make(chan string, 3), release: make(chan struct{})}
	// A smaller injected global budget makes sharing observable with two flows.
	p := &packetStack{ctx: ctx, tunnel: upstream, dnsSlots: make(chan struct{}, 2)}
	clients := make([]net.Conn, 2)
	var workers sync.WaitGroup
	for i := range clients {
		a, b := net.Pipe()
		clients[i] = &datagramStream{Conn: a}
		server := &datagramStream{Conn: b}
		workers.Add(1)
		go func() { defer workers.Done(); p.serveDNS(server, "udp") }()
		t.Cleanup(func() { cancel(); a.Close(); b.Close() })
		clients[i].SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := clients[i].Write(dnsConcurrencyQuery(t, "slow.example.", uint16(i+1))); err != nil {
			t.Fatal(err)
		}
		select {
		case <-upstream.entered:
		case <-time.After(time.Second):
			t.Fatal("query did not enter")
		}
	}
	if _, err := clients[0].Write(dnsConcurrencyQuery(t, "slow.example.", 3)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-upstream.entered:
		t.Fatal("global concurrency exceeded")
	case <-time.After(30 * time.Millisecond):
	}
	close(upstream.release)
	reads := make(chan error, 2)
	for i, client := range clients {
		go func() {
			buf := make([]byte, 4096)
			count := 1
			if i == 0 {
				count = 2
			}
			for j := 0; j < count; j++ {
				if _, err := client.Read(buf); err != nil {
					reads <- err
					return
				}
			}
			reads <- nil
		}()
	}
	for range clients {
		if err := <-reads; err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	for _, c := range clients {
		c.Close()
	}
	workers.Wait()
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.peak > 2 {
		t.Fatal("flows did not share global query budget")
	}
}
