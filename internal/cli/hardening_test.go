package cli

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/clement-tourriere/debux/internal/runtime"
)

func TestCommandTreesOwnTheirFlags(t *testing.T) {
	first := NewRootCmd()
	if err := first.Flags().Set("profile", runtime.ProfileRestricted); err != nil {
		t.Fatal(err)
	}
	if err := first.Flags().Set("image", "custom:one"); err != nil {
		t.Fatal(err)
	}
	if err := first.Flags().Set("env", "FOO=first"); err != nil {
		t.Fatal(err)
	}
	second := NewRootCmd()
	if flagString(first, "profile") != runtime.ProfileRestricted || flagString(first, "image") != "custom:one" {
		t.Fatal("constructing another root reset existing flags")
	}
	if flagString(second, "image") != "" || len(flagStrings(second, "env")) != 0 {
		t.Fatal("new root inherited a prior launch")
	}
	child, _, err := first.Find([]string{"cp"})
	if err != nil {
		t.Fatal(err)
	}
	_ = child.Flags().Set("profile", runtime.ProfileGeneral)
	if flagString(first, "profile") != runtime.ProfileRestricted {
		t.Fatal("subcommand changed root profile")
	}
}

func TestSelectedSessionSurvivesTUIAndExternalLaunch(t *testing.T) {
	t.Setenv("DEBUX_TERMINAL", "wezterm")
	selected := runtime.DebugSessionInfo{Runtime: "kubernetes", Kind: runtime.DebugSessionKindKubernetesEphemeral, Target: "k8s://@ctx/ns/pod/app", DebugName: "debux-43", ID: "pod-uid", Profile: runtime.ProfileGeneral}
	launch := tuiLaunch{}
	model := newTUIModel(&launch, "", "", "")
	first := selected
	first.DebugName = "debux-42"
	_ = model.setItems("Sessions", []tuiItem{tuiSessionItem(first), tuiSessionItem(selected)})
	model.list.Select(1)
	if _, quit := model.activateSelected(tuiLaunchTerminal); !quit {
		t.Fatal("selection was not launched")
	}
	if launch.session == nil || launch.session.DebugName != "debux-43" {
		t.Fatal("exact session identity was lost")
	}
	cmd := newTUICmd()
	args := buildExecArgs(cmd, launch)
	want := []string{"attach", selected.Target, "--debug-container", "debux-43", "--session-id", "pod-uid"}
	if !slices.Equal(args, want) {
		t.Fatalf("external launch re-resolves target: %v", args)
	}
	attach := newAttachCmd()
	if err := attach.ParseFlags(args[1:]); err != nil {
		t.Fatal(err)
	}
	matches := selectedDebugSessions(attach, []runtime.DebugSessionInfo{first, selected})
	if len(matches) != 1 || matches[0].DebugName != "debux-43" {
		t.Fatal(matches)
	}
}

func TestTUILaunchCarriesExplicitGeneralAndCopyLifetime(t *testing.T) {
	cmd := newTUICmd()
	_ = cmd.Flags().Set("keep", "true")
	_ = cmd.Flags().Set("ttl", "2h")
	launch := tuiLaunch{target: "k8s://ns/app", profile: runtime.ProfileGeneral, image: runtime.DefaultImage, copy: true, shareVolumes: true}
	args := buildExecArgs(cmd, launch)
	next := newExecCmd()
	if err := next.ParseFlags(args[1:]); err != nil {
		t.Fatal(err)
	}
	if !flagChanged(next, "profile") || flagString(next, "profile") != runtime.ProfileGeneral || !flagBool(next, "keep") || flagString(next, "ttl") != "2h" {
		t.Fatalf("launch dropped explicit choices: %v", args)
	}
	if flagChanged(cmd, "profile") {
		t.Fatal("launch mutated dashboard flags")
	}
	launch.target = "docker://app"
	launch.copy = false
	next = newExecCmd()
	if err := next.ParseFlags(buildExecArgs(cmd, launch)[1:]); err != nil {
		t.Fatal(err)
	}
	if err := validateExecFlags(next, "docker"); err != nil {
		t.Fatalf("Kubernetes flags leaked into Docker launch: %v", err)
	}
}

func TestConfigSafetyInOperationalCommands(t *testing.T) {
	mode := os.Getenv("DEBUX_CONFIG_TEST_CHILD")
	if mode != "" {
		body := "profile: restricted\nimage: custom:debug\n"
		if mode == "malformed" {
			body += "toolz: {}\n"
		}
		if err := os.WriteFile(os.Getenv("DEBUX_CONFIG"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if mode == "malformed" {
			for _, args := range [][]string{{"exec", "docker://never-contact"}, {"cp", "local", "k8s://ns/pod:/tmp/file"}} {
				cmd := NewRootCmd()
				cmd.SetArgs(args)
				err := cmd.Execute()
				if err == nil || !strings.Contains(err.Error(), "invalid debux config") {
					t.Fatalf("unsafe config fallback: %v", err)
				}
			}
		} else {
			cmd := newCpCmd()
			profile, err := resolveProfile(cmd)
			if err != nil || profile != runtime.ProfileRestricted {
				t.Fatalf("cp ignored restricted config: %s %v", profile, err)
			}
			if image := resolveImage(flagString(cmd, "image")); image != "custom:debug" {
				t.Fatal(image)
			}
		}
		return
	}
	for _, mode := range []string{"malformed", "restricted"} {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConfigSafetyInOperationalCommands$")
		command.Env = append(os.Environ(), "DEBUX_CONFIG_TEST_CHILD="+mode)
		output, err := command.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("%s: %v\n%s", mode, err, output)
		}
	}
}
