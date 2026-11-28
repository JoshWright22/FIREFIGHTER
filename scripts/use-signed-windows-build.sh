#!/usr/bin/env bash
# Called by GoReleaser before it packages or hashes the binaries.
set -euo pipefail

binary=$1
target=$2
case "$target" in
  windows_amd64*) arch=amd64 ;;
  windows_arm64*) arch=arm64 ;;
  *) exit 0 ;;
esac
[ -n "${FF_SIGNED_WINDOWS_DIR:-}" ] || exit 0

relative="$arch/$(basename "$binary")"
expected=$(cat "$FF_UNSIGNED_WINDOWS_HASHES/$relative.sha256")
actual=$(sha256sum < "$binary")
if [ "${actual%% *}" != "$expected" ]; then
  echo "Rebuilt $relative differs from the unsigned build sent to SignPath." >&2
  exit 1
fi

signed="$FF_SIGNED_WINDOWS_DIR/$relative"
if [ ! -s "$signed" ] || cmp -s "$binary" "$signed"; then
  echo "Missing signed output for $relative." >&2
  exit 1
fi
cp -- "$signed" "$binary"
