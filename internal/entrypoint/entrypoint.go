package entrypoint

import (
	_ "embed"
	"strings"
)

//go:embed script.sh
var scriptBytes []byte

//go:embed image_script.sh
var imageScriptBytes []byte

// Script is the entrypoint script injected into the debug container.
// It waits for the target's PID namespace to be visible, sets up
// convenience symlinks, writes the shell configuration, and launches zsh.
//
// The shell script is maintained in script.sh and embedded at build time so
// editors and shellcheck can treat it as a real shell file, while Go rebuilds
// still pick up changes immediately without requiring a Docker image rebuild.
var Script = string(scriptBytes)

// ImageScript is the entrypoint script for image debugging.
// Unlike Script, it does NOT wait for PID namespace sharing (there is no
// running target process). The image filesystem is copied into /target.
var ImageScript = string(imageScriptBytes)

// ShellBootstrapScript recreates the debux zsh startup files inside an already
// running debug container before opening an exec session. This makes reused
// Kubernetes ephemeral containers self-healing, including containers created by
// older debux versions or by the image ENTRYPOINT instead of the injected
// command.
func ShellBootstrapScript() string {
	// .zshenv and .zshrc carry DIFFERENT bodies: .zshenv only sets ZDOTDIR so
	// every zsh invocation (including non-interactive `zsh -c`) is pointed at
	// /tmp/.zshrc, while .zshrc holds the heavy interactive config. They must be
	// extracted from their respective heredocs — reusing one for both would run
	// the full config on every shell startup and drop the ZDOTDIR export.
	zshenv := heredocContent(Script, "ZSHENV_EOF")
	zshrc := heredocContent(Script, "ZSHRC_EOF")
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
` + zshenv + `
ZSHENV_EOF
cat > /tmp/.zshrc << 'ZSHRC_EOF'
` + zshrc + `
ZSHRC_EOF`
}

// heredocContent extracts the body of a `<< 'MARKER' ... MARKER` heredoc from
// a shell script. It is used to pull the embedded zsh configuration out of
// script.sh so it can be replayed into an already-running container.
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
