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

# If --user runs us as a non-root UID, /root is not writable. Keep the shell
# functional by falling back to /tmp for startup files and history.
if [ -z "${HOME:-}" ] || { [ -d "$HOME" ] && [ ! -w "$HOME" ]; }; then
  export HOME=/tmp
fi
export ZDOTDIR=/tmp

# Ensure zsh never falls into zsh-newuser-install in non-root/restricted mode.
cat > /tmp/.zshenv << 'ZSHENV_EOF'
export ZDOTDIR=/tmp
ZSHENV_EOF
if [ ! -s /tmp/.zshrc ]; then
  if [ -r /etc/debux/zshrc ]; then
    cp /etc/debux/zshrc /tmp/.zshrc 2>/dev/null || touch /tmp/.zshrc
  else
    touch /tmp/.zshrc
  fi
fi

# Ensure PATH includes all tool locations
# /nix/var/debux-profile/bin = user-installed packages via dctl
export PATH="/nix/var/debux-profile/bin:/usr/local/bin:${HOME:-/tmp}/.nix-profile/bin:$PATH"

# Export target root for easy access
export DEBUX_TARGET_ROOT="/proc/1/root"
ln -sfn "$DEBUX_TARGET_ROOT" /target 2>/dev/null || true

# Create convenience symlinks for target filesystem
ln -sf "$DEBUX_TARGET_ROOT/etc/hosts" /etc/hosts 2>/dev/null || true
ln -sf "$DEBUX_TARGET_ROOT/etc/resolv.conf" /etc/resolv.conf 2>/dev/null || true

# Ensure persistent data directory exists (for shell history etc.)
mkdir -p /nix/var/debux-data 2>/dev/null || mkdir -p /tmp/debux-data

# Launch shell (or daemon mode for k8s container reuse)
if [ "${DEBUX_DAEMON:-}" = "1" ]; then
  exec tail -f /dev/null
fi
exec zsh
