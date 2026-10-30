#!/usr/bin/env bash
# Build a labeled development bundle; --install replaces the local app without launching it.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
install_dev=false
case "${1:-}" in
  "") ;;
  --install) install_dev=true ;;
  *) echo "usage: scripts/dev-macos-app.sh [--install]" >&2; exit 2 ;;
esac
base="${OFFLOAD_DEV_BASE:-$(git -C "$root" describe --tags --abbrev=0 2>/dev/null || echo v0.0.0)}"
base="${base#v}"
commit="$(git -C "$root" rev-parse --short HEAD)"
dirty=""
if [ -n "$(git -C "$root" status --porcelain)" ]; then dirty=".dirty"; fi
export OFFLOAD_VERSION="$base-dev.$(date -u +%Y%m%d%H%M%S).$commit$dirty"
bash "$root/scripts/build-macos-app.sh"
if $install_dev; then
  parent="${OFFLOAD_DEV_INSTALL_DIR:-$HOME/Applications}"
  mkdir -p "$parent"
  staging="$(mktemp -d "$parent/.offload-dev.XXXXXX")"
  trap 'rm -rf "$staging"' EXIT
  ditto "$root/dist/Offload Monitor.app" "$staging/Offload Monitor.app"
  codesign --verify --deep --strict "$staging/Offload Monitor.app"
  codesign --verify --strict "$staging/Offload Monitor.app/Contents/Resources/offload"
  installed_version="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$staging/Offload Monitor.app/Contents/Info.plist")"
  cli_version="$("$staging/Offload Monitor.app/Contents/Resources/offload" version)"
  if [ "$installed_version" != "$OFFLOAD_VERSION" ] || [ "$cli_version" != "$OFFLOAD_VERSION" ]; then
    echo "App and bundled CLI versions must both match $OFFLOAD_VERSION" >&2
    exit 1
  fi
  destination="$parent/Offload Monitor.app"
  if [ -e "$destination" ]; then mv "$destination" "$staging/previous.app"; fi
  if ! mv "$staging/Offload Monitor.app" "$destination"; then
    if [ -e "$staging/previous.app" ]; then mv "$staging/previous.app" "$destination"; fi
    exit 1
  fi
  echo "Installed $OFFLOAD_VERSION at $destination"
  echo "Use the App's local restart action, or its opted-in restart-when-idle setting."
fi
