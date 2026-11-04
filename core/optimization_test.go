package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestHedgedHealthDoesNotWaitForHungPrimaryAndCancelsIt(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/primary" {
			<-r.Context().Done()
			cancelled <- struct{}{}
			return
		}
		io.WriteString(w, "ip=203.0.113.10\nloc=US\n")
	}))
	defer server.Close()
	tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
	start := time.Now()
	_, err := probeTargetsWithHedge(context.Background(), tunnel, []healthTarget{{url: server.URL + "/primary"}, {url: server.URL + "/backup", trace: true}}, time.Second, 20*time.Millisecond)
	if err != nil || time.Since(start) >= 700*time.Millisecond {
		t.Fatal("healthy backup waited for primary timeout", err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("losing health request was not cancelled")
	}
}

func TestHedgedHealthFastPrimaryAvoidsExtraTraffic(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer server.Close()
	tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
	_, err := probeTargetsWithHedge(context.Background(), tunnel, []healthTarget{{url: server.URL}, {url: server.URL}}, time.Second, 500*time.Millisecond)
	if err != nil || calls.Load() != 1 {
		t.Fatal("fast primary unnecessarily contacted backup", err, calls.Load())
	}
}

func TestOrdinaryProbeSeparatesColdSamplesFromActualReuse(t *testing.T) {
	var connections, requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "HEAD" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("non-anonymous measurement")
		}
		w.WriteHeader(204)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
	result := measureOrdinary(context.Background(), tunnel.DialContext, server.URL, "tunnel")
	if !result.OK || result.Samples != 3 || result.WarmSamples != 2 || connections.Load() != 4 || requests.Load() != 6 {
		t.Fatal(result, connections.Load(), requests.Load())
	}
}

func TestDownloadWholeRateIncludesResponseWait(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		io.WriteString(w, strings.Repeat("x", 1024))
	}))
	defer server.Close()
	tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
	result := measureDownload(context.Background(), tunnel.DialContext, server.URL, "tunnel", 1024, nil)
	if !result.OK || result.DurationMs < 75 || result.Mbps >= result.BodyMbps || result.DurationMs <= result.BodyDurationMs {
		t.Fatal(result)
	}
}

func TestHTTPProxyReusesUpstreamAcrossSeparateClients(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credential forwarded")
		}
		io.WriteString(w, "through-fixture")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
	p, err := NewProxy(tunnel, RoutingPolicy{Mode: "all"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	port, _ := p.ports()
	for i := 0; i < 2; i++ {
		client := clientAt(port)
		getFixture(t, client, "http://fixture.example/")
		client.CloseIdleConnections()
	}
	if connections.Load() != 1 {
		t.Fatal("upstream connection was rebuilt", connections.Load())
	}
}

func TestFirstWorkerCrashRetriesImmediatelyButCrashLoopBacksOff(t *testing.T) {
	f := startSessionFixture(t, "127.0.0.1:1", fixtureCommand())
	ready := f.event(t, "ready")
	for i := 0; i < 2; i++ {
		if err := syscall.Kill(eventNumber(ready["transportPID"]), syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		state := f.event(t, "transportState")
		for state["state"] != "recovering" {
			state = f.event(t, "transportState")
		}
		delay := eventNumber(state["retryAfterMs"])
		if i == 0 && delay != 0 {
			t.Fatal("first crash waited", delay)
		}
		if i == 1 && delay == 0 {
			t.Fatal("repeated crash bypassed backoff")
		}
		ready = f.event(t, "recovered")
	}
}

func TestHealthHedgeControlledLatencyReport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stall" {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
	targets := []healthTarget{{url: server.URL + "/stall"}, {url: server.URL + "/healthy"}}
	start := time.Now()
	for _, target := range targets {
		if _, err := probeTarget(context.Background(), tunnel, target, 4*time.Second); err == nil {
			break
		}
	}
	baseline := time.Since(start).Milliseconds()
	improved, err := probeTargets(context.Background(), tunnel, targets, 4*time.Second)
	if err != nil || improved >= baseline {
		t.Fatal("hedge did not avoid primary wait", baseline, improved, err)
	}
	if path := os.Getenv("CLEARLINE_HEALTH_REPORT"); path != "" {
		data, _ := json.Marshal(map[string]any{"method": "local simulated hung primary; healthy backup; production 4 s timeout and 600 ms hedge", "baselineMs": baseline, "candidateMs": improved})
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
