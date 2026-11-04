package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"
)

type connectionError struct {
	message   string
	retryable bool
	cause     error
}

func (e connectionError) Error() string { return e.message }
func (e connectionError) Unwrap() error { return e.cause }

func retryableConnectionError(err error) bool {
	var classified connectionError
	return !errors.As(err, &classified) || classified.retryable
}

func profileHost(p Profile) (string, error) {
	var endpoint string
	var err error
	switch p.Kind {
	case "hysteria2":
		var c HYConfig
		c, err = ParseHY(p.Config)
		endpoint = c.Server
	case "awg":
		var c AWGConfig
		c, err = ParseAWG(p.Config)
		endpoint = c.Endpoint
	default:
		err = errors.New("未知协议")
	}
	if err != nil {
		return "", err
	}
	host, _, err := net.SplitHostPort(endpoint)
	if ip, parseErr := netip.ParseAddr(host); parseErr == nil {
		host = ip.Unmap().String()
	}
	return strings.TrimSuffix(strings.ToLower(host), "."), err
}

// Automatic alternatives must use the same configured server host. Never
// change destination country/provider merely because another profile exists.
func connectionCandidates(primary Profile, alternatives []Profile, preferredID string) ([]Profile, error) {
	host, err := profileHost(primary)
	if err != nil {
		return nil, connectionError{message: "连接配置无效，请重新导入", cause: err}
	}
	result := []Profile{primary}
	for _, candidate := range alternatives {
		other, err := profileHost(candidate)
		if err == nil && other == host && candidate.Kind != primary.Kind {
			result = append(result, candidate)
			break // At most one identity per protocol, no concurrent peer roaming.
		}
	}
	if len(result) == 2 && preferredID != "" && result[1].ID == preferredID {
		result[0], result[1] = result[1], result[0]
	}
	return result, nil
}

type connectionResult struct {
	tunnel  Tunnel
	profile Profile
	latency int64
	err     error
}

type connector struct {
	open    func(context.Context, Profile) (Tunnel, error)
	check   func(context.Context, Tunnel) (int64, error)
	stagger time.Duration
	budget  time.Duration
}

func connectProfiles(ctx context.Context, profiles []Profile, onChecking func(), onAlternate func()) (connectionResult, error) {
	return (connector{open: OpenTunnel, check: probe, stagger: 2 * time.Second, budget: 18 * time.Second}).connect(ctx, profiles, onChecking, onAlternate)
}

// Prefer the fast transport. Start the existing alternate after a bounded
// wait, accept only a fully checked tunnel, and close every losing attempt.
func (c connector) connect(parent context.Context, profiles []Profile, onChecking func(), onAlternate func()) (connectionResult, error) {
	if len(profiles) == 0 || len(profiles) > 2 {
		return connectionResult{}, connectionError{message: "缺少可用的连接配置"}
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	results := make(chan connectionResult)
	checking := make(chan struct{}, len(profiles))
	start := func(profile Profile) {
		go func() {
			attempt, stop := context.WithTimeout(ctx, c.budget)
			defer stop()
			r := connectionResult{profile: profile}
			r.tunnel, r.err = c.open(attempt, profile)
			if r.err == nil {
				checking <- struct{}{}
				r.latency, r.err = c.check(attempt, r.tunnel)
			}
			if r.err != nil && r.tunnel != nil {
				r.tunnel.Close()
				r.tunnel = nil
			}
			select {
			case results <- r:
			case <-ctx.Done():
				if r.tunnel != nil {
					r.tunnel.Close()
				}
			}
		}()
	}
	start(profiles[0])
	started, completed := 1, 0
	timer := time.NewTimer(c.stagger)
	defer timer.Stop()
	var last error
	canRetry := false
	startAlternate := func() {
		if started < len(profiles) {
			if onAlternate != nil {
				onAlternate()
			}
			start(profiles[started])
			started++
		}
	}
	for {
		select {
		case <-ctx.Done():
			return connectionResult{}, connectionError{message: "连接服务器超时，正在恢复", retryable: true, cause: ctx.Err()}
		case <-checking:
			if onChecking != nil {
				onChecking()
			}
		case <-timer.C:
			startAlternate()
		case r := <-results:
			completed++
			if r.err == nil {
				if ctx.Err() != nil {
					r.tunnel.Close()
					return connectionResult{}, ctx.Err()
				}
				return r, nil
			}
			last = r.err
			canRetry = canRetry || retryableConnectionError(r.err)
			startAlternate()
			if completed == len(profiles) {
				if !canRetry {
					return connectionResult{}, connectionError{message: "连接配置或服务器认证无效，请检查配置", cause: last}
				}
				return connectionResult{}, connectionError{message: "服务器暂时无响应，正在恢复", retryable: true, cause: last}
			}
		}
	}
}
