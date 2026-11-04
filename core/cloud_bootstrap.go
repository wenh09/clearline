package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

const hysteriaServerURL = "https://github.com/HyNetworks/hysteria/releases/download/app/v2.13.0/hysteria-linux-amd64"
const hysteriaServerSHA = "907ba8c9693edb104b20582681fb7dc15639d5b64a9cbb616a7b539190a86691"

func cloudUserData(d *CloudDeployment) (string, error) {
	config := map[string]any{"listen": ":443", "tls": map[string]string{"cert": "/etc/clearline/server.crt", "key": "/etc/clearline/server.key"}, "auth": map[string]string{"type": "password", "password": d.Password}, "masquerade": map[string]any{"type": "string", "string": map[string]any{"content": "Welcome", "statusCode": 200}}, "acl": map[string]any{"inline": []string{"reject(127.0.0.0/8)", "reject(10.0.0.0/8)", "reject(172.16.0.0/12)", "reject(192.168.0.0/16)", "reject(169.254.0.0/16)", "reject(::1/128)", "reject(fc00::/7)", "reject(fe80::/10)", "direct(all)"}}}
	encoded, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	script := `#!/bin/bash
set -euo pipefail
umask 077
chmod 700 /etc/clearline
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates curl ufw
curl --fail --location --retry 3 --max-time 180 '` + hysteriaServerURL + `' -o /root/clearline-hysteria.download
echo '` + hysteriaServerSHA + `  /root/clearline-hysteria.download' | sha256sum --check --status
install -m 755 /root/clearline-hysteria.download /usr/local/bin/clearline-hysteria
rm -f /root/clearline-hysteria.download
systemctl daemon-reload
systemctl enable --now clearline-hysteria.service
ufw default deny incoming
ufw default allow outgoing
ufw allow 443/udp
ufw --force enable
`
	unit := `[Unit]
Description=Clearline Hysteria 2
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=/usr/local/bin/clearline-hysteria server -c /etc/clearline/server.json
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LimitNOFILE=65536
[Install]
WantedBy=multi-user.target
`
	var out strings.Builder
	out.WriteString("#cloud-config\nssh_pwauth: false\ndisable_root: true\nwrite_files:\n")
	for _, file := range []struct{ path, mode, data string }{{"/etc/clearline/server.json", "0600", string(encoded)}, {"/etc/clearline/server.crt", "0600", d.Certificate}, {"/etc/clearline/server.key", "0600", d.Key}, {"/usr/local/sbin/clearline-bootstrap", "0700", script}, {"/etc/systemd/system/clearline-hysteria.service", "0644", unit}} {
		out.WriteString("  - path: " + file.path + "\n    permissions: '" + file.mode + "'\n    encoding: b64\n    content: " + base64.StdEncoding.EncodeToString([]byte(file.data)) + "\n")
	}
	out.WriteString("runcmd:\n  - [bash, /usr/local/sbin/clearline-bootstrap]\n")
	return out.String(), nil
}
