package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthSurvivesSingleServiceFailureThroughTunnel(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/primary" {
			w.WriteHeader(503)
			return
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("private headers sent")
		}
		io.WriteString(w, "ip=203.0.113.10\nloc=US\n")
	}))
	defer server.Close()
	tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
	_, err := probeTargets(context.Background(), tunnel, []healthTarget{{url: "http://health.example/primary"}, {url: "http://health.example/backup", trace: true}}, time.Second)
	if err != nil || calls.Load() != 2 || tunnel.calls.Load() != 2 {
		t.Fatal("healthy tunnel was rejected", err, calls.Load())
	}
}

func TestHealthDoesNotBypassFailedTunnel(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer server.Close()
	tunnel := &routeTestTunnel{fail: true}
	_, err := probeTargets(context.Background(), tunnel, []healthTarget{{url: server.URL}, {url: server.URL}}, time.Second)
	if err == nil || calls.Load() != 0 || tunnel.calls.Load() != 2 {
		t.Fatal("failed tunnel fell back to local path", err)
	}
}

func TestHealthRejectsCaptivePortalAndInvalidTrace(t *testing.T) {
	for _, test := range []struct {
		status int
		body   string
		trace  bool
	}{{302, "", false}, {200, "login", false}, {200, "ip=127.0.0.1\nloc=US\n", true}, {200, "verification page", true}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "http://never-follow.example/")
			w.WriteHeader(test.status)
			io.WriteString(w, test.body)
		}))
		tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
		_, err := probeTargets(context.Background(), tunnel, []healthTarget{{url: server.URL, trace: test.trace}}, time.Second)
		server.Close()
		if err == nil || tunnel.calls.Load() != 1 {
			t.Fatal("invalid response appeared healthy", test.status, test.trace, err)
		}
	}
}

func TestHealthTimeoutFallsBackWithinOverallDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stall" {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := probeTargets(ctx, tunnel, []healthTarget{{url: server.URL + "/stall"}, {url: server.URL + "/backup"}}, 25*time.Millisecond)
	if err != nil || tunnel.calls.Load() != 2 {
		t.Fatal("did not recover from hung primary", err)
	}
}
