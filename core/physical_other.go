//go:build !darwin

package main

import (
	"context"
	"errors"
	"net"
)

func capturePhysicalTunnel(context.Context) (*physicalTunnel, error) {
	return nil, errors.New("分流直连仅支持 macOS")
}

type physicalTunnel struct{}

func (p *physicalTunnel) Close() {}
func (p *physicalTunnel) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("分流直连仅支持 macOS")
}

func (p *physicalTunnel) useTunnelDNS(Tunnel) {}
