package main

// DigitalOcean onboarding runs in a private child-process pipe. CloudState
// contains secrets and must only be stored by the GUI in the login keychain.
import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"
)

const cloudOAuth = "https://cloud.digitalocean.com/v1/oauth"
const cloudAPI = "https://api.digitalocean.com/v2"
const cloudScopes = "account:read droplet:read droplet:create regions:read sizes:read actions:read image:read snapshot:read vpc:read"

type CloudPlan struct {
	ID       string  `json:"id"`
	Region   string  `json:"region"`
	Size     string  `json:"size"`
	Monthly  float64 `json:"monthly"`
	Hourly   float64 `json:"hourly"`
	Transfer float64 `json:"transfer"`
	Expires  int64   `json:"expires"`
}
type CloudState struct {
	ClientID          string           `json:"clientID,omitempty"`
	RegistrationToken string           `json:"registrationToken,omitempty"`
	AccessToken       string           `json:"accessToken,omitempty"`
	RefreshToken      string           `json:"refreshToken,omitempty"`
	Expires           int64            `json:"expires,omitempty"`
	AccountID         string           `json:"accountID,omitempty"`
	Plan              *CloudPlan       `json:"plan,omitempty"`
	Deployment        *CloudDeployment `json:"deployment,omitempty"`
}
type CloudDeployment struct {
	Name        string   `json:"name"`
	DropletID   int64    `json:"dropletID"`
	Pending     bool     `json:"pending"`
	Password    string   `json:"password"`
	Certificate string   `json:"certificate"`
	Key         string   `json:"key"`
	Profile     *Profile `json:"profile,omitempty"`
}
type cloudCommand struct {
	Action string      `json:"action"`
	ID     string      `json:"id"`
	State  *CloudState `json:"state,omitempty"`
}
type cloudService struct {
	state      CloudState
	client     *http.Client
	oauth, api string
	save       func(context.Context, CloudState) error
	event      func(any)
	verify     func(context.Context, Profile) error
}

type cloudHTTPError struct{ status int }

func (e cloudHTTPError) Error() string {
	return fmt.Sprintf("DigitalOcean 返回 %d；请检查授权、账户状态或稍后重试", e.status)
}

func cloudRandom(n int) string {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func newCloudService() *cloudService {
	return &cloudService{client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, oauth: cloudOAuth, api: cloudAPI, event: emit, verify: verifyCloudProfile}
}

// HTTP response bodies and URLs are never included in errors: OAuth errors can
// contain credentials or account identifiers. Production endpoints are fixed.
func (c *cloudService) request(ctx context.Context, method, endpoint string, body any, bearer string, result any) error {
	var data []byte
	var err error
	content := "application/json"
	if form, ok := body.(url.Values); ok {
		data = []byte(form.Encode())
		content = "application/x-www-form-urlencoded"
	} else if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return errors.New("无法准备云请求")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(data))
	if err != nil {
		return errors.New("无法准备云请求")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", content)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return errors.New("DigitalOcean 网络请求未完成，请重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return cloudHTTPError{status: resp.StatusCode}
	}
	if result == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2*maxInput+1))
	if err != nil || len(b) > 2*maxInput || json.Unmarshal(b, result) != nil {
		return errors.New("DigitalOcean 响应无效")
	}
	return nil
}
func (c *cloudService) persist(ctx context.Context) error { return c.save(ctx, c.state) }
func cloudPKCE(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func cloudCallback(ctx context.Context, listener net.Listener, state string) (<-chan string, func()) {
	codes := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		q := r.URL.Query()
		if r.Method != "GET" || r.Host != listener.Addr().String() || len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "授权回调无效", http.StatusBadRequest)
			return
		}
		if q.Get("error") != "" {
			select {
			case codes <- "":
			default:
			}
			fmt.Fprint(w, "授权已取消，可返回轻连。")
			return
		}
		if len(q["code"]) != 1 || len(q.Get("code")) < 1 || len(q.Get("code")) > 4096 {
			http.Error(w, "授权码无效", http.StatusBadRequest)
			return
		}
		select {
		case codes <- q.Get("code"):
			fmt.Fprint(w, "授权完成，请返回轻连。")
		default:
			http.Error(w, "授权已处理", http.StatusConflict)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second}
	go server.Serve(listener)
	stop := func() { server.Close() }
	go func() { <-ctx.Done(); stop() }()
	return codes, stop
}

func (c *cloudService) login(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return errors.New("无法开启本机授权回调")
	}
	redirect := "http://" + listener.Addr().String() + "/callback"
	state, verifier := cloudRandom(32), cloudRandom(32)
	codes, stop := cloudCallback(ctx, listener, state)
	defer stop()
	if c.state.ClientID == "" {
		var registered struct {
			ClientID     string `json:"client_id"`
			Registration string `json:"registration_access_token"`
		}
		body := map[string]any{"client_name": "Clearline for macOS", "redirect_uris": []string{redirect}, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}}
		if err = c.request(ctx, "POST", c.oauth+"/register", body, "", &registered); err != nil {
			return err
		}
		if registered.ClientID == "" || registered.Registration == "" {
			return errors.New("DigitalOcean 未返回有效应用注册")
		}
		c.state.ClientID = registered.ClientID
		c.state.RegistrationToken = registered.Registration
		if err = c.persist(ctx); err != nil {
			return err
		}
	}
	q := url.Values{"client_id": {c.state.ClientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {cloudScopes}, "state": {state}, "code_challenge": {cloudPKCE(verifier)}, "code_challenge_method": {"S256"}, "prompt": {"select_account"}}
	c.event(map[string]any{"event": "cloudAuthorize", "url": c.oauth + "/authorize?" + q.Encode()})
	var code string
	select {
	case code = <-codes:
		if code == "" {
			return errors.New("DigitalOcean 授权已取消")
		}
	case <-ctx.Done():
		return errors.New("授权已结束，请重新登录")
	}
	if err = c.token(ctx, url.Values{"grant_type": {"authorization_code"}, "client_id": {c.state.ClientID}, "redirect_uri": {redirect}, "code": {code}, "code_verifier": {verifier}}); err != nil {
		return err
	}
	var account struct {
		Account struct {
			UUID   string `json:"uuid"`
			Status string `json:"status"`
		} `json:"account"`
	}
	if err = c.apiCall(ctx, "GET", "/account", nil, &account); err != nil {
		return err
	}
	if account.Account.UUID == "" || account.Account.Status != "active" {
		return errors.New("DigitalOcean 账户尚未激活，请在官网完成账户和付款方式设置")
	}
	if c.state.AccountID != "" && c.state.AccountID != account.Account.UUID {
		c.state.Deployment = nil
		c.state.Plan = nil
	}
	c.state.AccountID = account.Account.UUID
	if err = c.persist(ctx); err != nil {
		return err
	}
	c.event(map[string]any{"event": "cloudAuthorized"})
	return c.plan(ctx)
}
func (c *cloudService) token(ctx context.Context, form url.Values) error {
	var token struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int64  `json:"expires_in"`
	}
	if err := c.request(ctx, "POST", c.oauth+"/token", form, "", &token); err != nil {
		return err
	}
	if token.Access == "" || token.Refresh == "" || token.Expires <= 0 {
		return errors.New("DigitalOcean 授权返回无效")
	}
	c.state.AccessToken = token.Access
	c.state.RefreshToken = token.Refresh
	c.state.Expires = time.Now().Unix() + token.Expires
	return c.persist(ctx)
}
func (c *cloudService) apiCall(ctx context.Context, method, path string, body, result any) error {
	if c.state.AccessToken == "" {
		return errors.New("请先登录 DigitalOcean")
	}
	if c.state.Expires <= time.Now().Unix()+60 {
		if c.state.RefreshToken == "" {
			return errors.New("授权已过期，请重新登录")
		}
		if err := c.token(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {c.state.ClientID}, "refresh_token": {c.state.RefreshToken}}); err != nil {
			return err
		}
	}
	return c.request(ctx, method, c.api+path, body, c.state.AccessToken, result)
}

type cloudSize struct {
	Slug      string   `json:"slug"`
	Available bool     `json:"available"`
	Regions   []string `json:"regions"`
	Monthly   float64  `json:"price_monthly"`
	Hourly    float64  `json:"price_hourly"`
	Transfer  float64  `json:"transfer"`
	Memory    int      `json:"memory"`
}
type cloudDroplet struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Networks struct {
		V4 []struct {
			IP   string `json:"ip_address"`
			Type string `json:"type"`
		} `json:"v4"`
	} `json:"networks"`
}

func (d cloudDroplet) publicIP() string {
	for _, n := range d.Networks.V4 {
		ip, e := netip.ParseAddr(n.IP)
		if n.Type == "public" && e == nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return ip.String()
		}
	}
	return ""
}
func (c *cloudService) plan(ctx context.Context) error {
	if c.state.Deployment != nil {
		c.event(map[string]any{"event": "cloudExisting", "canResume": true})
		return nil
	}
	var regions struct {
		Regions []struct {
			Slug      string `json:"slug"`
			Available bool   `json:"available"`
		} `json:"regions"`
	}
	if err := c.apiCall(ctx, "GET", "/regions?per_page=200", nil, &regions); err != nil {
		return err
	}
	available := map[string]bool{}
	for _, r := range regions.Regions {
		available[r.Slug] = r.Available
	}
	var sizes struct {
		Sizes []cloudSize `json:"sizes"`
	}
	if err := c.apiCall(ctx, "GET", "/sizes?per_page=200", nil, &sizes); err != nil {
		return err
	}
	sort.Slice(sizes.Sizes, func(i, j int) bool { return sizes.Sizes[i].Monthly < sizes.Sizes[j].Monthly })
	for _, region := range []string{"sfo3", "sfo2", "nyc3", "nyc1"} {
		if !available[region] {
			continue
		}
		for _, size := range sizes.Sizes {
			if !size.Available || !strings.HasPrefix(size.Slug, "s-") || size.Memory < 512 || size.Monthly <= 0 || size.Hourly <= 0 {
				continue
			}
			if !containsString(size.Regions, region) {
				continue
			}
			c.state.Plan = &CloudPlan{ID: cloudRandom(24), Region: region, Size: size.Slug, Monthly: size.Monthly, Hourly: size.Hourly, Transfer: size.Transfer, Expires: time.Now().Add(15 * time.Minute).Unix()}
			if err := c.persist(ctx); err != nil {
				return err
			}
			c.event(map[string]any{"event": "cloudPlan", "plan": c.state.Plan})
			return nil
		}
	}
	return errors.New("当前没有可用的美国基础服务器方案，请稍后重试")
}
func containsString(items []string, s string) bool {
	for _, item := range items {
		if item == s {
			return true
		}
	}
	return false
}

func newCloudDeployment() (*CloudDeployment, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "clearline.internal"}, DNSNames: []string{"clearline.internal"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(2, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	priv, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	nameHash := sha256.Sum256([]byte(cloudRandom(32)))
	return &CloudDeployment{Name: fmt.Sprintf("clearline-%x", nameHash[:6]), Password: cloudRandom(32), Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), Key: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv}))}, nil
}
func (d *CloudDeployment) profile(ip string) Profile {
	u := url.URL{Scheme: "hysteria2", User: url.User(d.Password), Host: net.JoinHostPort(ip, "443")}
	u.RawQuery = url.Values{"sni": {"clearline.internal"}, "ca": {base64.RawURLEncoding.EncodeToString([]byte(d.Certificate))}}.Encode()
	return Profile{ID: d.Name, Name: "我的 DigitalOcean", Kind: "hysteria2", Config: u.String()}
}

func (c *cloudService) deploy(ctx context.Context, confirmation string) error {
	if c.state.Deployment != nil {
		return c.resume(ctx)
	}
	plan := c.state.Plan
	if plan == nil || confirmation != plan.ID || time.Now().Unix() > plan.Expires {
		return errors.New("服务器方案已失效，请重新查看费用并确认")
	}
	var current struct {
		Sizes []cloudSize `json:"sizes"`
	}
	if err := c.apiCall(ctx, "GET", "/sizes?per_page=200", nil, &current); err != nil {
		return err
	}
	var chosen cloudSize
	for _, size := range current.Sizes {
		if size.Slug == plan.Size {
			chosen = size
		}
	}
	if !chosen.Available || !containsString(chosen.Regions, plan.Region) || chosen.Monthly != plan.Monthly || chosen.Hourly != plan.Hourly {
		return errors.New("服务器价格或可用性已变化，请重新查看方案")
	}
	deployment, err := newCloudDeployment()
	if err != nil {
		return errors.New("无法生成服务器凭据")
	}
	deployment.Pending = true
	c.state.Deployment = deployment
	if err = c.persist(ctx); err != nil {
		c.state.Deployment = nil
		return err
	}
	data, err := cloudUserData(deployment)
	if err != nil {
		return err
	}
	c.event(map[string]any{"event": "cloudProgress", "message": "正在创建你的美国服务器"})
	var response struct {
		Droplet cloudDroplet `json:"droplet"`
	}
	// A request of uncertain outcome is never automatically repeated. Its name
	// and credentials are acknowledged in the keychain before this paid call.
	err = c.apiCall(ctx, "POST", "/droplets", map[string]any{"name": deployment.Name, "region": plan.Region, "size": plan.Size, "image": "ubuntu-24-04-x64", "backups": false, "ipv6": false, "monitoring": false, "user_data": data}, &response)
	if err != nil {
		var rejected cloudHTTPError
		if errors.As(err, &rejected) && rejected.status >= 400 && rejected.status < 500 && rejected.status != 408 && rejected.status != 429 {
			c.state.Deployment = nil
			c.state.Plan = nil
			if saveErr := c.persist(ctx); saveErr != nil {
				return saveErr
			}
			return errors.New("DigitalOcean 拒绝了创建请求。请在官网检查授权、付款方式或配额，再刷新方案")
		}
		return errors.New("创建结果尚未确认。请到 DigitalOcean 查看服务器；返回后可继续核对，本应用不会重复创建")
	}
	if response.Droplet.ID <= 0 {
		return errors.New("创建结果缺少服务器编号，请到 DigitalOcean 核对")
	}
	deployment.DropletID = response.Droplet.ID
	deployment.Pending = false
	if err = c.persist(ctx); err != nil {
		return err
	}
	return c.resume(ctx)
}
func (c *cloudService) resume(ctx context.Context) error {
	d := c.state.Deployment
	if d == nil {
		return errors.New("没有待恢复的部署")
	}
	if d.DropletID == 0 {
		// Query every page before deciding that the outcome is unknown.
		for page := 1; page <= 100; page++ {
			var listed struct {
				Droplets []cloudDroplet `json:"droplets"`
			}
			if err := c.apiCall(ctx, "GET", fmt.Sprintf("/droplets?per_page=200&page=%d", page), nil, &listed); err != nil {
				return err
			}
			for _, candidate := range listed.Droplets {
				if candidate.Name == d.Name {
					if d.DropletID != 0 {
						return errors.New("发现同名服务器，请在官网核对")
					}
					d.DropletID = candidate.ID
				}
			}
			if len(listed.Droplets) < 200 {
				break
			}
		}
		if d.DropletID == 0 {
			return errors.New("尚未找到上次部署。请先在 DigitalOcean 核对创建记录；不会再次创建付费服务器")
		}
		d.Pending = false
		if err := c.persist(ctx); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	for {
		var response struct {
			Droplet cloudDroplet `json:"droplet"`
		}
		if err := c.apiCall(ctx, "GET", fmt.Sprintf("/droplets/%d", d.DropletID), nil, &response); err != nil {
			return err
		}
		ip := response.Droplet.publicIP()
		if response.Droplet.Status == "active" && ip != "" {
			p := d.profile(ip)
			c.event(map[string]any{"event": "cloudProgress", "message": "服务器已创建，正在安装服务并验证美国出口"})
			probeCtx, stop := context.WithTimeout(ctx, 12*time.Second)
			err := c.verify(probeCtx, p)
			stop()
			if err == nil {
				d.Profile = &p
				if err = c.persist(ctx); err != nil {
					return err
				}
				c.event(map[string]any{"event": "cloudDeployed", "profile": p})
				return nil
			}
		} else {
			c.event(map[string]any{"event": "cloudProgress", "message": "正在等待服务器启动"})
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return errors.New("服务器验证尚未完成。服务器可能仍在计费，可继续检查或在 DigitalOcean 管理")
		}
	}
}
func verifyCloudProfile(ctx context.Context, p Profile) error {
	t, err := OpenTunnel(ctx, p)
	if err != nil {
		return err
	}
	defer t.Close()
	info, err := verifyExit(ctx, t)
	if err != nil {
		return err
	}
	parsed, err := ParseHY(p.Config)
	if err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(parsed.Server)
	if info.IP != host || info.Country != "US" {
		return errors.New("出口与服务器或美国地区不符")
	}
	return nil
}

func runCloud(input *bufio.Reader) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	line, err := readJSONLine(input)
	if err != nil {
		return err
	}
	var initial cloudCommand
	if json.Unmarshal(line, &initial) != nil {
		return errors.New("云配置格式无效")
	}
	service := newCloudService()
	if initial.State != nil {
		service.state = *initial.State
	}
	commands := make(chan cloudCommand, 8)
	go func() {
		defer cancel()
		for {
			line, e := readJSONLine(input)
			if e != nil {
				return
			}
			var command cloudCommand
			if json.Unmarshal(line, &command) != nil {
				continue
			}
			if command.Action == "cancel" {
				return
			}
			select {
			case commands <- command:
			case <-ctx.Done():
				return
			}
		}
	}()
	service.save = func(ctx context.Context, state CloudState) error {
		id := cloudRandom(16)
		emit(map[string]any{"event": "cloudState", "id": id, "state": state})
		select {
		case command := <-commands:
			if command.Action == "stored" && command.ID == id {
				return nil
			}
			return errors.New("钥匙串保存未确认，操作已停止")
		case <-time.After(30 * time.Second):
			return errors.New("钥匙串保存超时，操作已停止")
		case <-ctx.Done():
			return errors.New("云配置操作已停止")
		}
	}
	emit(map[string]any{"event": "cloudReady", "authorized": service.state.AccessToken != "", "hasDeployment": service.state.Deployment != nil})
	for {
		select {
		case <-ctx.Done():
			return nil
		case command := <-commands:
			var err error
			switch command.Action {
			case "login":
				err = service.login(ctx)
			case "plan":
				err = service.plan(ctx)
			case "deploy":
				err = service.deploy(ctx, command.ID)
			case "resume":
				err = service.resume(ctx)
			default:
				err = errors.New("云操作无效")
			}
			if err != nil {
				emit(map[string]any{"event": "cloudError", "message": err.Error(), "hasDeployment": service.state.Deployment != nil})
			}
		}
	}
}
