# Clearline contributor and AI setup instructions

This is a public macOS client. Each user authorizes their own DigitalOcean
account and pays for their own cloud resources. Keep deployment details
private to that user. Never use a developer's personal server as a default.

## Helping a user set up the app

1. Read README.md and inspect the installed version and proxy ownership.
   Keep the user's working connection online during installation or updates.
2. Install the signed release or build with scripts/build.sh. Open “连接设置”.
   Default to “已有服务器”: authorize the owner's account, list existing
   Droplets, and preserve the selected server and its current plan. Do not
   infer that an empty local profile list means a new server is needed.
3. The user completes login, OAuth consent and macOS/keychain prompts.
   Reopen the current OAuth URL in the chosen browser before restarting an
   authorization; signing in to the dashboard alone is not OAuth consent.
4. Read an existing Hysteria 2 connection through the app. It uses trusted,
   noninteractive SSH or an encrypted, one-time official-console pairing.
   The reader does not install packages, change services or install SSH keys.
   Never copy plaintext server credentials or private protocol events into AI
   tools. Existing WireGuard clients require their own identity/configuration.
5. Run the connection preflight. Resolve other VPN routes to the server
   endpoint, preserve working connectivity, then connect and verify the exit.
   AI split mode requires the matching component and protects all system DNS,
   public UDP and IPv6 through the server. Only ordinary IPv4 TCP goes direct.
   Ordinary proxy mode retains HTTP/HTTPS/SOCKS without DNS/UDP protection.
   A failed AI check switches visibly to ordinary proxy, never claiming AI
   protection there. The owner installs the component with macOS admin approval.
   Do not bypass that confirmation or run the credential-handling core as root.
   Inspect packetTunnelActive/dnsProtected/udpProtected separately from exit IP.
6. Only if the user separately chooses “新建服务器”, request create permission
   and show the actual quote. The user approves recurring expense in the app.
   Resume uncertain deployments; never repeat an uncertain create request or
   modify unrelated Droplets to work around an error.
7. Check the saved connection and built-in exit verification. Report actual
   checks separately from tests or simulated results. Do not open or log into
   an AI account to prove network connectivity without permission.

Read the credential-free progress summary at
`~/Library/Application Support/Clearline/setup-status.json` to inspect progress.
Schema 2 has separate `connection` and `cloud` sections. Top-level
`connectionSaved` and `connected` come from the actual client, not the cloud
wizard. Cloud app-deployment records do not describe every existing Droplet.
It excludes account IDs, server IPs, tokens, URLs and connection contents.
Use the displayed error when attention is needed; do not retrieve keychain
secrets or infer a need to buy resources from an inactive cloud quote.

Cloud state/profile events from `clearline-core cloud` are a private GUI
protocol and contain secrets. Never pipe them into chat, logs, shared files
or an AI tool. Pairing codes are encrypted, bound to a server/session and
usable once; they expire after 15 minutes.
Use the application for authorization, persistence and paid deployment.
Opening the app does not authorize cloud account changes by an AI agent.

## Development and release

- Run `swift test` and, under core, `go test -race ./...` for relevant changes.
- Cloud tests use local mock APIs. Production provider URLs are fixed in code.
  A local test result does not establish real-account deployment acceptance.
- Preserve keychain acknowledgement before cloud creation and the protection
  against repeated creation after an uncertain outcome.
- Limit scopes to account/resource inspection and Droplet creation.
  Destructive operations require a separate explicit request.
- Use documentation-only IP addresses in tests. Keep personal server addresses,
  account records, local usernames, transfer files and tokens out of Git.
- Release the app ZIP, source ZIP, README, build metadata and checksums only.
  Scan archives and binary strings for credentials and local build paths.
  Swift builds use prefix maps; Go uses `-trimpath`.
- Keep personal deployment snapshots and original Git history private.
- The privileged tunnel helper accepts only the exact signed core hashes sealed
  in that build. Never replace this with an identifier/path-only client check,
  arbitrary command execution, or a user-writable signing whitelist.
- Network tests must inject command runners and use local packet/UDP fixtures.
  Do not change the developer's DNS/routes to run automated tests. Preserve
  durable recovery, both IP families, entry bypass and no local DNS fallback.
