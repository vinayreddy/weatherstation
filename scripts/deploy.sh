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
DO_UNIT=1
DO_CGROUP=1
DO_REBOOT=0
GOMEMLIMIT='900MiB'
MEMORY_MAX='1G'
ENVFILE='.env.production'
BACKFILL=''
CGROUP_REBOOT_NEEDED=0
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
      --no-unit       do not install/update the systemd unit (deploy binary only)
      --no-cgroup     do not auto-enable the memory cgroup on the target
      --reboot        reboot the target if enabling the memory cgroup needs it
      --env-file NAME env file the binary loads via -env   (default: .env.production)
      --backfill DATE pass -backfill DATE in ExecStart      (default: none; auto-resumes)
      --memory-max V  systemd MemoryMax for the unit        (default: 1G, sized for 4 GB)
      --gomemlimit V  GOMEMLIMIT for the unit               (default: 900MiB; keep < MemoryMax)
  -h, --help          show this help
EOF
}
die() { echo "error: $*" >&2; exit 1; }

# ensure_cgroup_memory makes sure the memory cgroup controller is active on the
# target — otherwise MemoryMax/MemorySwapMax in the unit are SILENTLY IGNORED. If
# it's off, it appends `cgroup_enable=memory cgroup_memory=1` to the (single-line)
# boot cmdline, backing it up first, and flags CGROUP_REBOOT_NEEDED so the caller
# prompts for the reboot that makes it take effect. Skipped with --no-cgroup.
ensure_cgroup_memory() {
  echo "==> Checking memory cgroup controller on $TARGET ..."
  local active
  active="$(ssh "${SSH_OPTS[@]}" "$TARGET" bash -s <<'REMOTE'
if [ -f /sys/fs/cgroup/cgroup.controllers ] && grep -qw memory /sys/fs/cgroup/cgroup.controllers; then
  echo active
elif [ -f /proc/cgroups ] && awk '$1=="memory"{f=1; if($4==1) ok=1} END{exit !(f&&ok)}' /proc/cgroups; then
  echo active
fi
REMOTE
)" || true
  if [ "$active" = active ]; then
    echo "    memory cgroup controller is active"
    return 0
  fi

  local cmdline
  cmdline="$(ssh "${SSH_OPTS[@]}" "$TARGET" 'for f in /boot/firmware/cmdline.txt /boot/cmdline.txt; do [ -f "$f" ] && { echo "$f"; break; }; done')"
  if [ -z "$cmdline" ]; then
    echo "    !! WARNING: memory cgroup is OFF and no cmdline.txt found — cannot"
    echo "    !! auto-enable it; MemoryMax/MemorySwapMax will be ignored."
    return 0
  fi

  # Already patched but awaiting a reboot?
  if ssh "${SSH_OPTS[@]}" "$TARGET" "grep -q 'cgroup_enable=memory' '$cmdline'"; then
    echo "    cgroup params already present in $cmdline — reboot pending to activate"
    CGROUP_REBOOT_NEEDED=1
    return 0
  fi

  if [ "$DO_CGROUP" -eq 0 ]; then
    echo "    !! memory cgroup is OFF and --no-cgroup set — not touching $cmdline."
    echo "    !! MemoryMax/MemorySwapMax will be ignored until enabled + rebooted."
    return 0
  fi

  echo "    enabling memory cgroup in $cmdline (backup: $cmdline.bak) ..."
  # `1 s` edits only the first line and never inserts a newline, so cmdline.txt
  # stays the single line the bootloader requires.
  ssh "${SSH_OPTS[@]}" "$TARGET" "
    sudo cp -n '$cmdline' '$cmdline.bak' &&
    sudo sed -i '1 s|\$| cgroup_enable=memory cgroup_memory=1|' '$cmdline' &&
    grep -q 'cgroup_enable=memory' '$cmdline'
  " || die "failed to enable memory cgroup in $cmdline"
  CGROUP_REBOOT_NEEDED=1
}

# install_unit renders deploy/weatherstation.service (filling in the remote abs
# dir/user, MemoryMax, GOMEMLIMIT, ExecStart args, and install target) and
# installs + enables it via systemd.
install_unit() {
  local unit_src="$REPO_ROOT/deploy/weatherstation.service"
  [ -f "$unit_src" ] || die "unit template not found: $unit_src"

  echo "==> Resolving remote install dir + user for the unit ..."
  local abs_dir remote_user
  abs_dir="$(ssh "${SSH_OPTS[@]}" "$TARGET" "cd $DIR && pwd")" || die "could not resolve remote dir $DIR"
  remote_user="$(ssh "${SSH_OPTS[@]}" "$TARGET" 'id -un')" || die "could not resolve remote user"

  # The binary loads config via -env; warn (don't fail) if it's missing, since
  # loadEnvFile fatals on a missing non-default env file.
  if ! ssh "${SSH_OPTS[@]}" "$TARGET" "test -f '$abs_dir/$ENVFILE'"; then
    echo "    !! WARNING: $abs_dir/$ENVFILE not found on $TARGET — the service loads"
    echo "    !! config via '-env $ENVFILE' and will fail to start without it."
  fi

  # Compose the ExecStart args. -backfill is only needed once to set the origin;
  # main.go auto-resumes any incomplete backfill from the DB cursor, so it's
  # omitted by default.
  local execargs="-env $ENVFILE -fork_and_monitor=false"
  [ -n "$BACKFILL" ] && execargs="-env $ENVFILE -backfill $BACKFILL -fork_and_monitor=false"

  local wantedby='multi-user.target'
  [ "$USE_USER" -eq 1 ] && wantedby='default.target'

  local tmp_unit; tmp_unit="$(mktemp)"
  sed -e "s|@@DIR@@|$abs_dir|g" \
      -e "s|@@USER@@|$remote_user|g" \
      -e "s|@@GOMEMLIMIT@@|$GOMEMLIMIT|g" \
      -e "s|@@MEMORY_MAX@@|$MEMORY_MAX|g" \
      -e "s|@@EXECARGS@@|$execargs|g" \
      -e "s|@@WANTEDBY@@|$wantedby|g" \
      "$unit_src" > "$tmp_unit"
  # A --user unit must not set User=.
  if [ "$USE_USER" -eq 1 ]; then
    sed -i.bak '/^User=/d' "$tmp_unit" && rm -f "$tmp_unit.bak"
  fi

  echo "==> Installing systemd unit '$SERVICE.service' on $TARGET (MemoryMax=$MEMORY_MAX, GOMEMLIMIT=$GOMEMLIMIT) ..."
  scp "${SSH_OPTS[@]}" "$tmp_unit" "$TARGET:/tmp/$SERVICE.service.new"
  rm -f "$tmp_unit"
  if [ "$USE_USER" -eq 1 ]; then
    ssh "${SSH_OPTS[@]}" "$TARGET" "
      mkdir -p ~/.config/systemd/user &&
      install -m0644 /tmp/$SERVICE.service.new ~/.config/systemd/user/$SERVICE.service &&
      rm -f /tmp/$SERVICE.service.new &&
      systemctl --user daemon-reload &&
      systemctl --user enable $SERVICE &&
      (loginctl enable-linger '$remote_user' 2>/dev/null || true)
    " || die "user unit install failed"
  else
    ssh "${SSH_OPTS[@]}" "$TARGET" "
      sudo install -m0644 /tmp/$SERVICE.service.new /etc/systemd/system/$SERVICE.service &&
      rm -f /tmp/$SERVICE.service.new &&
      sudo systemctl daemon-reload &&
      sudo systemctl enable $SERVICE
    " || die "unit install failed (needs sudo on $TARGET)"
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    -d|--dir)     DIR="$2"; shift 2 ;;
    -s|--service) SERVICE="$2"; shift 2 ;;
    --arch)       ARCH="$2"; shift 2 ;;
    --user)       USE_USER=1; shift ;;
    --no-build)   DO_BUILD=0; shift ;;
    --no-restart) DO_RESTART=0; shift ;;
    --no-unit)    DO_UNIT=0; shift ;;
    --no-cgroup)  DO_CGROUP=0; shift ;;
    --reboot)     DO_REBOOT=1; shift ;;
    --env-file)   ENVFILE="$2"; shift 2 ;;
    --backfill)   BACKFILL="$2"; shift 2 ;;
    --memory-max) MEMORY_MAX="$2"; shift 2 ;;
    --gomemlimit) GOMEMLIMIT="$2"; shift 2 ;;
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

# Install/refresh the hardened systemd unit before restarting so the restart
# picks up any changes. Skipped with --no-unit. Ensure the memory cgroup is on
# first, else the unit's MemoryMax is a no-op.
if [ "$DO_UNIT" -eq 1 ]; then
  ensure_cgroup_memory
  install_unit
fi

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

# A cmdline change to enable the memory cgroup only takes effect after a reboot.
if [ "$CGROUP_REBOOT_NEEDED" -eq 1 ]; then
  if [ "$DO_REBOOT" -eq 1 ]; then
    echo "==> Rebooting $TARGET to activate the memory cgroup ..."
    ssh "${SSH_OPTS[@]}" "$TARGET" 'sudo reboot' || true
    echo "    reboot issued (this also bounces other services on the host)."
  else
    echo
    echo "    ****************************************************************"
    echo "    * REBOOT REQUIRED: the memory cgroup was just enabled on the   *"
    echo "    * boot cmdline but is not active until $TARGET reboots.        *"
    echo "    * MemoryMax/MemorySwapMax stay INACTIVE until then.            *"
    echo "    * Reboot when ready (also bounces nginx/homebridge):          *"
    echo "    *     ssh $TARGET sudo reboot"
    echo "    * Verify after:  systemctl show $SERVICE -p MemoryMax          *"
    echo "    ****************************************************************"
  fi
fi
echo "==> Done."
