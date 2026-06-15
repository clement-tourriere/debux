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
export PATH="/nix/var/debux-profile/bin:/usr/local/bin:${HOME:-/tmp}/.nix-profile/bin:$PATH"

# Export target root for easy access
export DEBUX_TARGET_ROOT="/target"

# Ensure persistent data directory exists (for shell history etc.)
mkdir -p /nix/var/debux-data 2>/dev/null || mkdir -p /tmp/debux-data

# Ensure XDG config directory exists so tools can write their configs
mkdir -p "${HOME:-/tmp}/.config" 2>/dev/null || true

# Write shell configuration (ZDOTDIR not used for image debugging)
cat > "${HOME:-/tmp}/.zshrc" << 'ZSHRC_EOF'
# debux shell configuration

# Ensure terminal-aware programs have a usable terminal type.
if [[ -z "${TERM:-}" || "$TERM" == "dumb" ]]; then
  export TERM=xterm
else
  export TERM
fi

# Enable syntax highlighting
if [[ -f "${HOME:-/tmp}/.nix-profile/share/zsh-syntax-highlighting/zsh-syntax-highlighting.zsh" ]]; then
  source "${HOME:-/tmp}/.nix-profile/share/zsh-syntax-highlighting/zsh-syntax-highlighting.zsh"
fi

# Enable autosuggestions
if [[ -f "${HOME:-/tmp}/.nix-profile/share/zsh-autosuggestions/zsh-autosuggestions.zsh" ]]; then
  source "${HOME:-/tmp}/.nix-profile/share/zsh-autosuggestions/zsh-autosuggestions.zsh"
fi

# Source command-not-found handler
if [[ -f /etc/zsh/command-not-found-handler ]]; then
  source /etc/zsh/command-not-found-handler
fi

# Prompt
target="${DEBUX_TARGET:-unknown}"
PS1="%F{cyan}[debux]%f %F{magenta}image:${target}%f %F{blue}%~%f %# "

if [[ -o interactive && -z "${DEBUX_BANNER_SHOWN:-}" ]]; then
  export DEBUX_BANNER_SHOWN=1
  print -P "%F{cyan}debux%f image debug · target filesystem: %F{yellow}/target%f"
  echo "  target    cd into image filesystem"
  echo "  dctl      install/list/remove Nix tools"
  echo ""
fi

# History — stored on persistent volume so it survives container restarts
if [[ -d /nix/var/debux-data && -w /nix/var/debux-data ]]; then
  HISTFILE=/nix/var/debux-data/.zsh_history
else
  HISTFILE=/tmp/debux-data/.zsh_history
fi
HISTSIZE=10000
SAVEHIST=10000
setopt SHARE_HISTORY
setopt HIST_IGNORE_DUPS
setopt HIST_IGNORE_SPACE
setopt HIST_REDUCE_BLANKS
setopt INC_APPEND_HISTORY

# Aliases
alias l='ls -lah --color=auto'
alias ll='ls -alh --color=auto'
alias la='ls -A --color=auto'
alias ls='ls --color=auto'
alias grep='grep --color=auto'
alias ..='cd ..'
alias ...='cd ../..'
alias md='mkdir -p'
alias rd='rmdir'

# Target filesystem shortcut
alias target='cd $DEBUX_TARGET_ROOT'

# Wrap dctl to rehash after install/remove so new binaries are found immediately
dctl() { command dctl "$@"; local ret=$?; rehash; return $ret; }

# Auto-install requested tool sets (--tools / config file) once per container
if [[ -n "${DEBUX_TOOLS:-}" ]]; then
  _debux_tools_marker="${DEBUX_TOOLS//[^A-Za-z0-9._-]/-}"
  _debux_tools_marker="${HOME:-/tmp}/.debux-tools-${_debux_tools_marker}"
  if [[ ! -e "$_debux_tools_marker" ]]; then
    printf '%s\n' "Installing requested tools: ${DEBUX_TOOLS}"
    for _debux_tool in ${(s. .)DEBUX_TOOLS}; do
      dctl install "$_debux_tool" || printf '  install failed: %s (try: dctl search %s)\n' "$_debux_tool" "$_debux_tool"
    done
    : > "$_debux_tools_marker" 2>/dev/null || true
  fi
  unset _debux_tools_marker _debux_tool
fi

# Key bindings
bindkey -e
ZSHRC_EOF

echo "Image filesystem available at /target"
echo ""

# Launch shell (or run a one-shot command passed by the CLI)
if [ -n "${DEBUX_EXEC_COMMAND:-}" ]; then
  export DEBUX_BANNER_SHOWN=1
  exec zsh -ic "$DEBUX_EXEC_COMMAND"
fi
exec zsh
