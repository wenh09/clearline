package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"
)

type Request struct {
	ID            string    `json:"id"`
	Profile       Profile   `json:"profile"`
	Alternatives  []Profile `json:"alternatives"`
	PreferredID   string    `json:"preferredID"`
	Profiles      []Profile `json:"profiles"`
	RoutingMode   string    `json:"routingMode"`
	ProxyDomains  []string  `json:"proxyDomains"`
	Hosts         []string  `json:"hosts"`
	AuditRoutes   bool      `json:"auditRoutes"`
	Bypass        []string  `json:"bypass"`
	Password      string    `json:"password"`
	Data          string    `json:"data"`
	InternalToken string    `json:"internalToken,omitempty"`
}

var outMu sync.Mutex

func emit(v any) { outMu.Lock(); defer outMu.Unlock(); json.NewEncoder(os.Stdout).Encode(v) }
func fail(e error) {
	emit(map[string]any{"event": "error", "message": e.Error(), "retryable": retryableConnectionError(e)})
}

// Preserve buffered input so a control command sent immediately after startup
// cannot disappear into a discarded reader. Bound every individual JSON line.
func readJSONLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > 2*maxInput {
			return nil, errors.New("输入过大")
		}
		line = append(line, fragment...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF && len(line) > 0 {
			return line, nil
		}
		return line, err
	}
}
func readRequest(reader *bufio.Reader) (Request, error) {
	var r Request
	line, e := readJSONLine(reader)
	if e != nil {
		return r, e
	}
	if json.Unmarshal(line, &r) != nil {
		return r, errors.New("请求格式无效")
	}
	return r, nil
}

type ControlRequest struct {
	Action     string `json:"action"`
	ID         string `json:"id"`
	Available  *bool  `json:"available,omitempty"`
	Reason     string `json:"reason,omitempty"`
	SkipClaude bool   `json:"skipClaude,omitempty"`
}

func readControls(reader *bufio.Reader, commands chan<- ControlRequest, eof chan<- struct{}, done <-chan struct{}) {
	defer close(eof)
	for {
		line, err := readJSONLine(reader)
		if err != nil {
			return
		}
		var command ControlRequest
		if json.Unmarshal(line, &command) != nil || (command.Action != "verifyExit" && command.Action != "speedTest" && command.Action != "cancelSpeedTest" && command.Action != "check" && command.Action != "network" && command.Action != "reconnect") || command.ID == "" || len(command.ID) > 64 {
			continue
		}
		select {
		case commands <- command:
		case <-done:
			return
		}
	}
}

func run(r Request, input *bufio.Reader) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runSession(ctx, r, input, emit, defaultSessionOptions())
}

func runTransport(r Request, input *bufio.Reader) error {
	policy, e := NewRoutingPolicy(r.RoutingMode, r.ProxyDomains, r.Bypass)
	if e != nil {
		return e
	}
	profiles, e := connectionCandidates(r.Profile, r.Alternatives, r.PreferredID)
	if e != nil {
		return e
	}
	parent, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	lifeCtx, stopChecks := context.WithCancel(parent)
	defer stopChecks()
	eof := make(chan struct{})
	commands := make(chan ControlRequest, 4)
	done := make(chan struct{})
	defer close(done)
	go readControls(input, commands, eof, done)
	go func() {
		select {
		case <-eof:
			stopChecks()
		case <-lifeCtx.Done():
		}
	}()
	ctx, cancel := context.WithTimeout(lifeCtx, 22*time.Second)
	start := time.Now()
	emit(map[string]any{"event": "progress", "stage": "server", "elapsedMs": 0})
	onChecking := func() {
		emit(map[string]any{"event": "progress", "stage": "checking", "elapsedMs": time.Since(start).Milliseconds()})
	}
	connected, e := connectProfiles(ctx, profiles, onChecking, func() { emit(map[string]any{"event": "alternate"}) })
	cancel()
	if e != nil {
		return e
	}
	t, lat := connected.tunnel, connected.latency
	onChecking()
	var audit func(string, string)
	if r.AuditRoutes {
		audit = func(host, route string) { emit(map[string]any{"event": "route", "host": host, "route": route}) }
	}
	p, e := NewProxy(t, policy, audit, r.InternalToken)
	if e != nil {
		t.Close()
		return e
	}
	defer p.Close()
	h, s := p.ports()
	emit(map[string]any{"event": "ready", "httpPort": h, "socksPort": s, "latencyMs": lat, "connectMs": time.Since(start).Milliseconds(), "profileID": connected.profile.ID})
	type exitResult struct {
		id   string
		info ExitInfo
		err  error
	}
	exitResults := make(chan exitResult, 1)
	checkingExit := false
	type speedResult struct {
		id     string
		report SpeedReport
		err    error
	}
	speedResults := make(chan speedResult, 1)
	speedID := ""
	var speedCancel context.CancelFunc
	defer func() {
		if speedCancel != nil {
			speedCancel()
		}
	}()
	stats := time.NewTicker(time.Second)
	defer stats.Stop()
	health := time.NewTicker(30 * time.Second)
	defer health.Stop()
	type sample struct {
		ms  int64
		err error
	}
	results := make(chan sample, 1)
	var checking bool
	var failed int
	var checkID string
	launchCheck := func(id string) {
		checking = true
		checkID = id
		go func() {
			ctx, cancel := context.WithTimeout(lifeCtx, 10*time.Second)
			defer cancel()
			ms, e := probe(ctx, t)
			select {
			case results <- sample{ms, e}:
			case <-lifeCtx.Done():
			}
		}()
	}
	for {
		select {
		case <-eof:
			return nil
		case <-lifeCtx.Done():
			return nil
		case command := <-commands:
			switch command.Action {
			case "check":
				if checking {
					checkID = command.ID
				} else {
					launchCheck(command.ID)
				}
			case "cancelSpeedTest":
				if speedID == command.ID && speedCancel != nil {
					speedCancel()
				}
			case "speedTest":
				if speedID != "" || checkingExit {
					emit(map[string]any{"event": "speedResult", "id": command.ID, "ok": false, "message": "有检测正在进行，请稍后重试"})
					continue
				}
				speedID = command.ID
				testCtx, cancel := context.WithTimeout(lifeCtx, 40*time.Second)
				speedCancel = cancel
				go func(id string, skipClaude bool) {
					defer cancel()
					report, err := runNetworkTest(testCtx, p, defaultSpeedTargets(), skipClaude, func(stage string, partial SpeedReport, bytes int64) {
						emit(map[string]any{"event": "speedProgress", "id": id, "stage": stage, "report": partial, "bytes": bytes, "totalBytes": speedSampleBytes})
					})
					select {
					case speedResults <- speedResult{id, report, err}:
					case <-lifeCtx.Done():
					}
				}(command.ID, command.SkipClaude)
			case "verifyExit":
				if checkingExit || speedID != "" {
					emit(map[string]any{"event": "exitVerification", "id": command.ID, "ok": false, "message": "有检测正在进行，请稍后重试"})
					continue
				}
				checkingExit = true
				go func(id string) {
					ctx, cancel := context.WithTimeout(lifeCtx, 12*time.Second)
					defer cancel()
					info, err := verifyExit(ctx, t)
					select {
					case exitResults <- exitResult{id, info, err}:
					case <-lifeCtx.Done():
					}
				}(command.ID)
			}
		case result := <-speedResults:
			if result.id != speedID {
				continue
			}
			speedID = ""
			speedCancel = nil
			event := map[string]any{"event": "speedResult", "id": result.id, "ok": result.err == nil, "report": result.report}
			if result.err != nil {
				if errors.Is(result.err, context.Canceled) {
					event["message"] = "测速已停止"
				} else if errors.Is(result.err, context.DeadlineExceeded) {
					event["message"] = "测速超时，请重试"
				} else {
					event["message"] = result.err.Error()
				}
			}
			emit(event)
		case result := <-exitResults:
			checkingExit = false
			event := map[string]any{"event": "exitVerification", "id": result.id, "ok": result.err == nil}
			if result.err != nil {
				event["message"] = result.err.Error()
			} else {
				event["ip"] = result.info.IP
				event["country"] = result.info.Country
				event["verifiedAt"] = result.info.VerifiedAt
				event["durationMs"] = result.info.DurationMs
			}
			emit(event)
		case <-stats.C:
			emit(map[string]any{"event": "stats", "uploadBytes": p.up.Load(), "downloadBytes": p.down.Load()})
		case <-health.C:
			if !checking && speedID == "" {
				launchCheck("")
			}
		case result := <-results:
			checking = false
			forced := checkID != ""
			id := checkID
			checkID = ""
			if result.err != nil {
				failed++
				emit(map[string]any{"event": "health", "ok": false, "checkID": id})
				if failed >= 2 || forced {
					return errors.New("连接已失去响应，正在恢复")
				}
			} else {
				failed = 0
				emit(map[string]any{"event": "health", "ok": true, "latencyMs": result.ms, "checkID": id})
			}
		}
	}
}
func main() {
	runtime.GOMAXPROCS(4)
	if len(os.Args) != 2 {
		fail(errors.New("使用 run / routes / validate / benchmark / seal / unseal"))
		os.Exit(1)
	}
	input := bufio.NewReader(os.Stdin)
	if os.Args[1] == "cloud" {
		if e := runCloud(input); e != nil {
			fail(e)
			os.Exit(1)
		}
		return
	}
	r, e := readRequest(input)
	if e != nil {
		fail(e)
		os.Exit(1)
	}
	switch os.Args[1] {
	case "run":
		e = run(r, input)
	case "transport":
		if len(r.InternalToken) < 32 || len(r.InternalToken) > 128 {
			e = connectionError{message: "内部连接凭据无效"}
			break
		}
		r.RoutingMode = "all"
		r.ProxyDomains = nil
		r.Bypass = nil
		e = runTransport(r, input)
	case "speedDirect":
		e = runDirectSpeed(r.ID, input)
	case "routes":
		var policy RoutingPolicy
		policy, e = NewRoutingPolicy(r.RoutingMode, r.ProxyDomains, r.Bypass)
		if e == nil {
			for _, host := range r.Hosts {
				emit(map[string]any{"event": "route", "host": host, "route": policy.Route(host)})
			}
		}
	case "validate":
		e = ValidateProfile(r.Profile)
		if e == nil {
			emit(map[string]any{"event": "valid"})
		}
	case "seal":
		var d string
		d, e = seal(r.Password, r.Profiles)
		if e == nil {
			emit(map[string]any{"event": "sealed", "data": d})
		}
	case "unseal":
		var p []Profile
		p, e = unseal(r.Password, r.Data)
		if e == nil {
			emit(map[string]any{"event": "unsealed", "profiles": p})
		}
	case "benchmark":
		if len(r.Profiles) > 16 {
			e = errors.New("最多比较 16 条连接")
			break
		}
		// Sequential tests prevent identical WireGuard peers roaming between sessions.
		for _, p := range r.Profiles {
			ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
			start := time.Now()
			t, err := OpenTunnel(ctx, p)
			var first, second int64
			if err == nil {
				first, err = probe(ctx, t)
				if err == nil {
					second, err = probe(ctx, t)
				}
				t.Close()
			}
			cancel()
			v := map[string]any{"event": "benchmark", "id": p.ID, "ok": err == nil, "connectMs": time.Since(start).Milliseconds(), "latencyMs": (first + second) / 2}
			if err != nil {
				v["message"] = "该连接未通过验证"
			}
			emit(v)
		}
	default:
		e = errors.New("未知命令")
	}
	if e != nil {
		fail(e)
		os.Exit(1)
	}
}
