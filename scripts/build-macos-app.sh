#!/usr/bin/env bash
# Builds dist/Offload Monitor.app: the SwiftUI menu bar app plus the offload CLI built from this repo.
#   OFFLOAD_VERSION  version stamped into the app and the CLI (a leading "v" is dropped). Default: the
#                    latest local git tag (git describe --tags --abbrev=0), or 0.3.0 when there is none.
#                    A release build passes the tag being released explicitly.
#   OFFLOAD_ARCH     arm64 or amd64 (default: this machine)
#   OFFLOAD_SIGN_ID  codesign identity (default "-", ad-hoc)
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
default_version="$(git -C "$root" describe --tags --abbrev=0 2>/dev/null || true)"
version="${OFFLOAD_VERSION:-${default_version:-0.3.0}}"
version="${version#v}"
if ! [[ "$version" =~ ^[0-9]+(\.[0-9]+){1,3}(-dev\.[0-9]+\.[a-f0-9]+(\.dirty)?)?$ ]]; then
  echo "OFFLOAD_VERSION must be numeric or use -dev.<timestamp>.<commit>[.dirty] (got '$version')" >&2
  exit 2
fi

case "${OFFLOAD_ARCH:-$(uname -m)}" in
  arm64|aarch64) arch=arm64; swift_arch=arm64 ;;
  amd64|x86_64) arch=amd64; swift_arch=x86_64 ;;
  *) echo "unsupported OFFLOAD_ARCH '${OFFLOAD_ARCH:-}'" >&2; exit 2 ;;
esac

app="$root/dist/Offload Monitor.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"

echo "==> offload CLI $version ($arch)"
(cd "$root" && CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" \
  go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$app/Contents/Resources/offload" .)

echo "==> Offload Monitor $version ($arch)"
swift build --package-path "$root/macos" -c release --arch "$swift_arch" --product OffloadMonitor
bin="$(swift build --package-path "$root/macos" -c release --arch "$swift_arch" --show-bin-path)/OffloadMonitor"
install -m 0755 "$bin" "$app/Contents/MacOS/OffloadMonitor"

cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key><string>local.offload.monitor</string>
  <key>CFBundleName</key><string>Offload Monitor</string>
  <key>CFBundleDisplayName</key><string>Offload Monitor</string>
  <key>CFBundleExecutable</key><string>OffloadMonitor</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$version</string>
  <key>CFBundleVersion</key><string>$version</string>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
  <key>LSUIElement</key><true/>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST

echo "==> codesign (${OFFLOAD_SIGN_ID:--})"
codesign --force --sign "${OFFLOAD_SIGN_ID:--}" "$app/Contents/Resources/offload"
codesign --force --sign "${OFFLOAD_SIGN_ID:--}" "$app"
codesign --verify --deep --strict "$app"

echo "built: $app"
