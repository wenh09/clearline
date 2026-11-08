package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

type packetDevice interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
}

// DNS, TCP and UDP follow the selected policy; direct sockets are explicitly
// bound to the original physical interface and never use this virtual resolver.
type packetStack struct {
	stack       *stack.Stack
	link        *channel.Endpoint
	device      packetDevice
	tunnel      Tunnel
	routing     *packetRouting
	ctx         context.Context
	cancel      context.CancelFunc
	connections sync.Map
	slots       chan struct{}
	dnsSlots    chan struct{}
	once        sync.Once
	up, down    atomic.Int64
	failureLock sync.Mutex
	failure     error
}

func (p *packetStack) deviceFailure(operation string, err error) {
	if p.ctx.Err() != nil {
		return
	}
	var code syscall.Errno
	message := "虚拟网卡" + operation + "失败"
	if errors.As(err, &code) {
		message += fmt.Sprintf("（系统错误 %d）", code)
	}
	p.failureLock.Lock()
	if p.failure == nil {
		p.failure = errors.New(message)
	}
	p.failureLock.Unlock()
	p.cancel()
}
func (p *packetStack) deviceError() error {
	p.failureLock.Lock()
	defer p.failureLock.Unlock()
	return p.failure
}

func newPacketStack(parent context.Context, device packetDevice, tunnel Tunnel, routing ...*packetRouting) (*packetStack, error) {
	ctx, cancel := context.WithCancel(parent)
	p := &packetStack{device: device, tunnel: tunnel, ctx: ctx, cancel: cancel, slots: make(chan struct{}, 1024), dnsSlots: make(chan struct{}, maxConcurrentDNSQueries)}
	if len(routing) > 0 {
		p.routing = routing[0]
	}
	p.stack = stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol}})
	p.link = channel.New(1024, 1280, "")
	if e := p.stack.CreateNIC(1, p.link); e != nil {
		p.Close()
		return nil, errors.New("无法创建隧道网络栈")
	}
	p.stack.SetPromiscuousMode(1, true)
	p.stack.SetSpoofing(1, true)
	p.stack.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}, {Destination: header.IPv6EmptySubnet, NIC: 1}})
	tcpForwarder := tcp.NewForwarder(p.stack, 0, 512, func(r *tcp.ForwarderRequest) {
		select {
		case p.slots <- struct{}{}:
		default:
			r.Complete(true)
			return
		}
		go func() {
			defer func() { <-p.slots }()
			id := r.ID()
			var q waiter.Queue
			ep, e := r.CreateEndpoint(&q)
			if e != nil {
				r.Complete(true)
				return
			}
			r.Complete(false)
			p.forward(gonet.NewTCPConn(&q, ep), id, "tcp")
		}()
	})
	p.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)
	udpForwarder := udp.NewForwarder(p.stack, func(r *udp.ForwarderRequest) {
		id := r.ID()
		var q waiter.Queue
		ep, e := r.CreateEndpoint(&q)
		if e != nil {
			return
		}
		select {
		case p.slots <- struct{}{}:
		default:
			ep.Close()
			return
		}
		go func() { defer func() { <-p.slots }(); p.forward(gonet.NewUDPConn(p.stack, &q, ep), id, "udp") }()
	})
	p.stack.SetTransportProtocolHandler(udp.ProtocolNumber, func(id stack.TransportEndpointID, packet stack.PacketBufferPtr) bool {
		// The pinned gVisor UDP forwarder takes a reference but exposes no
		// request Release method. Endpoint creation above is synchronous and
		// the endpoint takes its own reference, so release the request's here.
		defer packet.DecRef()
		return udpForwarder.HandlePacket(id, packet)
	})
	go func() {
		b := make([]byte, 65536)
		for {
			n, e := device.Read(b)
			if e != nil {
				p.deviceFailure("读取", e)
				return
			}
			if n < 1 {
				continue
			}
			p.up.Add(int64(n))
			var proto tcpip.NetworkProtocolNumber
			switch b[0] >> 4 {
			case 4:
				proto = ipv4.ProtocolNumber
			case 6:
				proto = ipv6.ProtocolNumber
			default:
				continue
			}
			packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), b[:n]...))})
			p.link.InjectInbound(proto, packet)
			packet.DecRef()
		}
	}()
	go func() {
		for {
			packet := p.link.ReadContext(ctx)
			if packet == nil {
				return
			}
			view := packet.ToView()
			n, err := device.Write(view.AsSlice())
			p.down.Add(int64(n))
			view.Release()
			packet.DecRef()
			if err != nil {
				p.deviceFailure("写入", err)
				return
			}
		}
	}()
	return p, nil
}
func (p *packetStack) forward(local net.Conn, id stack.TransportEndpointID, network string) {
	p.connections.Store(local, true)
	defer p.connections.Delete(local)
	defer local.Close()
	if p.ctx.Err() != nil {
		return
	}
	if id.LocalPort == 53 {
		p.serveDNS(local, network)
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, 12*time.Second)
	address := net.JoinHostPort(id.LocalAddress.String(), strconv.Itoa(int(id.LocalPort)))
	var remote net.Conn
	var err error
	if p.routing != nil {
		remote, err = p.routing.dial(ctx, p.tunnel, network, address)
	} else {
		remote, err = p.tunnel.DialContext(ctx, network, address)
	}
	cancel()
	if err != nil {
		return
	}
	p.connections.Store(remote, true)
	defer p.connections.Delete(remote)
	defer remote.Close()
	if network == "udp" {
		relayDatagrams(local, remote)
		return
	}
	done := make(chan struct{}, 2)
	copyStream := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if err != nil {
			dst.Close()
			src.Close()
		}
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
		done <- struct{}{}
	}
	go copyStream(local, remote)
	go copyStream(remote, local)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-p.ctx.Done():
			return
		}
	}
}
func (p *packetStack) Close() {
	p.once.Do(func() {
		p.cancel()
		p.device.Close()
		p.connections.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
		p.stack.Close()
		p.link.Close()
	})
}

func dnsThroughTunnel(ctx context.Context, t Tunnel, query []byte) ([]byte, error) {
	return dnsThroughServers(ctx, t, query, []string{"1.1.1.1:53", "9.9.9.9:53"})
}
func dnsThroughServers(ctx context.Context, t Tunnel, query []byte, servers []string, local ...bool) ([]byte, error) {
	if t == nil {
		return nil, errors.New("DNS 入口不可用")
	}
	var parser dnsmessage.Parser
	h, err := parser.Start(query)
	if err != nil || h.Response {
		return nil, errors.New("DNS 查询无效")
	}
	questions, err := parser.AllQuestions()
	if err != nil || len(questions) != 1 {
		return nil, errors.New("DNS 查询无效")
	}
	for _, server := range servers {
		networks := []string{"tcp"}
		if len(local) > 0 && local[0] {
			networks = []string{"udp", "tcp"}
		}
		for _, network := range networks {
			attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
			c, e := t.DialContext(attempt, network, server)
			if e != nil {
				cancel()
				continue
			}
			stop := context.AfterFunc(attempt, func() { c.Close() })
			deadline, _ := attempt.Deadline()
			c.SetDeadline(deadline)
			var reply []byte
			if network == "tcp" {
				e = writeDatagram(c, query)
				if e == nil {
					reply, e = readDatagram(c)
				}
			} else {
				_, e = c.Write(query)
				if e == nil {
					b := make([]byte, 65535)
					var n int
					n, e = c.Read(b)
					reply = b[:n]
				}
			}
			stop()
			c.Close()
			cancel()
			if e != nil {
				continue
			}
			var response dnsmessage.Parser
			rh, e := response.Start(reply)
			if e != nil || !rh.Response || rh.ID != h.ID || rh.Truncated {
				continue
			}
			rq, e := response.AllQuestions()
			if e != nil || len(rq) != 1 || rq[0].Type != questions[0].Type || rq[0].Class != questions[0].Class || !strings.EqualFold(rq[0].Name.String(), questions[0].Name.String()) {
				continue
			}
			return reply, nil
		}
	}
	return nil, errors.New("隧道 DNS 暂不可用")
}

const maxDNSQueriesPerSocket = 16
const maxConcurrentDNSQueries = 128

func (p *packetStack) dnsReply(parent context.Context, query []byte, truncate bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	var reply []byte
	var e error
	if p.routing != nil {
		reply, e = p.routing.dns(ctx, p.tunnel, query)
	} else {
		reply, e = dnsThroughTunnel(ctx, p.tunnel, query)
	}
	if e != nil {
		var parser dnsmessage.Parser
		header, err := parser.Start(query)
		if err != nil {
			return nil, err
		}
		questions, err := parser.AllQuestions()
		if err != nil {
			return nil, err
		}
		builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: header.ID, Response: true, RecursionDesired: header.RecursionDesired, RCode: dnsmessage.RCodeServerFailure})
		builder.StartQuestions()
		for _, question := range questions {
			builder.Question(question)
		}
		reply, err = builder.Finish()
		if err != nil {
			return nil, err
		}
	}
	if truncate && len(reply) > 512 {
		var parser dnsmessage.Parser
		header, err := parser.Start(reply)
		if err != nil {
			return nil, err
		}
		questions, err := parser.AllQuestions()
		if err != nil {
			return nil, err
		}
		header.Truncated = true
		builder := dnsmessage.NewBuilder(nil, header)
		builder.StartQuestions()
		for _, question := range questions {
			builder.Question(question)
		}
		reply, err = builder.Finish()
		if err != nil {
			return nil, err
		}
	}
	return reply, nil
}
func (p *packetStack) serveDNS(c net.Conn, network string) {
	if network == "udp" {
		p.serveUDPDNS(c)
		return
	}
	for {
		c.SetReadDeadline(time.Now().Add(30 * time.Second))
		query, err := readDatagram(c)
		if err != nil {
			return
		}
		reply, err := p.dnsReply(p.ctx, query, false)
		if err != nil {
			return
		}
		if writeDatagram(c, reply) != nil {
			return
		}
	}
}
func (p *packetStack) serveUDPDNS(c net.Conn) {
	// macOS can multiplex a page's A/AAAA/HTTPS lookups on one UDP socket.
	// Waiting for each remote reply here serializes every cross-ocean round trip.
	// Bound concurrency per socket and across the stack, without changing the
	// resolver, routing policy, transaction IDs, truncation or failure replies.
	ctx, cancel := context.WithCancel(p.ctx)
	var pending sync.WaitGroup
	var writeLock sync.Mutex
	defer func() { cancel(); c.Close(); pending.Wait() }()
	slots := make(chan struct{}, maxDNSQueriesPerSocket)
	buf := make([]byte, 65535)
	for {
		c.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		select {
		case p.dnsSlots <- struct{}{}:
		case <-ctx.Done():
			<-slots
			return
		}
		query := append([]byte(nil), buf[:n]...)
		pending.Add(1)
		go func() {
			defer pending.Done()
			defer func() { <-p.dnsSlots; <-slots }()
			reply, err := p.dnsReply(ctx, query, true)
			if err != nil || ctx.Err() != nil {
				return
			}
			// Keep datagram framing atomic even for stream-backed test connections.
			writeLock.Lock()
			defer writeLock.Unlock()
			if ctx.Err() != nil {
				return
			}
			if _, err = c.Write(reply); err != nil {
				cancel()
				c.Close()
			}
		}()
	}
}
