package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"
	hyserver "github.com/apernet/hysteria/core/v2/server"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/net/proxy"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func keys(t *testing.T) ([]byte, []byte) {
	t.Helper()
	sk := make([]byte, 32)
	if _, e := rand.Read(sk); e != nil {
		t.Fatal(e)
	}
	pk, e := curve25519.X25519(sk, curve25519.Basepoint)
	if e != nil {
		t.Fatal(e)
	}
	return sk, pk
}
func awgFixture(t *testing.T, obfuscated bool) (string, string) {
	t.Helper()
	ssk, spk := keys(t)
	csk, cpk := keys(t)
	tun, tnet, e := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.77.0.1")}, nil, 1380)
	if e != nil {
		t.Fatal(e)
	}
	dev := device.NewDevice(tun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	t.Cleanup(dev.Close)
	params := ""
	native := ""
	if obfuscated {
		params = "jc=4\njmin=10\njmax=50\ns1=149\ns2=54\ns3=38\ns4=5\nh1=361806169-448878574\nh2=1588936955-1781236186\nh3=2040932237-2091026621\nh4=2125376707-2130358387\n"
		native = "Jc = 4\nJmin = 10\nJmax = 50\nS1 = 149\nS2 = 54\nS3 = 38\nS4 = 5\nH1 = 361806169-448878574\nH2 = 1588936955-1781236186\nH3 = 2040932237-2091026621\nH4 = 2125376707-2130358387\n"
	}
	ipc := "private_key=" + hex.EncodeToString(ssk) + "\n" + params + "listen_port=0\npublic_key=" + hex.EncodeToString(cpk) + "\nallowed_ip=10.77.0.2/32\n"
	if e = dev.IpcSet(ipc); e != nil {
		t.Fatal(e)
	}
	if e = dev.Up(); e != nil {
		t.Fatal(e)
	}
	state, e := dev.IpcGet()
	if e != nil {
		t.Fatal(e)
	}
	port := ""
	for _, l := range strings.Split(state, "\n") {
		if strings.HasPrefix(l, "listen_port=") {
			port = strings.TrimPrefix(l, "listen_port=")
		}
	}
	if port == "" || port == "0" {
		t.Fatal("no UDP port")
	}
	listener, e := tnet.ListenTCPAddrPort(netip.MustParseAddrPort("10.77.0.1:8080"))
	if e != nil {
		t.Fatal(e)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test-Tunnel", "awg")
		io.WriteString(w, "encrypted-through-awg")
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	raw := fmt.Sprintf("[Interface]\nPrivateKey = %s\nAddress = 10.77.0.2/32\nDNS = 1.1.1.1\n%s[Peer]\nPublicKey = %s\nEndpoint = 127.0.0.1:%s\nAllowedIPs = 0.0.0.0/0\n", base64.StdEncoding.EncodeToString(csk), native, base64.StdEncoding.EncodeToString(spk), port)
	return raw, "http://10.77.0.1:8080/"
}

func requestThrough(t *testing.T, tunnel Tunnel, target string) {
	t.Helper()
	p, e := NewProxy(tunnel, RoutingPolicy{Mode: "all"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	hp, sp := p.ports()
	for _, socks := range []bool{false, true} {
		tr := &http.Transport{}
		if socks {
			dial, e := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", sp), nil, proxy.Direct)
			if e != nil {
				t.Fatal(e)
			}
			tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				return dial.(proxy.ContextDialer).DialContext(ctx, network, address)
			}
		} else {
			u, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", hp))
			tr.Proxy = http.ProxyURL(u)
		}
		client := &http.Client{Transport: tr, Timeout: 8 * time.Second}
		resp, e := client.Get(target)
		if e != nil {
			t.Fatalf("socks=%v: %v", socks, e)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		tr.CloseIdleConnections()
		if string(b) != "encrypted-through-awg" {
			t.Fatalf("unexpected content %q", b)
		}
	}
}
func TestRealWireGuardHTTPAndSOCKS(t *testing.T) {
	raw, target := awgFixture(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tunnel, e := NewAWG(ctx, raw)
	if e != nil {
		t.Fatal(e)
	}
	requestThrough(t, tunnel, target)
}
func TestRealAmneziaWGHTTPAndSOCKS(t *testing.T) {
	raw, target := awgFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tunnel, e := NewAWG(ctx, raw)
	if e != nil {
		t.Fatal(e)
	}
	requestThrough(t, tunnel, target)
}
func TestHTTPConnectPreservesPipelinedBytes(t *testing.T) {
	raw, _ := awgFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tunnel, e := NewAWG(ctx, raw)
	if e != nil {
		t.Fatal(e)
	}
	p, e := NewProxy(tunnel, RoutingPolicy{Mode: "all"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	hp, _ := p.ports()
	c, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", hp), time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(8 * time.Second))
	io.WriteString(c, "CONNECT 10.77.0.1:8080 HTTP/1.1\r\nHost: 10.77.0.1:8080\r\n\r\nGET / HTTP/1.1\r\nHost: 10.77.0.1\r\nConnection: close\r\n\r\n")
	b, e := io.ReadAll(c)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(b), "200 Connection Established") || !strings.Contains(string(b), "encrypted-through-awg") {
		t.Fatalf("CONNECT did not tunnel data: %q", b)
	}
}
func TestImportRejectsScriptsAndDuplicateKeys(t *testing.T) {
	raw, _ := awgFixture(t, false)
	for _, bad := range []string{strings.Replace(raw, "[Peer]", "PostUp = touch /tmp/bad\n[Peer]", 1), raw + "\nPublicKey = bad", strings.Replace(raw, "[Peer]", "[Peer]\n[Peer]", 1)} {
		if _, e := ParseAWG(bad); e == nil {
			t.Fatal("accepted unsafe config")
		}
	}
}
func TestVaultIntegrityAndWrongPassword(t *testing.T) {
	raw, _ := awgFixture(t, false)
	profiles := []Profile{{ID: "one", Name: "test", Kind: "awg", Config: raw}}
	data, e := seal("correct-passphrase", profiles)
	if e != nil {
		t.Fatal(e)
	}
	out, e := unseal("correct-passphrase", data)
	if e != nil || len(out) != 1 || out[0].Config != raw {
		t.Fatalf("roundtrip: %v", e)
	}
	if _, e = unseal("wrong-passphrase", data); e == nil {
		t.Fatal("wrong password accepted")
	}
	b, _ := base64.StdEncoding.DecodeString(data)
	b[len(b)-1] ^= 1
	if _, e = unseal("correct-passphrase", base64.StdEncoding.EncodeToString(b)); e == nil {
		t.Fatal("tampered file accepted")
	}
}
func TestHysteriaRequiresCertificateValidation(t *testing.T) {
	for _, s := range []string{"hy2://pass@example.com?insecure=1", "hy2://pass@example.com?obfs=unknown", "hy2://pass@example.com:70000"} {
		if _, e := ParseHY(s); e == nil {
			t.Fatalf("accepted %s", s)
		}
	}
	if c, e := ParseHY("hy2://a%3Ab@example.com:443?sni=example.com"); e != nil || c.Auth != "a:b" {
		t.Fatal(c, e)
	}
}

type testAuth struct{}

func (testAuth) Authenticate(addr net.Addr, auth string, tx uint64) (bool, string) {
	return auth == "correct", "test"
}
func testCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, e := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	keyDER, e := x509.MarshalECPrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, e := tls.X509KeyPair(certPEM, keyPEM)
	if e != nil {
		t.Fatal(e)
	}
	return cert, string(certPEM)
}
func TestRealHysteria2HTTPAndSOCKS(t *testing.T) {
	cert, ca := testCertificate(t)
	pc, e := net.ListenPacket("udp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	s, e := hyserver.NewServer(&hyserver.Config{TLSConfig: hyserver.TLSConfig{Certificates: []tls.Certificate{cert}}, Conn: pc, Authenticator: testAuth{}})
	if e != nil {
		t.Fatal(e)
	}
	go s.Serve()
	defer s.Close()
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "encrypted-through-awg") }))
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tunnel, e := newHYConfig(ctx, HYConfig{Server: pc.LocalAddr().String(), Auth: "correct", SNI: "localhost", CA: ca})
	if e != nil {
		t.Fatal(e)
	}
	requestThrough(t, tunnel, h.URL)
	badctx, badcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer badcancel()
	if bad, e := newHYConfig(badctx, HYConfig{Server: pc.LocalAddr().String(), Auth: "wrong", SNI: "localhost", CA: ca}); e == nil {
		bad.Close()
		t.Fatal("invalid credentials accepted")
	} else if retryableConnectionError(e) {
		t.Fatal("invalid credentials would retry forever", e)
	}
}
