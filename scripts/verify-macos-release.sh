#!/usr/bin/env bash
# Verifies a published release asset by downloading it back from GitHub:
#   verify-macos-release.sh <tag> <arch>        (arch = arm64 or amd64)
# Checks that the asset's API digest ("sha256:<hex>") matches the downloaded bytes, that the archive holds
# only "Offload Monitor.app", that the bundle id and version match the tag, that the code signature is valid,
# and that the bundled CLI reports the same version. Needs gh (authenticated), jq, unzip, ditto, codesign.
# The bundled CLI is executed, so run this on a runner whose architecture matches <arch>.
# Environment: GITHUB_REPOSITORY (default: from gh), OFFLOAD_VERIFY_DIR (default: a temporary directory).
set -euo pipefail

tag="${1:?usage: verify-macos-release.sh <tag> <arch>}"
arch="${2:?usage: verify-macos-release.sh <tag> <arch>}"
case "$arch" in arm64|amd64) ;; *) echo "arch must be arm64 or amd64" >&2; exit 2 ;; esac
version="${tag#v}"
name="offload-monitor-macos-$arch.zip"
repo="${GITHUB_REPOSITORY:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}"

work="${OFFLOAD_VERIFY_DIR:-$(mktemp -d)}"
mkdir -p "$work"
trap '[ -n "${OFFLOAD_VERIFY_DIR:-}" ] || rm -rf "$work"' EXIT
fail() { echo "verify failed: $*" >&2; exit 1; }

# GitHub computes the digest asynchronously; wait briefly for it.
digest=""
for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
  digest="$(gh api "repos/$repo/releases/tags/$tag" --jq ".assets[] | select(.name == \"$name\") | .digest // \"\"")"
  [ -n "$digest" ] && break
  sleep 5
done
[ -n "$digest" ] || fail "release $tag has no $name asset with an API digest"
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || fail "digest for $name is not sha256:<64 hex> (got '$digest')"

gh release download "$tag" --repo "$repo" --pattern "$name" --dir "$work" --clobber
actual="sha256:$(shasum -a 256 "$work/$name" | awk '{print $1}')"
[ "$actual" = "$digest" ] || fail "downloaded bytes are $actual but the API digest is $digest"

# Archive layout: every entry lives under the app bundle, with no absolute paths or "..".
entries="$(unzip -Z1 "$work/$name")"
[ -n "$entries" ] || fail "archive is empty"
while IFS= read -r e; do
  case "$e" in
    "Offload Monitor.app"|"Offload Monitor.app/"*) ;;
    *) fail "unexpected archive entry '$e'" ;;
  esac
  case "/$e/" in */../*|*/./*) fail "unsafe archive entry '$e'" ;; esac
done <<<"$entries"

mkdir "$work/extract"
ditto -x -k "$work/$name" "$work/extract"
[ "$(ls "$work/extract")" = "Offload Monitor.app" ] || fail "archive must contain only Offload Monitor.app"
app="$work/extract/Offload Monitor.app"

plist="$app/Contents/Info.plist"
[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$plist")" = "local.offload.monitor" ] || fail "unexpected bundle identifier"
[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$plist")" = "$version" ] || fail "bundle version does not match $tag"
[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleVersion' "$plist")" = "$version" ] || fail "bundle build number does not match $tag"

codesign --verify --deep --strict "$app" || fail "code signature is invalid"
codesign --verify --strict "$app/Contents/Resources/offload" || fail "bundled CLI signature is invalid"

lipo_arch="$([ "$arch" = arm64 ] && echo arm64 || echo x86_64)"
for bin in "$app/Contents/MacOS/OffloadMonitor" "$app/Contents/Resources/offload"; do
  [ "$(lipo -archs "$bin")" = "$lipo_arch" ] || fail "$(basename "$bin") is not a $lipo_arch binary"
done
[ "$("$app/Contents/Resources/offload" version)" = "$version" ] || fail "bundled CLI version does not match $tag"

echo "verified $name for $tag: $digest"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  echo "- \`$name\` (\`$tag\`): \`$digest\`" >> "$GITHUB_STEP_SUMMARY"
fi
