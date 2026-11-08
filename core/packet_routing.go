package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Stable, private mappings keep browser DNS caches usable across reconnects.
// Never recycle a slot: a stale browser entry must not connect to another host.
type fakeIPCache struct {
	mu    sync.Mutex
	path  string
	hosts []string
	ids   map[string]int
}

const packetTraceAddress = "198.19.255.254"

var fakeIPv4 = netip.MustParsePrefix("198.19.0.0/16")
var fakeIPv6 = netip.MustParsePrefix("fd7a:115c:a1e0:1::/112")

func openFakeIPCache(path string) (*fakeIPCache, error) {
	c := &fakeIPCache{path: path, ids: map[string]int{}}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, errors.New("无法读取分流 DNS 缓存")
	}
	if err == nil {
		if len(data) > 8<<20 || json.Unmarshal(data, &c.hosts) != nil || len(c.hosts) > 65000 {
			return nil, errors.New("分流 DNS 缓存无效")
		}
		for i, host := range c.hosts {
			if _, e := normalizeDomains([]string{host}); e != nil || host == "" {
				return nil, errors.New("分流 DNS 缓存无效")
			}
			if _, exists := c.ids[host]; exists {
				return nil, errors.New("分流 DNS 缓存重复")
			}
			c.ids[host] = i + 1
		}
	}
	return c, nil
}
func (c *fakeIPCache) address(host string, ipv6 bool) (netip.Addr, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	id := c.ids[host]
	if id == 0 {
		if _, err := normalizeDomains([]string{host}); err != nil || host == "" {
			return netip.Addr{}, errors.New("分流域名无效")
		}
		if len(c.hosts) >= 65000 {
			return netip.Addr{}, errors.New("分流 DNS 缓存已满")
		}
		if c.path != "" {
			if err := os.MkdirAll(filepath.Dir(c.path), 0700); err != nil {
				return netip.Addr{}, err
			}
			data, _ := json.Marshal(append(append([]string(nil), c.hosts...), host))
			f, err := os.CreateTemp(filepath.Dir(c.path), ".dns-map-")
			if err != nil {
				return netip.Addr{}, err
			}
			name := f.Name()
			defer os.Remove(name)
			if _, err = f.Write(data); err == nil {
				err = f.Sync()
			}
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
			if err == nil {
				err = os.Rename(name, c.path)
			}
			if err == nil {
				var directory *os.File
				directory, err = os.Open(filepath.Dir(c.path))
				if err == nil {
					err = directory.Sync()
					directory.Close()
				}
			}
			if err != nil {
				return netip.Addr{}, errors.New("无法保存分流 DNS 缓存")
			}
		}
		c.hosts = append(c.hosts, host)
		id = len(c.hosts)
		c.ids[host] = id
	}
	if ipv6 {
		a := fakeIPv6.Addr().As16()
		a[14], a[15] = byte(id>>8), byte(id)
		return netip.AddrFrom16(a), nil
	}
	return netip.AddrFrom4([4]byte{198, 19, byte(id >> 8), byte(id)}), nil
}
func (c *fakeIPCache) lookup(ip netip.Addr) (string, bool) {
	if !fakeIPv4.Contains(ip) && !fakeIPv6.Contains(ip) {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var id int
	if ip.Is4() {
		a := ip.As4()
		id = int(a[2])<<8 | int(a[3])
	} else {
		a := ip.As16()
		id = int(a[14])<<8 | int(a[15])
	}
	if id < 1 || id > len(c.hosts) {
		return "", true
	}
	return c.hosts[id-1], true
}

type packetRouting struct {
	policy   RoutingPolicy
	direct   Tunnel
	localDNS []string
	names    *fakeIPCache
	mu       sync.Mutex
	ordinary map[netip.Addr]time.Time
}

func (r *packetRouting) dial(ctx context.Context, tunnel Tunnel, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return nil, err
	}
	if host == packetTraceAddress {
		return tunnel.DialContext(ctx, network, net.JoinHostPort("1.1.1.1", port))
	}
	if r.names != nil {
		if name, fake := r.names.lookup(ip); fake {
			if name == "" {
				return nil, errors.New("分流 DNS 缓存已变化，请重新解析域名")
			}
			return tunnel.DialContext(ctx, network, net.JoinHostPort(name, port))
		}
	}
	if r.policy.Mode == "all" {
		return tunnel.DialContext(ctx, network, address)
	}
	if r.direct == nil {
		return nil, errors.New("本地直连入口不可用")
	}
	if network == "udp" {
		// Literal/unknown UDP (including WebRTC STUN) has no reliable domain
		// identity. Protect its exit; locally resolved ordinary hosts go direct.
		r.mu.Lock()
		expiry := r.ordinary[ip]
		r.mu.Unlock()
		if time.Now().After(expiry) || port == "3478" || port == "5349" || port == "19302" {
			return tunnel.DialContext(ctx, network, address)
		}
	}
	return r.direct.DialContext(ctx, network, address)
}
func (r *packetRouting) dns(ctx context.Context, tunnel Tunnel, query []byte) ([]byte, error) {
	var message dnsmessage.Message
	if message.Unpack(query) != nil || message.Header.Response || len(message.Questions) != 1 {
		return nil, errors.New("DNS 查询无效")
	}
	question := message.Questions[0]
	ai := r.policy.Route(question.Name.String()) == "tunnel"
	var reply []byte
	var err error
	if ai {
		reply, err = dnsThroughTunnel(ctx, tunnel, query)
	} else {
		reply, err = dnsThroughServers(ctx, r.direct, query, r.localDNS, true)
	}
	if err != nil {
		return nil, err
	}
	if r.policy.Mode == "all" {
		return reply, nil
	}
	var response dnsmessage.Message
	if response.Unpack(reply) != nil {
		return nil, errors.New("DNS 响应无效")
	}
	if ai {
		if question.Type == 64 || question.Type == 65 {
			// HTTPS/SVCB address hints can bypass synthetic A/AAAA routing.
			// A no-data answer makes clients use the protected address queries.
			response.Answers, response.Authorities, response.Additionals = nil, nil, nil
			response.Header.RCode = dnsmessage.RCodeSuccess
		} else {
			for i := range response.Answers {
				a := &response.Answers[i]
				switch body := a.Body.(type) {
				case *dnsmessage.AResource:
					ip, e := r.names.address(question.Name.String(), false)
					if e != nil {
						return nil, e
					}
					body.A = ip.As4()
					a.Header.TTL = min(a.Header.TTL, 60)
				case *dnsmessage.AAAAResource:
					ip, e := r.names.address(question.Name.String(), true)
					if e != nil {
						return nil, e
					}
					body.AAAA = ip.As16()
					a.Header.TTL = min(a.Header.TTL, 60)
				}
			}
			response.Additionals = nil
		}
		response.Header.AuthenticData = false // Synthetic addresses are not DNSSEC signatures.
		return response.Pack()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ordinary == nil {
		r.ordinary = map[netip.Addr]time.Time{}
	}
	now := time.Now()
	if len(r.ordinary) > 32768 {
		for ip, expiry := range r.ordinary {
			if now.After(expiry) {
				delete(r.ordinary, ip)
			}
		}
	}
	for _, a := range append(response.Answers, response.Additionals...) {
		var ip netip.Addr
		switch body := a.Body.(type) {
		case *dnsmessage.AResource:
			ip = netip.AddrFrom4(body.A)
		case *dnsmessage.AAAAResource:
			ip = netip.AddrFrom16(body.AAAA)
		}
		if ip.IsValid() && len(r.ordinary) < 65536 {
			r.ordinary[ip] = now.Add(time.Duration(min(a.Header.TTL, 86400)) * time.Second)
		}
	}
	return reply, nil
}
