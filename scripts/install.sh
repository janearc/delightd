#!/usr/bin/env bash
# install.sh -- clone-or-update, build, and link delightd from a checkout.
#
# Run from anywhere. It brings a delightd checkout up to date (or clones it if
# absent), builds the daemon, and links the binary onto your PATH surface at
# $HOME/var/bin/delightd. Idempotent and hand-step free: no prompts, no sudo, no
# curl-pipe-to-shell. The only thing that touches the network is git itself.
set -euo pipefail

# Where the checkout lives; override with DELIGHTD_SRC.
SRC="${DELIGHTD_SRC:-$HOME/work/delightd}"
REMOTE="git@github.com:janearc/delightd.git"
BIN_DIR="$HOME/var/bin"

# Clone if absent; otherwise fast-forward the existing checkout.
if [ -d "$SRC/.git" ]; then
  echo "updating: $SRC"
  git -C "$SRC" pull --ff-only
else
  echo "cloning: $REMOTE -> $SRC"
  git clone "$REMOTE" "$SRC"
fi

# Build. Prefer the Taskfile; fall back to a plain go build against the COMMITTED
# generated bindings -- gen-freshness holds on main, so building without the buf
# toolchain is legitimate and keeps this script working on a bare machine.
if command -v task >/dev/null 2>&1; then
  ( cd "$SRC" && task build )
else
  ( cd "$SRC" && go build -o bin/delightd ./cmd/delightd )
fi

# Link the built binary onto the PATH surface.
mkdir -p "$BIN_DIR"
ln -sf "$SRC/bin/delightd" "$BIN_DIR/delightd"

# Smoke check: --help exits 0 with no side effects. Never start the daemon.
"$BIN_DIR/delightd" --help >/dev/null 2>&1

echo "installed: $BIN_DIR/delightd ($(git -C "$SRC" rev-parse --short HEAD))"
