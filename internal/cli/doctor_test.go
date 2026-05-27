package cli

import (
	"testing"

	"github.com/clement-tourriere/debux/internal/runtime"
)

func TestResolveKubeNamespace(t *testing.T) {
	cmd := newExecCmd()
	if got, err := resolveKubeNamespace(cmd, "prod"); err != nil || got != "prod" {
		t.Fatalf("resolveKubeNamespace without flag = %q, %v; want prod, nil", got, err)
	}

	cmd = newExecCmd()
	if err := cmd.Flags().Set("namespace", "staging"); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveKubeNamespace(cmd, ""); err != nil || got != "staging" {
		t.Fatalf("resolveKubeNamespace with flag = %q, %v; want staging, nil", got, err)
	}

	if _, err := resolveKubeNamespace(cmd, "prod"); err == nil {
		t.Fatalf("resolveKubeNamespace should reject conflicting target and flag namespaces")
	}
}

func TestApplyKubeNamespaceFlagContainerShorthand(t *testing.T) {
	cmd := newExecCmd()
	if err := cmd.Flags().Set("namespace", "prod"); err != nil {
		t.Fatal(err)
	}
	target := &runtime.Target{Runtime: "kubernetes", Namespace: "api-pod", Name: "app"}

	applyKubeNamespaceFlagContainerShorthand(cmd, target)

	if target.Namespace != "" || target.Name != "api-pod" || target.Container != "app" {
		t.Fatalf("target = %#v, want pod api-pod container app with namespace resolved by flag", target)
	}
}

func TestValidateExecFlagsRejectsNamespaceForDocker(t *testing.T) {
	cmd := newExecCmd()
	if err := cmd.Flags().Set("namespace", "prod"); err != nil {
		t.Fatal(err)
	}
	if err := validateExecFlags(cmd, "docker"); err == nil {
		t.Fatalf("validateExecFlags should reject --namespace for Docker targets")
	}
}

func TestReportHasFailures(t *testing.T) {
	report := doctorReport{Sections: []doctorReportSection{{
		Name:   "test",
		Checks: []runtime.DoctorCheck{{Name: "ok", Status: runtime.CheckPass}},
	}}}
	if reportHasFailures(report) {
		t.Fatalf("reportHasFailures returned true without failures")
	}

	report.Sections[0].Checks = append(report.Sections[0].Checks, runtime.DoctorCheck{Name: "bad", Status: runtime.CheckFail})
	if !reportHasFailures(report) {
		t.Fatalf("reportHasFailures returned false with a failure")
	}
}
