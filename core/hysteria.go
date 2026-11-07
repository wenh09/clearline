package main

import (
	"context"
	"crypto/x509"
	"errors"
	hy "github.com/apernet/hysteria/core/v2/client"
	hyerrors "github.com/apernet/hysteria/core/v2/errors"
	"github.com/apernet/hysteria/extras/v2/obfs"
	"net"
	"sync"
	"time"
)

type hyFactory struct {
	ctx      context.Context
	password string
	mu       sync.Mutex
	conn     net.PacketConn
}

func (f *hyFactory) New(addr net.Addr) (net.PacketConn, error) {
	c, e := net.ListenUDP("udp", nil)
	if e != nil {
		return nil, e
	}
	var pc net.PacketConn = c
	if f.password != "" {
		pc, e = obfs.WrapPacketConnSalamander(c, []byte(f.password))
		if e != nil {
			c.Close()
			return nil, e
		}
	}
	f.mu.Lock()
	f.conn = pc
	f.mu.Unlock()
	go func() { <-f.ctx.Done(); pc.Close() }()
	return pc, nil
}

type HYTunnel struct {
	client hy.Client
	cancel context.CancelFunc
	once   sync.Once
}

func NewHY(ctx context.Context, raw string) (*HYTunnel, error) {
	c, e := ParseHY(raw)
	if e != nil {
		return nil, e
	}
	return newHYConfig(ctx, c)
}
func newHYConfig(ctx context.Context, c HYConfig) (*HYTunnel, error) {
	return newHYConfigWithFastOpen(ctx, c, true)
}
func newHYConfigWithFastOpen(ctx context.Context, c HYConfig, fastOpen bool) (*HYTunnel, error) {
	addr, e := net.ResolveUDPAddr("udp", c.Server)
	if e != nil {
		return nil, errors.New("无法解析服务器地址")
	}
	life, cancel := context.WithCancel(context.Background())
	f := &hyFactory{ctx: life, password: c.Obfs}
	// Overlap the remote TCP connect with the client TLS handshake. Remote
	// rejection is still returned on Read; TLS and the pinned CA stay enforced.
	cfg := &hy.Config{ConnFactory: f, ServerAddr: addr, Auth: c.Auth, TLSConfig: hy.TLSConfig{ServerName: c.SNI}, QUICConfig: hy.QUICConfig{MaxIdleTimeout: 30 * time.Second, KeepAlivePeriod: 10 * time.Second}, FastOpen: fastOpen}
	if c.CA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(c.CA)) {
			cancel()
			return nil, connectionError{message: "CA 证书无效，请重新导入配置"}
		}
		cfg.TLSConfig.RootCAs = pool
	}
	type result struct {
		client hy.Client
		err    error
	}
	ch := make(chan result)
	go func() {
		cl, _, e := hy.NewClient(cfg)
		select {
		case ch <- result{cl, e}:
		case <-life.Done():
			if cl != nil {
				cl.Close()
			}
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			cancel()
			var auth hyerrors.AuthError
			var cert x509.UnknownAuthorityError
			var hostname x509.HostnameError
			var invalid x509.CertificateInvalidError
			permanent := errors.As(r.err, &auth) || errors.As(r.err, &cert) || errors.As(r.err, &hostname) || errors.As(r.err, &invalid)
			message := "加密连接暂时失败，正在恢复"
			if permanent {
				message = "服务器认证或证书无效，请检查配置"
			}
			return nil, connectionError{message: message, retryable: !permanent, cause: r.err}
		}
		return &HYTunnel{client: r.client, cancel: cancel}, nil
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}
func (t *HYTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if network == "udp" {
		u, err := t.client.UDP()
		if err != nil {
			return nil, err
		}
		a, b := net.Pipe()
		var once sync.Once
		closeBoth := func() { once.Do(func() { u.Close(); b.Close() }) }
		go func() {
			defer closeBoth()
			for {
				packet, err := readDatagram(b)
				if err != nil {
					return
				}
				if u.Send(packet, address) != nil {
					return
				}
			}
		}()
		go func() {
			defer closeBoth()
			for {
				packet, _, err := u.Receive()
				if err != nil {
					return
				}
				if writeDatagram(b, packet) != nil {
					return
				}
			}
		}()
		return &datagramStream{Conn: a}, nil
	}
	if network != "tcp" {
		return nil, net.UnknownNetworkError(network)
	}
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result)
	go func() {
		c, e := t.client.TCP(address)
		select {
		case ch <- result{c, e}:
		case <-ctx.Done():
			if c != nil {
				c.Close()
			}
		}
	}()
	select {
	case r := <-ch:
		return r.conn, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (t *HYTunnel) Close() { t.once.Do(func() { t.cancel(); t.client.Close() }) }
