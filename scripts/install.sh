#!/usr/bin/env bash
# install.sh -- clone-or-update delightd, build its container image, and install the
# `delightd` host wrapper onto your PATH surface.
#
# delightd now runs as a host-level container, not an on-disk binary. This installs the
# wrapper (the SRE front door: start/stop/logs/status, everything else forwarded to the
# control port) and builds the image so the first `delightd start` is ready. Idempotent,
# no prompts, no sudo, no curl-pipe-to-shell. Only git and the image build touch the wire.
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

# The image builds its own Go/buf toolchain inside the builder stage, so the host needs
# only docker (build + runtime) and git -- not task/buf/go. colima provides docker on
# macOS; the build below needs it up, and the wrapper starts it on `delightd start`.
for tool in docker git; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "install: missing required tool: $tool" >&2
    exit 1
  fi
done
if command -v colima >/dev/null 2>&1 && ! colima status >/dev/null 2>&1; then
  echo "install: starting colima (the container runtime)..."
  colima start
fi

# Build the image, commit-stamped exactly as the wrapper stamps it on `start` (the
# Dockerfile requires GIT_SHA and fails loud without it).
GIT_SHA=$(git -C "$SRC" rev-parse --short HEAD)
[ -n "$(git -C "$SRC" status --porcelain)" ] && GIT_SHA="${GIT_SHA}-dirty"
echo "building image (stamp $GIT_SHA)..."
GIT_SHA="$GIT_SHA" docker compose --project-directory "$SRC" -f "$SRC/docker-compose.yml" build delightd

# Install the wrapper onto the bin surface (a symlink, so it tracks the checkout).
mkdir -p "$BIN_DIR"
ln -sf "$SRC/scripts/delightd" "$BIN_DIR/delightd"

# An install that lands off PATH helps nobody (PR 75 review): say so loudly. Informational
# only -- editing shell config is the operator's decision, and this takes no hand steps.
case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo "note: $BIN_DIR is NOT on your PATH; to invoke delightd add:" >&2
     echo "  export PATH=\"$BIN_DIR:\$PATH\"" >&2 ;;
esac

# Smoke check: the wrapper's help exits 0 with no side effects. Never start the daemon.
"$BIN_DIR/delightd" --help >/dev/null 2>&1

echo "installed: $BIN_DIR/delightd (wrapper) + image (stamp $GIT_SHA)"
echo "next: delightd start && delightd status"
