#!/bin/sh
set -e

# Wait for target PID 1 to be visible (namespace sharing)
timeout=30
elapsed=0
while [ ! -d /proc/1/root ] && [ "$elapsed" -lt "$timeout" ]; do
  sleep 0.1
  elapsed=$((elapsed + 1))
done

if [ ! -d /proc/1/root ]; then
  echo "Warning: could not find target process namespace"
fi

# Ensure PATH includes all tool locations
# /nix/var/debux-profile/bin = user-installed packages via dctl
export PATH="/nix/var/debux-profile/bin:/usr/local/bin:/root/.nix-profile/bin:$PATH"

# Export target root for easy access
export DEBUX_TARGET_ROOT="/proc/1/root"
ln -sfn "$DEBUX_TARGET_ROOT" /target 2>/dev/null || true

# Create convenience symlinks for target filesystem
ln -sf "$DEBUX_TARGET_ROOT/etc/hosts" /etc/hosts 2>/dev/null || true
ln -sf "$DEBUX_TARGET_ROOT/etc/resolv.conf" /etc/resolv.conf 2>/dev/null || true

# Ensure persistent data directory exists (for shell history etc.)
mkdir -p /nix/var/debux-data

# Launch shell (or daemon mode for k8s container reuse)
if [ "${DEBUX_DAEMON:-}" = "1" ]; then
  exec tail -f /dev/null
fi
exec zsh
