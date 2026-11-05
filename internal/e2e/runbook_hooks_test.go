//go:build e2e

package e2e

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// TestRunbookHooksKeepTheirCitedNames holds the trusted Test runbook in
// .no-mistakes.yaml to the hooks it sends the agent to. A function's name comes
// from its compiled symbol, so a rename that also rewrites the reference here
// still fails against a stale runbook. The gh variables go through the real
// stub, because only their consumer can show they still do what the runbook
// says: without them it stays fail-closed and records no PR body.
func TestRunbookHooksKeepTheirCitedNames(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadRepo(repoRoot)
	if err != nil {
		t.Fatalf("LoadRepo: %v", err)
	}
	const modeVariable, logVariable = "FAKEAGENT_GH_MODE", "FAKEAGENT_GH_LOG"
	cited := []string{modeVariable, logVariable}
	for _, hook := range []any{(*Harness).AgentInvocations, readGHStubInvocations} {
		symbol := runtime.FuncForPC(reflect.ValueOf(hook).Pointer()).Name()
		cited = append(cited, symbol[strings.LastIndex(symbol, ".")+1:])
	}
	for _, name := range cited {
		if !strings.Contains(cfg.Test.Instructions, name) {
			t.Errorf("trusted test.instructions no longer cites %s:\n%s", name, cfg.Test.Instructions)
		}
	}

	h := NewHarness(t, SetupOpts{})
	ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-runbook.log")
	t.Setenv(modeVariable, "fork-pr")
	t.Setenv(logVariable, ghLog)
	const prBody = "body the PR phase would publish\n"

	gh := exec.Command(filepath.Join(h.BinDir, executableName("gh")), "pr", "create", "--body-file", "-")
	winproc.Harden(gh)
	gh.Stdin = strings.NewReader(prBody)
	if out, err := gh.CombinedOutput(); err != nil {
		t.Fatalf("gh stub pr create: %v\n%s", err, out)
	}

	invocations := readGHStubInvocations(t, ghLog)
	if len(invocations) != 1 || invocations[0].Body != prBody {
		t.Fatalf("gh stub recorded %+v, want one call with body %q", invocations, prBody)
	}
}
