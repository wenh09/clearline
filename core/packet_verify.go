package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const probeDNSAddress = "192.0.2.254"
const ipv4TraceURL = "https://1.1.1.1/cdn-cgi/trace"

type dialConnection func(context.Context, string, string) (net.Conn, error)
type directProbeTunnel struct{}

func effectiveDNSOwned(output string) bool {
	count := 0
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && strings.HasPrefix(fields[0], "nameserver[") && fields[1] == ":" {
			if fields[2] != "198.18.0.2" {
				return false
			}
			count++
		}
	}
	return count > 0
}

func (directProbeTunnel) Close() {}
func (directProbeTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

func dnsProbeQuery(name string) ([]byte, error) {
	var id [2]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: binary.BigEndian.Uint16(id[:]), RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	n, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, err
	}
	if err = b.Question(dnsmessage.Question{Name: n, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}); err != nil {
		return nil, err
	}
	return b.Finish()
}

func dnsProbeAnswers(query, reply []byte) ([]netip.Addr, error) {
	var q, r dnsmessage.Parser
	qh, qe := q.Start(query)
	rh, re := r.Start(reply)
	if qe != nil || re != nil || !rh.Response || rh.Truncated || rh.ID != qh.ID || rh.RCode != dnsmessage.RCodeSuccess {
		return nil, errors.New("DNS 检测响应无效")
	}
	qq, qe := q.AllQuestions()
	rq, re := r.AllQuestions()
	if qe != nil || re != nil || len(qq) != 1 || len(rq) != 1 || qq[0] != rq[0] {
		return nil, errors.New("DNS 检测问题不匹配")
	}
	answers, err := r.AllAnswers()
	if err != nil {
		return nil, errors.New("DNS 检测响应无效")
	}
	var ips []netip.Addr
	for _, a := range answers {
		if ip, ok := a.Body.(*dnsmessage.AResource); ok {
			ips = append(ips, netip.AddrFrom4(ip.A))
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("DNS 检测未返回地址")
	}
	return ips, nil
}

func verifyDNSPath(ctx context.Context, dial dialConnection, address, network string) error {
	_, err := queryDNSPath(ctx, dial, address, network, "example.com.")
	return err
}
func queryDNSPath(ctx context.Context, dial dialConnection, address, network, name string) ([]netip.Addr, error) {
	query, err := dnsProbeQuery(name)
	if err != nil {
		return nil, err
	}
	c, err := dial(ctx, network, net.JoinHostPort(address, "53"))
	if err != nil {
		return nil, errors.New("DNS 检测连接失败")
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		c.SetDeadline(deadline)
	}
	var reply []byte
	if network == "tcp" {
		if err = writeDatagram(c, query); err == nil {
			reply, err = readDatagram(c)
		}
	} else {
		_, err = c.Write(query)
		if err == nil {
			b := make([]byte, 4096)
			var n int
			n, err = c.Read(b)
			reply = b[:n]
		}
	}
	if err != nil {
		return nil, errors.New("DNS 检测未收到响应")
	}
	return dnsProbeAnswers(query, reply)
}

type packetTraceTunnel struct{}

func (packetTraceTunnel) Close() {}
func (packetTraceTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(packetTraceAddress, port))
}
func verifySplitPacketPath(parent context.Context, tunnel Tunnel, policy RoutingPolicy) error {
	ctx, cancel := context.WithTimeout(parent, 16*time.Second)
	defer cancel()
	expected, err := verifyExitAt(ctx, tunnel, ipv4TraceURL)
	if err != nil {
		return errors.New("分流验证：无法读取服务器出口")
	}
	actual, err := verifyExitAt(ctx, packetTraceTunnel{}, ipv4TraceURL)
	if err != nil || actual.IP != expected.IP {
		return errors.New("分流验证：AI 系统 TCP 未经过原服务器")
	}
	if expected.Country != "US" {
		return errors.New("AI 验证：服务器出口不在预期美国地区")
	}
	if len(policy.Domains) == 0 {
		return errors.New("分流验证：AI 域名列表为空")
	}
	for _, network := range []string{"udp", "tcp"} {
		ips, err := queryDNSPath(ctx, (directProbeTunnel{}).DialContext, "198.18.0.2", network, policy.Domains[0]+".")
		if err != nil || len(ips) == 0 {
			return errors.New("分流验证：AI " + network + " DNS 不可用")
		}
		for _, ip := range ips {
			if !fakeIPv4.Contains(ip) {
				return errors.New("分流验证：AI DNS 未受分流规则保护")
			}
		}
		ips, err = queryDNSPath(ctx, (directProbeTunnel{}).DialContext, "198.18.0.2", network, "example.com.")
		if err != nil || len(ips) == 0 {
			return errors.New("分流验证：普通域名的隧道 DNS 不可用")
		}
		for _, ip := range ips {
			if fakeIPv4.Contains(ip) {
				return errors.New("分流验证：普通 DNS 分流错误")
			}
		}
	}
	target, err := resolveSTUNThroughTunnel(ctx, tunnel)
	if err != nil {
		return errors.New("分流验证：UDP 检测解析失败")
	}
	if err = verifyUDPExit(ctx, (directProbeTunnel{}).DialContext, target, expected.IP); err != nil {
		return errors.New("分流验证：UDP 出口未受保护")
	}
	return checkEffectiveDNS(ctx)
}

func resolveSTUNThroughTunnel(ctx context.Context, t Tunnel) (string, error) {
	q, err := dnsProbeQuery("stun.cloudflare.com.")
	if err != nil {
		return "", err
	}
	r, err := dnsThroughTunnel(ctx, t, q)
	if err != nil {
		return "", err
	}
	ips, err := dnsProbeAnswers(q, r)
	if err != nil {
		return "", err
	}
	for _, ip := range ips {
		if ip.IsGlobalUnicast() && !ip.IsPrivate() {
			return net.JoinHostPort(ip.String(), "3478"), nil
		}
	}
	return "", errors.New("UDP 检测服务器地址无效")
}

func parseSTUNAddress(reply, request []byte) (netip.Addr, error) {
	invalid := errors.New("UDP 检测响应无效")
	if len(request) != 20 || len(reply) < 20 || binary.BigEndian.Uint16(reply[:2]) != 0x0101 || !bytes.Equal(reply[4:20], request[4:20]) {
		return netip.Addr{}, invalid
	}
	length := int(binary.BigEndian.Uint16(reply[2:4]))
	if length%4 != 0 || length+20 != len(reply) {
		return netip.Addr{}, invalid
	}
	for offset := 20; offset+4 <= len(reply); {
		kind, size := binary.BigEndian.Uint16(reply[offset:]), int(binary.BigEndian.Uint16(reply[offset+2:]))
		offset += 4
		if offset+size > len(reply) {
			return netip.Addr{}, invalid
		}
		if kind == 0x0020 && size == 8 && reply[offset+1] == 1 {
			var address [4]byte
			for i := range address {
				address[i] = reply[offset+4+i] ^ request[4+i]
			}
			ip := netip.AddrFrom4(address)
			if ip.IsGlobalUnicast() && !ip.IsPrivate() {
				return ip, nil
			}
			return netip.Addr{}, invalid
		}
		offset += (size + 3) &^ 3
	}
	return netip.Addr{}, invalid
}

func verifyUDPExit(ctx context.Context, dial dialConnection, target, expected string) error {
	request := make([]byte, 20)
	binary.BigEndian.PutUint16(request[:2], 1)
	binary.BigEndian.PutUint32(request[4:8], 0x2112a442)
	if _, err := rand.Read(request[8:]); err != nil {
		return err
	}
	c, err := dial(ctx, "udp", target)
	if err != nil {
		return errors.New("UDP 转发连接失败")
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		c.SetDeadline(deadline)
	}
	if _, err = c.Write(request); err != nil {
		return errors.New("UDP 检测发送失败")
	}
	reply := make([]byte, 4096)
	n, err := c.Read(reply)
	if err != nil {
		return errors.New("UDP 检测未收到响应")
	}
	ip, err := parseSTUNAddress(reply[:n], request)
	if err != nil {
		return err
	}
	if ip.String() != expected {
		return errors.New("UDP 出口与服务器通道出口不一致")
	}
	return nil
}

// No proxy environment or system HTTP proxy is used by the direct checks.
// They prove the installed kernel routes, rather than just the inner tunnel.
func verifyGlobalPacketPath(parent context.Context, t Tunnel) error {
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	expected, err := verifyExitAt(ctx, t, ipv4TraceURL)
	if err != nil {
		return errors.New("全局验证：无法读取服务器 IPv4 出口")
	}
	target, err := resolveSTUNThroughTunnel(ctx, t)
	if err != nil {
		return errors.New("全局验证：隧道内 DNS 不可用")
	}
	direct := directProbeTunnel{}
	for _, network := range []string{"udp", "tcp"} {
		if err = verifyDNSPath(ctx, direct.DialContext, "198.18.0.2", network); err != nil {
			return errors.New("全局验证：系统 " + network + " DNS 未通过")
		}
	}
	actual, err := verifyExitAt(ctx, direct, ipv4TraceURL)
	if err != nil || actual.IP != expected.IP {
		return errors.New("全局验证：系统 TCP 未经过原服务器")
	}
	if err = verifyUDPExit(ctx, direct.DialContext, target, expected.IP); err != nil {
		return errors.New("全局验证：" + err.Error())
	}
	if err = checkEffectiveDNS(ctx); err != nil {
		return err
	}
	return nil
}

func diagnosePacketTunnel(profile Profile, emit func(any)) error {
	parent, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(parent, 40*time.Second)
	defer cancel()
	progress := func(stage string) { emit(map[string]any{"event": "diagnosticProgress", "stage": stage}) }
	progress("transport")
	t, err := OpenTunnel(ctx, profile)
	if err != nil {
		return errors.New("诊断：原服务器加密连接失败")
	}
	defer t.Close()
	expected, err := verifyExitAt(ctx, t, ipv4TraceURL)
	if err != nil {
		return errors.New("诊断：服务器 IPv4 出口验证失败")
	}
	progress("udp_relay")
	target, err := resolveSTUNThroughTunnel(ctx, t)
	if err != nil {
		return errors.New("诊断：隧道内 DNS 不可用")
	}
	if err = verifyUDPExit(ctx, t.DialContext, target, expected.IP); err != nil {
		return errors.New("诊断：" + err.Error())
	}
	progress("utun")
	packets, err := startPacketProbe(ctx, t)
	if err != nil {
		return err
	}
	defer packets.Close()
	for _, address := range []string{probeDNSAddress, "fd7a:115c:a1e0:ffff::2"} {
		for _, network := range []string{"udp", "tcp"} {
			prefix := "utun_dns_"
			if address != probeDNSAddress {
				prefix = "utun6_dns_"
			}
			progress(prefix + network)
			attempt, done := context.WithTimeout(ctx, 5*time.Second)
			err = verifyDNSPath(attempt, (directProbeTunnel{}).DialContext, address, network)
			done()
			if err != nil {
				if failure := packets.deviceError(); failure != nil {
					return failure
				}
				return errors.New("诊断：虚拟网卡 " + prefix + network + " DNS 数据路径未通过")
			}
		}
	}
	packets.Close()
	emit(map[string]any{"event": "tunnelDiagnostic", "ok": true, "systemChanged": false, "message": "虚拟网卡、DNS 双向读写与 UDP 出口验证通过；未修改系统 DNS 或默认路由。"})
	return nil
}
