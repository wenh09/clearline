package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func existingFixture(w http.ResponseWriter) {
	fmt.Fprint(w, `{"droplet":{"id":42,"name":"existing-server","status":"active","memory":512,"disk":10,"region":{"slug":"sfo2"},"size":{"price_monthly":4},"networks":{"v4":[{"type":"public","ip_address":"192.0.2.10"}]}}}`)
}
func existingService(t *testing.T) *cloudService {
	c := cloudTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v2/droplets/42" {
			t.Errorf("unexpected provider mutation or endpoint: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 400)
			return
		}
		existingFixture(w)
	})
	c.intent = "existing"
	c.state.Scopes = cloudReadScopes
	c.sshRead = func(context.Context, string, string) (string, error) { return "", errors.New("no existing SSH trust") }
	return c
}
func existingCertificate(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("192.0.2.10")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
func existingProfile(t *testing.T) Profile {
	q := url.Values{"sni": {"192.0.2.10"}, "ca": {base64.RawURLEncoding.EncodeToString([]byte(existingCertificate(t)))}}
	return Profile{Kind: "hysteria2", Name: "fixture", Config: "hysteria2://fixture-user:fixture-password@192.0.2.10:443/?" + q.Encode()}
}
func encryptExistingCode(t *testing.T, p *existingPairing, profile Profile, challenge string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"challenge": challenge, "serverID": p.serverID, "profile": profile})
	key := make([]byte, 32)
	rand.Read(key)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	data := gcm.Seal(nil, nonce, raw, []byte("clearline-existing-server-v1"))
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &p.key.PublicKey, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(map[string]string{"key": base64.StdEncoding.EncodeToString(wrapped), "nonce": base64.StdEncoding.EncodeToString(nonce), "data": base64.StdEncoding.EncodeToString(data)})
	return "CLEARLINE1:" + base64.RawURLEncoding.EncodeToString(encoded) + ":END"
}

func TestExistingServerListPreservesSmallCurrentPlanAndOnlyReads(t *testing.T) {
	c := cloudTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v2/droplets" {
			t.Errorf("unexpected provider mutation: %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"droplets":[{"id":42,"name":"existing-server","status":"active","memory":512,"disk":10,"region":{"slug":"sfo2"},"size":{"price_monthly":4},"networks":{"v4":[{"type":"public","ip_address":"192.0.2.10"}]}}],"meta":{"total":1}}`)
	})
	var servers []existingServer
	c.event = func(raw any) {
		e := raw.(map[string]any)
		if e["event"] == "cloudServers" {
			servers = e["servers"].([]existingServer)
		}
	}
	if err := c.listExisting(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Memory != 512 || servers[0].Monthly != 4 || !servers[0].CanRead {
		t.Fatal("existing plan was filtered, replaced or repriced")
	}
}
func TestExistingServerListEmptyAndPagination(t *testing.T) {
	for _, total := range []int{0, 201} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			calls := 0
			c := cloudTestService(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				page := r.URL.Query().Get("page")
				var items []map[string]any
				count := total
				if total > 200 {
					count = 200
					if page == "2" {
						count = 1
					}
				}
				for i := 0; i < count; i++ {
					items = append(items, map[string]any{"id": i + 1, "name": fmt.Sprintf("fixture-%d-%d", calls, i), "status": "off"})
				}
				if items == nil {
					items = []map[string]any{}
				}
				json.NewEncoder(w).Encode(map[string]any{"droplets": items, "meta": map[string]int{"total": total}})
			})
			var list []existingServer
			c.event = func(raw any) { list = raw.(map[string]any)["servers"].([]existingServer) }
			if err := c.listExisting(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(list) != total || list == nil || (total == 201 && calls != 2) {
				t.Fatal("list was truncated or encoded as null")
			}
		})
	}
}
func TestExistingModeCannotCreateEvenWithOldQuoteAndPermissions(t *testing.T) {
	c := cloudTestService(t, func(http.ResponseWriter, *http.Request) { t.Error("existing mode reached paid API") })
	c.intent = "existing"
	c.state.Plan = &CloudPlan{ID: "old", Expires: time.Now().Add(time.Minute).Unix()}
	if c.deploy(context.Background(), "old") == nil {
		t.Fatal("existing flow created a resource")
	}
	if newCloudService().intent != "existing" {
		t.Fatal("first launch does not default to existing servers")
	}
}

func TestExistingLoginOnlyRequestsReadScopesAndReturnsServerList(t *testing.T) {
	var challenge, redirect string
	c := cloudTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/register":
			fmt.Fprint(w, `{"client_id":"fixture-client","registration_access_token":"fixture-registration"}`)
		case "/oauth/token":
			r.ParseForm()
			if cloudPKCE(r.Form.Get("code_verifier")) != challenge || r.Form.Get("redirect_uri") != redirect {
				http.Error(w, "PKCE", 400)
				return
			}
			fmt.Fprint(w, `{"access_token":"fixture-access","refresh_token":"fixture-refresh","expires_in":3600,"scope":"account:read droplet:read"}`)
		case "/v2/account":
			fmt.Fprint(w, `{"account":{"uuid":"fixture-account","status":"active"}}`)
		case "/v2/droplets":
			fmt.Fprint(w, `{"droplets":[],"meta":{"total":0}}`)
		default:
			t.Errorf("read-only login requested unexpected endpoint: %s", r.URL.Path)
			http.Error(w, "unexpected", 400)
		}
	})
	c.intent = "existing"
	c.state = CloudState{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	callback := make(chan error, 1)
	listed := false
	c.event = func(raw any) {
		e := raw.(map[string]any)
		if e["event"] == "cloudServers" {
			listed = true
		}
		if e["event"] != "cloudAuthorize" {
			return
		}
		u, _ := url.Parse(e["url"].(string))
		q := u.Query()
		if q.Get("scope") != cloudReadScopes {
			t.Error("default login expanded to create permissions")
		}
		challenge, redirect = q.Get("code_challenge"), q.Get("redirect_uri")
		go func() {
			response, err := http.Get(redirect + "?" + url.Values{"state": {q.Get("state")}, "code": {"fixture-code"}}.Encode())
			if err == nil {
				response.Body.Close()
			}
			callback <- err
		}()
	}
	if err := c.login(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-callback; err != nil {
		t.Fatal(err)
	}
	if !listed || c.canCreate() || c.state.Plan != nil {
		t.Fatal("read-only login did not finish at existing server selection")
	}
}
func TestExistingPairingIsBoundToServerSessionCertificateAndOneUse(t *testing.T) {
	c := existingService(t)
	var paired *Profile
	c.event = func(raw any) {
		e := raw.(map[string]any)
		if e["event"] == "cloudExistingProfile" {
			v := e["profile"].(Profile)
			paired = &v
		}
	}
	if err := c.prepareExisting(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	p := c.pairing
	profile := existingProfile(t)
	if c.acceptExisting(context.Background(), encryptExistingCode(t, p, profile, "other-session")) == nil {
		t.Fatal("foreign session accepted")
	}
	foreign := profile
	foreign.Config = strings.Replace(profile.Config, "@192.0.2.10:", "@192.0.2.20:", 1)
	if c.acceptExisting(context.Background(), encryptExistingCode(t, p, foreign, p.challenge)) == nil {
		t.Fatal("foreign endpoint accepted")
	}
	insecure := profile
	insecure.Config += "&insecure=1"
	if c.acceptExisting(context.Background(), encryptExistingCode(t, p, insecure, p.challenge)) == nil {
		t.Fatal("disabled certificate validation accepted")
	}
	wrongName := profile
	wrongName.Config = strings.Replace(profile.Config, "sni=192.0.2.10", "sni=wrong.invalid", 1)
	if c.acceptExisting(context.Background(), encryptExistingCode(t, p, wrongName, p.challenge)) == nil {
		t.Fatal("wrong certificate identity accepted")
	}
	valid := encryptExistingCode(t, p, profile, p.challenge)
	// A pasted terminal line may wrap and have a prompt before/after it.
	wrapped := "print('CLEARLINE_READ_ERROR:MISSING_CRYPTOGRAPHY')\nconsole> " + valid[:120] + "\n" + valid[120:] + "\nroot@fixture:~#"
	if err := c.acceptExisting(context.Background(), wrapped); err != nil {
		t.Fatal(err)
	}
	if paired == nil || paired.Name != "existing-server" || c.pairing != nil {
		t.Fatal("profile was not delivered exactly once")
	}
	if c.acceptExisting(context.Background(), valid) == nil {
		t.Fatal("replay accepted")
	}
}
func TestExistingPairingExpiresAndNeverInstallsSSHAccess(t *testing.T) {
	c := existingService(t)
	var command string
	c.event = func(raw any) {
		e := raw.(map[string]any)
		if e["event"] == "cloudPairing" {
			command = e["command"].(string)
		}
	}
	if err := c.prepareExisting(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"apt-get", "authorized_keys", "server.key'", "systemctl restart", "systemctl enable"} {
		if strings.Contains(command, forbidden) {
			t.Errorf("reader modifies or exposes server: %s", forbidden)
		}
	}
	c.pairing.expires = time.Now().Add(-time.Second)
	if c.acceptExisting(context.Background(), "anything") == nil || c.pairing != nil {
		t.Fatal("expired pairing accepted")
	}
	if _, err := pairingEnvelope(strings.Repeat("x", 128*1024+1)); err == nil {
		t.Fatal("oversized input accepted")
	}
}

func TestBundledExistingReaderEncryptsBothAuthTypesWithoutChangingFiles(t *testing.T) {
	python := os.Getenv("CLEARLINE_PAIRING_PYTHON")
	if python == "" {
		python, _ = exec.LookPath("python3")
	}
	if python == "" || exec.Command(python, "-c", "import cryptography").Run() != nil {
		t.Skip("Python cryptography required for reader integration")
	}
	for _, kind := range []string{"password", "userpass"} {
		t.Run(kind, func(t *testing.T) {
			c := existingService(t)
			if err := c.prepareExisting(context.Background(), 42); err != nil {
				t.Fatal(err)
			}
			folder := t.TempDir()
			certPath := filepath.Join(folder, "server.crt")
			configPath := filepath.Join(folder, "server.json")
			cert := existingCertificate(t)
			os.WriteFile(certPath, []byte(cert), 0600)
			auth := map[string]any{"type": kind, "password": "fixture-password"}
			if kind == "userpass" {
				auth = map[string]any{"type": kind, "userpass": map[string]string{"fixture-user": "fixture-password"}}
			}
			doc, _ := json.Marshal(map[string]any{"listen": ":443", "auth": auth, "tls": map[string]string{"cert": certPath, "key": "/nonexistent/private-key-never-read"}})
			os.WriteFile(configPath, doc, 0600)
			pub, _ := x509.MarshalPKIXPublicKey(&c.pairing.key.PublicKey)
			cfg, _ := json.Marshal(map[string]any{"host": "192.0.2.10", "serverID": 42, "name": "fixture", "challenge": c.pairing.challenge, "publicKey": base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}))})
			script := existingReader + fmt.Sprintf("\nread_config=lambda: json.loads(pathlib.Path(%q).read_text())\nmain(json.loads(base64.b64decode('%s')))\n", configPath, base64.StdEncoding.EncodeToString(cfg))
			command := exec.Command(python, "-")
			command.Stdin = strings.NewReader(script)
			output, err := command.Output()
			if err != nil {
				t.Fatal("reader process failed")
			}
			if strings.Contains(string(output), "fixture-password") {
				t.Fatal("reader printed plaintext credentials")
			}
			if err := c.acceptExisting(context.Background(), string(output)); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(configPath)
			afterCert, _ := os.ReadFile(certPath)
			if string(after) != string(doc) || string(afterCert) != cert {
				t.Fatal("read-only pairing changed server files")
			}
		})
	}
}
