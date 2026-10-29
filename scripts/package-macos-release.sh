#!/usr/bin/env bash
# Builds the app and zips it as dist/offload-monitor-macos-<arch>.zip (arch = arm64 or amd64).
# Takes the same environment as build-macos-app.sh. Publishes nothing.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
case "${OFFLOAD_ARCH:-$(uname -m)}" in
  arm64|aarch64) arch=arm64 ;;
  amd64|x86_64) arch=amd64 ;;
  *) echo "unsupported OFFLOAD_ARCH '${OFFLOAD_ARCH:-}'" >&2; exit 2 ;;
esac
export OFFLOAD_ARCH="$arch"

"$root/scripts/build-macos-app.sh"

zip="$root/dist/offload-monitor-macos-$arch.zip"
rm -f "$zip"
# ditto keeps the code signature and symlinks intact; --keepParent puts "Offload Monitor.app" at the top level.
ditto -c -k --norsrc --noextattr --keepParent "$root/dist/Offload Monitor.app" "$zip"
(cd "$root/dist" && shasum -a 256 "$(basename "$zip")")
echo "packaged: $zip"
