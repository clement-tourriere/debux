#!/bin/sh
set -e

# If --user runs us as a non-root UID, /root is not writable. Keep the shell
# functional with a per-UID home owned by the current user.
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
export DEBUX_TARGET_ROOT="/target"
# Stopped-container snapshots supply an explicit saved environ file.
: "${DEBUX_TARGET_ENVIRON:=/dev/null}"
: "${DEBUX_TARGET_CWD_LINK:=/dev/null}"
export DEBUX_TARGET_ENVIRON DEBUX_TARGET_CWD_LINK

# Ensure persistent data directory exists (for shell history etc.)
if [ -n "${DEBUX_DATA_DIR:-}" ]; then
  mkdir -p "$DEBUX_DATA_DIR" 2>/dev/null || mkdir -p /tmp/debux-data
else
  mkdir -p /nix/var/debux-data 2>/dev/null || mkdir -p /tmp/debux-data
fi

# Ensure XDG config directory exists so tools can write their configs
mkdir -p "${HOME:-/tmp}/.config" 2>/dev/null || true

# Write shell configuration (ZDOTDIR not used for image debugging)
cat > "${HOME:-/tmp}/.zshrc" << 'ZSHRC_EOF'
__DEBUX_ZSHRC__
ZSHRC_EOF

echo "Image filesystem available at /target"
echo ""

# Launch shell (or run a one-shot command passed by the CLI)
if [ -n "${DEBUX_EXEC_COMMAND:-}" ]; then
  export DEBUX_BANNER_SHOWN=1
  exec zsh -ic "$DEBUX_EXEC_COMMAND"
fi
exec zsh
