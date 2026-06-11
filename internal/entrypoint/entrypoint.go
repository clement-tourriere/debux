package entrypoint

import "strings"

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
# Use a per-UID home owned by the current user so zsh and Nix do not fall back
# to zsh-newuser-install or /var/empty.
if [ -z "${HOME:-}" ] || [ ! -d "$HOME" ] || [ ! -w "$HOME" ]; then
  debux_uid="$(id -u 2>/dev/null || echo 0)"
  export HOME="/tmp/debux-home-$debux_uid"
  mkdir -p "$HOME" 2>/dev/null || export HOME=/tmp
  unset debux_uid
fi

# Ensure PATH includes all tool locations
# /nix/var/debux-profile/bin = user-installed packages via dctl
export PATH="/nix/var/debux-profile/bin:/usr/local/bin:${HOME:-/tmp}/.nix-profile/bin:$PATH"

# Export target root for easy access
: "${DEBUX_TARGET_ROOT:=/proc/1/root}"
: "${DEBUX_TARGET_ENVIRON:=/proc/1/environ}"
: "${DEBUX_TARGET_CWD_LINK:=/proc/1/cwd}"
export DEBUX_TARGET_ROOT DEBUX_TARGET_ENVIRON DEBUX_TARGET_CWD_LINK

# Create convenience symlinks for target filesystem
ln -sf "$DEBUX_TARGET_ROOT/etc/hosts" /etc/hosts 2>/dev/null || true
ln -sf "$DEBUX_TARGET_ROOT/etc/resolv.conf" /etc/resolv.conf 2>/dev/null || true

# Ensure persistent data directory exists (for shell history etc.)
mkdir -p /nix/var/debux-data 2>/dev/null || mkdir -p /tmp/debux-data

# Ensure XDG config directory exists so tools can write their configs
mkdir -p "${HOME:-/tmp}/.config" 2>/dev/null || true

# Write .zshenv to set ZDOTDIR for all zsh sessions (including exec).
# Keep it in /tmp so --user non-root sessions can start too.
cat > /tmp/.zshenv << 'ZSHENV_EOF'
export ZDOTDIR=/tmp
ZSHENV_EOF

# Write shell configuration to /tmp/.zshrc (ZDOTDIR points here)
cat > /tmp/.zshrc << 'ZSHRC_EOF'
# debux shell configuration

# Ensure PATH includes all tool locations (needed for exec sessions in daemon mode)
export PATH="/nix/var/debux-profile/bin:/usr/local/bin:${HOME:-/tmp}/.nix-profile/bin:${PATH}"
export DEBUX_TARGET_ROOT="${DEBUX_TARGET_ROOT:-/proc/1/root}"

# Ensure terminal-aware programs have a usable terminal type. Docker/Kubernetes
# exec sessions and target process environments do not always provide TERM.
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

# Command-not-found handler with chroot fallback for target binaries
command_not_found_handler() {
  local cmd="$1"
  shift

  # Check if command exists in target container by searching its PATH dirs
  if [[ -n "$DEBUX_TARGET_ROOT" && -d "$DEBUX_TARGET_ROOT" ]]; then
    local target_bin=""
    # Read target's PATH from /proc/1/environ. Some processes (notably nginx)
    # overwrite argv/environ memory for process titles, so treat environ as
    # untrusted and ignore malformed entries.
    local target_path=""
    if [[ -f "${DEBUX_TARGET_ENVIRON:-/proc/1/environ}" ]]; then
      local env_entry env_key
      while IFS= read -r -d '' env_entry; do
        [[ "$env_entry" == *=* ]] || continue
        env_key="${env_entry%%=*}"
        [[ "$env_key" == PATH ]] || continue
        target_path="${env_entry#*=}"
        break
      done < "${DEBUX_TARGET_ENVIRON:-/proc/1/environ}" 2>/dev/null
    fi
    [[ -z "$target_path" ]] && target_path="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    # Split on colons with zsh native splitting; a here-string read loop leaves
    # a trailing newline on the last component, breaking lookups in it.
    local search_dir
    for search_dir in "${(@s.:.)target_path}"; do
      [[ -n "$search_dir" ]] || continue
      if [[ -x "${DEBUX_TARGET_ROOT}${search_dir}/${cmd}" || -L "${DEBUX_TARGET_ROOT}${search_dir}/${cmd}" ]]; then
        target_bin="${search_dir}/${cmd}"
        break
      fi
    done

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
        [[ "$entry" == *=* ]] || continue
        local env_key="${entry%%=*}"
        [[ "$env_key" =~ '^[A-Za-z_][A-Za-z0-9_]*$' ]] || continue
        target_env+=("$entry")
      done < "${DEBUX_TARGET_ENVIRON:-/proc/1/environ}" 2>/dev/null
      local chroot_bin=$(command -v chroot)
      local target_term="${TERM:-xterm}"
      [[ "$target_term" == "dumb" ]] && target_term=xterm
      env -i "${target_env[@]}" TERM="$target_term" COLUMNS="${COLUMNS:-80}" LINES="${LINES:-24}" \
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
      rehash
      if command -v "$cmd" >/dev/null 2>&1; then
        command "$cmd" "$@"
        return $?
      fi
      echo ""
      echo "  Install finished, but command '$cmd' is still not available."
      echo "  Try: dctl search $cmd"
    else
      echo ""
      echo "  Install failed. Try: dctl search $cmd"
    fi
  fi

  return 127
}

# Prompt
target="${DEBUX_TARGET:-unknown}"
if [[ -n "${DEBUX_CONTEXT:-}" && "$target" != "${DEBUX_CONTEXT}:"* && "$target" != "${DEBUX_CONTEXT}/"* ]]; then
  target="${DEBUX_CONTEXT}:${target}"
fi
PS1="%F{cyan}[debux]%f %F{yellow}${target}%f %F{blue}%~%f %# "

# Session banner
if [[ -o interactive && -z "${DEBUX_BANNER_SHOWN:-}" ]]; then
  export DEBUX_BANNER_SHOWN=1
  profile="${DEBUX_SECURITY_PROFILE:-general}"
  print -P "%F{cyan}debux%f profile: %F{yellow}${profile}%f · target: %F{yellow}${target}%f"
  echo "  target    cd into target filesystem (${DEBUX_TARGET_ROOT:-/proc/1/root})"
  echo "  dctl      install/list/remove Nix tools"
  if [[ "$profile" == "restricted" ]]; then
    echo "  restricted mode: non-root/no extra caps; target chroot integration may be limited"
  elif [[ "$profile" == "general" ]]; then
    echo "  general mode: root inside the pod/container with debugging capabilities"
  fi
  echo ""
  unset profile
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

# Auto-install requested tool sets (--tools / config file) once per container
if [[ -n "${DEBUX_TOOLS:-}" ]]; then
  _debux_tools_marker="${DEBUX_TOOLS//[^A-Za-z0-9._-]/-}"
  _debux_tools_marker="${HOME:-/tmp}/.debux-tools-${_debux_tools_marker}"
  if [[ ! -e "$_debux_tools_marker" ]]; then
    echo "Installing requested tools: ${DEBUX_TOOLS}"
    for _debux_tool in ${(s. .)DEBUX_TOOLS}; do
      dctl install "$_debux_tool" || echo "  install failed: $_debux_tool (try: dctl search $_debux_tool)"
    done
    : > "$_debux_tools_marker" 2>/dev/null || true
  fi
  unset _debux_tools_marker _debux_tool
fi

# Import target container environment variables
_debux_import_target_env() {
  local environ_file="${DEBUX_TARGET_ENVIRON:-/proc/1/environ}"
  [[ -f "$environ_file" ]] || return 0

  # Save sidecar's PATH before target env modification (used by wrapper generator)
  _debux_sidecar_path="$PATH"

  local -a skip_exact=(
    HOME USER LOGNAME SHELL TERM COLUMNS LINES HOSTNAME PWD OLDPWD SHLVL _ TMPDIR
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
    [[ "$key" =~ '^[A-Za-z_][A-Za-z0-9_]*$' ]] || continue

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
      for component in "${(@s.:.)val}"; do
        [[ -n "$component" ]] || continue
        translated+=("${DEBUX_TARGET_ROOT}${component}")
      done
      # Save original target PATH for wrapper generation
      _debux_target_path="$val"
      export PATH="${PATH}:${(j.:.)translated}"

    elif (( ${path_colon_vars[(Ie)$key]} )); then
      # Colon-separated path vars: translate each component
      local -a translated=()
      local component
      for component in "${(@s.:.)val}"; do
        [[ -n "$component" ]] || continue
        translated+=("${DEBUX_TARGET_ROOT}${component}")
      done
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
  local wrapper_dir="/tmp/debux-target-bin"
  rm -rf "$wrapper_dir" 2>/dev/null || true

  [[ -z "$DEBUX_TARGET_ROOT" || ! -d "$DEBUX_TARGET_ROOT" ]] && return 0
  [[ -z "$_debux_target_path" ]] && return 0

  mkdir -p "$wrapper_dir"

  # Create shared chroot-exec helper
  # Restores the target container's full original environment from
  # /proc/1/environ before chroot+exec — same env as "docker exec".
  # CWD is preserved by --skip-chdir: /proc/1/root/app becomes /app.
  cat > "$wrapper_dir/.chroot-exec" << 'HELPER_EOF'
#!/usr/bin/env zsh
TARGET_ROOT="${DEBUX_TARGET_ROOT:-/proc/1/root}"
CHROOT=$(command -v chroot)
cmd="$1"; shift
case "$PWD" in
  "${TARGET_ROOT}"/*) ;;
  *) cd "$TARGET_ROOT" 2>/dev/null || true ;;
esac
local -a target_env=()
while IFS= read -r -d '' entry; do
  [[ "$entry" == *=* ]] || continue
  env_key="${entry%%=*}"
  [[ "$env_key" =~ '^[A-Za-z_][A-Za-z0-9_]*$' ]] || continue
  target_env+=("$entry")
done < "${DEBUX_TARGET_ENVIRON:-/proc/1/environ}" 2>/dev/null
target_term="${TERM:-xterm}"
[ "$target_term" = "dumb" ] && target_term=xterm
exec env -i "${target_env[@]}" TERM="$target_term" COLUMNS="${COLUMNS:-80}" LINES="${LINES:-24}" "$CHROOT" --skip-chdir "$TARGET_ROOT" "$cmd" "$@"
HELPER_EOF
  chmod +x "$wrapper_dir/.chroot-exec"

  # Collect sidecar's own binaries from the pre-modification PATH
  local -A sidecar_cmds
  local p
  for p in "${(@s.:.)_debux_sidecar_path}"; do
    [[ -d "$p" ]] || continue
    for f in "$p"/*(-.:t N); do
      sidecar_cmds[$f]=1
    done
  done

  # Walk each target PATH dir and create wrappers for missing commands
  local dir
  for dir in "${(@s.:.)_debux_target_path}"; do
    [[ -n "$dir" ]] || continue
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
  done

  # Common aliases: create symlink if canonical exists but alias doesn't
  local -A cmd_aliases=(
    [python]=python3
    [pip]=pip3
  )
  for alias_name canonical in "${(@kv)cmd_aliases}"; do
    [[ ! -e "$wrapper_dir/$alias_name" && -e "$wrapper_dir/$canonical" ]] && \
      ln -sf "$canonical" "$wrapper_dir/$alias_name"
  done

  # Prepend wrapper dir to PATH (before /proc/1/root/... entries)
  export PATH="$wrapper_dir:$PATH"
  unset _debux_target_path _debux_sidecar_path
}
_debux_generate_wrappers
unfunction _debux_generate_wrappers

# Auto-cd to target container's working directory
if [[ -n "$DEBUX_TARGET_ROOT" && -r "${DEBUX_TARGET_CWD_LINK:-/proc/1/cwd}" ]]; then
  _debux_target_cwd=$(readlink "${DEBUX_TARGET_CWD_LINK:-/proc/1/cwd}" 2>/dev/null)
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
# Match the mountpoint/type fields, not the whole line: a device like /dev/sda1
# in field 1 must not hide a data volume mounted at /data.
echo "Volumes from target:"
awk '$2 !~ /^\/(nix|proc|sys|dev|tmp)(\/|$)/ && $3 != "overlay" {print "  " $2 " (" $3 ")"}' /proc/self/mounts 2>/dev/null || true
echo ""

# Launch shell (or daemon mode for k8s container reuse)
if [ "${DEBUX_DAEMON:-}" = "1" ]; then
  # Record the daemon PID (as seen in the shared PID namespace) so debux kill
  # can signal the daemon instead of the target's PID 1. Use $BASHPID, not $$:
  # Kubernetes collapses a literal $$ to $ during command substitution, and the
  # image's /bin/sh is bash. ${BASHPID:-$$} keeps Docker (no substitution)
  # working even on a non-bash shell.
  echo "${BASHPID:-$$}" > /tmp/.debux-daemon.pid 2>/dev/null || true
  trap 'exit 0' TERM INT
  while :; do sleep 2147483647 & wait $!; done
fi
exec zsh
`

// ShellBootstrapScript recreates the debux zsh startup files inside an already
// running debug container before opening an exec session. This makes reused
// Kubernetes ephemeral containers self-healing, including containers created by
// older debux versions or by the image ENTRYPOINT instead of the injected
// command.
func ShellBootstrapScript() string {
	return `# If the container runs as a non-root UID, /root is often not writable.
# Use a per-UID home owned by the current user so zsh and Nix behave normally.
if [ -z "${HOME:-}" ] || [ ! -d "$HOME" ] || [ ! -w "$HOME" ]; then
  debux_uid="$(id -u 2>/dev/null || echo 0)"
  export HOME="/tmp/debux-home-$debux_uid"
  mkdir -p "$HOME" 2>/dev/null || export HOME=/tmp
  unset debux_uid
fi
export ZDOTDIR=/tmp
export PATH="/nix/var/debux-profile/bin:/usr/local/bin:${HOME:-/tmp}/.nix-profile/bin:$PATH"
: "${DEBUX_TARGET_ROOT:=/proc/1/root}"
: "${DEBUX_TARGET_ENVIRON:=/proc/1/environ}"
: "${DEBUX_TARGET_CWD_LINK:=/proc/1/cwd}"
export DEBUX_TARGET_ROOT DEBUX_TARGET_ENVIRON DEBUX_TARGET_CWD_LINK
mkdir -p /nix/var/debux-data 2>/dev/null || mkdir -p /tmp/debux-data
mkdir -p "${HOME:-/tmp}/.config" 2>/dev/null || true
cat > /tmp/.zshenv << 'ZSHENV_EOF'
` + heredocContent(Script, "ZSHENV_EOF") + `
ZSHENV_EOF
cat > /tmp/.zshrc << 'ZSHRC_EOF'
` + heredocContent(Script, "ZSHRC_EOF") + `
ZSHRC_EOF`
}

func heredocContent(script, marker string) string {
	startMarker := "<< '" + marker + "'\n"
	start := strings.Index(script, startMarker)
	if start == -1 {
		return ""
	}
	start += len(startMarker)

	end := strings.Index(script[start:], "\n"+marker)
	if end == -1 {
		return ""
	}
	return script[start : start+end]
}

// ImageScript is the entrypoint script for image debugging.
// Unlike Script, it does NOT wait for PID namespace sharing (there is no
// running target process). The image filesystem is copied into /target.
const ImageScript = `#!/bin/sh
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
    echo "Installing requested tools: ${DEBUX_TOOLS}"
    for _debux_tool in ${(s. .)DEBUX_TOOLS}; do
      dctl install "$_debux_tool" || echo "  install failed: $_debux_tool (try: dctl search $_debux_tool)"
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
`
