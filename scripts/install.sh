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

# Build. The Go bindings are generated at build time and never committed
# (docs/events.md, Proto ownership), so there is no honest build without the
# task + buf toolchain -- a plain go build on a fresh clone cannot compile.
# Fail loud naming what is missing rather than pretending a fallback exists.
for tool in task buf go; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "install: missing required tool: $tool (see docs/operations.md, Build)" >&2
    exit 1
  fi
done
( cd "$SRC" && task build )

# Link the built binary onto the bin surface.
mkdir -p "$BIN_DIR"
ln -sf "$SRC/bin/delightd" "$BIN_DIR/delightd"

# An install that lands off PATH helps nobody (PR 75 review): say so loudly.
# Informational only -- editing shell config is the operator's decision, and
# this script takes no hand steps on their behalf.
case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo "note: $BIN_DIR is NOT on your PATH; to invoke delightd add:" >&2
     echo "  export PATH=\"$BIN_DIR:\$PATH\"" >&2 ;;
esac

# Smoke check: --help exits 0 with no side effects (documented in the
# operations flags table). Never start the daemon.
"$BIN_DIR/delightd" --help >/dev/null 2>&1

echo "installed: $BIN_DIR/delightd ($(git -C "$SRC" rev-parse --short HEAD))"
