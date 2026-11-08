package main

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type physicalTunnel struct {
	dialer  net.Dialer
	servers []string
}

func (p *physicalTunnel) Close() {}
func (p *physicalTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return p.dialer.DialContext(ctx, network, address)
}
func capturePhysicalTunnel(ctx context.Context) (*physicalTunnel, error) {
	data, err := exec.CommandContext(ctx, "/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return nil, errors.New("无法读取本地直连入口")
	}
	var name string
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[0] == "interface:" {
			name = parts[1]
		}
	}
	if !regexp.MustCompile(`^(en|bridge|bond)[0-9]+$`).MatchString(name) {
		return nil, errors.New("AI 分流需要物理网络入口，请先停用其他 VPN")
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, errors.New("本地直连网卡不可用")
	}
	data, err = exec.CommandContext(ctx, "/usr/sbin/scutil", "--dns").Output()
	if err != nil {
		return nil, errors.New("无法保存本地 DNS")
	}
	var servers []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 3 || !strings.HasPrefix(parts[0], "nameserver[") || parts[1] != ":" {
			continue
		}
		ip := net.ParseIP(parts[2])
		if ip == nil || ip.String() == "198.18.0.2" || seen[ip.String()] {
			continue
		}
		seen[ip.String()] = true
		servers = append(servers, net.JoinHostPort(ip.String(), "53"))
	}
	if len(servers) == 0 {
		return nil, errors.New("没有可用的本地 DNS，请先恢复原网络设置")
	}
	p := &physicalTunnel{servers: servers}
	p.dialer = net.Dialer{Timeout: 12 * time.Second, KeepAlive: 30 * time.Second, Control: func(network, address string, raw syscall.RawConn) error {
		var failure error
		err := raw.Control(func(fd uintptr) {
			if strings.HasSuffix(network, "6") {
				failure = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, iface.Index)
			} else {
				failure = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, iface.Index)
			}
		})
		if err != nil {
			return err
		}
		return failure
	}}
	// Even after system DNS switches to utun, direct domains resolve only via
	// the captured physical resolvers. No implicit resolver fallback is allowed.
	dnsDialer := p.dialer
	p.dialer.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var last error
		for _, server := range servers {
			c, err := dnsDialer.DialContext(ctx, network, server)
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}}
	return p, nil
}
