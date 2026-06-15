package runtime

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestSecurityContextForProfile(t *testing.T) {
	tests := []struct {
		profile            string
		wantPrivileged     bool
		wantRunAsNonRoot   *bool
		wantRunAsUser      int64
		wantRunAsGroup     int64
		wantDropAll        bool
		wantAddedCaps      []corev1.Capability
		wantAllowPrivEsc   *bool
		wantSeccompDefault bool
		wantErr            bool
	}{
		{
			profile:          ProfileGeneral,
			wantRunAsNonRoot: boolPtr(false),
			wantRunAsUser:    0,
			wantAddedCaps:    []corev1.Capability{"SYS_PTRACE", "SYS_ADMIN", "SYS_CHROOT"},
		},
		{
			profile: ProfileBaseline,
		},
		{
			profile:            ProfileRestricted,
			wantRunAsNonRoot:   boolPtr(true),
			wantRunAsUser:      65534,
			wantDropAll:        true,
			wantAllowPrivEsc:   boolPtr(false),
			wantSeccompDefault: true,
		},
		{
			profile:       ProfileNetadmin,
			wantAddedCaps: []corev1.Capability{"NET_ADMIN", "NET_RAW"},
		},
		{
			profile:        ProfileSysadmin,
			wantPrivileged: true,
		},
		{
			profile: "invalid",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		sc, err := SecurityContextForProfile(tc.profile)
		if tc.wantErr {
			if err == nil {
				t.Errorf("SecurityContextForProfile(%q): expected error", tc.profile)
			}
			continue
		}
		if err != nil {
			t.Errorf("SecurityContextForProfile(%q): %v", tc.profile, err)
			continue
		}
		if tc.wantPrivileged {
			if sc == nil || sc.Privileged == nil || !*sc.Privileged {
				t.Errorf("SecurityContextForProfile(%q): expected privileged", tc.profile)
			}
		} else if sc != nil && sc.Privileged != nil && *sc.Privileged {
			t.Errorf("SecurityContextForProfile(%q): unexpected privileged", tc.profile)
		}
		if tc.wantRunAsNonRoot != nil {
			if sc == nil || sc.RunAsNonRoot == nil || *sc.RunAsNonRoot != *tc.wantRunAsNonRoot {
				t.Errorf("SecurityContextForProfile(%q): RunAsNonRoot = %v, want %v", tc.profile, derefBool(sc.RunAsNonRoot), *tc.wantRunAsNonRoot)
			}
		}
		if tc.wantRunAsUser != 0 {
			if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != tc.wantRunAsUser {
				t.Errorf("SecurityContextForProfile(%q): RunAsUser = %v, want %d", tc.profile, derefInt64(sc.RunAsUser), tc.wantRunAsUser)
			}
		}
		if tc.wantRunAsGroup != 0 {
			if sc == nil || sc.RunAsGroup == nil || *sc.RunAsGroup != tc.wantRunAsGroup {
				t.Errorf("SecurityContextForProfile(%q): RunAsGroup = %v, want %d", tc.profile, derefInt64(sc.RunAsGroup), tc.wantRunAsGroup)
			}
		}
		if tc.wantDropAll {
			if sc == nil || sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
				t.Errorf("SecurityContextForProfile(%q): expected Drop ALL, got %v", tc.profile, sc)
			}
		}
		if len(tc.wantAddedCaps) > 0 {
			if sc == nil || sc.Capabilities == nil || len(sc.Capabilities.Add) != len(tc.wantAddedCaps) {
				t.Errorf("SecurityContextForProfile(%q): capabilities add = %v, want %v", tc.profile, capAdd(sc), tc.wantAddedCaps)
			} else {
				for i, c := range tc.wantAddedCaps {
					if sc.Capabilities.Add[i] != c {
						t.Errorf("SecurityContextForProfile(%q): cap[%d] = %v, want %v", tc.profile, i, sc.Capabilities.Add[i], c)
					}
				}
			}
		}
		if tc.wantAllowPrivEsc != nil {
			if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation != *tc.wantAllowPrivEsc {
				t.Errorf("SecurityContextForProfile(%q): AllowPrivilegeEscalation = %v, want %v", tc.profile, derefBool(sc.AllowPrivilegeEscalation), *tc.wantAllowPrivEsc)
			}
		}
		if tc.wantSeccompDefault {
			if sc == nil || sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Errorf("SecurityContextForProfile(%q): expected RuntimeDefault seccomp", tc.profile)
			}
		}
	}
}

func TestApplyExtraCapabilities(t *testing.T) {
	sc := &corev1.SecurityContext{}
	got := applyExtraCapabilities(sc, []string{"net_admin", "CAP_SYS_PTRACE"})
	if got == nil || got.Capabilities == nil || len(got.Capabilities.Add) != 2 {
		t.Fatalf("expected 2 added caps, got %v", got)
	}
	if got.Capabilities.Add[0] != "NET_ADMIN" || got.Capabilities.Add[1] != "SYS_PTRACE" {
		t.Errorf("unexpected caps: %v", got.Capabilities.Add)
	}

	// nil security context should be created
	got2 := applyExtraCapabilities(nil, []string{"NET_ADMIN"})
	if got2 == nil || got2.Capabilities == nil || len(got2.Capabilities.Add) != 1 {
		t.Errorf("expected cap add on fresh context, got %v", got2)
	}

	// empty caps should return the original context unchanged
	got3 := applyExtraCapabilities(sc, nil)
	if got3 != sc {
		t.Error("expected same context when no caps requested")
	}
}

func TestApplyKubernetesUser(t *testing.T) {
	sc, err := applyKubernetesUser(nil, "1000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 1000 {
		t.Errorf("expected uid 1000, got %v", sc)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("expected RunAsNonRoot true for uid 1000")
	}

	sc, err = applyKubernetesUser(nil, "1000:2000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup != 2000 {
		t.Errorf("expected gid 2000, got %v", derefInt64(sc.RunAsGroup))
	}

	for _, bad := range []string{"", "root", "1000:2000:3000", "-1", "1000:-1"} {
		if _, err := applyKubernetesUser(nil, bad); err == nil {
			t.Errorf("applyKubernetesUser(%q): expected error", bad)
		}
	}
}

func boolPtr(b bool) *bool { return &b }

func derefBool(b *bool) bool {
	if b == nil {
		return false
	}
	return *b
}

func derefInt64(i *int64) int64 {
	if i == nil {
		return 0
	}
	return *i
}

func capAdd(sc *corev1.SecurityContext) []corev1.Capability {
	if sc == nil || sc.Capabilities == nil {
		return nil
	}
	return sc.Capabilities.Add
}
