package main

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

type sessionOptions struct {
	command    func() (*exec.Cmd, error)
	retryDelay func(int) time.Duration
	stableFor  time.Duration
}

func defaultSessionOptions() sessionOptions {
	return sessionOptions{command: transportCommand, retryDelay: func(attempt int) time.Duration { return time.Duration(min(30, 1<<min(attempt, 5))) * time.Second }, stableFor: 30 * time.Second}
}
func eventNumber(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	}
	return 0
}

// The frontend owns the local ports and routing policy for the whole enabled
// session. A replaceable worker owns only the encrypted transport.
func runSession(parent context.Context, r Request, input *bufio.Reader, output func(any), options sessionOptions) error {
	if options.stableFor <= 0 {
		options.stableFor = 30 * time.Second
	}
	policy, err := NewRoutingPolicy(r.RoutingMode, r.ProxyDomains, r.Bypass)
	if err != nil {
		return err
	}
	if _, err = connectionCandidates(r.Profile, r.Alternatives, r.PreferredID); err != nil {
		return err
	}
	var endpoint string
	var routing *packetRouting
	var physical *physicalTunnel
	if r.PacketTunnel {
		// Only matching, validated candidates may receive the entry bypass.
		candidates, _ := connectionCandidates(r.Profile, r.Alternatives, r.PreferredID)
		r.Profile = candidates[0]
		r.Alternatives = candidates[1:]
		r, endpoint, err = pinPacketEndpoint(parent, r)
		if err != nil {
			return err
		}
		config, e := os.UserConfigDir()
		if e != nil {
			return errors.New("无法定位分流 DNS 缓存")
		}
		names, e := openFakeIPCache(filepath.Join(config, "Clearline", "dns-map-v1.json"))
		if e != nil {
			return e
		}
		routing = &packetRouting{policy: policy, names: names}
		if policy.Mode != "all" {
			direct, e := capturePhysicalTunnel(parent)
			if e != nil {
				return e
			}
			routing.direct, physical = direct, direct
		}
	}
	ctx, cancel := context.WithCancel(parent)
	retained := newRetainedTunnel()
	if physical != nil {
		physical.useTunnelDNS(retained)
	}
	var direct Tunnel
	if routing != nil {
		direct = routing.direct
	}
	p, err := newProxy(retained, policy, func(host, route string) {
		if r.AuditRoutes {
			output(map[string]any{"event": "route", "host": host, "route": route})
		}
	}, direct)
	if err != nil {
		cancel()
		retained.Close()
		return err
	}
	var worker *transportWorker
	var packets *packetSession
	var packetFailure <-chan error
	defer func() {
		if packets != nil {
			packets.Close()
		}
		cancel()
		p.Close()
		if worker != nil {
			worker.shutdown()
		}
	}()
	httpPort, socksPort := p.ports()
	events := make(chan workerEvent, 16)
	commands := make(chan ControlRequest, 4)
	eof := make(chan struct{})
	go readControls(input, commands, eof, ctx.Done())
	type diagnostic struct {
		event    map[string]any
		revision uint64
	}
	diagnostics := make(chan diagnostic, 8)
	var revision uint64
	var exitID, speedID string
	var exitCancel, speedCancel context.CancelFunc
	abortDiagnostics := func() {
		revision++
		if exitCancel != nil {
			exitCancel()
			exitCancel = nil
		}
		if speedCancel != nil {
			speedCancel()
			speedCancel = nil
		}
		if exitID != "" {
			output(map[string]any{"event": "exitVerification", "id": exitID, "ok": false, "message": "连接已变化，请重试"})
			exitID = ""
		}
		if speedID != "" {
			output(map[string]any{"event": "speedResult", "id": speedID, "ok": false, "message": "连接已变化，请重试"})
			speedID = ""
		}
	}
	defer abortDiagnostics()
	post := func(event map[string]any, rev uint64) {
		select {
		case diagnostics <- diagnostic{event, rev}:
		case <-ctx.Done():
		}
	}
	var retryTimer *time.Timer
	var retry <-chan time.Time
	stopRetry := func() {
		if retryTimer != nil {
			retryTimer.Stop()
		}
		retry = nil
	}
	defer stopRetry()
	online, everReady, immediate := true, false, false
	attempt := 0
	var readyAt time.Time
	preferred := r.PreferredID
	started := time.Now()
	recoveryStarted := started
	state := func(value, phase, message string, delay time.Duration) {
		output(map[string]any{"event": "transportState", "state": value, "stage": phase, "message": message, "retryAfterMs": delay.Milliseconds(), "proxyRetained": everReady})
	}
	start := func() error {
		stopRetry()
		if !online || worker != nil {
			return nil
		}
		state("connecting", "server", "正在连接美国服务器…", 0)
		request := Request{Profile: r.Profile, Alternatives: r.Alternatives, PreferredID: preferred, RoutingMode: "all", PacketTunnel: r.PacketTunnel}
		var startErr error
		worker, startErr = startTransportWorker(ctx, request, options.command, events)
		if startErr != nil {
			if worker != nil {
				worker.shutdown()
				worker = nil
			}
			return connectionError{message: "无法启动传输模块，请重新打开应用", cause: startErr}
		}
		return nil
	}
	invalidate := func() { retained.set(nil); p.httpTransport.CloseIdleConnections(); abortDiagnostics() }
	restart := func(message string) {
		stopRetry()
		invalidate()
		recoveryStarted = time.Now()
		immediate = true
		state("recovering", "server", message, 0)
		if worker != nil {
			worker.stop()
		}
	}
	if err := start(); err != nil {
		return err
	}
	stats := time.NewTicker(time.Second)
	defer stats.Stop()
	for {
		select {
		case <-packetFailure:
			return connectionError{message: "系统隧道已中断，请检查后台服务后重新连接"}
		case <-ctx.Done():
			return nil
		case <-eof:
			return nil
		case <-retry:
			if err := start(); err != nil {
				return err
			}
		case <-stats.C:
			up, down := p.up.Load(), p.down.Load()
			if packets != nil {
				a, b := packets.bytes()
				up += a
				down += b
			}
			output(map[string]any{"event": "stats", "uploadBytes": up, "downloadBytes": down})
		case item := <-events:
			if item.worker != worker {
				continue
			}
			if item.exited {
				last := worker.lastError
				if worker.ready && !readyAt.IsZero() && time.Since(readyAt) >= options.stableFor {
					attempt = 0
				}
				worker.finish()
				worker = nil
				invalidate()
				if !online {
					continue
				}
				if last != nil && !retryableConnectionError(last) && !immediate {
					return last
				}
				delay := options.retryDelay(attempt)
				// The first interruption of a usable session gets an immediate
				// retry. Short-lived ready/crash loops retain their backoff.
				if everReady && attempt == 0 {
					delay = 0
				}
				attempt = min(attempt+1, 30)
				if immediate {
					delay = 0
					immediate = false
				}
				if everReady && recoveryStarted.Equal(started) {
					recoveryStarted = time.Now()
				}
				state("recovering", "waiting", "正在恢复连接…", delay)
				retryTimer = time.NewTimer(delay)
				retry = retryTimer.C
				continue
			}
			value := item.value
			switch value["event"] {
			case "progress", "alternate":
				if online && worker.stopTimer == nil {
					output(value)
				}
			case "error":
				worker.lastError = workerFailure(value)
			case "health":
				if online && worker.ready && worker.stopTimer == nil {
					output(value)
				}
			case "ready":
				if worker.ready || !online || worker.stopTimer != nil {
					continue
				}
				port := eventNumber(value["socksPort"])
				if port < 1 || port > 65535 {
					return errors.New("传输模块未返回有效端口")
				}
				tunnel, e := newWorkerTunnel(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), worker.token)
				if e != nil {
					return e
				}
				retained.set(tunnel)
				if r.PacketTunnel && packets == nil {
					output(map[string]any{"event": "progress", "stage": "applying"})
					packets, e = startPacketSession(ctx, retained, endpoint, routing)
					if e != nil {
						return connectionError{message: e.Error()}
					}
					packetFailure = packets.done
					if policy.Mode == "all" {
						e = verifyGlobalPacketPath(ctx, retained)
					} else {
						e = verifySplitPacketPath(ctx, retained, policy)
					}
					if e != nil {
						if failure := packets.deviceError(); failure != nil {
							e = failure
						}
						return connectionError{message: e.Error()}
					}
				}
				worker.ready = true
				readyAt = time.Now()
				preferred, _ = value["profileID"].(string)
				event := map[string]any{"event": "ready", "retainsProxy": true, "httpPort": httpPort, "socksPort": socksPort, "latencyMs": value["latencyMs"], "profileID": preferred, "transportPID": worker.cmd.Process.Pid, "connectMs": time.Since(started).Milliseconds()}
				event["packetTunnel"] = packets != nil
				if everReady {
					event["event"] = "recovered"
					event["connectMs"] = time.Since(recoveryStarted).Milliseconds()
				}
				everReady = true
				recoveryStarted = started
				output(event)
			}
		case command := <-commands:
			switch command.Action {
			case "network":
				if command.Available == nil {
					continue
				}
				if !*command.Available {
					online = false
					stopRetry()
					invalidate()
					recoveryStarted = time.Now()
					immediate = true
					state("offline", "waiting", "等待网络恢复", 0)
					if worker != nil {
						worker.stop()
					}
				} else {
					if !online {
						attempt = 0
					}
					online = true
					immediate = true
					stopRetry()
					if worker == nil {
						immediate = false
						if err := start(); err != nil {
							return err
						}
					}
				}
			case "reconnect":
				restart("正在重建美国连接…")
				if worker == nil && online {
					immediate = false
					if err := start(); err != nil {
						return err
					}
				}
			case "check":
				if worker != nil && worker.ready {
					_ = worker.send(command)
				} else if online && worker != nil && everReady && command.Reason == "network" {
					restart("网络已切换，正在重建连接…")
				} else if online && worker == nil {
					if err := start(); err != nil {
						return err
					}
				}
			case "cancelSpeedTest":
				if command.ID == speedID && speedCancel != nil {
					speedCancel()
				}
			case "verifyExit", "speedTest":
				if retained.snapshot() == nil {
					event := "exitVerification"
					if command.Action == "speedTest" {
						event = "speedResult"
					}
					output(map[string]any{"event": event, "id": command.ID, "ok": false, "message": "连接正在恢复，请稍后重试"})
					continue
				}
				if exitID != "" || speedID != "" {
					event := "exitVerification"
					if command.Action == "speedTest" {
						event = "speedResult"
					}
					output(map[string]any{"event": event, "id": command.ID, "ok": false, "message": "有检测正在进行，请稍后重试"})
					continue
				}
				rev := revision
				if command.Action == "verifyExit" {
					exitID = command.ID
					testCtx, stop := context.WithTimeout(ctx, 12*time.Second)
					exitCancel = stop
					tunnel := retained.snapshot()
					go func(id string) {
						defer stop()
						info, e := verifyExit(testCtx, tunnel)
						event := map[string]any{"event": "exitVerification", "id": id, "ok": e == nil}
						if e != nil {
							event["message"] = e.Error()
						} else {
							event["ip"] = info.IP
							event["country"] = info.Country
							event["verifiedAt"] = info.VerifiedAt
							event["durationMs"] = info.DurationMs
						}
						post(event, rev)
					}(command.ID)
				} else {
					speedID = command.ID
					testCtx, stop := context.WithTimeout(ctx, 40*time.Second)
					speedCancel = stop
					go func(id string, skipClaude bool) {
						defer stop()
						report, e := runNetworkTest(testCtx, p, defaultSpeedTargets(), skipClaude, func(stage string, partial SpeedReport, bytes int64) {
							post(map[string]any{"event": "speedProgress", "id": id, "stage": stage, "report": partial, "bytes": bytes, "totalBytes": speedSampleBytes}, rev)
						})
						event := map[string]any{"event": "speedResult", "id": id, "ok": e == nil, "report": report}
						if errors.Is(e, context.Canceled) {
							event["message"] = "测速已停止"
						} else if e != nil {
							event["message"] = "测速失败，请重试"
						}
						post(event, rev)
					}(command.ID, command.SkipClaude)
				}
			}
		case result := <-diagnostics:
			if result.revision != revision {
				continue
			}
			switch result.event["event"] {
			case "exitVerification":
				exitID = ""
				exitCancel = nil
			case "speedResult":
				speedID = ""
				speedCancel = nil
			}
			output(result.event)
		}
	}
}
