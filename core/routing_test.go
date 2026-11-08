package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaudeDomainBoundaries(t *testing.T) {
	p, e := NewRoutingPolicy("", nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, host := range []string{"claude.ai", "api.anthropic.com", "sso.claude.com", "bridge.claudeusercontent.com", "widget.claudemcpcontent.com", "API.ANTHROPIC.COM."} {
		if p.Route(host) != "tunnel" {
			t.Errorf("must tunnel %s", host)
		}
	}
	for _, host := range []string{"www.apple.com", "github.com", "claude.ai.evil.example", "notclaude.ai", "anthropic.com.evil.example", "localhost", "printer.local", "127.0.0.1", "192.168.1.1", "::1", "8.8.8.8"} {
		if p.Route(host) != "direct" {
			t.Errorf("must direct %s", host)
		}
	}
}
func TestRoutingModesAndValidation(t *testing.T) {
	p, e := NewRoutingPolicy("claude", []string{" *.Example.COM. "}, nil)
	if e != nil || p.Route("a.example.com") != "tunnel" || p.Route("claude.ai") != "direct" {
		t.Fatal("custom list failed", e)
	}
	p, _ = NewRoutingPolicy("claude", []string{}, nil)
	if p.Route("claude.ai") != "direct" {
		t.Fatal("explicit empty list ignored")
	}
	p, _ = NewRoutingPolicy("smart", nil, []string{"example.com"})
	if p.Route("a.example.com") != "direct" || p.Route("github.com") != "tunnel" {
		t.Fatal("smart failed")
	}
	p, _ = NewRoutingPolicy("all", nil, nil)
	if p.Route("127.0.0.1") != "tunnel" {
		t.Fatal("all failed")
	}
	for _, domain := range []string{"https://claude.ai", "claude.ai:443", "bad..com", "-bad.com", "*", "127.0.0.1", "bad/com"} {
		if _, e := NewRoutingPolicy("claude", []string{domain}, nil); e == nil {
			t.Errorf("accepted %q", domain)
		}
	}
	if _, e := NewRoutingPolicy("unknown", nil, nil); e == nil {
		t.Fatal("unknown mode accepted")
	}
}

type routeTestTunnel struct {
	target string
	fail   bool
	calls  atomic.Int32
}

func (t *routeTestTunnel) Close() {}
func (t *routeTestTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	t.calls.Add(1)
	if t.fail {
		return nil, errors.New("offline")
	}
	return (&net.Dialer{}).DialContext(ctx, network, t.target)
}
func TestWhitelistProxyAndNoDirectFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "route-fixture") }))
	defer server.Close()
	tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
	policy, _ := NewRoutingPolicy("claude", []string{"tunnel.example"}, nil)
	p, e := NewProxy(tunnel, policy, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	hp, _ := p.ports()
	proxyURL, _ := url.Parse("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(hp)))
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: time.Second * 3}
	for _, target := range []string{server.URL, "http://tunnel.example/"} {
		r, e := client.Get(target)
		if e != nil {
			t.Fatal(e)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if string(b) != "route-fixture" {
			t.Fatal(string(b))
		}
	}
	if tunnel.calls.Load() != 1 {
		t.Fatal("direct traffic entered tunnel")
	}
	// A tunnel error must produce 502 and never contact the local direct target.
	failing := &routeTestTunnel{fail: true}
	all, _ := NewRoutingPolicy("all", nil, nil)
	q, e := NewProxy(failing, all, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer q.Close()
	port, _ := q.ports()
	u, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(port))
	tr2 := &http.Transport{Proxy: http.ProxyURL(u)}
	defer tr2.CloseIdleConnections()
	r, e := (&http.Client{Transport: tr2, Timeout: 3 * time.Second}).Get(server.URL)
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != 502 {
		t.Fatal("fell back to direct", r.StatusCode)
	}
}
