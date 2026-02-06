package entrypoint

// Script is the entrypoint script injected into the debug container.
// It waits for the target's PID namespace to be visible, sets up
// convenience symlinks, writes the shell configuration, and launches zsh.
//
// The zshrc is written at runtime (rather than relying on the baked-in
// image copy) so that Go rebuilds pick up config changes immediately
// without requiring a Docker image rebuild+push.
const Script = `#!/bin/sh
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
export PATH="/nix/var/debux-profile/bin:/usr/local/bin:${HOME:-/tmp}/.nix-profile/bin:$PATH"

# Export target root for easy access
export DEBUX_TARGET_ROOT="/proc/1/root"

# Create convenience symlinks for target filesystem
ln -sf "$DEBUX_TARGET_ROOT/etc/hosts" /etc/hosts 2>/dev/null || true
ln -sf "$DEBUX_TARGET_ROOT/etc/resolv.conf" /etc/resolv.conf 2>/dev/null || true

# Ensure persistent data directory exists (for shell history etc.)
mkdir -p /nix/var/debux-data 2>/dev/null || mkdir -p /tmp/debux-data

# Ensure XDG config directory exists so tools can write their configs
mkdir -p "${HOME:-/tmp}/.config" 2>/dev/null || true

# Determine writable home for zshrc
DEBUX_HOME="${HOME:-/tmp}"
if [ ! -w "$DEBUX_HOME" ]; then
  DEBUX_HOME="/tmp"
fi

# Write shell configuration (overrides image default)
cat > "$DEBUX_HOME/.zshrc" << 'ZSHRC_EOF'
# debux shell configuration

# Ensure PATH includes all tool locations (needed for exec sessions in daemon mode)
export PATH="/nix/var/debux-profile/bin:/usr/local/bin:${HOME:-/tmp}/.nix-profile/bin:${PATH}"
export DEBUX_TARGET_ROOT="${DEBUX_TARGET_ROOT:-/proc/1/root}"

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
PS1="%F{cyan}[debux]%f %F{yellow}${target}%f %F{blue}%~%f %# "

# History — stored on persistent volume so it survives container restarts
if [[ -d /nix/var/debux-data ]]; then
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

# Import target container environment variables
_debux_import_target_env() {
  local environ_file="/proc/1/environ"
  [[ -f "$environ_file" ]] || return 0

  local -a skip_exact=(
    HOME USER LOGNAME SHELL TERM HOSTNAME PWD OLDPWD SHLVL _ TMPDIR
    NOTIFY_SOCKET SSH_AUTH_SOCK XDG_RUNTIME_DIR container
  )
  local -a path_colon_vars=(
    PYTHONPATH LD_LIBRARY_PATH MANPATH PERL5LIB NODE_PATH
    GEM_PATH GOPATH CLASSPATH PKG_CONFIG_PATH
  )
  local -a path_single_vars=(
    VIRTUAL_ENV JAVA_HOME CONDA_PREFIX GEM_HOME GOROOT
    CARGO_HOME RUSTUP_HOME NVM_DIR PYENV_ROOT RBENV_ROOT
  )

  local key val entry
  while IFS= read -r -d '' entry; do
    key="${entry%%=*}"
    val="${entry#*=}"
    [[ -z "$key" || "$key" == "$entry" ]] && continue

    # Skip blocklist: exact matches
    if (( ${skip_exact[(Ie)$key]} )); then
      continue
    fi
    # Skip blocklist: pattern matches
    if [[ "$key" == LANG || "$key" == LC_* || "$key" == DEBUX_* || "$key" == KUBERNETES_* ]]; then
      continue
    fi

    if [[ "$key" == "PATH" ]]; then
      # Translate each PATH component and append to current PATH
      local -a translated=()
      local component
      while IFS= read -r -d ':' component || [[ -n "$component" ]]; do
        translated+=("${DEBUX_TARGET_ROOT}${component}")
      done <<< "$val"
      export PATH="${PATH}:${(j.:.)translated}"

    elif (( ${path_colon_vars[(Ie)$key]} )); then
      # Colon-separated path vars: translate each component
      local -a translated=()
      local component
      while IFS= read -r -d ':' component || [[ -n "$component" ]]; do
        translated+=("${DEBUX_TARGET_ROOT}${component}")
      done <<< "$val"
      export "$key"="${(j.:.)translated}"

    elif (( ${path_single_vars[(Ie)$key]} )); then
      # Single-path vars: prepend target root
      export "$key"="${DEBUX_TARGET_ROOT}${val}"

    else
      # Everything else: export as-is
      export "$key"="$val"
    fi
  done < <(command cat "$environ_file" 2>/dev/null)
}
_debux_import_target_env
unfunction _debux_import_target_env

# Key bindings
bindkey -e
ZSHRC_EOF

# Show shared volumes (read /proc/self/mounts directly — no external 'mount' command needed)
echo "Volumes from target:"
awk '!/\/(nix|proc|sys|dev)|overlay/{print "  " $2 " (" $3 ")"}' /proc/self/mounts 2>/dev/null || true
echo ""

# Launch shell (or daemon mode for k8s container reuse)
if [ "${DEBUX_DAEMON:-}" = "1" ]; then
  exec tail -f /dev/null
fi
exec zsh
`

// ImageScript is the entrypoint script for image debugging.
// Unlike Script, it does NOT wait for PID namespace sharing (there is no
// running target process). The image filesystem is copied into /target.
const ImageScript = `#!/bin/sh
set -e

# Ensure PATH includes all tool locations
export PATH="/nix/var/debux-profile/bin:/usr/local/bin:${HOME:-/tmp}/.nix-profile/bin:$PATH"

# Export target root for easy access
export DEBUX_TARGET_ROOT="/target"

# Ensure persistent data directory exists (for shell history etc.)
mkdir -p /nix/var/debux-data 2>/dev/null || mkdir -p /tmp/debux-data

# Ensure XDG config directory exists so tools can write their configs
mkdir -p "${HOME:-/tmp}/.config" 2>/dev/null || true

# Determine writable home for zshrc
DEBUX_HOME="${HOME:-/tmp}"
if [ ! -w "$DEBUX_HOME" ]; then
  DEBUX_HOME="/tmp"
fi

# Write shell configuration (overrides image default)
cat > "$DEBUX_HOME/.zshrc" << 'ZSHRC_EOF'
# debux shell configuration

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

# History — stored on persistent volume so it survives container restarts
if [[ -d /nix/var/debux-data ]]; then
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

# Key bindings
bindkey -e
ZSHRC_EOF

echo "Image filesystem available at /target"
echo ""

# Launch shell
exec zsh
`
