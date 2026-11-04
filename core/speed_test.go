package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSiteProbeReportsVerificationResponseHonestly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("probe must be anonymous HEAD")
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
	result := measureSite(context.Background(), tunnel.DialContext, "http://claude.ai/", "tunnel")
	if !result.OK || !result.Limited || result.Status != 403 || result.Message == "" || tunnel.calls.Load() != 1 {
		t.Fatal(result)
	}
}
func TestSpeedTestFollowsSelectedRoutingPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("unexpected account data")
		}
		if r.Method == "GET" {
			io.WriteString(w, strings.Repeat("x", 1024))
		}
	}))
	defer server.Close()
	for _, mode := range []string{"claude", "all"} {
		t.Run(mode, func(t *testing.T) {
			tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
			policy, _ := NewRoutingPolicy(mode, nil, nil)
			p, err := NewProxy(tunnel, policy, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			var stages []string
			report, err := runSpeedTest(context.Background(), p, speedTargets{"http://claude.ai/", server.URL + "/latency", server.URL + "/download", 1024}, func(stage string, _ SpeedReport, _ int64) { stages = append(stages, stage) })
			if err != nil || report.Claude == nil || report.Ordinary == nil || report.Download == nil || !report.Download.OK || report.Download.Bytes != 1024 {
				t.Fatal(report, err)
			}
			expected := "direct"
			calls := int32(1)
			if mode == "all" {
				expected = "tunnel"
				calls = 6
			}
			if report.Claude.Route != "tunnel" || report.Ordinary.Route != expected || report.Download.Route != expected || tunnel.calls.Load() != calls {
				t.Fatal("wrong diagnostic route", report, tunnel.calls.Load())
			}
			if len(stages) < 3 || stages[0] != "claude" || stages[1] != "ordinary" || stages[2] != "download" {
				t.Fatal(stages)
			}
		})
	}
}
func TestDownloadRejectsShortAndOversizedSamples(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, "short") }))
	defer server.Close()
	tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
	result := measureDownload(context.Background(), tunnel.DialContext, server.URL, "tunnel", 1024, nil)
	if result.OK || result.Mbps != 0 || result.Bytes != 5 {
		t.Fatal(result)
	}
	result = measureDownload(context.Background(), tunnel.DialContext, server.URL, "tunnel", speedSampleBytes+1, nil)
	if result.OK || calls.Load() != 1 {
		t.Fatal("unbounded sample accepted", result, calls.Load())
	}
}
func TestSpeedProbeNeverFallsBackAfterTunnelFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, strings.Repeat("x", 1024))
	}))
	defer server.Close()
	tunnel := &routeTestTunnel{fail: true}
	site := measureSite(context.Background(), tunnel.DialContext, server.URL, "tunnel")
	download := measureDownload(context.Background(), tunnel.DialContext, server.URL, "tunnel", 1024, nil)
	if site.OK || download.OK || calls.Load() != 0 {
		t.Fatal("diagnostic fell back to direct")
	}
}
func TestDownloadCancellationInterruptsBodyRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "x")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := measureDownload(ctx, tunnel.DialContext, server.URL, "tunnel", 1024, nil)
	if result.OK || result.Bytes >= 1024 || ctx.Err() == nil {
		t.Fatal(result, ctx.Err())
	}
}
func TestSpeedControlAndCancellationSurviveBufferedInput(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("{\"action\":\"speedTest\",\"id\":\"test\"}\n{\"action\":\"cancelSpeedTest\",\"id\":\"test\"}\n"))
	commands := make(chan ControlRequest, 4)
	eof := make(chan struct{})
	done := make(chan struct{})
	readControls(reader, commands, eof, done)
	if (<-commands).Action != "speedTest" || (<-commands).Action != "cancelSpeedTest" {
		t.Fatal("control command lost")
	}
}

func TestDisconnectedSpeedNeverContactsClaude(t *testing.T) {
	var claudeCalls atomic.Int32
	claude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { claudeCalls.Add(1); w.WriteHeader(200) }))
	defer claude.Close()
	ordinary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, strings.Repeat("x", 1024))
		}
	}))
	defer ordinary.Close()
	p := &Proxy{policy: RoutingPolicy{Mode: "claude"}}
	report, err := runNetworkTest(context.Background(), p, speedTargets{claude.URL, ordinary.URL, ordinary.URL, 1024}, true, func(string, SpeedReport, int64) {})
	if err != nil || claudeCalls.Load() != 0 || !report.Claude.Skipped || report.Claude.OK || !report.Ordinary.OK || report.Ordinary.Route != "direct" || !report.Download.OK {
		t.Fatal(report, err, claudeCalls.Load())
	}
}
