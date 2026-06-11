package cli

import (
	"strings"
	"testing"
	"time"
)

func TestParseCopyPodTTL(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "24h", want: 24 * time.Hour},
		{in: "2h45m", want: 2*time.Hour + 45*time.Minute},
		{in: " 30m ", want: 30 * time.Minute},
		{in: "0", want: 0},
		{in: "1s", want: time.Second},
		{in: "500ms", wantErr: true}, // below activeDeadlineSeconds granularity
		{in: "-5m", wantErr: true},
		{in: "tomorrow", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range tests {
		got, err := parseCopyPodTTL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseCopyPodTTL(%q) expected error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCopyPodTTL(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseCopyPodTTL(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestKeepAndTTLFlagsRequireCopy(t *testing.T) {
	// Without --copy the session is an ephemeral container that lives and dies
	// with the target pod; --keep/--ttl would silently do nothing.
	for _, args := range [][]string{
		{"k8s://prod/api-pod", "--keep"},
		{"k8s://prod/api-pod", "--ttl=8h"},
	} {
		root := NewRootCmd()
		root.SetArgs(args)
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), "--copy") {
			t.Errorf("args %v: expected an error pointing at --copy, got %v", args, err)
		}
	}

	// On non-Kubernetes targets the flags are rejected like --copy itself.
	root := NewRootCmd()
	root.SetArgs([]string{"docker://my-app", "--ttl=8h"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "Kubernetes") {
		t.Errorf("docker target with --ttl: expected Kubernetes-only error, got %v", err)
	}
}

func TestInvalidTTLFailsBeforeTouchingTheCluster(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"k8s://prod/api-pod/app", "--copy", "--ttl=tomorrow"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--ttl") {
		t.Errorf("expected a --ttl parse error, got %v", err)
	}
}
