#!/usr/bin/env bash
#
# Deploy the weatherstation binary to a host that runs it as a systemd service.
#
# The target host is an argument (never hardcoded), so this script is safe to
# keep in a public repository. It cross-compiles the right binary for the
# target's architecture, copies it over, atomically swaps it into place, and
# restarts the service. The remote .env and data/ directory are never touched —
# only the binary is replaced.
#
# Usage:
#   scripts/deploy.sh [options] <ssh-target>
#
# Arguments:
#   <ssh-target>        ssh destination, e.g. "myhost" or "user@host"
#
# Options:
#   -d, --dir DIR       remote install dir            (default: ~/weatherstation)
#   -s, --service NAME  systemd service name          (default: weatherstation)
#       --arch ARCH     target arch: arm64 | armv7    (default: auto-detect via uname -m)
#       --user          restart with `systemctl --user` instead of `sudo systemctl`
#       --no-build      deploy the existing bin/ binary (skip the cross-compile)
#       --no-restart    copy the binary but do not restart the service
#   -h, --help          show this help
#
# Examples:
#   scripts/deploy.sh myhost
#   scripts/deploy.sh --dir /opt/weatherstation --service wx user@10.0.0.5
set -euo pipefail

DIR='~/weatherstation'
SERVICE='weatherstation'
ARCH=''
USE_USER=0
DO_BUILD=1
DO_RESTART=1
TARGET=''

usage() {
  cat <<'EOF'
Deploy the weatherstation binary to a host that runs it as a systemd service.
The remote .env and data/ directory are never touched — only the binary.

Usage:
  scripts/deploy.sh [options] <ssh-target>

Arguments:
  <ssh-target>        ssh destination, e.g. "myhost" or "user@host"

Options:
  -d, --dir DIR       remote install dir            (default: ~/weatherstation)
  -s, --service NAME  systemd service name          (default: weatherstation)
      --arch ARCH     target arch: arm64 | armv7    (default: auto-detect via uname -m)
      --user          restart with systemctl --user instead of sudo systemctl
      --no-build      deploy the existing bin/ binary (skip the cross-compile)
      --no-restart    copy the binary but do not restart the service
  -h, --help          show this help
EOF
}
die() { echo "error: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    -d|--dir)     DIR="$2"; shift 2 ;;
    -s|--service) SERVICE="$2"; shift 2 ;;
    --arch)       ARCH="$2"; shift 2 ;;
    --user)       USE_USER=1; shift ;;
    --no-build)   DO_BUILD=0; shift ;;
    --no-restart) DO_RESTART=0; shift ;;
    -h|--help)    usage; exit 0 ;;
    --)           shift; break ;;
    -*)           usage >&2; die "unknown option: $1" ;;
    *)            [ -z "$TARGET" ] || die "unexpected argument: $1"; TARGET="$1"; shift ;;
  esac
done
[ -n "$TARGET" ] || { usage >&2; die "<ssh-target> is required"; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SSH_OPTS=(-o ConnectTimeout=10 -o BatchMode=yes -o StrictHostKeyChecking=accept-new)

# Resolve the target architecture (auto-detect unless given) and the build it maps to.
if [ -z "$ARCH" ]; then
  echo "==> Detecting architecture on $TARGET ..."
  uname_m="$(ssh "${SSH_OPTS[@]}" "$TARGET" 'uname -m')" || die "could not ssh to $TARGET"
  case "$uname_m" in
    aarch64|arm64)            ARCH=arm64 ;;
    armv7l|armv6l|armhf|arm)  ARCH=armv7 ;;
    *) die "unsupported remote arch '$uname_m' — add a Makefile target and pass --arch" ;;
  esac
  echo "    $TARGET is $uname_m -> building $ARCH"
fi

case "$ARCH" in
  arm64) MAKE_TARGET=build-raspi; BIN="$REPO_ROOT/bin/weatherstation-linux-arm64" ;;
  armv7) MAKE_TARGET=build-pi32;  BIN="$REPO_ROOT/bin/weatherstation-linux-armv7" ;;
  *)     die "unknown --arch '$ARCH' (expected arm64 or armv7)" ;;
esac

if [ "$DO_BUILD" -eq 1 ]; then
  echo "==> Building $ARCH binary (make $MAKE_TARGET) ..."
  make -C "$REPO_ROOT" "$MAKE_TARGET"
fi
[ -f "$BIN" ] || die "binary not found: $BIN (remove --no-build to build it)"

echo "==> Copying $(basename "$BIN") to $TARGET:$DIR/ ..."
ssh "${SSH_OPTS[@]}" "$TARGET" "mkdir -p $DIR"
scp "${SSH_OPTS[@]}" "$BIN" "$TARGET:$DIR/weatherstation.new"

if [ "$USE_USER" -eq 1 ]; then
  RESTART='systemctl --user restart'; STATUS='systemctl --user is-active'
else
  RESTART='sudo systemctl restart';   STATUS='systemctl is-active'
fi

# Renaming over the running binary is safe: the live process keeps the old
# inode, and the restart below launches the freshly-swapped file.
swap="cd $DIR && mv -f weatherstation.new weatherstation && chmod +x weatherstation"

if [ "$DO_RESTART" -eq 1 ]; then
  echo "==> Swapping in the new binary and restarting '$SERVICE' ..."
  ssh "${SSH_OPTS[@]}" "$TARGET" "$swap && $RESTART $SERVICE"
  echo "==> Deployed build:"
  ssh "${SSH_OPTS[@]}" "$TARGET" "$DIR/weatherstation -version" | sed 's/^/    /'
  status="$(ssh "${SSH_OPTS[@]}" "$TARGET" "$STATUS $SERVICE" || true)"
  echo "==> Service '$SERVICE': $status"
  [ "$status" = active ] || die "service is not active after restart (status: $status)"
else
  echo "==> Swapping in the new binary (service not restarted) ..."
  ssh "${SSH_OPTS[@]}" "$TARGET" "$swap"
fi
echo "==> Done."
