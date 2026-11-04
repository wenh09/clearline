package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	hyserver "github.com/apernet/hysteria/core/v2/server"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

// A bounded loopback link adds the same one-way delay in both directions.
// No production server or credentials are used in this experiment.
func delayedUDPLink(t *testing.T, target *net.UDPAddr, delay time.Duration) *net.UDPAddr {
	t.Helper()
	front, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	back, err := net.DialUDP("udp4", nil, target)
	if err != nil {
		front.Close()
		t.Fatal(err)
	}
	var mu sync.Mutex
	var client *net.UDPAddr
	var readers, pending sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		buf := make([]byte, 65536)
		for {
			n, addr, e := front.ReadFromUDP(buf)
			if e != nil {
				return
			}
			mu.Lock()
			client = addr
			mu.Unlock()
			data := append([]byte(nil), buf[:n]...)
			pending.Add(1)
			time.AfterFunc(delay, func() { defer pending.Done(); back.Write(data) })
		}
	}()
	go func() {
		defer readers.Done()
		buf := make([]byte, 65536)
		for {
			n, e := back.Read(buf)
			if e != nil {
				return
			}
			data := append([]byte(nil), buf[:n]...)
			mu.Lock()
			addr := client
			mu.Unlock()
			if addr == nil {
				continue
			}
			pending.Add(1)
			time.AfterFunc(delay, func() { defer pending.Done(); front.WriteToUDP(data, addr) })
		}
	}()
	t.Cleanup(func() { front.Close(); back.Close(); readers.Wait(); pending.Wait() })
	return front.LocalAddr().(*net.UDPAddr)
}
func TestFastOpenWithDelayedLink(t *testing.T) {
	cert, ca := testCertificate(t)
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := hyserver.NewServer(&hyserver.Config{TLSConfig: hyserver.TLSConfig{Certificates: []tls.Certificate{cert}}, Conn: pc, Authenticator: testAuth{}})
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve()
	defer server.Close()
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "verified-latency-fixture") }))
	defer target.Close()
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	report := map[string]any{"method": "real Hysteria 2 and HTTPS over a 300 ms RTT loopback link; three fresh TLS requests per variant", "oneWayDelayMs": 150, "baselineMs": []int64{}, "fastMs": []int64{}}
	for _, fast := range []bool{false, true} {
		link := delayedUDPLink(t, pc.LocalAddr().(*net.UDPAddr), 150*time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		tunnel, e := newHYConfigWithFastOpen(ctx, HYConfig{Server: link.String(), Auth: "correct", SNI: "localhost", CA: ca}, fast)
		cancel()
		if e != nil {
			t.Fatal(e)
		}
		values := []int64{}
		for i := 0; i < 3; i++ {
			tr := &http.Transport{DialContext: tunnel.DialContext, TLSClientConfig: &tls.Config{RootCAs: roots}, DisableKeepAlives: true}
			client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
			start := time.Now()
			resp, e := client.Get(target.URL)
			if e != nil {
				tunnel.Close()
				t.Fatal(e)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			tr.CloseIdleConnections()
			if string(body) != "verified-latency-fixture" {
				t.Fatal("incorrect response")
			}
			values = append(values, time.Since(start).Milliseconds())
		}
		tunnel.Close()
		if fast {
			report["fastMs"] = values
		} else {
			report["baselineMs"] = values
		}
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	t.Log(string(data))
	if path := os.Getenv("CLEARLINE_LATENCY_REPORT"); path != "" {
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
