package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
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

func TestAuthoredHomeLoadsFamilyAndRefusesMisleadingApprovalHelp(t *testing.T) {
	for _, scenario := range []string{"current branch", "other branch", "ledger error", "protected path", "unvalidated work"} {
		t.Run(scenario, func(t *testing.T) {
			dir, root, database, repo := setupAxiQueryRepo(t)
			run(t, dir, "git", "checkout", "-b", "feature")
			chdir(t, dir)
			selection := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
			assertion := &launchassert.Expectation{TrustedSHA: strings.Repeat("a", 40), Profiles: map[string][]agentcfg.Selection{"primary": {selection}, "reviewer": {selection}, "fixer": {selection}}}
			proof, err := assertion.Verify(assertion.TrustedSHA, assertion.Profiles)
			if err != nil {
				t.Fatal(err)
			}
			intent := db.RunIntent{Summary: "Validate the exact authored source", Source: db.RunIntentSourceAgent, Score: 1}
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(intent.Summary)))
			parent, err := database.InsertRunWithLaunchAssertion(repo.ID, "feature", strings.Repeat("b", 40), strings.Repeat("c", 40), &intent, "original", "generation", digest, "", true, nil, assertion, proof)
			if err != nil {
				t.Fatal(err)
			}
			step, err := database.InsertStepResult(parent.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			findings, selected := `{"findings":[{"id":"r1","description":"repair"}]}`, `["r1"]`
			round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.SetStepRoundSelection(round.ID, &selected, db.RoundSelectionSourceAutoFix); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(parent.ID, types.RunCancelled); err != nil {
				t.Fatal(err)
			}
			snapshot, err := database.ReadTerminalSnapshot(parent.ID)
			if err != nil {
				t.Fatal(err)
			}
			request := authoredvalidation.Request{PredecessorID: parent.ID, SnapshotDigest: snapshot.Digest(), RepoID: repo.ID, Branch: "feature", CallerDir: dir, CandidateSHA: strings.Repeat("d", 40), CandidateTree: strings.Repeat("e", 40), LaunchNonce: "original", ValidationGeneration: "generation", IntentDigest: digest,
				ControllerPolicy: authoredvalidation.ControllerPolicy{SHA256: strings.Repeat("f", 64), Class: "ordinary", ReviewCycles: 2}, LaunchAssertion: assertion, HistoricalSourceCompleteness: "unknown", Reason: "Validate this authored candidate"}
			event, _, err := database.AcceptValidationAdmission(context.Background(), db.ValidationAdmission{Request: request, Snapshot: *snapshot, AssertionProof: proof, WorktreeRoot: root.WorktreeDir(repo.ID, "fixture"), Operator: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(event.ChildID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			gate, err := database.InsertStepResult(event.ChildID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateStepStatus(gate.ID, types.StepStatusAwaitingApproval); err != nil {
				t.Fatal(err)
			}
			findingID := "ordinary"
			switch scenario {
			case "other branch":
				run(t, dir, "git", "checkout", "-b", "other")
			case "ledger error":
				if err := database.UpdateRunHeadSHA(parent.ID, strings.Repeat("9", 40)); err != nil {
					t.Fatal(err)
				}
			case "protected path":
				findingID = "protected-path-refusal"
			case "unvalidated work":
				findingID = types.FindingIDTestAgentUnvalidatedWork
			}
			if err := database.SetStepFindings(gate.ID, findingsJSON(t, []types.Finding{{ID: findingID, Severity: "error", Description: "condition requires an operator", Action: types.ActionAskUser}}, "review condition")); err != nil {
				t.Fatal(err)
			}
			command := &cobra.Command{}
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetContext(context.Background())

			err = runAxiHome(command)

			if scenario == "ledger error" {
				if err == nil || !strings.Contains(output.String(), "read validation family") {
					t.Fatalf("home silently lost invalid family authority: %s %v", output.String(), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var home struct {
				Active struct {
					Family struct {
						Origin   string `toon:"policy_origin"`
						Source   string `toon:"historical_source_completeness"`
						Spending []struct {
							Step      string `toon:"step"`
							Automatic int    `toon:"automatic_spent"`
							Remaining int    `toon:"remaining"`
						} `toon:"spending"`
					} `toon:"authored_validation"`
				} `toon:"active_run"`
			}
			text := output.String()
			if scenario == "other branch" {
				text = strings.Replace(text, "other_branch_active_run:", "active_run:", 1)
			}
			if err := toon.UnmarshalString(text, &home); err != nil {
				t.Fatal(err)
			}
			family := home.Active.Family
			hasReviewSpending := false
			for _, spending := range family.Spending {
				if spending.Step == "review" && spending.Automatic == 1 && spending.Remaining == 0 {
					hasReviewSpending = true
				}
			}
			if family.Origin != "controller_assertion" || family.Source != "unknown" || !hasReviewSpending {
				t.Fatalf("home lost cumulative provenance: %+v", family)
			}
			if scenario != "other branch" {
				for _, forbidden := range []string{"--action fix", "--action skip"} {
					if strings.Contains(text, forbidden) {
						t.Fatalf("authored home offered forbidden action %s: %s", forbidden, text)
					}
				}
			}
			if scenario == "protected path" || scenario == "unvalidated work" {
				if strings.Contains(text, "--action approve") || !strings.Contains(text, "axi abort") || !strings.Contains(text, "Approve is rejected") {
					t.Fatalf("refused authored approval advertised: %s", text)
				}
			}
		})
	}
}

func TestAuthoredGatePreservesApprovalRefusals(t *testing.T) {
	for _, id := range []string{"protected-path-refusal", types.FindingIDTestAgentUnvalidatedWork} {
		t.Run(id, func(t *testing.T) {
			command := newAxiRunCmd()
			var output bytes.Buffer
			command.SetOut(&output)
			findings := findingsJSON(t, []types.Finding{{ID: id, Description: "approval refused", Severity: "error", Action: types.ActionAskUser}}, "operator decision required")
			run := &ipc.RunInfo{ID: "child", Branch: "feature", Status: types.RunRunning, AuthoredValidation: &authoredvalidation.Family{AdmissionID: "admission", PredecessorID: "parent"}, Steps: []ipc.StepResultInfo{{StepName: types.StepTest, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings}}}

			if err := renderDriveResult(command, run, false); err != nil {
				t.Fatal(err)
			}

			text := output.String()
			for _, forbidden := range []string{"--action approve", "--action fix", "--action skip"} {
				if strings.Contains(text, forbidden) {
					t.Fatalf("authored refusal offered %s: %s", forbidden, text)
				}
			}
			if !strings.Contains(text, "axi abort") || !strings.Contains(text, "Approve is rejected") {
				t.Fatalf("gate lost refusal or permitted abort: %s", text)
			}
		})
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
