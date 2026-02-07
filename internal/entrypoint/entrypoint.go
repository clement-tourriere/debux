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

# Command-not-found handler with chroot fallback for target binaries
command_not_found_handler() {
  local cmd="$1"
  shift

  # Check if command exists in target container by searching its PATH dirs
  if [[ -n "$DEBUX_TARGET_ROOT" && -d "$DEBUX_TARGET_ROOT" ]]; then
    local target_bin=""
    # Read target's PATH from /proc/1/environ
    local target_path=""
    if [[ -f /proc/1/environ ]]; then
      target_path=$(command tr '\0' '\n' < /proc/1/environ 2>/dev/null | command sed -n 's/^PATH=//p')
    fi
    [[ -z "$target_path" ]] && target_path="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    local search_dir
    while IFS= read -r -d ':' search_dir || [[ -n "$search_dir" ]]; do
      if [[ -x "${DEBUX_TARGET_ROOT}${search_dir}/${cmd}" || -L "${DEBUX_TARGET_ROOT}${search_dir}/${cmd}" ]]; then
        target_bin="${search_dir}/${cmd}"
        break
      fi
    done <<< "$target_path"

    if [[ -n "$target_bin" ]]; then
      # Run via chroot with target's full original environment (same as docker exec)
      local save_dir="$PWD"
      case "$PWD" in
        "${DEBUX_TARGET_ROOT}"/*) ;;
        *) cd "$DEBUX_TARGET_ROOT" 2>/dev/null || true ;;
      esac
      local -a target_env=()
      local entry
      while IFS= read -r -d '' entry; do
        target_env+=("$entry")
      done < /proc/1/environ 2>/dev/null
      local chroot_bin=$(command -v chroot)
      env -i "${target_env[@]}" TERM="$TERM" \
        "$chroot_bin" --skip-chdir "$DEBUX_TARGET_ROOT" "$target_bin" "$@"
      local ret=$?
      cd "$save_dir" 2>/dev/null || true
      return $ret
    fi
  fi

  # Fallback: offer to install via dctl
  echo -e "\e[33m$cmd\e[0m: command not found"
  echo ""
  echo -e "  Install with: \e[32mdctl install $cmd\e[0m"
  echo ""
  read "REPLY?  Install now? [y/N] "
  if [[ "$REPLY" =~ ^[Yy]$ ]]; then
    if dctl install "$cmd"; then
      command "$cmd" "$@"
      return $?
    else
      echo ""
      echo "  Package '$cmd' not found. Try: dctl search $cmd"
    fi
  fi

  return 127
}

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
setopt AUTO_CD

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

  # Save sidecar's PATH before target env modification (used by wrapper generator)
  _debux_sidecar_path="$PATH"

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
      # Save original target PATH for wrapper generation
      _debux_target_path="$val"
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

# Generate chroot wrapper scripts for target binaries
_debux_generate_wrappers() {
  [[ -z "$DEBUX_TARGET_ROOT" || ! -d "$DEBUX_TARGET_ROOT" ]] && return 0
  [[ -z "$_debux_target_path" ]] && return 0

  local wrapper_dir="/tmp/debux-target-bin"
  mkdir -p "$wrapper_dir"

  # Create shared chroot-exec helper
  # Restores the target container's full original environment from
  # /proc/1/environ before chroot+exec — same env as "docker exec".
  # CWD is preserved by --skip-chdir: /proc/1/root/app becomes /app.
  cat > "$wrapper_dir/.chroot-exec" << 'HELPER_EOF'
#!/bin/sh
TARGET_ROOT="${DEBUX_TARGET_ROOT:-/proc/1/root}"
CHROOT=$(command -v chroot)
cmd="$1"; shift
case "$PWD" in
  "${TARGET_ROOT}"/*) ;;
  *) cd "$TARGET_ROOT" 2>/dev/null || true ;;
esac
# Restore target container's original environment
while IFS= read -r line; do
  case "$line" in *=*) export "$line" ;; esac
done <<ENVEOF
$(tr '\0' '\n' < /proc/1/environ 2>/dev/null)
ENVEOF
exec "$CHROOT" --skip-chdir "$TARGET_ROOT" "$cmd" "$@"
HELPER_EOF
  chmod +x "$wrapper_dir/.chroot-exec"

  # Collect sidecar's own binaries from the pre-modification PATH
  local -A sidecar_cmds
  local p
  while IFS= read -r -d ':' p || [[ -n "$p" ]]; do
    [[ -d "$p" ]] || continue
    for f in "$p"/*(-.:t N); do
      sidecar_cmds[$f]=1
    done
  done <<< "$_debux_sidecar_path"

  # Walk each target PATH dir and create wrappers for missing commands
  local dir
  while IFS= read -r -d ':' dir || [[ -n "$dir" ]]; do
    local target_dir="${DEBUX_TARGET_ROOT}${dir}"
    [[ -d "$target_dir" ]] || continue
    for bin_path in "$target_dir"/*(N^/); do
      local bin_name="${bin_path:t}"
      # Skip if sidecar already has this command or wrapper already exists
      (( ${+sidecar_cmds[$bin_name]} )) && continue
      [[ -e "$wrapper_dir/$bin_name" ]] && continue
      # Create a one-line wrapper
      printf '#!/bin/sh\nexec /tmp/debux-target-bin/.chroot-exec "%s" "$@"\n' "${dir}/${bin_name}" > "$wrapper_dir/$bin_name"
      chmod +x "$wrapper_dir/$bin_name"
    done
  done <<< "$_debux_target_path"

  # Prepend wrapper dir to PATH (before /proc/1/root/... entries)
  export PATH="$wrapper_dir:$PATH"
  unset _debux_target_path _debux_sidecar_path
}
_debux_generate_wrappers
unfunction _debux_generate_wrappers

# Auto-cd to target container's working directory
if [[ -n "$DEBUX_TARGET_ROOT" && -r /proc/1/cwd ]]; then
  _debux_target_cwd=$(readlink /proc/1/cwd 2>/dev/null)
  if [[ -n "$_debux_target_cwd" && -d "${DEBUX_TARGET_ROOT}${_debux_target_cwd}" ]]; then
    cd "${DEBUX_TARGET_ROOT}${_debux_target_cwd}"
  elif [[ -d "$DEBUX_TARGET_ROOT" ]]; then
    cd "$DEBUX_TARGET_ROOT"
  fi
  unset _debux_target_cwd
fi

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
