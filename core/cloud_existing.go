package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// The reader only reads the selected server's existing Hysteria configuration.
// Its output is encrypted to a one-time key held by this private GUI process.
//
//go:embed pairing_reader.py
var existingReader string

type existingPairing struct {
	key       *rsa.PrivateKey
	challenge string
	serverID  int64
	host      string
	name      string
	expires   time.Time
}

type existingServer struct {
	ID      int64   `json:"id"`
	Name    string  `json:"name"`
	Status  string  `json:"status"`
	Region  string  `json:"region"`
	Memory  int     `json:"memory"`
	Disk    int     `json:"disk"`
	Monthly float64 `json:"monthly"`
	CanRead bool    `json:"canRead"`
}

func (c *cloudService) canCreate() bool {
	for _, scope := range strings.Fields(c.state.Scopes) {
		if scope == "droplet:create" {
			return true
		}
	}
	return false
}

func (c *cloudService) listExisting(ctx context.Context) error {
	servers := make([]existingServer, 0)
	for page := 1; page <= 10; page++ {
		var response struct {
			Droplets []cloudDroplet `json:"droplets"`
			Meta     struct {
				Total int `json:"total"`
			} `json:"meta"`
		}
		if err := c.apiCall(ctx, "GET", fmt.Sprintf("/droplets?per_page=200&page=%d", page), nil, &response); err != nil {
			return err
		}
		for _, d := range response.Droplets {
			servers = append(servers, existingServer{ID: d.ID, Name: d.Name, Status: d.Status, Region: d.Region.Slug, Memory: d.Memory, Disk: d.Disk, Monthly: d.Size.Monthly, CanRead: d.Status == "active" && d.publicIP() != ""})
		}
		if len(response.Droplets) < 200 || (response.Meta.Total > 0 && len(servers) >= response.Meta.Total) {
			sort.SliceStable(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
			c.event(map[string]any{"event": "cloudServers", "servers": servers})
			return nil
		}
	}
	return errors.New("服务器列表过长，请缩小账号资源范围后重试")
}

func (c *cloudService) selectedExisting(ctx context.Context, id int64) (cloudDroplet, error) {
	var response struct {
		Droplet cloudDroplet `json:"droplet"`
	}
	if id <= 0 {
		return response.Droplet, errors.New("请先选择已有服务器")
	}
	if err := c.apiCall(ctx, "GET", fmt.Sprintf("/droplets/%d", id), nil, &response); err != nil {
		return response.Droplet, err
	}
	d := response.Droplet
	if d.ID != id || d.Status != "active" || d.publicIP() == "" {
		return d, errors.New("这台服务器暂不可接入，请确认已启动且具有公网地址")
	}
	return d, nil
}

func (c *cloudService) prepareExisting(ctx context.Context, id int64) error {
	c.pairing = nil
	d, err := c.selectedExisting(ctx, id)
	if err != nil {
		return err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return errors.New("无法生成本机配对信息")
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return errors.New("无法生成本机配对信息")
	}
	pairing := &existingPairing{key: key, challenge: cloudRandom(32), serverID: id, host: d.publicIP(), name: d.Name, expires: time.Now().Add(15 * time.Minute)}
	c.pairing = pairing
	config, _ := json.Marshal(map[string]any{"publicKey": base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public})), "challenge": pairing.challenge, "serverID": id, "host": pairing.host, "name": pairing.name})
	script := existingReader + "\nmain(json.loads(base64.b64decode('" + base64.StdEncoding.EncodeToString(config) + "')))\n"
	c.event(map[string]any{"event": "cloudProgress", "message": "正在尝试本机已信任的 SSH 连接，只读取现有配置…"})
	if c.sshRead != nil {
		if code, readErr := c.sshRead(ctx, pairing.host, script); readErr == nil {
			if err = c.acceptExisting(ctx, code); err == nil {
				return nil
			}
			if c.pairing == nil {
				return err
			}
		}
	}
	// No host-key bypass, password reset, key installation or service mutation.
	command := "python3 - <<'CLEARLINE_READ_ONLY'\n" + script + "CLEARLINE_READ_ONLY"
	console := fmt.Sprintf("https://cloud.digitalocean.com/droplets/%d/terminal/ui/?os_user=root", id)
	c.event(map[string]any{"event": "cloudPairing", "command": command, "consoleURL": console, "expires": pairing.expires.Unix()})
	return nil
}

func readExistingSSH(parent context.Context, host, script string) (string, error) {
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return "", errors.New("服务器地址无效")
	}
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/ssh", "-T", "-oBatchMode=yes", "-oStrictHostKeyChecking=yes", "-oPasswordAuthentication=no", "-oKbdInteractiveAuthentication=no", "-oForwardAgent=no", "-oClearAllForwardings=yes", "-oPermitLocalCommand=no", "-oConnectTimeout=5", "-oConnectionAttempts=1", "root@"+host, "python3 -")
	cmd.Stdin = strings.NewReader(script)
	var output boundedPairingOutput
	cmd.Stdout = &output
	if cmd.Run() != nil {
		return "", errors.New("本机尚无可用的已信任 SSH 连接")
	}
	return output.String(), nil
}

type boundedPairingOutput struct{ bytes.Buffer }

func (b *boundedPairingOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 128*1024 {
		return 0, errors.New("配对输出过大")
	}
	return b.Buffer.Write(p)
}

func pairingEnvelope(code string) (map[string]string, error) {
	const prefix = "CLEARLINE1:"
	if len(code) > 128*1024 {
		return nil, errors.New("连接码过大")
	}
	start := strings.LastIndex(code, prefix)
	if start >= 0 {
		tail := code[start+len(prefix):]
		if end := strings.Index(tail, ":END"); end >= 0 {
			encoded := strings.Join(strings.Fields(tail[:end]), "")
			raw, err := base64.RawURLEncoding.DecodeString(encoded)
			var envelope map[string]string
			if err == nil && len(raw) <= 96*1024 && json.Unmarshal(raw, &envelope) == nil {
				return envelope, nil
			}
		}
	}
	const failure = "CLEARLINE_READ_ERROR:"
	if index := strings.LastIndex(code, failure); index >= 0 {
		fields := strings.Fields(code[index+len(failure):])
		if len(fields) > 0 {
			messages := map[string]string{
				"CONFIG_NOT_FOUND":     "未找到现有 Hysteria 2 配置，可改用已有连接或加密文件导入",
				"MISSING_CRYPTOGRAPHY": "服务器缺少加密读取组件，本次未修改服务器，可改用配置导入",
				"INVALID_CERTIFICATE":  "现有服务的证书身份不完整，请先检查服务器证书",
				"UNSUPPORTED_AUTH":     "现有认证方式暂不支持直接读取，可改用配置导入",
			}
			if message, ok := messages[fields[0]]; ok {
				return nil, errors.New(message)
			}
		}
		return nil, errors.New("服务器未能读取已有连接，本次未修改服务")
	}
	return nil, errors.New("请粘贴控制台返回的完整连接码")
}

func (c *cloudService) acceptExisting(ctx context.Context, code string) error {
	p := c.pairing
	if p == nil || time.Now().After(p.expires) {
		c.pairing = nil
		return errors.New("配对已结束，请重新读取已有连接")
	}
	envelope, err := pairingEnvelope(code)
	if err != nil {
		return err
	}
	decode := func(name string) []byte { b, _ := base64.StdEncoding.DecodeString(envelope[name]); return b }
	key, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, p.key, decode("key"), nil)
	if err != nil || len(key) != 32 {
		return errors.New("连接码不属于本次配对，请使用当前控制台返回的连接码")
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := decode("nonce")
	if len(nonce) != gcm.NonceSize() {
		return errors.New("连接码校验失败")
	}
	plain, err := gcm.Open(nil, nonce, decode("data"), []byte("clearline-existing-server-v1"))
	if err != nil {
		return errors.New("连接码校验失败")
	}
	var payload struct {
		Challenge string  `json:"challenge"`
		ServerID  int64   `json:"serverID"`
		Profile   Profile `json:"profile"`
	}
	if json.Unmarshal(plain, &payload) != nil || subtle.ConstantTimeCompare([]byte(payload.Challenge), []byte(p.challenge)) != 1 || payload.ServerID != p.serverID {
		return errors.New("连接码与所选服务器不匹配")
	}
	profile := payload.Profile
	hy, err := ParseHY(profile.Config)
	endpoint, parseErr := url.Parse(profile.Config)
	if err != nil || parseErr != nil || profile.Kind != "hysteria2" || endpoint.Hostname() != p.host || hy.CA == "" || ValidateProfile(profile) != nil {
		return errors.New("已有连接的地址或证书无效，未保存")
	}
	roots := x509.NewCertPool()
	certPEM, _ := pem.Decode([]byte(hy.CA))
	if certPEM == nil || !roots.AppendCertsFromPEM([]byte(hy.CA)) {
		return errors.New("已有服务器证书无效")
	}
	cert, err := x509.ParseCertificate(certPEM.Bytes)
	if err != nil {
		return errors.New("已有服务器证书无效")
	}
	if _, err = cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: hy.SNI}); err != nil {
		return errors.New("已有服务器证书身份或有效期不正确")
	}
	current, err := c.selectedExisting(ctx, p.serverID)
	if err != nil {
		return err
	}
	if current.publicIP() != p.host {
		c.pairing = nil
		return errors.New("服务器地址已变化，请重新读取连接")
	}
	profile.ID = cloudRandom(16)
	profile.Name = current.Name
	c.pairing = nil
	c.event(map[string]any{"event": "cloudExistingProfile", "profile": profile})
	return nil
}
