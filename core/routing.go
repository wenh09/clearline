package main

import (
	"embed"
	"errors"
	"net"
	"strings"
)

//go:embed claude-domains.txt ai-domains.txt
var domainFiles embed.FS

// RoutingPolicy matches the requested hostname before DNS resolution. Tunnel
// failures never fall back to a direct connection.
type RoutingPolicy struct {
	Mode            string
	Domains, Bypass []string
}

func normalizeDomains(values []string) ([]string, error) {
	var result []string
	for _, value := range values {
		domain := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "*."), "."), ".")
		if domain == "" {
			continue
		}
		if len(domain) > 253 || net.ParseIP(domain) != nil || !strings.Contains(domain, ".") {
			return nil, errors.New("请输入有效域名，每行一个")
		}
		for _, label := range strings.Split(domain, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, errors.New("域名格式无效")
			}
			for _, c := range label {
				if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
					return nil, errors.New("域名格式无效")
				}
			}
		}
		result = append(result, domain)
	}
	return result, nil
}
func NewRoutingPolicy(mode string, domains, bypass []string) (RoutingPolicy, error) {
	if mode == "" {
		mode = "ai"
	}
	p := RoutingPolicy{Mode: mode}
	if mode != "ai" && mode != "claude" && mode != "smart" && mode != "all" {
		return p, errors.New("未知分流模式")
	}
	if domains == nil {
		file := "claude-domains.txt"
		if mode == "ai" {
			file = "ai-domains.txt"
		}
		data, _ := domainFiles.ReadFile(file)
		domains = strings.Fields(string(data))
	}
	var e error
	if p.Domains, e = normalizeDomains(domains); e != nil {
		return p, e
	}
	if p.Bypass, e = normalizeDomains(bypass); e != nil {
		return p, e
	}
	return p, nil
}
func matchesDomain(host string, domains []string) bool {
	for _, domain := range domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}
func (p RoutingPolicy) Route(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if p.Mode == "all" {
		return "tunnel"
	}
	ip := net.ParseIP(host)
	if host == "localhost" || strings.HasSuffix(host, ".local") || (ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())) {
		return "direct"
	}
	if p.Mode == "ai" || p.Mode == "claude" {
		if matchesDomain(host, p.Domains) {
			return "tunnel"
		}
		return "direct"
	}
	if matchesDomain(host, p.Bypass) {
		return "direct"
	}
	return "tunnel"
}
