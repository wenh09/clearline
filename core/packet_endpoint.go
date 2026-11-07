package main

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
)

// Resolve once before installing routes/DNS, and preserve the original TLS
// identity. Recovery workers must not resolve their own entry via the tunnel.
func pinPacketEndpoint(ctx context.Context, r Request) (Request, string, error) {
	host, e := profileHost(r.Profile)
	if e != nil {
		return r, "", e
	}
	ips, e := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if e != nil || len(ips) == 0 {
		return r, "", errors.New("全局隧道需要可用的 IPv4 服务器入口")
	}
	ip := ips[0].To4()
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() {
		return r, "", errors.New("全局隧道服务器入口无效")
	}
	pin := func(p Profile) (Profile, error) {
		h, e := profileHost(p)
		if e != nil || h != host {
			return p, errors.New("备用连接入口不一致")
		}
		if p.Kind == "hysteria2" {
			c, e := ParseHY(p.Config)
			if e != nil {
				return p, e
			}
			u, e := url.Parse(p.Config)
			if e != nil {
				return p, e
			}
			_, port, _ := net.SplitHostPort(c.Server)
			u.Host = net.JoinHostPort(ip.String(), port)
			q := u.Query()
			q.Set("sni", c.SNI)
			u.RawQuery = q.Encode()
			p.Config = u.String()
		} else {
			c, e := ParseAWG(p.Config)
			if e != nil {
				return p, e
			}
			_, port, _ := net.SplitHostPort(c.Endpoint)
			lines := strings.Split(p.Config, "\n")
			for i, line := range lines {
				k, _, ok := strings.Cut(line, "=")
				if ok && strings.EqualFold(strings.TrimSpace(k), "endpoint") {
					lines[i] = "Endpoint = " + net.JoinHostPort(ip.String(), port)
				}
			}
			p.Config = strings.Join(lines, "\n")
		}
		return p, nil
	}
	r.Profile, e = pin(r.Profile)
	if e != nil {
		return r, "", e
	}
	for i, p := range r.Alternatives {
		r.Alternatives[i], e = pin(p)
		if e != nil {
			return r, "", e
		}
	}
	return r, ip.String(), nil
}
