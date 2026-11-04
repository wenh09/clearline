package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	hyerrors "github.com/apernet/hysteria/core/v2/errors"
	hyserver "github.com/apernet/hysteria/core/v2/server"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type testOutbound struct {
	tcp func(string) (net.Conn, error)
}

func (o testOutbound) TCP(addr string) (net.Conn, error) { return o.tcp(addr) }
func (o testOutbound) UDP(string) (hyserver.UDPConn, error) {
	return nil, errors.New("UDP not used in fixture")
}
func (o testOutbound) CheckUDP(string) error { return errors.New("UDP not used in fixture") }
func fastOpenFixture(t *testing.T, outbound hyserver.Outbound) *HYTunnel {
	t.Helper()
	cert, ca := testCertificate(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s, err := hyserver.NewServer(&hyserver.Config{TLSConfig: hyserver.TLSConfig{Certificates: []tls.Certificate{cert}}, Conn: pc, Authenticator: testAuth{}, Outbound: outbound})
	if err != nil {
		pc.Close()
		t.Fatal(err)
	}
	go s.Serve()
	t.Cleanup(func() { s.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tunnel, err := newHYConfig(ctx, HYConfig{Server: pc.LocalAddr().String(), Auth: "correct", SNI: "localhost", CA: ca})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnel.Close)
	return tunnel
}
func TestFastOpenOverlapsRemoteConnectAndPreservesEarlyData(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "early-data-arrived") }))
	defer target.Close()
	tunnel := fastOpenFixture(t, testOutbound{tcp: func(addr string) (net.Conn, error) {
		close(started)
		<-release
		return net.DialTimeout("tcp", addr, time.Second)
	}})
	type result struct {
		conn net.Conn
		err  error
	}
	got := make(chan result, 1)
	go func() {
		c, e := tunnel.DialContext(context.Background(), "tcp", strings.TrimPrefix(target.URL, "http://"))
		got <- result{c, e}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		unblock()
		t.Fatal("remote connect did not start")
	}
	var c net.Conn
	select {
	case r := <-got:
		if r.err != nil {
			unblock()
			t.Fatal(r.err)
		}
		c = r.conn
	case <-time.After(2 * time.Second):
		unblock()
		t.Fatal("fast open waited for remote confirmation")
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: fixture\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	unblock()
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "early-data-arrived" {
		t.Fatal("early request data lost")
	}
}
func TestFastOpenSurfacesRemoteRejectionOnRead(t *testing.T) {
	tunnel := fastOpenFixture(t, testOutbound{tcp: func(string) (net.Conn, error) { return nil, errors.New("fixture rejected") }})
	c, err := tunnel.DialContext(context.Background(), "tcp", "denied.example:443")
	if err != nil {
		t.Fatal("stream failed before deferred rejection", err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = c.Read(make([]byte, 1))
	var rejected hyerrors.DialError
	if !errors.As(err, &rejected) || !strings.Contains(err.Error(), "fixture rejected") {
		t.Fatalf("remote rejection lost: %v", err)
	}
}
func TestFastOpenHTTPSKeepsTargetCertificateValidation(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "verified-https") }))
	defer target.Close()
	tunnel := fastOpenFixture(t, nil)
	p, err := NewProxy(tunnel, RoutingPolicy{Mode: "all"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	port, _ := p.ports()
	proxyURL, _ := url.Parse("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	for _, trusted := range []bool{true, false} {
		pool := x509.NewCertPool()
		if trusted {
			pool = roots
		}
		tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool}}
		client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
		resp, e := client.Get(target.URL)
		tr.CloseIdleConnections()
		if trusted {
			if e != nil {
				t.Fatal(e)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) != "verified-https" {
				t.Fatal(string(b))
			}
		} else {
			if resp != nil {
				resp.Body.Close()
			}
			var unknown x509.UnknownAuthorityError
			if !errors.As(e, &unknown) {
				t.Fatalf("untrusted target certificate accepted: %v", e)
			}
		}
	}
}
func TestFastOpenHonorsAlreadyCanceledDial(t *testing.T) {
	tunnel := fastOpenFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c, err := tunnel.DialContext(ctx, "tcp", "unused.example:443"); !errors.Is(err, context.Canceled) {
		if c != nil {
			c.Close()
		}
		t.Fatalf("canceled dial accepted: %v", err)
	}
}
