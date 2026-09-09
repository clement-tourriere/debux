#!/bin/sh
set -e

# Wait for target PID 1 to be visible (namespace sharing)
timeout_ticks=300
elapsed=0
while [ ! -d /proc/1/root ] && [ "$elapsed" -lt "$timeout_ticks" ]; do
  sleep 0.1
  elapsed=$((elapsed + 1))
done

if [ ! -d /proc/1/root ]; then
  echo "Warning: could not find target process namespace"
fi

# If --user/profile=restricted runs us as a non-root UID, /root is not writable.
# Use a per-UID home owned by the current user so tools do not fall back
# to zsh-newuser-install or /var/empty.
if [ -z "${HOME:-}" ] || [ ! -d "$HOME" ] || [ ! -w "$HOME" ]; then
  debux_uid="$(id -u 2>/dev/null || echo 0)"
  export HOME="/tmp/debux-home-$debux_uid"
  mkdir -p "$HOME" 2>/dev/null || export HOME=/tmp
  unset debux_uid
fi

# Ensure PATH includes all tool locations
if [ -r /etc/debux/environment.sh ]; then
  # shellcheck disable=SC1091
  . /etc/debux/environment.sh
else
  export PATH="/nix/var/debux-profile/bin:/usr/local/bin:${HOME:-/tmp}/.nix-profile/bin:$PATH"
fi

# Export target root for easy access
: "${DEBUX_TARGET_ROOT:=/proc/1/root}"
: "${DEBUX_TARGET_ENVIRON:=/proc/1/environ}"
: "${DEBUX_TARGET_CWD_LINK:=/proc/1/cwd}"
export DEBUX_TARGET_ROOT DEBUX_TARGET_ENVIRON DEBUX_TARGET_CWD_LINK

# Create convenience symlinks for target filesystem
ln -sf "$DEBUX_TARGET_ROOT/etc/hosts" /etc/hosts 2>/dev/null || true
ln -sf "$DEBUX_TARGET_ROOT/etc/resolv.conf" /etc/resolv.conf 2>/dev/null || true

# Ensure persistent data directory exists (for shell history etc.)
if [ -n "${DEBUX_DATA_DIR:-}" ]; then
  mkdir -p "$DEBUX_DATA_DIR" 2>/dev/null || mkdir -p /tmp/debux-data
else
  mkdir -p /nix/var/debux-data 2>/dev/null || mkdir -p /tmp/debux-data
fi

# Ensure XDG config directory exists so tools can write their configs
mkdir -p "${HOME:-/tmp}/.config" 2>/dev/null || true

export ZDOTDIR=/tmp

# Write .zshenv to set ZDOTDIR for all zsh sessions (including exec).
# Keep it in /tmp so --user non-root sessions can start too.
cat > /tmp/.zshenv << 'ZSHENV_EOF'
export ZDOTDIR=/tmp
ZSHENV_EOF

# Write shell configuration to /tmp/.zshrc (ZDOTDIR points here)
cat > /tmp/.zshrc << 'ZSHRC_EOF'
__DEBUX_ZSHRC__
ZSHRC_EOF

# Show shared volumes (read /proc/self/mounts directly — no external 'mount' command needed)
# Match the mountpoint/type fields, not the whole line: a device like /dev/sda1
# in field 1 must not hide a data volume mounted at /data.
echo "Volumes from target:"
awk '$2 !~ /^\/(nix|proc|sys|dev|tmp|var\/lib\/debux)(\/|$)/ && $3 != "overlay" {print "  " $2 " (" $3 ")"}' /proc/self/mounts 2>/dev/null || true
echo ""

# Launch shell (or daemon mode for k8s container reuse)
if [ "${DEBUX_DAEMON:-}" = "1" ]; then
  # Record the daemon PID (as seen in the shared PID namespace) so debux kill
  # can signal the daemon instead of the target's PID 1. Use $BASHPID, not $$:
  # Kubernetes collapses a literal $$ to $ during command substitution, and the
  # image's /bin/sh is bash. ${BASHPID:-$$} keeps Docker (no substitution)
  # working even on a non-bash shell.
  # Intentional bash extension with a POSIX fallback (the image's sh is bash).
  # shellcheck disable=SC3028
  echo "${BASHPID:-$$}" > /tmp/.debux-daemon.pid 2>/dev/null || true
  trap 'exit 0' TERM INT
  while :; do sleep 2147483647 & wait $!; done
fi
exec zsh
