# Clearline contributor and AI setup instructions

This is a public macOS client. Each user authorizes their own DigitalOcean
account and pays for their own cloud resources. Keep deployment details
private to that user. Never use a developer's personal server as a default.

## Helping a user set up the app

1. Read README.md and inspect the installed version and proxy ownership.
   Keep the user's working connection online during installation or updates.
2. Install the signed release or build with scripts/build.sh. Launch Clearline
   and open “设置 DigitalOcean”. The user completes login, OAuth consent and
   macOS security or keychain prompts themselves.
3. The app reads current cloud prices. Present the selected region and quoted
   price. The user explicitly approves the recurring expense in the app
   before deployment. Account inspection authorization does not authorize
   creation of billable resources.
4. Let the app create and verify its isolated Droplet. Resume its existing
   deployment after interruptions. Never repeat an uncertain create request
   or modify unrelated Droplets to work around an error.
5. Check the saved connection and built-in exit verification. Report actual
   checks separately from tests or simulated results. Do not open or log into
   an AI account to prove network connectivity without permission.

Read the credential-free progress summary at
`~/Library/Application Support/Clearline/setup-status.json` to inspect progress.
It excludes account IDs, server IPs, tokens, URLs and connection contents.
Ask for the displayed error when `needsAttention` is true; do not retrieve
keychain secrets.

CloudState events from `clearline-core cloud` are a private GUI protocol and
contain secrets. Never pipe them into chat, logs, shared files or an AI tool.
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
