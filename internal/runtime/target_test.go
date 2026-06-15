package runtime

import (
	"strings"
	"testing"
)

func TestParseTargetKubernetes(t *testing.T) {
	tests := []struct {
		in      string
		want    Target
		wantErr bool
	}{
		{in: "k8s://", want: Target{Runtime: "kubernetes"}},
		{in: "k8s://my-pod", want: Target{Runtime: "kubernetes", Name: "my-pod"}},
		{in: "k8s://my-namespace/my-pod", want: Target{Runtime: "kubernetes", Namespace: "my-namespace", Name: "my-pod"}},
		{in: "k8s://my-namespace/my-pod/my-container", want: Target{Runtime: "kubernetes", Namespace: "my-namespace", Name: "my-pod", Container: "my-container"}},
		{in: "k8s://@eks-preprod-01", want: Target{Runtime: "kubernetes", Context: "eks-preprod-01"}},
		{in: "k8s://@eks-preprod-01/my-pod", want: Target{Runtime: "kubernetes", Context: "eks-preprod-01", Name: "my-pod"}},
		{in: "k8s://@eks-preprod-01/my-namespace/my-pod", want: Target{Runtime: "kubernetes", Context: "eks-preprod-01", Namespace: "my-namespace", Name: "my-pod"}},
		{in: "k8s://@eks-preprod-01/my-namespace/my-pod/my-container", want: Target{Runtime: "kubernetes", Context: "eks-preprod-01", Namespace: "my-namespace", Name: "my-pod", Container: "my-container"}},
		{in: "k8s://@context%2Fwith%2Fslashes/my-pod", want: Target{Runtime: "kubernetes", Context: "context/with/slashes", Name: "my-pod"}},
		{in: "k8s://my-namespace/", want: Target{Runtime: "kubernetes", Namespace: "my-namespace"}},
		{in: "k8s://a/b/c/d/e", wantErr: true},
		{in: "k8s://@", wantErr: true},
	}
	for _, tc := range tests {
		got, err := ParseTarget(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseTarget(%q): expected error, got %+v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", tc.in, err)
			continue
		}
		if *got != tc.want {
			t.Errorf("ParseTarget(%q) = %+v, want %+v", tc.in, *got, tc.want)
		}
	}
}

func TestParseTargetCompose(t *testing.T) {
	tests := []struct {
		in      string
		want    Target
		wantErr bool
	}{
		{in: "compose://web", want: Target{Runtime: "docker", ComposeService: "web"}},
		{in: "compose://shop/web", want: Target{Runtime: "docker", ComposeProject: "shop", ComposeService: "web"}},
		{in: "compose://", wantErr: true},
		{in: "compose://a/b/c", wantErr: true},
		{in: "compose:////", wantErr: true},
	}
	for _, tc := range tests {
		got, err := ParseTarget(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseTarget(%q): expected error, got %+v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", tc.in, err)
			continue
		}
		if *got != tc.want {
			t.Errorf("ParseTarget(%q) = %+v, want %+v", tc.in, *got, tc.want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "''"},
		{"hello", "'hello'"},
		{"it's", "'it'\\''s'"},
		{"a'b'c", "'a'\\''b'\\''c'"},
	}
	for _, tc := range tests {
		got := shellQuote(tc.in)
		if got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"echo", "hello world", "it's"})
	want := "'echo' 'hello world' 'it'\\''s'"
	if got != want {
		t.Errorf("shellJoin() = %q, want %q", got, want)
	}
}

func TestSanitizeImageRef(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"gcr.io/distroless/static:latest", "gcr-io-distroless-static-latest-"},
		{"my-app:1.0.0@sha256:abc123", "my-app-1-0-0-sha256-abc123-"},
		{"registry.io:5000/image", "registry-io-5000-image-"},
	}
	for _, tc := range tests {
		got := sanitizeImageRef(tc.in)
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("sanitizeImageRef(%q) = %q, want prefix %q", tc.in, got, tc.want)
		}
		if len(got) != len(tc.want)+8 {
			t.Errorf("sanitizeImageRef(%q) length = %d, want %d", tc.in, len(got), len(tc.want)+8)
		}
	}

	// Different refs that used to collide after replacement must now be distinct.
	if a, b := sanitizeImageRef("a/b"), sanitizeImageRef("a-b"); a == b {
		t.Errorf("expected no collision between a/b and a-b, got %q", a)
	}
}

func TestValidateEnvVarsRejectsNewlineInKey(t *testing.T) {
	if err := ValidateEnvVars([]string{"FOO\nBAR=value"}); err == nil {
		t.Error("expected newline in key to be rejected")
	}
}

func TestNormalizeCapabilitiesRejectsEmpty(t *testing.T) {
	got := normalizeCapabilities([]string{"", "  ", "NET_ADMIN"})
	want := []string{"NET_ADMIN"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("normalizeCapabilities() = %v, want %v", got, want)
	}
}

func TestToolNamePatternMatchesExpectedInputs(t *testing.T) {
	valid := []string{
		"python3",
		"py-spy",
		"postgresql_16",
		"nixpkgs#hello",
		"github:owner/repo#pkg",
		"nixpkgs#legacyPackages.x86_64-linux.hello",
	}
	for _, v := range valid {
		if !toolNamePattern.MatchString(v) {
			t.Errorf("toolNamePattern rejected valid %q", v)
		}
	}
	invalid := []string{
		"foo;bar",
		"foo bar",
		"foo$(x)",
		"foo`bar",
		"foo*",
	}
	for _, v := range invalid {
		if toolNamePattern.MatchString(v) {
			t.Errorf("toolNamePattern accepted invalid %q", v)
		}
	}
}

func TestValidateToolsAcceptsEmptySlice(t *testing.T) {
	if err := ValidateTools(nil); err != nil {
		t.Errorf("ValidateTools(nil): %v", err)
	}
}

func TestValidateToolsWhitespace(t *testing.T) {
	if err := ValidateTools([]string{"  py-spy  "}); err != nil {
		t.Errorf("ValidateTools with whitespace: %v", err)
	}
}

func TestValidateToolsRejectsControlChars(t *testing.T) {
	for _, bad := range []string{"foo\x00bar", "foo\x1fbar", "foo\x7fbar"} {
		if err := ValidateTools([]string{bad}); err == nil {
			t.Errorf("ValidateTools(%q): expected error", strings.ReplaceAll(bad, "\x00", "<NUL>"))
		}
	}
}
