#!/bin/sh
# firefighter installer for macOS and Linux:
#   curl -fsSL https://raw.githubusercontent.com/JoshWright22/FIREFIGHTER/main/scripts/install.sh | sh
# Puts the binary in ~/.local/bin, then runs `firefighter setup` (start at logon, MCP for agent
# CLIs, shared-machine rules in their global instructions). Undo: firefighter uninstall.
set -eu
# Not as root: the old binary it stops is user-writable, and setup edits your own configs.
if [ "$(id -u)" = "0" ] && [ "${FIREFIGHTER_ALLOW_ELEVATED:-}" != "1" ]; then
  echo "Run this as your normal user, not root or sudo. firefighter runs as you." >&2
  exit 1
fi
REPO="JoshWright22/FIREFIGHTER"
BIN_DIR="${FIREFIGHTER_BIN_DIR:-$HOME/.local/bin}"
mkdir -p "$BIN_DIR"

# Replace by rename (a running binary can't be overwritten), after stopping the old daemon.
put() {
  [ -x "$BIN_DIR/firefighter" ] && "$BIN_DIR/firefighter" daemon --stop >/dev/null 2>&1 || true
  cp "$1" "$BIN_DIR/.firefighter.new"
  chmod +x "$BIN_DIR/.firefighter.new"
  mv -f "$BIN_DIR/.firefighter.new" "$BIN_DIR/firefighter"
}

if [ -n "${FIREFIGHTER_LOCAL_BINARY:-}" ]; then
  put "$FIREFIGHTER_LOCAL_BINARY"
else
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "unsupported CPU: $(uname -m)" >&2; exit 1 ;;
  esac
  name="firefighter_${os}_${arch}.tar.gz"
  base="https://github.com/$REPO/releases/latest/download"
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  echo "downloading $name"
  curl -fsSL "$base/$name" -o "$tmp/$name"
  curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
  want=$(grep " $name\$" "$tmp/checksums.txt" | cut -d' ' -f1)
  if command -v sha256sum >/dev/null; then got=$(sha256sum "$tmp/$name" | cut -d' ' -f1); else got=$(shasum -a 256 "$tmp/$name" | cut -d' ' -f1); fi
  [ "$want" = "$got" ] || { echo "checksum mismatch for $name" >&2; exit 1; }
  # The checksum only proves the download wasn't corrupted. With the GitHub CLI signed in, also
  # check the release's signed build provenance (it can't be forged by editing the release).
  if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    gh attestation verify "$tmp/$name" --repo "$REPO" >/dev/null || { echo "provenance check failed for $name" >&2; exit 1; }
    echo "verified signed build provenance"
  fi
  tar -xzf "$tmp/$name" -C "$tmp"
  put "$tmp/firefighter"
fi

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *)
    for rc in "$HOME/.bashrc" "$HOME/.zshrc" "$HOME/.profile"; do
      [ -f "$rc" ] && ! grep -q "$BIN_DIR" "$rc" && printf '\nexport PATH="%s:$PATH"\n' "$BIN_DIR" >> "$rc"
    done
    ;;
esac

"$BIN_DIR/firefighter" setup
