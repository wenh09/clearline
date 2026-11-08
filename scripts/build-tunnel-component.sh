#!/bin/bash
set -euo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo"
cache="$repo/.cache"
core="${1:-/Applications/Clearline.app/Contents/Helpers/clearline-core}"
output="${CLEARLINE_TUNNEL_OUTPUT:-$repo/dist/ClearlineSystemTunnel-hotfix.pkg}"
version="${CLEARLINE_TUNNEL_PACKAGE_VERSION:-0.3.0.10}"
mkdir -p "$cache" "$(dirname "$output")"
codesign --verify --strict "$core"
info="$cache/tunnel-component-$(shasum -a 256 "$core" | awk '{print $1}').plist"
python3 - "$core" "$info" <<'PYINFO'
import plistlib, re, subprocess, sys
hashes=[]
for arch in ('arm64','x86_64'):
    value=subprocess.check_output(['codesign','-d','--arch',arch,'--verbose=4',sys.argv[1]],stderr=subprocess.STDOUT,text=True)
    hashes.append(re.search(r'^CDHash=([0-9a-f]{40})$',value,re.M).group(1))
with open(sys.argv[2],'wb') as f:
    plistlib.dump({'CFBundleIdentifier':'org.clearline.tunnel','ClearlineCoreHashes':hashes,'ClearlineTunnelProtocol':3},f)
PYINFO
for arch in arm64 x86_64; do
    scratch="$cache/component-swift-$arch"
    swift build -c release --product ClearlineTunnelAgent --triple "$arch-apple-macosx13.0" --scratch-path "$scratch" -Xswiftc -file-prefix-map -Xswiftc "$repo=/src/clearline" -Xswiftc -file-prefix-map -Xswiftc "$HOME=/home/build" -Xswiftc -debug-prefix-map -Xswiftc "$repo=/src/clearline" -Xlinker -sectcreate -Xlinker __TEXT -Xlinker __info_plist -Xlinker "$info" > "$cache/component-build-$arch.log" 2>&1 || { tail -40 "$cache/component-build-$arch.log" >&2; exit 1; }
    bin="$(swift build -c release --triple "$arch-apple-macosx13.0" --scratch-path "$scratch" --show-bin-path)"
    cp "$bin/ClearlineTunnelAgent" "$cache/component-agent-$arch"
done
helper="$cache/component-agent-universal"
lipo -create "$cache/component-agent-arm64" "$cache/component-agent-x86_64" -output "$helper"
strip -S "$helper"
codesign --force --identifier org.clearline.tunnel --sign - "$helper"
codesign --verify --strict "$helper"
payload="$(mktemp -d "$cache/component-payload.XXXXXX")"
trap 'rm -rf "$payload"' EXIT
mkdir -p "$payload/Library/PrivilegedHelperTools" "$payload/Library/LaunchDaemons" "$payload/Library/Application Support/Clearline"
chmod 711 "$payload/Library/Application Support/Clearline"
cp "$helper" "$payload/Library/PrivilegedHelperTools/org.clearline.tunnel"
cp "$info" "$payload/Library/PrivilegedHelperTools/org.clearline.tunnel.version.plist"
cp Resources/org.clearline.tunnel.plist "$payload/Library/LaunchDaemons/"
chmod 755 "$payload/Library/PrivilegedHelperTools/org.clearline.tunnel"
chmod 644 "$payload/Library/PrivilegedHelperTools/org.clearline.tunnel.version.plist" "$payload/Library/LaunchDaemons/org.clearline.tunnel.plist"
pkgbuild --root "$payload" --ownership recommended --scripts scripts/tunnel-installer --identifier org.clearline.tunnel.pkg --version "$version" "$output" >/dev/null
echo "Built component for exact selected core: $output"
