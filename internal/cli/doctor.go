package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/clement-tourriere/debux/internal/version"
	"github.com/spf13/cobra"
)

type doctorReport struct {
	Version      string                `json:"version"`
	DefaultImage string                `json:"defaultImage"`
	Sections     []doctorReportSection `json:"sections"`
}

type doctorReportSection struct {
	Name   string                `json:"name"`
	Checks []runtime.DoctorCheck `json:"checks"`
}

func newDoctorCmd() *cobra.Command {
	var outputJSON bool
	cmd := &cobra.Command{
		Use:   "doctor [target]",
		Short: "Diagnose Docker/Kubernetes readiness for debux",
		Long: `Run local diagnostics for debux.

With no target, doctor checks the debux binary, Docker, and the current
Kubernetes context when available. With a target, doctor focuses on that runtime
and checks common Kubernetes RBAC permissions for debug sessions.`,
		Example: `  debux doctor
  debux doctor docker://my-app
  debux doctor k8s://prod/api-pod/app
  debux doctor k8s://prod/api-pod/app --profile=restricted
  debux doctor --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile, err := resolveProfile(cmd)
			if err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			report, err := buildDoctorReport(ctx, cmd, args, profile)
			if err != nil {
				return err
			}
			if outputJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			printDoctorReport(cmd, report)
			return nil
		},
	}
	addKubeconfigFlag(cmd)
	cmd.Flags().StringVar(&flagProfile, "profile", runtime.ProfileGeneral, "Kubernetes security profile to evaluate")
	cmd.Flags().BoolVar(&outputJSON, "json", false, "Print diagnostics as JSON")
	return cmd
}

func buildDoctorReport(ctx context.Context, cmd *cobra.Command, args []string, profile string) (doctorReport, error) {
	report := doctorReport{
		Version:      version.Details(),
		DefaultImage: runtime.DefaultImage,
	}
	report.Sections = append(report.Sections, doctorReportSection{
		Name: "debux",
		Checks: []runtime.DoctorCheck{{
			Name:   "Version",
			Status: runtime.CheckPass,
			Detail: version.Details(),
		}, {
			Name:   "Default debug image",
			Status: runtime.CheckPass,
			Detail: runtime.DefaultImage,
		}},
	})

	if len(args) == 0 {
		report.Sections = append(report.Sections, doctorReportSection{Name: "Docker", Checks: runtime.DockerDoctor(ctx)})
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
		report.Sections = append(report.Sections, doctorReportSection{Name: "Kubernetes", Checks: runtime.KubernetesDoctor(ctx, kubeconfig, flagKubeContext, "", "", profile)})
		return report, nil
	}

	target, err := runtime.ParseTarget(args[0])
	if err != nil {
		return doctorReport{}, fmt.Errorf("invalid target: %w", err)
	}
	switch target.Runtime {
	case "docker":
		report.Sections = append(report.Sections, doctorReportSection{Name: "Docker", Checks: runtime.DockerDoctor(ctx, target.Name)})
	case "kubernetes":
		kubeContext, err := resolveKubeContext(cmd, target.Context)
		if err != nil {
			return doctorReport{}, err
		}
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
		report.Sections = append(report.Sections, doctorReportSection{Name: "Kubernetes", Checks: runtime.KubernetesDoctor(ctx, kubeconfig, kubeContext, target.Namespace, target.Name, profile)})
	default:
		report.Sections = append(report.Sections, doctorReportSection{Name: target.Runtime, Checks: []runtime.DoctorCheck{{Name: "Runtime", Status: runtime.CheckWarn, Detail: "doctor does not support this runtime yet"}}})
	}
	return report, nil
}

func printDoctorReport(cmd *cobra.Command, report doctorReport) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "debux doctor\nVersion: %s\nDefault image: %s\n", report.Version, report.DefaultImage)
	for _, section := range report.Sections {
		_, _ = fmt.Fprintf(out, "\n%s\n", section.Name)
		for _, check := range section.Checks {
			_, _ = fmt.Fprintf(out, "  %s %s", doctorSymbol(check.Status), check.Name)
			if check.Detail != "" {
				_, _ = fmt.Fprintf(out, " — %s", check.Detail)
			}
			_, _ = fmt.Fprintln(out)
		}
	}
}

func doctorSymbol(status string) string {
	switch status {
	case runtime.CheckPass:
		return "✓"
	case runtime.CheckWarn:
		return "!"
	case runtime.CheckFail:
		return "✗"
	default:
		return "?"
	}
}
