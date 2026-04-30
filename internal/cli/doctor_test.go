package cli

import (
	"testing"

	"github.com/clement-tourriere/debux/internal/runtime"
)

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
