#!/usr/bin/env bash
# install.sh -- clone-or-update delightd, build its container image, and install the
# `delightd` host wrapper onto your PATH surface.
#
# delightd now runs as a host-level container, not an on-disk binary. This installs the
# wrapper (the SRE front door: start/stop/logs/status, everything else forwarded to the
# control port) and builds the image so the first `delightd start` is ready. Idempotent,
# no prompts, no sudo, no curl-pipe-to-shell. Only git and the image build touch the wire.
set -euo pipefail

# Where the checkout lives. DELIGHT_INSTALL_ROOT moves the whole fleet layout with one
# variable (/opt, /usr/local, ~/mesh/prod); DELIGHTD_SRC overrides just this checkout.
# BIN_DIR is overridable only for tests (DELIGHTD_BIN_DIR) -- a real install always
# wants the standard bin surface.
INSTALL_ROOT="${DELIGHT_INSTALL_ROOT:-$HOME/mesh/prod}"
SRC="${DELIGHTD_SRC:-$INSTALL_ROOT/delightd}"
REMOTE="git@github.com:janearc/delightd.git"
BIN_DIR="${DELIGHTD_BIN_DIR:-$HOME/var/bin}"

# The image builds its own Go/buf toolchain inside the builder stage, so the host needs
# only docker (build + runtime) and git -- not task/buf/go. colima provides docker on
# macOS; the build below needs it up, and the wrapper starts it on `delightd start`.
# Check prerequisites BEFORE touching the network or the checkout: git itself is one of
# the prerequisites, so checking after the clone/pull is too late to matter, and a
# missing docker should fail immediately rather than after a clone has already run.
for tool in docker git; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "install: missing required tool: $tool" >&2
    exit 1
  fi
done

# Clone if absent; otherwise fast-forward the existing checkout.
if [ -d "$SRC/.git" ]; then
  echo "updating: $SRC"
  git -C "$SRC" pull --ff-only
else
  echo "cloning: $REMOTE -> $SRC"
  git clone "$REMOTE" "$SRC"
fi

if command -v colima >/dev/null 2>&1 && ! colima status >/dev/null 2>&1; then
  echo "install: starting colima (the container runtime)..."
  colima start
fi

# Per-workstation config (mount roots, the 1Password vault reference) lives in a
# gitignored .env; docker-compose.yml guards the required vars with ${VAR:?} and compose
# interpolates the whole file before ANY subcommand, including build. A clean-machine
# first run has no .env yet -- bootstrap it from .env.example and stop: the template's
# paths are placeholders (wrong for this machine), so we refuse to build against them
# rather than silently mounting garbage on a later `delightd start`.
if [ ! -f "$SRC/.env" ]; then
  if [ ! -f "$SRC/.env.example" ]; then
    echo "install: $SRC/.env.example is missing; checkout looks incomplete" >&2
    exit 1
  fi
  cp "$SRC/.env.example" "$SRC/.env"
  echo "install: created $SRC/.env from .env.example -- it is NOT ready yet." >&2
  echo "install: edit it and set, for this machine:" >&2
  echo "  DELIGHT_MONITOR_ROOT_HOST, DELIGHT_DAEMON_ROOT_HOST (container mount roots)" >&2
  echo "  DELIGHT_TOKEN_ITEM                                  (1Password vault reference)" >&2
  echo "  DELIGHT_APISERVER, DELIGHT_CA_SOURCE                (cluster access, once k3d is up)" >&2
  echo "install: then rerun install.sh" >&2
  exit 1
fi

# Source .env the same way the `delightd` wrapper does, so the build sees the same vars
# a later `delightd start` would.
set -a
# shellcheck source=/dev/null
. "$SRC/.env"
set +a

# Build the image, commit-stamped exactly as the wrapper stamps it on `start` (the
# Dockerfile requires GIT_SHA and fails loud without it).
GIT_SHA=$(git -C "$SRC" rev-parse --short HEAD)
[ -n "$(git -C "$SRC" status --porcelain)" ] && GIT_SHA="${GIT_SHA}-dirty"
# compose interpolates the runtime mounts for EVERY subcommand, build included, and
# DELIGHT_CREDS_DIR is guarded with :? -- export the same default the wrapper uses so
# a build does not trip a runtime-only guard. The value is not baked into the image.
export DELIGHT_CREDS_DIR="${DELIGHT_CREDS_DIR:-$HOME/.delightd/creds}"
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
