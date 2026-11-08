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

type physicalTunnel struct{ dialer net.Dialer }

func (p *physicalTunnel) Close() {}
func (p *physicalTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	// AI mode may only send ordinary IPv4 TCP directly. IPv6 is protected by
	// the packet policy; do not let a hostname dial pick a physical AAAA path.
	if network == "tcp" {
		network = "tcp4"
	}
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
	p := &physicalTunnel{}
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
	return p, nil
}

func (p *physicalTunnel) useTunnelDNS(t Tunnel) { p.dialer.Resolver = resolverThroughTunnel(t) }
