package main

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCloudPKCERFC7636(t *testing.T) {
	if cloudPKCE("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk") != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatal("PKCE S256 mismatch")
	}
}
func TestCloudCallbackRejectsForgery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	codes, stop := cloudCallback(ctx, listener, "secret-state")
	defer stop()
	endpoint := "http://" + listener.Addr().String() + "/callback"
	for _, bad := range []string{"?code=a&state=forged", "?code=a&state=secret-state&state=forged", "?code=a&code=b&state=secret-state"} {
		r, e := http.Get(endpoint + bad)
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != 400 {
			t.Fatal("forged callback accepted")
		}
	}
	req, _ := http.NewRequest("GET", endpoint+"?code=a&state=secret-state", nil)
	req.Host = "attacker.example"
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 400 {
		t.Fatal("invalid host accepted")
	}
	select {
	case <-codes:
		t.Fatal("forged callback delivered a code")
	default:
	}
	r, err = http.Get(endpoint + "?code=valid&state=secret-state")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	select {
	case code := <-codes:
		if code != "valid" {
			t.Fatal("wrong code")
		}
	case <-time.After(time.Second):
		t.Fatal("missing valid code")
	}
}

func cloudTestService(t *testing.T, handler http.HandlerFunc) *cloudService {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := newCloudService()
	// Legacy deployment fixtures represent an explicitly chosen create flow.
	c.intent = "create"
	c.api = server.URL + "/v2"
	c.oauth = server.URL + "/oauth"
	c.client = server.Client()
	c.state = CloudState{ClientID: "test-client", AccessToken: "test-access", RefreshToken: "test-refresh", Expires: time.Now().Add(time.Hour).Unix(), AccountID: "test-account"}
	c.state.Scopes = cloudScopes
	c.save = func(context.Context, CloudState) error { return nil }
	c.event = func(any) {}
	c.verify = func(context.Context, Profile) error { return nil }
	return c
}
func writeCloudSizes(w http.ResponseWriter, price float64) {
	fmt.Fprintf(w, `{"sizes":[{"slug":"s-1vcpu-1gb","available":true,"regions":["sfo3"],"memory":1024,"price_monthly":%f,"price_hourly":0.01,"transfer":1}]}`, price)
}
func TestCloudAuthorizationEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var redirect, challenge string
	var publicRegistration bool
	c := cloudTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/register":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			publicRegistration = body["token_endpoint_auth_method"] == "none" && body["client_secret"] == nil
			mu.Unlock()
			fmt.Fprint(w, `{"client_id":"native-client","registration_access_token":"private-registration"}`)
		case "/oauth/token":
			r.ParseForm()
			mu.Lock()
			defer mu.Unlock()
			if r.Form.Get("client_secret") != "" || cloudPKCE(r.Form.Get("code_verifier")) != challenge || r.Form.Get("redirect_uri") != redirect || r.Form.Get("code") != "owner-code" {
				http.Error(w, "invalid PKCE", 400)
				return
			}
			fmt.Fprint(w, `{"access_token":"private-access","refresh_token":"private-refresh","expires_in":3600}`)
		case "/v2/account":
			fmt.Fprint(w, `{"account":{"uuid":"owner-account","status":"active"}}`)
		case "/v2/regions":
			fmt.Fprint(w, `{"regions":[{"slug":"sfo3","available":true}]}`)
		case "/v2/sizes":
			writeCloudSizes(w, 6)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.Error(w, "unexpected", 404)
		}
	})
	c.state = CloudState{}
	var saved []CloudState
	c.save = func(_ context.Context, state CloudState) error { saved = append(saved, state); return nil }
	callbackDone := make(chan error, 1)
	c.event = func(raw any) {
		e := raw.(map[string]any)
		if e["event"] != "cloudAuthorize" {
			return
		}
		u, _ := url.Parse(e["url"].(string))
		q := u.Query()
		mu.Lock()
		redirect = q.Get("redirect_uri")
		challenge = q.Get("code_challenge")
		mu.Unlock()
		if q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" || strings.Contains(q.Get("scope"), "delete") {
			t.Error("invalid native authorization")
		}
		go func() {
			r, err := http.Get(q.Get("redirect_uri") + "?" + url.Values{"code": {"owner-code"}, "state": {q.Get("state")}}.Encode())
			if err == nil {
				r.Body.Close()
			}
			callbackDone <- err
		}()
	}
	if err := c.login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-callbackDone; err != nil {
		t.Fatal(err)
	}
	if !publicRegistration || len(saved) < 4 || c.state.AccountID != "owner-account" || c.state.Plan == nil {
		t.Fatal("native onboarding incomplete")
	}
}

func TestCloudAuthorizationRefreshesRegisteredCallback(t *testing.T) {
	var mu sync.Mutex
	registeredRedirect := "http://127.0.0.1:1/callback"
	var challenge string
	var updated bool
	c := cloudTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/register/test-client":
			var body struct {
				ClientID  string   `json:"client_id"`
				Redirects []string `json:"redirect_uris"`
			}
			if r.Method != http.MethodPut || r.Header.Get("Authorization") != "Bearer test-registration" || json.NewDecoder(r.Body).Decode(&body) != nil || body.ClientID != "test-client" || len(body.Redirects) != 1 {
				http.Error(w, "invalid registration update", 400)
				return
			}
			mu.Lock()
			registeredRedirect, updated = body.Redirects[0], true
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"client_id": "test-client", "redirect_uris": body.Redirects})
		case "/oauth/token":
			r.ParseForm()
			mu.Lock()
			valid := r.Form.Get("redirect_uri") == registeredRedirect && cloudPKCE(r.Form.Get("code_verifier")) == challenge && r.Form.Get("code") == "owner-code"
			mu.Unlock()
			if !valid {
				http.Error(w, "invalid callback or PKCE", 400)
				return
			}
			fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
		case "/v2/account":
			fmt.Fprint(w, `{"account":{"uuid":"test-account","status":"active"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 404)
		}
	})
	c.state.RegistrationToken = "test-registration"
	deployment := &CloudDeployment{Name: "existing-deployment", DropletID: 42}
	c.state.Deployment = deployment
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	callbackDone := make(chan error, 1)
	c.event = func(raw any) {
		e := raw.(map[string]any)
		if e["event"] != "cloudAuthorize" {
			return
		}
		u, _ := url.Parse(e["url"].(string))
		q := u.Query()
		mu.Lock()
		redirect := registeredRedirect
		ready := updated && redirect == q.Get("redirect_uri")
		challenge = q.Get("code_challenge")
		mu.Unlock()
		if !ready {
			t.Error("authorization opened before registered callback matched the active listener")
			cancel()
			return
		}
		if c.state.AccessToken != "test-access" || c.state.RefreshToken != "test-refresh" || c.state.Deployment != deployment {
			t.Error("existing authorization or deployment changed before owner consent")
		}
		go func() {
			response, err := http.Get(redirect + "?" + url.Values{"code": {"owner-code"}, "state": {q.Get("state")}}.Encode())
			if err == nil {
				response.Body.Close()
			}
			callbackDone <- err
		}()
	}
	if err := c.login(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-callbackDone; err != nil {
		t.Fatal(err)
	}
	if c.state.ClientID != "test-client" || c.state.RegistrationToken != "test-registration" || c.state.Deployment != deployment || c.state.AccessToken != "new-access" {
		t.Fatal("reauthorization did not preserve the existing client and deployment")
	}
}

func TestCloudAuthorizationStopsWhenCallbackUpdateFails(t *testing.T) {
	for _, response := range []struct {
		name   string
		status int
		body   string
	}{
		{"provider failure", 503, `{}`},
		{"stale callback", 200, `{"client_id":"test-client","redirect_uris":["http://127.0.0.1:1/callback"]}`},
		{"wrong client", 200, `{"client_id":"other-client","redirect_uris":[]}`},
	} {
		t.Run(response.name, func(t *testing.T) {
			c := cloudTestService(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.URL.Path != "/oauth/register/test-client" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(response.status)
				fmt.Fprint(w, response.body)
			})
			c.state.RegistrationToken = "test-registration"
			opened := false
			c.event = func(raw any) {
				if raw.(map[string]any)["event"] == "cloudAuthorize" {
					opened = true
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if c.login(ctx) == nil || opened || c.state.AccessToken != "test-access" || c.state.RefreshToken != "test-refresh" {
				t.Fatal("failed callback update opened authorization or changed existing credentials")
			}
		})
	}
}

func TestCloudPlanReadsOnlyAndPrefersUS(t *testing.T) {
	var mutated bool
	c := cloudTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutated = true
		}
		if strings.HasSuffix(r.URL.Path, "regions") {
			fmt.Fprint(w, `{"regions":[{"slug":"sfo3","available":true}]}`)
		} else {
			writeCloudSizes(w, 6)
		}
	})
	if err := c.plan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mutated || c.state.Plan == nil || c.state.Plan.Region != "sfo3" || c.state.Deployment != nil {
		t.Fatal("planning changed cloud resources")
	}
}
func TestCloudDeployRequiresCurrentConfirmation(t *testing.T) {
	c := cloudTestService(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unconfirmed request reached provider") })
	c.state.Plan = &CloudPlan{ID: "approved", Expires: time.Now().Add(time.Minute).Unix()}
	if c.deploy(context.Background(), "wrong") == nil {
		t.Fatal("unconfirmed deployment allowed")
	}
	c.state.Plan.Expires = time.Now().Add(-time.Minute).Unix()
	if c.deploy(context.Background(), "approved") == nil {
		t.Fatal("expired approval allowed")
	}
}
func TestCloudChangedPriceStopsBeforeCreation(t *testing.T) {
	posts := 0
	c := cloudTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
		}
		writeCloudSizes(w, 7)
	})
	c.state.Plan = &CloudPlan{ID: "approved", Region: "sfo3", Size: "s-1vcpu-1gb", Monthly: 6, Hourly: .01, Expires: time.Now().Add(time.Minute).Unix()}
	if c.deploy(context.Background(), "approved") == nil || posts != 0 || c.state.Deployment != nil {
		t.Fatal("changed price was deployed")
	}
}
func TestCloudKeychainFailurePreventsPaidCall(t *testing.T) {
	posts := 0
	c := cloudTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
		}
		writeCloudSizes(w, 6)
	})
	c.state.Plan = &CloudPlan{ID: "approved", Region: "sfo3", Size: "s-1vcpu-1gb", Monthly: 6, Hourly: .01, Expires: time.Now().Add(time.Minute).Unix()}
	c.save = func(context.Context, CloudState) error { return errors.New("keychain denied") }
	if c.deploy(context.Background(), "approved") == nil || posts != 0 {
		t.Fatal("paid request preceded durable state")
	}
}
func TestCloudAmbiguousCreateDoesNotRepeat(t *testing.T) {
	posts := 0
	c := cloudTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		if r.URL.Path == "/v2/sizes" {
			writeCloudSizes(w, 6)
		} else {
			fmt.Fprint(w, `{"droplets":[]}`)
		}
	})
	c.state.Plan = &CloudPlan{ID: "approved", Region: "sfo3", Size: "s-1vcpu-1gb", Monthly: 6, Hourly: .01, Expires: time.Now().Add(time.Minute).Unix()}
	if c.deploy(context.Background(), "approved") == nil || c.state.Deployment == nil || !c.state.Deployment.Pending {
		t.Fatal("uncertain create was forgotten")
	}
	if c.deploy(context.Background(), "approved") == nil || posts != 1 {
		t.Fatal("duplicate paid server was attempted")
	}
}
func TestCloudRejectedCreateCanReturnToFreshPlan(t *testing.T) {
	posts := 0
	c := cloudTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
			http.Error(w, "payment rejected", 402)
			return
		}
		writeCloudSizes(w, 6)
	})
	c.state.Plan = &CloudPlan{ID: "approved", Region: "sfo3", Size: "s-1vcpu-1gb", Monthly: 6, Hourly: .01, Expires: time.Now().Add(time.Minute).Unix()}
	if c.deploy(context.Background(), "approved") == nil || posts != 1 || c.state.Deployment != nil || c.state.Plan != nil {
		t.Fatal("definitive rejection prevented safe replanning")
	}
}
func TestCloudResumeFindsOwnedDeploymentWithoutCreating(t *testing.T) {
	d, err := newCloudDeployment()
	if err != nil {
		t.Fatal(err)
	}
	d.Pending = true
	c := cloudTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("resume attempted mutation")
		}
		if r.URL.Path == "/v2/droplets" {
			fmt.Fprintf(w, `{"droplets":[{"id":123,"name":%q}]}`, d.Name)
		} else {
			fmt.Fprint(w, `{"droplet":{"id":123,"status":"active","networks":{"v4":[{"ip_address":"203.0.113.10","type":"public"}]}}}`)
		}
	})
	c.state.Deployment = d
	if err := c.resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.DropletID != 123 || d.Profile == nil || d.Pending {
		t.Fatal("deployment not recovered")
	}
	parsed, err := ParseHY(d.Profile.Config)
	if err != nil || parsed.SNI != "clearline.internal" || parsed.Auth != d.Password || parsed.CA != d.Certificate {
		t.Fatal("invalid private profile")
	}
}
func TestCloudCertificatesAndBootstrap(t *testing.T) {
	d, err := newCloudDeployment()
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(d.Certificate))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "clearline.internal", Roots: pool}); err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "evil.example", Roots: pool}); err == nil {
		t.Fatal("hostname verification bypassed")
	}
	data, err := cloudUserData(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 64*1024 || strings.Contains(data, d.Password) || !strings.HasPrefix(data, "#cloud-config") {
		t.Fatal("invalid cloud-init")
	}
	count := 0
	for _, line := range strings.Split(data, "\n") {
		if !strings.HasPrefix(line, "    content: ") {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "    content: "))
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(string(b), "#!/bin/bash") && (!strings.Contains(string(b), hysteriaServerSHA) || strings.Contains(string(b), "set -x")) {
			t.Fatal("unsafe bootstrap download")
		}
		count++
	}
	if count != 5 {
		t.Fatal("bootstrap files missing")
	}
}
func TestCloudHTTPDoesNotFollowTokenRedirects(t *testing.T) {
	var leaked bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	c := newCloudService()
	var result any
	if c.request(context.Background(), "POST", redirect.URL, url.Values{"refresh_token": {"private"}}, "private", &result) == nil || leaked {
		t.Fatal("credential redirect allowed")
	}
}
func TestCloudRefreshRotatesAndPersists(t *testing.T) {
	persisted := false
	c := cloudTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			r.ParseForm()
			if r.Form.Get("refresh_token") != "test-refresh" {
				t.Error("incorrect refresh token")
			}
			fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
			return
		}
		if !persisted || r.Header.Get("Authorization") != "Bearer new-access" {
			t.Error("refreshed token used before persistence")
		}
		io.WriteString(w, `{}`)
	})
	c.state.Expires = 1
	c.save = func(_ context.Context, s CloudState) error { persisted = s.RefreshToken == "new-refresh"; return nil }
	var out any
	if err := c.apiCall(context.Background(), "GET", "/account", nil, &out); err != nil {
		t.Fatal(err)
	}
}
