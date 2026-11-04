package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

const exitTraceURL = "https://www.cloudflare.com/cdn-cgi/trace"

type ExitInfo struct {
	IP         string `json:"ip"`
	Country    string `json:"country"`
	VerifiedAt string `json:"verifiedAt"`
	DurationMs int64  `json:"durationMs"`
}

func parseExitTrace(body string) (ExitInfo, error) {
	var info ExitInfo
	if len(body) > 8192 {
		return info, errors.New("出口验证响应过大")
	}
	ipCount, countryCount := 0, 0
	for _, line := range strings.Split(body, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ip":
			info.IP = value
			ipCount++
		case "loc":
			info.Country = value
			countryCount++
		}
	}
	addr, err := netip.ParseAddr(info.IP)
	if err != nil || ipCount != 1 || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return ExitInfo{}, errors.New("未获得有效公网 IP")
	}
	if countryCount != 1 || len(info.Country) != 2 || info.Country[0] < 'A' || info.Country[0] > 'Z' || info.Country[1] < 'A' || info.Country[1] > 'Z' {
		return ExitInfo{}, errors.New("未获得有效地区信息")
	}
	info.IP = addr.Unmap().String()
	return info, nil
}

// The diagnostic uses the already-connected tunnel, independent of the domain
// allowlist. It never uses a second connection or falls back to the local path.
func verifyExit(ctx context.Context, t Tunnel) (ExitInfo, error) {
	return verifyExitAt(ctx, t, exitTraceURL)
}
func verifyExitAt(ctx context.Context, t Tunnel, endpoint string) (ExitInfo, error) {
	tr := &http.Transport{DialContext: t.DialContext, TLSHandshakeTimeout: 8 * time.Second, ResponseHeaderTimeout: 8 * time.Second, DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ExitInfo{}, errors.New("无法发起出口验证")
	}
	req.Header.Set("User-Agent", "Clearline/0.3.0 IP-verification")
	start := time.Now()
	response, err := client.Do(req)
	if err != nil {
		return ExitInfo{}, errors.New("出口验证失败，请重试")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ExitInfo{}, errors.New("IP 检测服务暂不可用")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil {
		return ExitInfo{}, errors.New("无法读取出口验证结果")
	}
	info, err := parseExitTrace(string(body))
	if err != nil {
		return ExitInfo{}, err
	}
	info.VerifiedAt = time.Now().UTC().Format(time.RFC3339)
	info.DurationMs = time.Since(start).Milliseconds()
	return info, nil
}
