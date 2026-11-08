//go:build !darwin

package main

import (
	"context"
	"errors"
)

type packetSession struct{ done chan error }

func startPacketSession(context.Context, Tunnel, string, ...*packetRouting) (*packetSession, error) {
	return nil, errors.New("系统隧道仅支持 macOS")
}
func startPacketProbe(context.Context, Tunnel) (*packetSession, error) {
	return nil, errors.New("系统隧道仅支持 macOS")
}
func (s *packetSession) deviceError() error     { return nil }
func (s *packetSession) Close()                 {}
func (s *packetSession) bytes() (int64, int64)  { return 0, 0 }
func recoverPacketTunnel(context.Context) error { return errors.New("系统隧道仅支持 macOS") }
