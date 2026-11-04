package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

type healthTarget struct {
	url   string
	trace bool
}

func probe(ctx context.Context, t Tunnel) (int64, error) {
	return probeTargets(ctx, t, []healthTarget{{url: "https://www.gstatic.com/generate_204"}, {url: exitTraceURL, trace: true}}, 4*time.Second)
}

// Both services are reached only through the supplied tunnel. A single
// unavailable service must not tear down an otherwise working connection.
func probeTargets(ctx context.Context, t Tunnel, targets []healthTarget, timeout time.Duration) (int64, error) {
	return probeTargetsWithHedge(ctx, t, targets, timeout, 600*time.Millisecond)
}

// A slow detection site must not delay an otherwise healthy tunnel for the
// full primary timeout. Start the backup only after the hedge delay or an
// explicit failure, cancel losers, and still validate every response.
func probeTargetsWithHedge(parent context.Context, t Tunnel, targets []healthTarget, timeout, hedge time.Duration) (int64, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if len(targets) == 0 {
		return 0, errors.New("缺少检测地址")
	}
	started := time.Now()
	results := make(chan error, len(targets))
	launched, completed := 0, 0
	launch := func() {
		target := targets[launched]
		launched++
		go func() { _, err := probeTarget(ctx, t, target, timeout); results <- err }()
	}
	launch()
	timer := time.NewTimer(hedge)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-timer.C:
			if launched < len(targets) {
				launch()
				timer.Reset(hedge)
			}
		case err := <-results:
			completed++
			if err == nil {
				if ctx.Err() != nil {
					return 0, ctx.Err()
				}
				return time.Since(started).Milliseconds(), nil
			}
			if launched < len(targets) {
				launch()
			}
			if completed == len(targets) {
				return 0, errors.New("连接未通过验证，请检查配置和网络")
			}
		}
	}
}

func probeTarget(parent context.Context, t Tunnel, target healthTarget, timeout time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	tr := &http.Transport{DialContext: t.DialContext, TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout, DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.url, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if target.trace {
		if resp.StatusCode != http.StatusOK {
			return 0, errors.New("检测服务响应异常")
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8193))
		if err != nil {
			return 0, err
		}
		if _, err := parseExitTrace(string(body)); err != nil {
			return 0, err
		}
	} else if resp.StatusCode != http.StatusNoContent {
		return 0, errors.New("检测服务响应异常")
	}
	return time.Since(start).Milliseconds(), nil
}
