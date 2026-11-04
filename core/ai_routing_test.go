package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAIDefaultsCoverServicesAndNarrowDependencies(t *testing.T) {
	for _, mode := range []string{"", "ai"} {
		p, err := NewRoutingPolicy(mode, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, host := range []string{"claude.ai", "api.anthropic.com", "claude.com", "bridge.claudeusercontent.com", "widget.claudemcpcontent.com", "chatgpt.com", "ws.chatgpt.com", "auth.openai.com", "api.openai.com", "desktop.chat.openai.com", "cdn.oaistatic.com", "files.oaiusercontent.com", "ab.oaistatsig.com", "cdn.openaimerge.com", "challenges.cloudflare.com", "cdn.workos.com", "forwarder.workos.com", "frontend-apps-multi-region.workos.com", "images.workoscdn.com", "setup.workos.com", "workos.imgix.net", "API.OPENAI.COM."} {
			if p.Route(host) != "tunnel" {
				t.Errorf("AI domain must tunnel: %s", host)
			}
		}
		for _, host := range []string{"www.baidu.com", "www.apple.com.cn", "github.com", "www.cloudflare.com", "cloudflare.com", "workos.com", "www.workos.com", "auth0.com", "js.stripe.com", "intercom.io", "chatgpt.com.evil.test", "notchatgpt.com", "api.openai.com.evil.test", "oaistatic.com.evil.test", "evilchallenges.cloudflare.com", "challenges.cloudflare.com.evil.test", "127.0.0.1", "192.168.1.1", "::1", "8.8.8.8"} {
			if p.Route(host) != "direct" {
				t.Errorf("ordinary or lookalike domain must stay direct: %s", host)
			}
		}
	}
	legacy, _ := NewRoutingPolicy("claude", nil, nil)
	if legacy.Route("chatgpt.com") != "direct" || legacy.Route("claude.ai") != "tunnel" {
		t.Fatal("legacy core mode changed")
	}
}

func TestAIProxyKeepsOrdinaryDirectAndFailsClosedForAI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "through-fixture") }))
	defer server.Close()
	policy, _ := NewRoutingPolicy("ai", []string{"ai.test"}, nil)
	tunnel := &routeTestTunnel{fail: true}
	p, err := NewProxy(tunnel, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	port, _ := p.ports()
	client := clientAt(port)
	defer client.CloseIdleConnections()
	getFixture(t, client, server.URL)
	response, err := client.Get("http://api.ai.test/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 502 || tunnel.calls.Load() != 1 {
		t.Fatal("AI failure escaped routing policy", response.StatusCode, tunnel.calls.Load())
	}
}

func TestAIHTTPSWebSocketHandshakeAndFramesRemainIntact(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" || r.Header.Get("Connection") != "Upgrade" || r.Header.Get("Sec-WebSocket-Key") != "dGhlIHNhbXBsZSBub25jZQ==" {
			t.Error("WebSocket handshake was rewritten")
			w.WriteHeader(400)
			return
		}
		c, b, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		io.WriteString(b, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
		b.Flush()
		frame := make([]byte, 10)
		if _, err = io.ReadFull(b, frame); err != nil {
			t.Error(err)
			return
		}
		if frame[0] != 0x81 || frame[1] != 0x84 {
			t.Error("masked client frame changed")
			return
		}
		payload := make([]byte, 4)
		for i := range payload {
			payload[i] = frame[6+i] ^ frame[2+i%4]
		}
		if string(payload) != "ping" {
			t.Error("stream data changed")
			return
		}
		c.Write(append([]byte{0x81, 4}, payload...))
	}))
	defer server.Close()
	tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "https://")}
	policy, _ := NewRoutingPolicy("ai", []string{"ai.test"}, nil)
	p, err := NewProxy(tunnel, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	port, _ := p.ports()
	raw, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(raw, "CONNECT ws.ai.test:443 HTTP/1.1\r\nHost: ws.ai.test:443\r\n\r\n")
	connected, err := http.ReadResponse(bufio.NewReader(raw), nil)
	if err != nil || connected.StatusCode != 200 {
		t.Fatal("CONNECT failed", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	secured := tls.Client(raw, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1"})
	defer secured.Close()
	if err = secured.Handshake(); err != nil {
		t.Fatal(err)
	}
	io.WriteString(secured, "GET / HTTP/1.1\r\nHost: ws.ai.test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	reader := bufio.NewReader(secured)
	response, err := http.ReadResponse(reader, nil)
	if err != nil || response.StatusCode != 101 || response.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatal("upgrade rejected", err)
	}
	secured.Write([]byte{0x81, 0x84, 1, 2, 3, 4, 'p' ^ 1, 'i' ^ 2, 'n' ^ 3, 'g' ^ 4})
	echo := make([]byte, 6)
	if _, err = io.ReadFull(reader, echo); err != nil || !bytes.Equal(echo, []byte{0x81, 4, 'p', 'i', 'n', 'g'}) || tunnel.calls.Load() != 1 {
		t.Fatal("WebSocket stream failed", echo, err)
	}
}
