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

func TestExitTraceValidation(t *testing.T) {
	for _, test := range []struct {
		body  string
		valid bool
	}{
		{"ip=203.0.113.10\nloc=US\ncolo=SJC\n", true},
		{"ip=2606:4700:4700::1111\nloc=US\n", true},
		{"ip=192.168.1.1\nloc=US\n", false},
		{"ip=127.0.0.1\nloc=US\n", false},
		{"ip=::1\nloc=US\n", false},
		{"ip=::ffff:10.0.0.1\nloc=US\n", false},
		{"ip=garbage\nloc=US\n", false},
		{"loc=US\n", false},
		{"ip=203.0.113.10\n", false},
		{"ip=203.0.113.10\nloc=usa\n", false},
		{"ip=203.0.113.10\nip=8.8.8.8\nloc=US\n", false},
		{"ip=203.0.113.10\nloc=US\nloc=CN\n", false},
		{strings.Repeat("x", 8193), false},
	} {
		_, err := parseExitTrace(test.body)
		if (err == nil) != test.valid {
			t.Errorf("valid=%v, err=%v, body=%q", test.valid, err, test.body[:min(len(test.body), 120)])
		}
	}
}
func TestExitVerificationUsesCurrentTunnel(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Host != "exit-check.example" || r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("unexpected diagnostic request")
		}
		io.WriteString(w, "ip=203.0.113.10\nloc=US\n")
	}))
	defer server.Close()
	tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
	info, err := verifyExitAt(context.Background(), tunnel, "http://exit-check.example/trace")
	if err != nil || info.IP != "203.0.113.10" || info.Country != "US" || info.VerifiedAt == "" || tunnel.calls.Load() != 1 || requests.Load() != 1 {
		t.Fatal(info, err, tunnel.calls.Load(), requests.Load())
	}
	if _, err := time.Parse(time.RFC3339, info.VerifiedAt); err != nil {
		t.Fatal(err)
	}
}
func TestExitVerificationDoesNotFallBack(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.WriteString(w, "ip=8.8.8.8\nloc=US\n")
	}))
	defer server.Close()
	tunnel := &routeTestTunnel{fail: true}
	if _, err := verifyExitAt(context.Background(), tunnel, server.URL); err == nil {
		t.Fatal("failed tunnel appeared verified")
	}
	if requests.Load() != 0 || tunnel.calls.Load() != 1 {
		t.Fatal("verification bypassed failed tunnel")
	}
}
func TestExitVerificationRejectsServiceFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		body string
	}{{"blocked", 403, "blocked"}, {"redirect", 302, ""}, {"invalid", 200, "ip=local\nloc=US\n"}, {"oversized", 200, strings.Repeat("x", 8193)}} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "http://never-follow.example/")
				w.WriteHeader(test.code)
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			tunnel := &routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}
			if _, err := verifyExitAt(context.Background(), tunnel, server.URL); err == nil {
				t.Fatal("service failure appeared verified")
			}
			if tunnel.calls.Load() != 1 {
				t.Fatal("followed diagnostic redirect")
			}
		})
	}
}
func TestControlInputPreservesBufferedCommands(t *testing.T) {
	input := bufio.NewReader(strings.NewReader("{\"routingMode\":\"all\"}\n{\"action\":\"verifyExit\",\"id\":\"first\"}\n{\"action\":\"unknown\",\"id\":\"skip\"}\n{\"action\":\"verifyExit\",\"id\":\"second\"}\n"))
	request, err := readRequest(input)
	if err != nil || request.RoutingMode != "all" {
		t.Fatal(request, err)
	}
	commands := make(chan ControlRequest, 4)
	eof := make(chan struct{})
	done := make(chan struct{})
	readControls(input, commands, eof, done)
	for _, id := range []string{"first", "second"} {
		select {
		case command := <-commands:
			if command.ID != id {
				t.Fatal(command)
			}
		default:
			t.Fatal("buffered control lost")
		}
	}
	select {
	case <-eof:
	default:
		t.Fatal("EOF not observed")
	}
	select {
	case command := <-commands:
		t.Fatal("unknown command accepted", command)
	default:
	}
}
func TestControlInputLineLimit(t *testing.T) {
	input := bufio.NewReader(strings.NewReader(strings.Repeat("x", 2*maxInput+1)))
	if _, err := readJSONLine(input); err == nil {
		t.Fatal("unbounded input accepted")
	}
}
