package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAuthoredValidationHasAnExplicitGuardedAdmissionCommand(t *testing.T) {
	root := newRootCmd()
	command, _, err := root.Find([]string{"axi", "validate-authored"})
	if err != nil || command.CommandPath() != "no-mistakes axi validate-authored" {
		t.Fatalf("explicit authored admission command is unavailable: command=%s error=%v", command.CommandPath(), err)
	}
	if !mutatesPipelineControl(command) {
		t.Fatal("authored admission bypasses the native gate-context guard")
	}
	if command.Flags().Lookup("admission-file") == nil {
		t.Fatal("authored admission needs an explicit source-bound input file")
	}
	for _, forbidden := range []string{"yes", "skip", "model", "effort", "run"} {
		if command.Flags().Lookup(forbidden) != nil {
			t.Errorf("authored admission must not expose %s", forbidden)
		}
	}
}

func TestAuthoredGateReportsZeroCapacityWithoutOfferingFixOrSkip(t *testing.T) {
	command := newAxiRunCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	findings := `{"findings":[{"id":"r1","severity":"error","description":"repair needed","action":"auto-fix"}]}`
	run := &ipc.RunInfo{ID: "child", Branch: "feature", Status: types.RunRunning, AuthoredValidation: &authoredvalidation.Family{AdmissionID: "admission", PredecessorID: "parent", PolicyOrigin: "controller_assertion", HistoricalSourceCompleteness: "unknown"}, Steps: []ipc.StepResultInfo{{StepName: types.StepReview, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings}}}
	if err := renderDriveResult(command, run, false); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, forbidden := range []string{"--action fix", "--action skip", "override re-enables"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("validation-only gate offers unavailable authority %q:\n%s", forbidden, text)
		}
	}
	for _, required := range []string{"zero automatic/manual repair authority", "controller_assertion", "unknown"} {
		if !strings.Contains(text, required) {
			t.Fatalf("gate lost %q:\n%s", required, text)
		}
	}
}
