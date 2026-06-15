package entrypoint

import (
	"strings"
	"testing"
)

func TestDaemonRecordsPidWithBashpid(t *testing.T) {
	// Kubernetes collapses a literal $$ to $ in container commands, so the
	// daemon must use $BASHPID to record a usable PID for `debux kill`.
	if !strings.Contains(Script, "/tmp/.debux-daemon.pid") {
		t.Fatal("daemon must write a pidfile")
	}
	if !strings.Contains(Script, "BASHPID") {
		t.Fatal("daemon must record its PID via $BASHPID, not $$ (Kubernetes mangles $$ to $)")
	}
	if strings.Contains(Script, `echo "$$" > /tmp/.debux-daemon.pid`) {
		t.Fatal("daemon must not write the pidfile with a bare $$ (Kubernetes collapses it to $)")
	}
}

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

// TestShellBootstrapScriptZshenvOnlySetsZdotdir guards against regressing the
// .zshenv heredoc back to carrying the full interactive zshrc. zsh sources
// .zshenv on EVERY invocation (including non-interactive `zsh -c`), so it must
// stay minimal — only `export ZDOTDIR=/tmp` — while the heavy config lives in
// .zshrc. A substring check is not enough here (see the bug this caught): we
// extract each heredoc body and compare it exactly.
func TestShellBootstrapScriptZshenvOnlySetsZdotdir(t *testing.T) {
	script := ShellBootstrapScript()

	zshenv := heredocContent(script, "ZSHENV_EOF")
	if zshenv != "export ZDOTDIR=/tmp" {
		t.Fatalf("/tmp/.zshenv heredoc = %q, want exactly %q", zshenv, "export ZDOTDIR=/tmp")
	}
	if strings.Contains(zshenv, "command_not_found_handler()") {
		t.Fatal("/tmp/.zshenv must not carry the interactive zshrc config")
	}

	zshrc := heredocContent(script, "ZSHRC_EOF")
	if !strings.Contains(zshrc, "command_not_found_handler()") {
		t.Fatal("/tmp/.zshrc heredoc is missing the debux zsh config")
	}
}

func TestHeredocContent(t *testing.T) {
	tests := []struct {
		name   string
		script string
		marker string
		want   string
	}{
		{"multi-line body", "head\ncat > f << 'M'\nfoo\nbar\nM\ntail", "M", "foo\nbar"},
		{"single-line body", "cat > f << 'M'\nonly\nM", "M", "only"},
		{"missing marker", "cat > f << 'M'\nfoo\nM", "NOPE", ""},
		{"unterminated heredoc", "cat > f << 'M'\nfoo", "M", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := heredocContent(tt.script, tt.marker); got != tt.want {
				t.Fatalf("heredocContent(_, %q) = %q, want %q", tt.marker, got, tt.want)
			}
		})
	}
}
