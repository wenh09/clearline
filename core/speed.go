package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

const speedSampleBytes int64 = 5_000_000

type SiteMeasurement struct {
	Samples     int    `json:"samples,omitempty"`
	MinMs       int64  `json:"minMs,omitempty"`
	MaxMs       int64  `json:"maxMs,omitempty"`
	WarmMs      int64  `json:"warmMs,omitempty"`
	WarmSamples int    `json:"warmSamples,omitempty"`
	Skipped     bool   `json:"skipped"`
	Route       string `json:"route"`
	ResponseMs  int64  `json:"responseMs"`
	ConnectMs   int64  `json:"connectMs"`
	TLSMs       int64  `json:"tlsMs"`
	Status      int    `json:"status"`
	OK          bool   `json:"ok"`
	Limited     bool   `json:"limited"`
	Message     string `json:"message"`
}
type DownloadMeasurement struct {
	Route          string  `json:"route"`
	BodyMbps       float64 `json:"bodyMbps,omitempty"`
	BodyDurationMs int64   `json:"bodyDurationMs,omitempty"`
	Mbps           float64 `json:"mbps"`
	Bytes          int64   `json:"bytes"`
	DurationMs     int64   `json:"durationMs"`
	OK             bool    `json:"ok"`
	Message        string  `json:"message"`
}
type SpeedReport struct {
	Claude    *SiteMeasurement     `json:"claude,omitempty"`
	Ordinary  *SiteMeasurement     `json:"ordinary,omitempty"`
	Download  *DownloadMeasurement `json:"download,omitempty"`
	CheckedAt string               `json:"checkedAt"`
}
type speedTargets struct {
	claude, ordinary, download string
	bytes                      int64
}

func defaultSpeedTargets() speedTargets {
	return speedTargets{"https://claude.ai/", "https://speed.cloudflare.com/__down?bytes=0", "https://speed.cloudflare.com/__down?bytes=5000000", speedSampleBytes}
}

type diagnosticDial func(context.Context, string, string) (net.Conn, error)

func diagnosticTransport(dial diagnosticDial) *http.Transport {
	return &http.Transport{DialContext: dial, TLSHandshakeTimeout: 8 * time.Second, ResponseHeaderTimeout: 10 * time.Second, DisableKeepAlives: true, ForceAttemptHTTP2: true, DisableCompression: true}
}
func diagnosticRequest(ctx context.Context, method, target string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Clearline/0.3.0 network-diagnostic")
	req.Header.Set("Cache-Control", "no-store")
	req.Header.Set("Accept-Encoding", "identity")
	return req, nil
}
func measureSite(ctx context.Context, dial diagnosticDial, target, route string) SiteMeasurement {
	result := SiteMeasurement{Route: route}
	started := time.Now()
	var mu sync.Mutex
	var tlsStarted time.Time
	wrapped := func(ctx context.Context, network, address string) (net.Conn, error) {
		start := time.Now()
		c, err := dial(ctx, network, address)
		mu.Lock()
		result.ConnectMs = time.Since(start).Milliseconds()
		mu.Unlock()
		return c, err
	}
	tr := diagnosticTransport(wrapped)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	trace := &httptrace.ClientTrace{
		TLSHandshakeStart: func() { mu.Lock(); tlsStarted = time.Now(); mu.Unlock() },
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			mu.Lock()
			if !tlsStarted.IsZero() {
				result.TLSMs = time.Since(tlsStarted).Milliseconds()
			}
			mu.Unlock()
		},
		GotFirstResponseByte: func() { mu.Lock(); result.ResponseMs = time.Since(started).Milliseconds(); mu.Unlock() },
	}
	req, err := diagnosticRequest(httptrace.WithClientTrace(ctx, trace), http.MethodHead, target)
	if err != nil {
		result.Message = "无法发起检测"
		return result
	}
	resp, err := client.Do(req)
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		result.Message = "连接超时或不可达"
		return result
	}
	defer resp.Body.Close()
	result.Status = resp.StatusCode
	result.OK = true
	if result.ResponseMs == 0 {
		result.ResponseMs = time.Since(started).Milliseconds()
	}
	switch {
	case resp.StatusCode == 403:
		result.Limited = true
		result.Message = "返回验证页或访问受限 (403)"
	case resp.StatusCode == 429:
		result.Limited = true
		result.Message = "请求受限 (429)"
	case resp.StatusCode >= 500:
		result.Limited = true
		result.Message = "服务暂时异常"
	case resp.StatusCode >= 400:
		result.Message = "已收到未登录响应"
	case resp.StatusCode >= 300:
		result.Message = "已响应，发生跳转"
	default:
		result.Message = "已收到响应"
	}
	return result
}
func medianTiming(values []int64) int64 {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	if len(values)%2 == 0 {
		return (values[len(values)/2-1] + values[len(values)/2]) / 2
	}
	return values[len(values)/2]
}

// Ordinary public probes sample cold starts separately from actual reuse.
// Claude receives only its existing single anonymous HEAD, never these repeats.
func measureOrdinary(ctx context.Context, dial diagnosticDial, target, route string) SiteMeasurement {
	var result SiteMeasurement
	var cold, connects, handshakes []int64
	for i := 0; i < 3 && ctx.Err() == nil; i++ {
		sample := measureSite(ctx, dial, target, route)
		if len(cold) == 0 {
			result = sample
		}
		if sample.OK {
			cold = append(cold, sample.ResponseMs)
			connects = append(connects, sample.ConnectMs)
			handshakes = append(handshakes, sample.TLSMs)
		}
	}
	if len(cold) == 0 {
		return result
	}
	result.Samples = len(cold)
	result.ResponseMs = medianTiming(cold)
	result.MinMs, result.MaxMs = cold[0], cold[len(cold)-1]
	result.ConnectMs, result.TLSMs = medianTiming(connects), medianTiming(handshakes)
	tr := diagnosticTransport(dial)
	tr.DisableKeepAlives = false
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var warm []int64
	for i := 0; i < 3 && ctx.Err() == nil; i++ {
		reused := false
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
		req, err := diagnosticRequest(httptrace.WithClientTrace(ctx, trace), http.MethodHead, target)
		if err != nil {
			break
		}
		start := time.Now()
		resp, err := client.Do(req)
		elapsed := time.Since(start).Milliseconds()
		if err != nil {
			break
		}
		resp.Body.Close()
		if reused {
			warm = append(warm, elapsed)
		}
	}
	if len(warm) > 0 {
		result.WarmSamples = len(warm)
		result.WarmMs = medianTiming(warm)
	}
	return result
}

func measureDownload(ctx context.Context, dial diagnosticDial, target, route string, limit int64, progress func(int64)) DownloadMeasurement {
	result := DownloadMeasurement{Route: route}
	if limit <= 0 || limit > speedSampleBytes {
		result.Message = "测速样本无效"
		return result
	}
	u, err := url.Parse(target)
	if err != nil {
		result.Message = "测速地址无效"
		return result
	}
	query := u.Query()
	query.Set("r", strconv.FormatInt(time.Now().UnixNano(), 10))
	u.RawQuery = query.Encode()
	tr := diagnosticTransport(dial)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := diagnosticRequest(ctx, http.MethodGet, u.String())
	if err != nil {
		result.Message = "无法发起下载采样"
		return result
	}
	requestStarted := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		result.Message = "下载服务不可达"
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || (resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity") {
		result.Message = "下载服务返回异常"
		return result
	}
	start := time.Now()
	last := start
	buffer := make([]byte, 32768)
	reader := io.LimitReader(resp.Body, limit)
	for result.Bytes < limit {
		n, readErr := reader.Read(buffer)
		result.Bytes += int64(n)
		if progress != nil && time.Since(last) >= 250*time.Millisecond {
			progress(result.Bytes)
			last = time.Now()
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			result.Message = "下载采样中断或超时"
			return result
		}
	}
	elapsed := time.Since(start)
	result.BodyDurationMs = elapsed.Milliseconds()
	totalElapsed := time.Since(requestStarted)
	result.DurationMs = totalElapsed.Milliseconds()
	if result.Bytes != limit {
		result.Message = "下载样本不完整"
		return result
	}
	if elapsed <= 0 {
		result.Message = "样本耗时无效"
		return result
	}
	result.BodyMbps = float64(result.Bytes) * 8 / elapsed.Seconds() / 1_000_000
	result.Mbps = float64(result.Bytes) * 8 / totalElapsed.Seconds() / 1_000_000
	result.OK = true
	if progress != nil {
		progress(result.Bytes)
	}
	return result
}
func speedRoute(policy RoutingPolicy, target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return "direct"
	}
	return policy.Route(u.Hostname())
}
func runSpeedTest(ctx context.Context, p *Proxy, targets speedTargets, progress func(string, SpeedReport, int64)) (SpeedReport, error) {
	return runNetworkTest(ctx, p, targets, false, progress)
}
func runNetworkTest(ctx context.Context, p *Proxy, targets speedTargets, skipClaude bool, progress func(string, SpeedReport, int64)) (SpeedReport, error) {
	report := SpeedReport{}
	dial := func(ctx context.Context, _ string, address string) (net.Conn, error) { return p.dial(ctx, address) }
	progress("claude", report, 0)
	sample := SiteMeasurement{Route: "tunnel", Skipped: true, Message: "请先连接代理"}
	if !skipClaude {
		sample = measureSite(ctx, dial, targets.claude, speedRoute(p.policy, targets.claude))
	}
	report.Claude = &sample
	if ctx.Err() != nil {
		return report, ctx.Err()
	}
	progress("ordinary", report, 0)
	ordinary := measureOrdinary(ctx, dial, targets.ordinary, speedRoute(p.policy, targets.ordinary))
	report.Ordinary = &ordinary
	if ctx.Err() != nil {
		return report, ctx.Err()
	}
	progress("download", report, 0)
	downloaded := measureDownload(ctx, dial, targets.download, speedRoute(p.policy, targets.download), targets.bytes, func(n int64) { progress("download", report, n) })
	report.Download = &downloaded
	report.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	if ctx.Err() != nil {
		return report, ctx.Err()
	}
	if !sample.OK && !ordinary.OK && !downloaded.OK {
		return report, errors.New("所有检测均未通过，请检查网络")
	}
	return report, nil
}

// A disconnected diagnostic tests only ordinary direct traffic. Never probe
// Claude from the local public IP while the proxy is unavailable.
func runDirectSpeed(id string, input *bufio.Reader) error {
	if id == "" || len(id) > 64 {
		return errors.New("测速编号无效")
	}
	life, stop := context.WithCancel(context.Background())
	defer stop()
	ctx, cancel := context.WithTimeout(life, 40*time.Second)
	defer cancel()
	commands := make(chan ControlRequest, 4)
	eof := make(chan struct{})
	done := make(chan struct{})
	defer close(done)
	go readControls(input, commands, eof, done)
	go func() {
		for {
			select {
			case command := <-commands:
				if command.Action == "cancelSpeedTest" && command.ID == id {
					cancel()
				}
			case <-eof:
				stop()
				return
			case <-done:
				return
			}
		}
	}()
	p := &Proxy{policy: RoutingPolicy{Mode: "claude"}}
	report, err := runNetworkTest(ctx, p, defaultSpeedTargets(), true, func(stage string, partial SpeedReport, bytes int64) {
		emit(map[string]any{"event": "speedProgress", "id": id, "stage": stage, "report": partial, "bytes": bytes, "totalBytes": speedSampleBytes})
	})
	event := map[string]any{"event": "speedResult", "id": id, "ok": err == nil, "report": report}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			event["message"] = "测速已停止"
		} else {
			event["message"] = "普通网络测速未完成，请重试"
		}
	}
	emit(event)
	// Keep the process alive until the UI consumes the final event and closes
	// stdin; this avoids losing the final result to process-exit notification.
	<-eof
	return nil
}
