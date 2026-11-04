package main

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

const maxInput = 1024 * 1024

type Profile struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Config string `json:"config"`
}

type AWGConfig struct {
	Addresses []netip.Addr
	DNS       []netip.Addr
	MTU       int
	Endpoint  string
	IPC       string
}

// ParseAWG intentionally accepts data only. Shell hooks and multiple peers are
// rejected; an imported WireGuard file can never execute commands on this Mac.
func ParseAWG(raw string) (AWGConfig, error) {
	c := AWGConfig{MTU: 1380}
	if len(raw) > maxInput {
		return c, errors.New("配置过大")
	}
	var section string
	var peers int
	var private, public, psk, endpoint string
	var params []string
	var allowed []netip.Prefix
	keepalive := "25"
	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 65536)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			switch strings.ToLower(line) {
			case "[interface]":
				section = "interface"
			case "[peer]":
				section = "peer"
				peers++
			default:
				return c, errors.New("配置包含未知分段")
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || section == "" {
			return c, errors.New("配置格式不正确")
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		// A value is a single UAPI line, never a source of new UAPI commands.
		if strings.ContainsAny(v, "\r\n\x00") {
			return c, errors.New("配置含非法字符")
		}
		if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		key := section + "/" + k
		if seen[key] {
			return c, fmt.Errorf("重复配置项 %s", k)
		}
		seen[key] = true
		if section == "interface" {
			switch k {
			case "privatekey":
				private = v
			case "address":
				for _, a := range strings.Split(v, ",") {
					p, e := netip.ParsePrefix(strings.TrimSpace(a))
					if e != nil {
						return c, errors.New("隧道地址无效")
					}
					c.Addresses = append(c.Addresses, p.Addr())
				}
			case "dns":
				for _, a := range strings.Split(v, ",") {
					ip, e := netip.ParseAddr(strings.TrimSpace(a))
					if e != nil {
						return c, errors.New("DNS 需要填写 IP 地址")
					}
					c.DNS = append(c.DNS, ip)
				}
			case "mtu":
				n, e := strconv.Atoi(v)
				if e != nil || n < 1280 || n > 1500 {
					return c, errors.New("MTU 需在 1280–1500 之间")
				}
				c.MTU = n
			case "listenport": // local ephemeral UDP ports are managed by the core
			case "jc", "jmin", "jmax", "s1", "s2", "s3", "s4", "h1", "h2", "h3", "h4", "i1", "i2", "i3", "i4", "i5":
				if len(v) > 4096 {
					return c, errors.New("混淆参数过长")
				}
				params = append(params, k+"="+v)
			default:
				return c, fmt.Errorf("不支持配置项 %s；请导出原生连接配置，移除脚本或路由指令", k)
			}
		} else {
			switch k {
			case "publickey":
				public = v
			case "presharedkey":
				psk = v
			case "endpoint":
				endpoint = v
			case "allowedips":
				for _, a := range strings.Split(v, ",") {
					p, e := netip.ParsePrefix(strings.TrimSpace(a))
					if e != nil {
						return c, errors.New("AllowedIPs 无效")
					}
					allowed = append(allowed, p)
				}
			case "persistentkeepalive":
				n, e := strconv.Atoi(v)
				if e != nil || n < 0 || n > 65535 {
					return c, errors.New("Keepalive 无效")
				}
				keepalive = strconv.Itoa(n)
			default:
				return c, fmt.Errorf("未知 Peer 配置项 %s", k)
			}
		}
	}
	if e := scanner.Err(); e != nil {
		return c, errors.New("配置行过长")
	}
	if peers != 1 || len(c.Addresses) == 0 || private == "" || public == "" || endpoint == "" || len(allowed) == 0 {
		return c, errors.New("需要一个完整的 Interface 和一个 Peer")
	}
	keyhex := func(s string) (string, error) {
		b, e := base64.StdEncoding.DecodeString(s)
		if e != nil || len(b) != 32 {
			return "", errors.New("VPN 密钥格式无效")
		}
		return hex.EncodeToString(b), nil
	}
	sk, e := keyhex(private)
	if e != nil {
		return c, e
	}
	pk, e := keyhex(public)
	if e != nil {
		return c, e
	}
	if _, port, e := net.SplitHostPort(endpoint); e != nil {
		return c, errors.New("服务器地址需包含端口")
	} else {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return c, errors.New("服务器端口无效")
		}
	}
	c.Endpoint = endpoint
	c.IPC = "private_key=" + sk + "\n"
	if len(params) > 0 {
		c.IPC += strings.Join(params, "\n") + "\n"
	}
	c.IPC += "replace_peers=true\npublic_key=" + pk + "\n"
	if psk != "" {
		x, e := keyhex(psk)
		if e != nil {
			return c, e
		}
		c.IPC += "preshared_key=" + x + "\n"
	}
	for _, p := range allowed {
		c.IPC += "allowed_ip=" + p.String() + "\n"
	}
	c.IPC += "persistent_keepalive_interval=" + keepalive + "\n"
	if len(c.DNS) == 0 {
		c.DNS = []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	}
	return c, nil
}

type HYConfig struct {
	Server string `json:"server"`
	Auth   string `json:"auth"`
	SNI    string `json:"sni,omitempty"`
	CA     string `json:"ca,omitempty"`
	Obfs   string `json:"obfs,omitempty"`
}

func ParseHY(raw string) (HYConfig, error) {
	var c HYConfig
	u, e := url.Parse(strings.TrimSpace(raw))
	if e != nil || (u.Scheme != "hy2" && u.Scheme != "hysteria2") {
		return c, errors.New("需要 hysteria2:// 或 hy2:// 连接链接")
	}
	if u.Hostname() == "" || u.User == nil {
		return c, errors.New("Hysteria 2 链接缺少服务器或密码")
	}
	q := u.Query()
	if q.Get("insecure") == "1" || q.Get("insecure") == "true" {
		return c, errors.New("该链接关闭了证书校验；请使用有效证书或可信 CA 配置")
	}
	if q.Get("pinSHA256") != "" {
		return c, errors.New("暂不支持指纹链接，请使用有效证书或 CA 配置")
	}
	if q.Get("obfs") != "" && q.Get("obfs") != "salamander" {
		return c, errors.New("不支持该混淆方式")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 1 || n > 65535 {
		return c, errors.New("服务器端口无效")
	}
	c.Server = net.JoinHostPort(u.Hostname(), port)
	c.Auth = u.User.Username()
	if p, ok := u.User.Password(); ok {
		c.Auth += ":" + p
	}
	c.SNI = q.Get("sni")
	if c.SNI == "" {
		c.SNI = u.Hostname()
	}
	c.Obfs = q.Get("obfs-password")
	if encoded := q.Get("ca"); encoded != "" {
		b, e := base64.RawURLEncoding.DecodeString(encoded)
		if e != nil || len(b) > 65536 {
			return c, errors.New("CA 证书格式无效")
		}
		c.CA = string(b)
	}
	if q.Get("obfs") == "salamander" && c.Obfs == "" {
		return c, errors.New("缺少 Salamander 密码")
	}
	return c, nil
}

func ValidateProfile(p Profile) error {
	if len(p.Name) > 120 || len(p.Config) > maxInput {
		return errors.New("配置过大")
	}
	switch p.Kind {
	case "awg":
		_, e := ParseAWG(p.Config)
		return e
	case "hysteria2":
		_, e := ParseHY(p.Config)
		return e
	default:
		return errors.New("支持 AmneziaWG / WireGuard 配置和 Hysteria 2 链接")
	}
}
