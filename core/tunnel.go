package main

import (
	"context"
	"errors"
	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"
	"net"
	"strings"
	"sync"
)

type Tunnel interface {
	DialContext(context.Context, string, string) (net.Conn, error)
	Close()
}
type AWGTunnel struct {
	net  *netstack.Net
	dev  *device.Device
	once sync.Once
}

func NewAWG(ctx context.Context, raw string) (*AWGTunnel, error) {
	c, e := ParseAWG(raw)
	if e != nil {
		return nil, e
	}
	host, port, _ := net.SplitHostPort(c.Endpoint)
	ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
	if e != nil || len(ips) == 0 {
		return nil, errors.New("无法解析服务器地址")
	}
	var resolved string
	for _, a := range ips {
		if a.IP.To4() != nil {
			resolved = a.IP.String()
			break
		}
	}
	if resolved == "" {
		resolved = ips[0].IP.String()
	}
	tun, tnet, e := netstack.CreateNetTUN(c.Addresses, c.DNS, c.MTU)
	if e != nil {
		return nil, errors.New("创建连接内核失败")
	}
	dev := device.NewDevice(tun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	// Avoid allocating two workers per host CPU for a personal client.
	ipc := c.IPC + "endpoint=" + net.JoinHostPort(resolved, port) + "\n"
	if e = dev.IpcSet(ipc); e != nil {
		dev.Close()
		return nil, errors.New("内核不接受该协议配置，请检查 AWG 参数")
	}
	if e = dev.Up(); e != nil {
		dev.Close()
		return nil, errors.New("无法启动 VPN 连接")
	}
	return &AWGTunnel{net: tnet, dev: dev}, nil
}
func (t *AWGTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return t.net.DialContext(ctx, network, address)
}
func (t *AWGTunnel) Close() { t.once.Do(func() { t.dev.Close() }) }

func OpenTunnel(ctx context.Context, p Profile) (Tunnel, error) {
	switch strings.ToLower(p.Kind) {
	case "awg":
		t, err := NewAWG(ctx, p.Config)
		if err != nil {
			return nil, err
		}
		return t, nil
	case "hysteria2":
		t, err := NewHY(ctx, p.Config)
		if err != nil {
			return nil, err
		}
		return t, nil
	default:
		return nil, errors.New("未知协议")
	}
}
