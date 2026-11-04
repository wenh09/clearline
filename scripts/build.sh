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
for arch in arm64 x86_64; do
  swift build -c release --triple "$arch-apple-macosx13.0" --scratch-path "$cache/swift-$arch" -Xswiftc -file-prefix-map -Xswiftc "$repo=/src/clearline" -Xswiftc -file-prefix-map -Xswiftc "$HOME=/home/build" -Xswiftc -debug-prefix-map -Xswiftc "$repo=/src/clearline" > "$cache/build-$arch.log" 2>&1 || { tail -60 "$cache/build-$arch.log" >&2; exit 1; }
  bin="$(swift build -c release --triple "$arch-apple-macosx13.0" --scratch-path "$cache/swift-$arch" --show-bin-path)"
  cp "$bin/Clearline" "$cache/ui-$arch"
  cp "$bin/ClearlineNetworkAgent" "$cache/agent-$arch"
done
app="$out/Clearline.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Helpers" "$app/Contents/Resources"
lipo -create "$cache/ui-arm64" "$cache/ui-x86_64" -output "$app/Contents/MacOS/Clearline"
lipo -create "$cache/agent-arm64" "$cache/agent-x86_64" -output "$app/Contents/Helpers/ClearlineNetworkAgent"
lipo -create "$cache/core-arm64" "$cache/core-amd64" -output "$app/Contents/Helpers/clearline-core"
# Swift's linker debug map still contains object-file paths after prefix maps.
strip -S "$app/Contents/MacOS/Clearline" "$app/Contents/Helpers/ClearlineNetworkAgent"
cp Resources/Info.plist "$app/Contents/Info.plist"
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
for executable in "$app/Contents/Helpers/clearline-core" "$app/Contents/Helpers/ClearlineNetworkAgent"; do codesign --force --sign - "$executable"; done
codesign --force --sign - "$app"
codesign --verify --deep --strict "$app"
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
