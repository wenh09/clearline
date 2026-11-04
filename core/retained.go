package main

import (
	"context"
	"errors"
	"net"
	"sync"

	"golang.org/x/net/proxy"
)

// Local listeners outlive individual encrypted transports. Only the tunnel
// branch waits during recovery; ordinary direct requests keep their own path.
type retainedTunnel struct {
	mu      sync.Mutex
	active  Tunnel
	changed chan struct{}
	closed  bool
}

func newRetainedTunnel() *retainedTunnel { return &retainedTunnel{changed: make(chan struct{})} }
func (t *retainedTunnel) set(active Tunnel) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		if active != nil {
			active.Close()
		}
		return
	}
	if t.active != nil {
		t.active.Close()
	}
	t.active = active
	close(t.changed)
	t.changed = make(chan struct{})
}
func (t *retainedTunnel) snapshot() Tunnel { t.mu.Lock(); defer t.mu.Unlock(); return t.active }
func (t *retainedTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	for {
		t.mu.Lock()
		active, changed, closed := t.active, t.changed, t.closed
		t.mu.Unlock()
		if closed {
			return nil, errors.New("连接已停止")
		}
		if active != nil {
			return active.DialContext(ctx, network, address)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}
func (t *retainedTunnel) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	if t.active != nil {
		t.active.Close()
		t.active = nil
	}
	close(t.changed)
}

type workerTunnel struct {
	dialer proxy.ContextDialer
	ctx    context.Context
	cancel context.CancelFunc
}

func newWorkerTunnel(parent context.Context, address, token string) (*workerTunnel, error) {
	dialer, err := proxy.SOCKS5("tcp", address, &proxy.Auth{User: "clearline", Password: token}, &net.Dialer{})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	return &workerTunnel{dialer: dialer.(proxy.ContextDialer), ctx: ctx, cancel: cancel}, nil
}
func (t *workerTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	request, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.ctx, cancel)
	defer stop()
	defer cancel()
	if t.ctx.Err() != nil {
		return nil, t.ctx.Err()
	}
	return t.dialer.DialContext(request, network, address)
}
func (t *workerTunnel) Close() { t.cancel() }
