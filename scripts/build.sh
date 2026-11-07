#!/bin/bash
set -euo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo"
cache="$repo/.cache"
out="${CLEARLINE_OUTPUT_DIR:-$repo/dist}"
mkdir -p "$cache" "$out"
go_bin="${CLEARLINE_GO:-}"
if [[ -z "$go_bin" ]]; then
  if command -v go >/dev/null 2>&1; then go_bin="$(command -v go)"; else
    arch="$(uname -m)"
    if [[ "$arch" == arm64 ]]; then go_arch=arm64; checksum=dd3908cf20959d798c1e06ebe5b72213f0505ce01373b40ce3110975a6cd99c2
    else go_arch=amd64; checksum=b6f8e931e4ab497fcde6da8ae4499a58f9d258e55c8a80824a516eae5950a750; fi
    if [[ ! -x "$cache/toolchain/go/bin/go" ]]; then
      curl --fail --location --retry 2 "https://go.dev/dl/go1.27.3.darwin-$go_arch.tar.gz" -o "$cache/go.tar.gz"
      actual="$(shasum -a 256 "$cache/go.tar.gz" | awk '{print $1}')"
      [[ "$actual" == "$checksum" ]] || { echo 'Go checksum mismatch' >&2; exit 1; }
      mkdir -p "$cache/toolchain"; tar -xzf "$cache/go.tar.gz" -C "$cache/toolchain"
    fi
    go_bin="$cache/toolchain/go/bin/go"
  fi
fi
export GOPATH="${CLEARLINE_GOPATH:-$cache/gopath}"
export GOCACHE="${CLEARLINE_GOCACHE:-$cache/gocache}"
export GOPROXY="${CLEARLINE_GOPROXY:-https://proxy.golang.org|https://goproxy.cn|direct}"
export GOMAXPROCS=4
for arch in arm64 amd64; do
  (cd core; CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" "$go_bin" build -trimpath -ldflags='-s -w' -o "$cache/core-$arch" .)
done
lipo -create "$cache/core-arm64" "$cache/core-amd64" -output "$cache/core-universal"
codesign --force --identifier org.clearline.core --sign - "$cache/core-universal"
tunnel_info="$cache/tunnel-agent-info-v2-$(shasum -a 256 "$cache/core-universal" | awk '{print $1}').plist"
# The privileged daemon pins the exact signed core hashes in a read-only Mach-O
# section. It never trusts a client PID, path, team name or user-writable file alone.
python3 - "$cache/core-universal" "$tunnel_info" <<'PYINFO'
import plistlib, re, subprocess, sys
hashes = []
for arch in ('arm64', 'x86_64'):
    info = subprocess.check_output(['codesign', '-d', '--arch', arch, '--verbose=4', sys.argv[1]], stderr=subprocess.STDOUT, text=True)
    hashes.append(re.search(r'^CDHash=([0-9a-f]{40})$', info, re.M).group(1))
with open(sys.argv[2], 'wb') as out:
    plistlib.dump({'CFBundleIdentifier': 'org.clearline.tunnel', 'ClearlineCoreHashes': hashes, 'ClearlineTunnelProtocol': 2}, out)
PYINFO
for arch in arm64 x86_64; do
  swift build -c release --triple "$arch-apple-macosx13.0" --scratch-path "$cache/swift-$arch" -Xswiftc -file-prefix-map -Xswiftc "$repo=/src/clearline" -Xswiftc -file-prefix-map -Xswiftc "$HOME=/home/build" -Xswiftc -debug-prefix-map -Xswiftc "$repo=/src/clearline" > "$cache/build-$arch.log" 2>&1 || { tail -60 "$cache/build-$arch.log" >&2; exit 1; }
  bin="$(swift build -c release --triple "$arch-apple-macosx13.0" --scratch-path "$cache/swift-$arch" --show-bin-path)"
  cp "$bin/Clearline" "$cache/ui-$arch"
  cp "$bin/ClearlineNetworkAgent" "$cache/agent-$arch"
  swift build -c release --product ClearlineTunnelAgent --triple "$arch-apple-macosx13.0" --scratch-path "$cache/tunnel-swift-$arch" -Xswiftc -file-prefix-map -Xswiftc "$repo=/src/clearline" -Xswiftc -file-prefix-map -Xswiftc "$HOME=/home/build" -Xswiftc -debug-prefix-map -Xswiftc "$repo=/src/clearline" -Xlinker -sectcreate -Xlinker __TEXT -Xlinker __info_plist -Xlinker "$tunnel_info" > "$cache/tunnel-build-$arch.log" 2>&1 || { tail -60 "$cache/tunnel-build-$arch.log" >&2; exit 1; }
  tunnel_bin="$(swift build -c release --triple "$arch-apple-macosx13.0" --scratch-path "$cache/tunnel-swift-$arch" --show-bin-path)"
  cp "$tunnel_bin/ClearlineTunnelAgent" "$cache/tunnel-agent-$arch"
done
app="$out/Clearline.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Helpers" "$app/Contents/Resources"
lipo -create "$cache/ui-arm64" "$cache/ui-x86_64" -output "$app/Contents/MacOS/Clearline"
lipo -create "$cache/agent-arm64" "$cache/agent-x86_64" -output "$app/Contents/Helpers/ClearlineNetworkAgent"
cp "$cache/core-universal" "$app/Contents/Helpers/clearline-core"
lipo -create "$cache/tunnel-agent-arm64" "$cache/tunnel-agent-x86_64" -output "$app/Contents/Helpers/ClearlineTunnelAgent"
# Swift's linker debug map still contains object-file paths after prefix maps.
strip -S "$app/Contents/MacOS/Clearline" "$app/Contents/Helpers/ClearlineNetworkAgent" "$app/Contents/Helpers/ClearlineTunnelAgent"
cp Resources/Info.plist "$app/Contents/Info.plist"
rm -f "$app/Contents/Library/LaunchDaemons/org.clearline.tunnel.plist"
swift scripts/make-icon.swift "$cache/icon.png"
mkdir -p "$cache/AppIcon.iconset"
for pixels in 16 32 64 128 256 512; do
  sips -z "$pixels" "$pixels" "$cache/icon.png" --out "$cache/AppIcon.iconset/icon_${pixels}x${pixels}.png" >/dev/null
done
cp "$cache/icon.png" "$cache/AppIcon.iconset/icon_512x512@2x.png"
iconutil -c icns "$cache/AppIcon.iconset" -o "$app/Contents/Resources/AppIcon.icns"
python3 scripts/notices.py "$go_bin" "$app/Contents/Resources/ThirdPartyNotices.txt"
cp LICENSE "$app/Contents/Resources/LICENSE.txt"
cp core/claude-domains.txt "$app/Contents/Resources/claude-domains.txt"
cp core/ai-domains.txt "$app/Contents/Resources/ai-domains.txt"
codesign --force --identifier org.clearline.tunnel --sign - "$app/Contents/Helpers/ClearlineTunnelAgent"
codesign --force --sign - "$app/Contents/Helpers/ClearlineNetworkAgent"
payload="$(mktemp -d "$cache/tunnel-package-payload.XXXXXX")"
mkdir -p "$payload/Library/PrivilegedHelperTools" "$payload/Library/LaunchDaemons"
mkdir -p "$payload/Library/Application Support/Clearline"
chmod 711 "$payload/Library/Application Support/Clearline"
cp "$app/Contents/Helpers/ClearlineTunnelAgent" "$payload/Library/PrivilegedHelperTools/org.clearline.tunnel"
cp "$tunnel_info" "$payload/Library/PrivilegedHelperTools/org.clearline.tunnel.version.plist"
cp Resources/org.clearline.tunnel.plist "$payload/Library/LaunchDaemons/"
chmod 755 "$payload/Library/PrivilegedHelperTools/org.clearline.tunnel"
chmod 644 "$payload/Library/PrivilegedHelperTools/org.clearline.tunnel.version.plist" "$payload/Library/LaunchDaemons/org.clearline.tunnel.plist"
pkgbuild --root "$payload" --ownership recommended --scripts scripts/tunnel-installer --identifier org.clearline.tunnel.pkg --version 0.3.0.4 "$app/Contents/Resources/ClearlineSystemTunnel.pkg" >/dev/null
rm -rf "$payload"
codesign --force --sign - "$app"
codesign --verify --deep --strict "$app"
installer_parts="$(mktemp -d "$cache/installer-parts.XXXXXX")"
app_payload="$(mktemp -d "$cache/application-payload.XXXXXX")"
mkdir -p "$app_payload/Applications" "$installer_parts/scripts"
ditto "$app" "$app_payload/Applications/Clearline.app"
cp scripts/app-installer/preinstall "$installer_parts/scripts/preinstall"
cp scripts/tunnel-installer/preinstall "$installer_parts/scripts/network-preinstall"
cp scripts/tunnel-installer/postinstall "$installer_parts/scripts/postinstall"
chmod 755 "$installer_parts/scripts/preinstall" "$installer_parts/scripts/network-preinstall" "$installer_parts/scripts/postinstall"
pkgbuild --analyze --root "$app_payload" "$installer_parts/components.plist"
python3 - "$installer_parts/components.plist" <<'PYCOMP'
import plistlib, sys
with open(sys.argv[1], 'rb') as f: components = plistlib.load(f)
for component in components:
    component['BundleIsRelocatable'] = False
with open(sys.argv[1], 'wb') as f: plistlib.dump(components, f)
PYCOMP
pkgbuild --root "$app_payload" --component-plist "$installer_parts/components.plist" --ownership recommended --scripts "$installer_parts/scripts" --identifier org.clearline.client.pkg --version 0.3.0.4 "$installer_parts/ClearlineApplication.pkg" >/dev/null
cp "$app/Contents/Resources/ClearlineSystemTunnel.pkg" "$installer_parts/"
productbuild --synthesize --product Resources/InstallerRequirements.plist --package "$installer_parts/ClearlineApplication.pkg" --package "$installer_parts/ClearlineSystemTunnel.pkg" "$installer_parts/distribution.xml" >/dev/null
python3 - "$installer_parts/distribution.xml" <<'PYTITLE'
import sys, xml.etree.ElementTree as ET
tree = ET.parse(sys.argv[1])
title = tree.getroot().find('title')
if title is None: title = ET.SubElement(tree.getroot(), 'title')
title.text = 'Clearline'
tree.write(sys.argv[1], encoding='utf-8', xml_declaration=True)
PYTITLE
productbuild --distribution "$installer_parts/distribution.xml" --package-path "$installer_parts" "$out/Clearline-macOS-installer.pkg" >/dev/null
rm -rf "$app_payload" "$installer_parts"
python3 - "$app" "$out/Clearline-macOS-universal.zip" <<'PYZIP'
import sys, zipfile
from pathlib import Path
app = Path(sys.argv[1])
for path in app.rglob('*'):
    if path.is_file() and b'/Users/' in path.read_bytes():
        raise SystemExit('Private build path found in ' + str(path.relative_to(app)))
with zipfile.ZipFile(sys.argv[2], 'w', zipfile.ZIP_DEFLATED) as archive:
    for path in sorted(app.rglob('*')):
        if path.is_file(): archive.write(path, path.relative_to(app.parent))
PYZIP
cp README.md "$out/README-先读.md"
echo "Built: $app"
