//go:build !darwin

package main

import (
	"context"
	"errors"
)

type packetSession struct{ done chan error }

func startPacketSession(context.Context, Tunnel, string) (*packetSession, error) {
	return nil, errors.New("系统隧道仅支持 macOS")
}
func (s *packetSession) Close()                 {}
func recoverPacketTunnel(context.Context) error { return errors.New("系统隧道仅支持 macOS") }
