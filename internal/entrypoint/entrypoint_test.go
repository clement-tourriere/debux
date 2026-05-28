package entrypoint

import (
	"strings"
	"testing"
)

func TestShellBootstrapScriptIncludesDebuxZshConfig(t *testing.T) {
	script := ShellBootstrapScript()
	for _, want := range []string{
		"export ZDOTDIR=/tmp",
		"cat > /tmp/.zshrc << 'ZSHRC_EOF'",
		"command_not_found_handler()",
		"PS1=",
		"DEBUX_CONTEXT",
		"alias target='cd $DEBUX_TARGET_ROOT'",
		"[[ \"$entry\" == *=* ]] || continue",
		"rm -rf \"$wrapper_dir\"",
		"export TERM=xterm",
		"TERM=\"$target_term\" COLUMNS=\"${COLUMNS:-80}\" LINES=\"${LINES:-24}\"",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("ShellBootstrapScript() missing %q", want)
		}
	}
	if strings.Contains(script, "ZSHRC_EOF\nZSHRC_EOF") {
		t.Fatalf("ShellBootstrapScript() wrote an empty zshrc heredoc")
	}
}
