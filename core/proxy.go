package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Proxy struct {
	internalToken string
	tunnel        Tunnel
	policy        RoutingPolicy
	audit         func(string, string)
	httpListener  net.Listener
	socksListener net.Listener
	httpServer    *http.Server
	httpTransport *http.Transport
	connections   sync.Map
	closed        chan struct{}
	once          sync.Once
	up            atomic.Int64
	down          atomic.Int64
}

func (p *Proxy) dial(ctx context.Context, address string) (net.Conn, error) {
	host, _, e := net.SplitHostPort(address)
	if e != nil {
		return nil, errors.New("invalid address")
	}
	route := p.policy.Route(host)
	if p.audit != nil {
		p.audit(host, route)
	}
	direct := route == "direct"
	if direct {
		return (&net.Dialer{Timeout: 12 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", address)
	}
	return p.tunnel.DialContext(ctx, "tcp", address)
}

func NewProxy(t Tunnel, policy RoutingPolicy, audit func(string, string), tokens ...string) (*Proxy, error) {
	h, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return nil, e
	}
	s, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		h.Close()
		return nil, e
	}
	token := ""
	if len(tokens) != 0 {
		token = tokens[0]
	}
	p := &Proxy{internalToken: token, tunnel: t, policy: policy, audit: audit, httpListener: h, socksListener: s, closed: make(chan struct{})}
	p.httpTransport = &http.Transport{DialContext: func(ctx context.Context, _, address string) (net.Conn, error) { return p.dial(ctx, address) }, ResponseHeaderTimeout: 20 * time.Second, MaxIdleConns: 32, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}
	p.httpServer = &http.Server{Handler: http.HandlerFunc(p.handleHTTP), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32768}
	go p.httpServer.Serve(h)
	go func() {
		for {
			c, e := s.Accept()
			if e != nil {
				return
			}
			p.connections.Store(c, true)
			go func() { defer p.connections.Delete(c); defer c.Close(); p.handleSOCKS(c) }()
		}
	}()
	return p, nil
}
func (p *Proxy) ports() (int, int) {
	return p.httpListener.Addr().(*net.TCPAddr).Port, p.socksListener.Addr().(*net.TCPAddr).Port
}
func (p *Proxy) Close() {
	p.once.Do(func() {
		close(p.closed)
		p.httpServer.Close()
		p.httpTransport.CloseIdleConnections()
		p.httpListener.Close()
		p.socksListener.Close()
		p.connections.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
		p.tunnel.Close()
	})
}

type countWriter struct {
	io.Writer
	counter *atomic.Int64
}

func (w countWriter) Write(b []byte) (int, error) {
	n, e := w.Writer.Write(b)
	w.counter.Add(int64(n))
	return n, e
}
func (p *Proxy) bridge(client net.Conn, reader io.Reader, upstream net.Conn) {
	p.connections.Store(client, true)
	p.connections.Store(upstream, true)
	defer p.connections.Delete(client)
	defer p.connections.Delete(upstream)
	defer client.Close()
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(countWriter{upstream, &p.up}, reader)
		if x, ok := upstream.(interface{ CloseWrite() error }); ok {
			x.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		io.Copy(countWriter{client, &p.down}, upstream)
		if x, ok := client.(interface{ CloseWrite() error }); ok {
			x.CloseWrite()
		}
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-p.closed:
	}
	client.SetDeadline(time.Now().Add(2 * time.Second))
	upstream.SetDeadline(time.Now().Add(2 * time.Second))
}
func stripHop(h http.Header) {
	for _, s := range strings.Split(h.Get("Connection"), ",") {
		h.Del(strings.TrimSpace(s))
	}
	for _, s := range []string{"Connection", "Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(s)
	}
}
func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if p.internalToken != "" {
		expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("clearline:"+p.internalToken))
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Proxy-Authorization")), []byte(expected)) != 1 {
			w.Header().Set("Proxy-Authenticate", "Basic realm=clearline")
			http.Error(w, "内部连接需要认证", http.StatusProxyAuthRequired)
			return
		}
	}
	if r.Method == http.MethodConnect {
		dest := r.Host
		if _, _, e := net.SplitHostPort(dest); e != nil {
			dest = net.JoinHostPort(dest, "443")
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		up, e := p.dial(ctx, dest)
		cancel()
		if e != nil {
			http.Error(w, "连接失败", http.StatusBadGateway)
			return
		}
		client, buf, e := w.(http.Hijacker).Hijack()
		if e != nil {
			up.Close()
			return
		}
		if _, e = buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); e != nil {
			client.Close()
			up.Close()
			return
		}
		buf.Flush()
		p.bridge(client, buf, up)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "absolute HTTP URL required", 400)
		return
	}
	clone := r.Clone(r.Context())
	clone.RequestURI = ""
	stripHop(clone.Header)
	resp, e := p.httpTransport.RoundTrip(clone)
	if e != nil {
		http.Error(w, "连接失败", 502)
		return
	}
	defer resp.Body.Close()
	stripHop(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(countWriter{w, &p.down}, resp.Body)
}

func (p *Proxy) handleSOCKS(c net.Conn) {
	c.SetDeadline(time.Now().Add(15 * time.Second))
	b := bufio.NewReader(c)
	v, e := b.ReadByte()
	if e != nil || v != 5 {
		return
	}
	n, e := b.ReadByte()
	if e != nil {
		return
	}
	methods := make([]byte, int(n))
	if _, e = io.ReadFull(b, methods); e != nil {
		return
	}
	method := byte(255)
	for _, m := range methods {
		if p.internalToken == "" && m == 0 {
			method = 0
		}
		if p.internalToken != "" && m == 2 {
			method = 2
		}
	}
	if _, e = c.Write([]byte{5, method}); e != nil || method == 255 {
		return
	}
	if method == 2 {
		version, e := b.ReadByte()
		if e != nil || version != 1 {
			return
		}
		length, e := b.ReadByte()
		if e != nil {
			return
		}
		user := make([]byte, int(length))
		if _, e = io.ReadFull(b, user); e != nil {
			return
		}
		length, e = b.ReadByte()
		if e != nil {
			return
		}
		password := make([]byte, int(length))
		if _, e = io.ReadFull(b, password); e != nil {
			return
		}
		if string(user) != "clearline" || subtle.ConstantTimeCompare(password, []byte(p.internalToken)) != 1 {
			c.Write([]byte{1, 1})
			return
		}
		if _, e = c.Write([]byte{1, 0}); e != nil {
			return
		}
	}

	header := make([]byte, 4)
	if _, e = io.ReadFull(b, header); e != nil || header[0] != 5 || header[2] != 0 {
		return
	}
	if header[1] != 1 {
		c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	var host string
	switch header[3] {
	case 1:
		x := make([]byte, 4)
		if _, e = io.ReadFull(b, x); e != nil {
			return
		}
		host = net.IP(x).String()
	case 4:
		x := make([]byte, 16)
		if _, e = io.ReadFull(b, x); e != nil {
			return
		}
		host = net.IP(x).String()
	case 3:
		n, e := b.ReadByte()
		if e != nil || n == 0 {
			return
		}
		x := make([]byte, n)
		if _, e = io.ReadFull(b, x); e != nil {
			return
		}
		host = string(x)
	default:
		return
	}
	pb := make([]byte, 2)
	if _, e = io.ReadFull(b, pb); e != nil {
		return
	}
	port := binary.BigEndian.Uint16(pb)
	if port == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	up, e := p.dial(ctx, net.JoinHostPort(host, strconv.Itoa(int(port))))
	cancel()
	if e != nil {
		c.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	if _, e = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); e != nil {
		up.Close()
		return
	}
	c.SetDeadline(time.Time{})
	p.bridge(c, b, up)
}
