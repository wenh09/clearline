package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

// A private child-process fixture exercises the actual stdio supervisor and
// authenticated SOCKS hop without contacting an external website or Keychain.
func TestTransportProcessHelper(t *testing.T) {
	if os.Getenv("CLEARLINE_WORKER_FIXTURE") != "1" {
		return
	}
	input := bufio.NewReader(os.Stdin)
	r, err := readRequest(input)
	if err != nil {
		os.Exit(2)
	}
	if os.Getenv("CLEARLINE_WORKER_NATIVE") == "1" {
		_ = runTransport(r, input)
		os.Exit(0)
	}
	if os.Getenv("CLEARLINE_WORKER_FAIL") == "1" {
		emit(map[string]any{"event": "error", "retryable": true, "message": "fixture temporary failure"})
		os.Exit(1)
	}
	if os.Getenv("CLEARLINE_WORKER_LATE_READY") == "1" {
		signal.Ignore(syscall.SIGTERM)
		emit(map[string]any{"event": "progress", "stage": "server"})
		time.Sleep(100 * time.Millisecond)
	}
	if r.Profile.Name == "hold-init" {
		emit(map[string]any{"event": "progress", "stage": "server", "fixturePID": os.Getpid()})
		io.Copy(io.Discard, input)
		os.Exit(0)
	}
	tunnel := &routeTestTunnel{target: r.Profile.Name}
	p, err := NewProxy(tunnel, RoutingPolicy{Mode: "all"}, nil, r.InternalToken)
	if err != nil {
		os.Exit(3)
	}
	h, s := p.ports()
	emit(map[string]any{"event": "ready", "httpPort": h, "socksPort": s, "latencyMs": 1, "profileID": r.Profile.ID})
	for {
		line, err := readJSONLine(input)
		if err != nil {
			break
		}
		var command ControlRequest
		if json.Unmarshal(line, &command) == nil && command.Action == "check" {
			failed := os.Getenv("CLEARLINE_WORKER_CHECK_FAIL") == "1"
			emit(map[string]any{"event": "health", "ok": !failed, "latencyMs": 1, "checkID": command.ID})
			if failed {
				p.Close()
				os.Exit(1)
			}
		}
	}
	p.Close()
	os.Exit(0)
}

func fixtureCommand(extra ...string) func() (*exec.Cmd, error) {
	return func() (*exec.Cmd, error) {
		c := exec.Command(os.Args[0], "-test.run=^TestTransportProcessHelper$")
		c.Env = append(os.Environ(), "CLEARLINE_WORKER_FIXTURE=1", "GORACE=atexit_sleep_ms=0")
		c.Env = append(c.Env, extra...)
		return c, nil
	}
}

type sessionFixture struct {
	input  *io.PipeWriter
	events chan map[string]any
	done   chan error
	cancel context.CancelFunc
}

func startSessionFixture(t *testing.T, target string, factory func() (*exec.Cmd, error)) *sessionFixture {
	t.Helper()
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	f := &sessionFixture{input: writer, events: make(chan map[string]any, 100), done: make(chan error, 1), cancel: cancel}
	go func() {
		f.done <- runSession(ctx, Request{Profile: Profile{ID: "fixture", Name: target, Kind: "hysteria2", Config: "hy2://fixture@127.0.0.1:443"}, RoutingMode: "claude", ProxyDomains: []string{"tunnel.example"}}, bufio.NewReader(reader), func(v any) {
			if event, ok := v.(map[string]any); ok {
				select {
				case f.events <- event:
				case <-ctx.Done():
				}
			}
		}, sessionOptions{command: factory, retryDelay: func(int) time.Duration { return 5 * time.Millisecond }})
	}()
	t.Cleanup(func() {
		writer.Close()
		cancel()
		select {
		case <-f.done:
		case <-time.After(5 * time.Second):
			t.Error("session did not stop")
		}
		reader.Close()
	})
	return f
}
func (f *sessionFixture) event(t *testing.T, kind string) map[string]any {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case value := <-f.events:
			if value["event"] == kind {
				return value
			}
		case <-deadline.C:
			t.Fatalf("missing %s event", kind)
			return nil
		}
	}
}
func (f *sessionFixture) send(t *testing.T, c ControlRequest) {
	t.Helper()
	if err := json.NewEncoder(f.input).Encode(c); err != nil {
		t.Fatal(err)
	}
}
func clientAt(port int) *http.Client {
	u, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(port))
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}, Timeout: time.Second}
}
func getFixture(t *testing.T, client *http.Client, target string) {
	t.Helper()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "through-fixture" {
		t.Fatal("unexpected body", string(body))
	}
}

func TestRetainedPortsSurviveTransportProcessCrash(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "through-fixture") }))
	defer server.Close()
	f := startSessionFixture(t, strings.TrimPrefix(server.URL, "http://"), fixtureCommand())
	ready := f.event(t, "ready")
	client := clientAt(eventNumber(ready["httpPort"]))
	defer client.CloseIdleConnections()
	getFixture(t, client, "http://tunnel.example/")
	pid := eventNumber(ready["transportPID"])
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	recovered := f.event(t, "recovered")
	if recovered["httpPort"] != ready["httpPort"] || recovered["socksPort"] != ready["socksPort"] || eventNumber(recovered["transportPID"]) == pid {
		t.Fatal("frontend ports changed or transport was not replaced")
	}
	getFixture(t, client, "http://tunnel.example/")
}
func TestOfflineKeepsDirectTrafficAndHoldsTunnelTraffic(t *testing.T) {
	var targetCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); io.WriteString(w, "through-fixture") }))
	defer server.Close()
	f := startSessionFixture(t, strings.TrimPrefix(server.URL, "http://"), fixtureCommand())
	ready := f.event(t, "ready")
	available := false
	f.send(t, ControlRequest{Action: "network", ID: "offline", Available: &available})
	for {
		event := f.event(t, "transportState")
		if event["state"] == "offline" {
			break
		}
	}
	client := clientAt(eventNumber(ready["httpPort"]))
	defer client.CloseIdleConnections()
	getFixture(t, client, server.URL)
	client.Timeout = 80 * time.Millisecond
	if response, err := client.Get("http://tunnel.example/"); err == nil {
		response.Body.Close()
		t.Fatal("tunnel request succeeded while offline")
	}
	if targetCalls.Load() != 1 {
		t.Fatal("tunnel traffic bypassed recovery")
	}
	available = true
	f.send(t, ControlRequest{Action: "network", ID: "online", Available: &available})
	recovered := f.event(t, "recovered")
	if recovered["httpPort"] != ready["httpPort"] {
		t.Fatal("port changed after network recovery")
	}
	client.Timeout = time.Second
	getFixture(t, client, "http://tunnel.example/")
}
func TestWakeCheckFailureRebuildsOnlyTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "through-fixture") }))
	defer server.Close()
	f := startSessionFixture(t, strings.TrimPrefix(server.URL, "http://"), fixtureCommand("CLEARLINE_WORKER_CHECK_FAIL=1"))
	ready := f.event(t, "ready")
	f.send(t, ControlRequest{Action: "check", ID: "wake", Reason: "wake"})
	health := f.event(t, "health")
	if health["ok"] != false {
		t.Fatal("failed wake check was hidden")
	}
	recovered := f.event(t, "recovered")
	if recovered["httpPort"] != ready["httpPort"] {
		t.Fatal("wake recovery recreated local proxy")
	}
}
func TestSessionRetriesBeyondFiveAndEOFStopsFurtherAttempts(t *testing.T) {
	var attempts atomic.Int32
	factory := func() (*exec.Cmd, error) {
		if attempts.Add(1) <= 7 {
			return fixtureCommand("CLEARLINE_WORKER_FAIL=1")()
		}
		return fixtureCommand()()
	}
	f := startSessionFixture(t, "127.0.0.1:1", factory)
	f.event(t, "ready")
	if attempts.Load() != 8 {
		t.Fatal("retries did not continue", attempts.Load())
	}
	// Cleanup closes stdin; no further attempt can be launched afterwards.
}
func TestWorkerEOFInterruptsNativeHandshake(t *testing.T) {
	blackhole, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan workerEvent, 20)
	w, err := startTransportWorker(ctx, Request{Profile: Profile{ID: "native", Kind: "hysteria2", Config: "hy2://fixture@" + blackhole.LocalAddr().String()}}, fixtureCommand("CLEARLINE_WORKER_NATIVE=1"), events)
	if err != nil {
		t.Fatal(err)
	}
	defer w.shutdown()
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	w.input.Close() // No signal: losing the parent pipe alone must stop startup.
	select {
	case <-w.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("orphan transport kept connecting after EOF")
	}
}
func TestInternalTransportProxyRequiresPrivateCredential(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("internal credential leaked to destination")
		}
		io.WriteString(w, "through-fixture")
	}))
	defer server.Close()
	token := strings.Repeat("a", 64)
	p, err := NewProxy(&routeTestTunnel{target: strings.TrimPrefix(server.URL, "http://")}, RoutingPolicy{Mode: "all"}, nil, token)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	h, s := p.ports()
	client := clientAt(h)
	defer client.CloseIdleConnections()
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 407 {
		t.Fatal("unauthenticated internal HTTP accepted")
	}
	bad, _ := proxy.SOCKS5("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(s)), nil, proxy.Direct)
	if c, err := bad.Dial("tcp", "tunnel.example:80"); err == nil {
		c.Close()
		t.Fatal("unauthenticated internal SOCKS accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("unauthenticated internal request reached target")
	}
	good, _ := proxy.SOCKS5("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(s)), &proxy.Auth{User: "clearline", Password: token}, proxy.Direct)
	tr := &http.Transport{DialContext: good.(proxy.ContextDialer).DialContext}
	defer tr.CloseIdleConnections()
	getFixture(t, &http.Client{Transport: tr, Timeout: time.Second}, "http://tunnel.example/")
}
func TestRetainedCloseUnblocksWaitingRequests(t *testing.T) {
	tunnel := newRetainedTunnel()
	done := make(chan error, 1)
	go func() { _, err := tunnel.DialContext(context.Background(), "tcp", "tunnel.example:443"); done <- err }()
	tunnel.Close()
	tunnel.Close()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting request leaked after closing proxy")
	}
}

func TestLateWorkerReadyCannotUndoOfflineState(t *testing.T) {
	f := startSessionFixture(t, "127.0.0.1:1", fixtureCommand("CLEARLINE_WORKER_LATE_READY=1"))
	f.event(t, "progress")
	available := false
	f.send(t, ControlRequest{Action: "network", ID: "offline", Available: &available})
	for {
		event := f.event(t, "transportState")
		if event["state"] == "offline" {
			break
		}
	}
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case event := <-f.events:
			if event["event"] == "ready" || event["event"] == "recovered" {
				t.Fatal("late worker revived an offline tunnel")
			}
		case <-timer.C:
			return
		}
	}
}
