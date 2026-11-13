package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCIWorkflowRunsTestsOnAllSupportedDesktopPlatforms(t *testing.T) {
	job := ciTestJob(t)
	got := make(map[string]int)
	for _, row := range job.Strategy.Matrix.Include {
		got[row["os"]]++
	}

	want := map[string]int{
		"ubuntu-latest":  1,
		"macos-latest":   3,
		"windows-latest": 18,
	}
	if len(got) != len(want) {
		t.Fatalf("test matrix operating systems = %v, want %v", got, want)
	}
	for osName, count := range want {
		if got[osName] != count {
			t.Errorf("test matrix rows for %q = %d, want %d", osName, got[osName], count)
		}
	}
}

func unixTestStep(t *testing.T) wfStep {
	t.Helper()
	for _, step := range ciTestJob(t).Steps {
		if exactRunnerOSCondition(step.If, "!=", "Windows") && step.Env["NM_CI_UNIX_SHARD_EXCLUDE"] != "" {
			return step
		}
	}
	t.Fatal("CI workflow has no Unix test step declaring NM_CI_UNIX_SHARD_EXCLUDE")
	return wfStep{}
}

// unixShardExcludePattern reads the exclusion the Unix step declares so a guard
// resolves the package set the step actually runs instead of trusting the word
// ./... to sit somewhere in the script.
func unixShardExcludePattern(t *testing.T, step wfStep) *regexp.Regexp {
	t.Helper()
	pattern := step.Env["NM_CI_UNIX_SHARD_EXCLUDE"]
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("NM_CI_UNIX_SHARD_EXCLUDE %q: %v", pattern, err)
	}
	return compiled
}

// unixRemainderPackages is the package set the Unix leg's go-list remainder
// runs: every package in the module the step's declared exclusion does not
// match.
func unixRemainderPackages(t *testing.T) []string {
	t.Helper()
	return filterPackages(goListPackages(t, "./..."), unixShardExcludePattern(t, unixTestStep(t)), false)
}

func unixGoTestCommands(t *testing.T) []workflowCommand {
	t.Helper()
	var tests []workflowCommand
	for _, command := range workflowCommandsMatching(ciTestJob(t).Steps, func(step wfStep) bool {
		return exactRunnerOSCondition(step.If, "!=", "Windows")
	}) {
		if command.name == "go" && len(command.args) > 0 && command.args[0] == "test" {
			tests = append(tests, command)
		}
	}
	if len(tests) == 0 {
		t.Fatal("CI workflow has no Unix go test command")
	}
	return tests
}

func TestCIWorkflowUsesRaceTestsOnUnixRunners(t *testing.T) {
	for _, command := range unixGoTestCommands(t) {
		if !command.hasArg("-race") {
			t.Errorf("Unix go test command must run with -race: %#v", command.args)
		}
	}
}

// internal/pipeline/steps and internal/daemon each outgrew the per-package
// budget on macos-latest: pipeline/steps timed out at 1201s in the merge train
// while internal/daemon passed the same run at 1136s, every parked goroutine in
// the dump was a t.Parallel waiter and the three running tests were a second
// old or less. That is exhausted capacity, not a hang - the signature the
// testing-conventions skill records - so both packages run their own tests in
// complementary -run/-skip shards, the same split the Windows rows use, and
// every shard starts at once (on its own macOS runner, issue #170, where
// ^Test[A-K] of steps and both daemon halves reached 20m again, so each is cut
// once more along the Windows rows' -v timings). Serializing the suite
// instead (-p=1) summed the package walls and overran the job cap, so the
// remainder keeps its concurrency and only the two packages that exceed the
// budget on their own are sharded. Each shard owns its own budget while no
// test, timeout or coverage is given up, so the shards and the remainder must
// cover go list ./... exactly once and select every declared test exactly once.
func TestCIWorkflow_UnixShardsThePackagesThatOutgrewTheBudget(t *testing.T) {
	wantShards := map[string]int{
		"./internal/pipeline/steps": 4,
		"./internal/daemon":         4,
	}
	var wanted []string
	for pkg := range wantShards {
		wanted = append(wanted, pkg)
	}
	slices.Sort(wanted)

	commands := unixGoTestCommands(t)
	var remainder *workflowCommand
	shardsByPackage := make(map[string][]namedTestShard, len(wantShards))
	for index, command := range commands {
		patterns := goTestPackagePatterns(command)
		if len(patterns) == 0 {
			if remainder != nil {
				t.Fatalf("Unix go test runs a second computed package set: %#v and %#v", remainder.args, command.args)
			}
			remainder = &commands[index]
			continue
		}
		if len(patterns) != 1 {
			t.Fatalf("each Unix shard must name one package, got %v", patterns)
		}
		if _, ok := wantShards[patterns[0]]; !ok {
			t.Fatalf("Unix go test shards %v; only %v exceed the per-package budget on their own", patterns[0], wanted)
		}
		shardsByPackage[patterns[0]] = append(shardsByPackage[patterns[0]], namedTestShard{
			name: strings.Join(command.args, " "),
			run:  namedTestPattern(t, command, "-run"),
			skip: namedTestPattern(t, command, "-skip"),
		})
	}
	if remainder == nil {
		t.Fatalf("Unix go test must still run the rest of the suite beside the shards: %#v", commands)
	}
	if patterns := goTestPackagePatterns(*remainder); len(patterns) != 0 {
		t.Fatalf("the Unix command that runs the rest of the suite must run the computed go list package set, not the literal patterns %v", patterns)
	}

	covered := unixRemainderPackages(t)
	for _, pkg := range wanted {
		shards, ok := shardsByPackage[pkg]
		if !ok {
			t.Fatalf("Unix go test does not shard %s, whose own wall exceeds the per-package budget: %#v", pkg, commands)
		}
		if want := wantShards[pkg]; len(shards) != want {
			t.Fatalf("Unix go test runs %s as %d shards, want %d complementary shards so each stays inside -timeout: %#v", pkg, len(shards), want, commands)
		}
		covered = append(covered, goListPackages(t, pkg)...)

		names := append(declaredTestNames(t, filepath.FromSlash(strings.TrimPrefix(pkg, "./"))), "Test", "Test_Probe")
		for c := 'A'; c <= 'Z'; c++ {
			names = append(names, "Test"+string(c)+"Probe")
		}
		for c := '0'; c <= '9'; c++ {
			names = append(names, "Test"+string(c)+"Probe")
		}
		for _, name := range names {
			var selectedBy []string
			for _, shard := range shards {
				if shard.selects(name) {
					selectedBy = append(selectedBy, shard.name)
				}
			}
			if len(selectedBy) != 1 {
				t.Errorf("%s is selected by %d Unix %s shards (%v), want exactly 1", name, len(selectedBy), pkg, selectedBy)
			}
		}
	}
	slices.Sort(covered)
	if all := goListPackages(t, "./..."); !slices.Equal(covered, all) {
		t.Fatalf("the Unix remainder plus every sharded package must cover go list ./... exactly once: covered %v, module %v", covered, all)
	}
}

func namedTestPattern(t *testing.T, command workflowCommand, flag string) *regexp.Regexp {
	t.Helper()
	pattern, ok := commandFlagValue(command, flag)
	if !ok || pattern == "" {
		return nil
	}
	if strings.Contains(pattern, "/") {
		t.Fatalf("Unix steps shard %s %q contains /, which go test splits into subtest levels", flag, pattern)
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("Unix steps shard %s %q: %v", flag, pattern, err)
	}
	return compiled
}

func commandFlagValue(command workflowCommand, flag string) (string, bool) {
	for index, arg := range command.args {
		if arg == flag && index+1 < len(command.args) {
			return unquoteWorkflowArg(command.args[index+1]), true
		}
		if value, found := strings.CutPrefix(arg, flag+"="); found {
			return unquoteWorkflowArg(value), true
		}
	}
	return "", false
}

// The Unix legs ran on go test's implicit 10m per-package budget until
// internal/pipeline/steps, internal/daemon and internal/cli outran it on a slow
// macos-latest runner with no test hung (issue #73). Every shard binary starts
// with the rest, so its own budget is all the slack there is: it has to stay
// above that default and still fire before the job cap, so a real hang lands as
// a goroutine dump rather than a cancelled job.
func TestCIWorkflow_UnixPackageBudgetExceedsGoDefaultAndFiresBeforeJobCap(t *testing.T) {
	job := ciTestJob(t)

	const deliberatePackageBudget = 20 * time.Minute
	// Checkout, setup-go and the go list run before the first test binary
	// starts; they took a couple of minutes on the macos-latest legs that were
	// cancelled.
	const measuredPrelude = 5 * time.Minute

	for _, command := range unixGoTestCommands(t) {
		budget := goTestTimeout(t, command)
		if budget != deliberatePackageBudget {
			t.Errorf("Unix go test -timeout is %s, want the unchanged %s package budget that clears go test's 10m default for every shard: %v", budget, deliberatePackageBudget, command.args)
		}
		if jobCap := time.Duration(job.TimeoutMinutes) * time.Minute; budget+measuredPrelude >= jobCap {
			t.Errorf("Unix go test -timeout %s plus the %s measured prelude reaches the %s job cap; the Go timeout must fire first so a hang in any shard produces a goroutine dump", budget, measuredPrelude, jobCap)
		}
	}
}

// The Unix remainder's package set comes from go list, so a failed or empty
// listing must fail the step. An empty expansion runs go test against the
// repository root alone while every shard still reports green, and
// internal/daemon, internal/cli, internal/git and the rest of the suite would
// never run on that leg.
func TestCIWorkflow_UnixStepFailsClosedWhenTheRemainderPackageSetIsEmpty(t *testing.T) {
	t.Parallel()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable to execute the Unix test step")
	}
	if runtime.GOOS == "windows" && !isNativeGitBash(t, bash) {
		t.Skipf("resolved bash %q is not native Git Bash, and the guarded Unix step never runs on Windows", bash)
	}
	step := unixTestStep(t)

	cases := []struct {
		name        string
		goList      string
		goListExit  string
		wantTests   []string
		wantFailure bool
	}{
		{
			name:        "empty listing",
			goListExit:  "0",
			wantFailure: true,
		},
		{
			name:        "failed listing",
			goListExit:  "1",
			wantFailure: true,
		},
		{
			name:      "resolved listing",
			goList:    unixResolvedGoList,
			wantTests: append([]string{unixRemainderInvocation}, unixShardInvocations...),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tests, err, output := runUnixTestStep(t, bash, step, "", tc.goList, tc.goListExit)
			if tc.wantFailure {
				if err == nil {
					t.Fatalf("the Unix test step exited 0 after go list %q (exit %s); it must fail instead of running a narrowed suite\n%s", tc.goList, tc.goListExit, output)
				}
				if len(tests) != 0 {
					t.Fatalf("the Unix test step ran go %v after its package listing yielded nothing", tests)
				}
				return
			}
			if err != nil {
				t.Fatalf("the Unix test step failed with a resolved package listing: %v\n%s", err, output)
			}
			slices.Sort(tests)
			want := slices.Clone(tc.wantTests)
			slices.Sort(want)
			if !slices.Equal(tests, want) {
				t.Fatalf("Unix go test invocations = %v, want %v", tests, want)
			}
		})
	}
}

const unixResolvedGoList = "github.com/kunchenguid/no-mistakes\ngithub.com/kunchenguid/no-mistakes/internal/daemon\ngithub.com/kunchenguid/no-mistakes/internal/pipeline/steps\ngithub.com/kunchenguid/no-mistakes/internal/git\n"

const unixRemainderInvocation = "test -race -timeout=20m -p=2 github.com/kunchenguid/no-mistakes github.com/kunchenguid/no-mistakes/internal/git"

var unixStepsShardInvocations = []string{
	"test -race -timeout=20m -run ^TestCIStep ./internal/pipeline/steps",
	"test -race -timeout=20m -run ^Test[A-K] -skip ^TestCIStep ./internal/pipeline/steps",
	"test -race -timeout=20m -run ^Test[L-Q] ./internal/pipeline/steps",
	"test -race -timeout=20m -skip ^Test[A-Q] ./internal/pipeline/steps",
}

var unixDaemonShardInvocations = []string{
	"test -race -timeout=20m -run ^Test[A-L] ./internal/daemon",
	"test -race -timeout=20m -run ^Test[M-O] ./internal/daemon",
	"test -race -timeout=20m -run ^Test[P-R] ./internal/daemon",
	"test -race -timeout=20m -skip ^Test[A-R] ./internal/daemon",
}

var unixShardInvocations = slices.Concat(unixStepsShardInvocations, unixDaemonShardInvocations)

// The macOS leg is capacity-bound: splitting its binaries on one runner moved
// work between them but finished the step no sooner, within minutes of the job
// cap (issue #170), and with all eight shard binaries on one runner the shards
// row still took up to 1200s per shard on a slow runner (issue #237). So each
// Unix OS runs the whole step on one row, or splits it into exactly one
// remainder row, one pipeline/steps shards row and one daemon shards row, and
// macos-latest splits it.
func TestCIWorkflow_UnixRowsRunEachPartExactlyOnce(t *testing.T) {
	t.Parallel()

	if got := unixTestStep(t).Env["NM_CI_UNIX_PART"]; normalizeWorkflowCondition(got) != "matrix.unix_part" {
		t.Fatalf("Unix test step NM_CI_UNIX_PART = %q, want it read from matrix.unix_part", got)
	}
	parts := make(map[string][]string)
	for _, row := range ciTestJob(t).Strategy.Matrix.Include {
		if row["os"] == "windows-latest" {
			continue
		}
		parts[row["os"]] = append(parts[row["os"]], row["unix_part"])
	}
	want := map[string][]string{
		"ubuntu-latest": {""},
		"macos-latest":  {"daemon", "rest", "steps"},
	}
	if len(parts) != len(want) {
		t.Fatalf("Unix matrix rows = %v, want %v", parts, want)
	}
	for osName, wantParts := range want {
		gotParts := slices.Clone(parts[osName])
		slices.Sort(gotParts)
		if !slices.Equal(gotParts, wantParts) {
			t.Errorf("%s rows run Unix parts %q, want %q so every test runs exactly once on that OS", osName, gotParts, wantParts)
		}
	}
}

// A split row must run only its own part, and a part the step does not know
// must fail rather than run nothing or everything.
func TestCIWorkflow_UnixStepRunsOnlyItsRowsPart(t *testing.T) {
	t.Parallel()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable to execute the Unix test step")
	}
	if runtime.GOOS == "windows" && !isNativeGitBash(t, bash) {
		t.Skipf("resolved bash %q is not native Git Bash, and the guarded Unix step never runs on Windows", bash)
	}
	step := unixTestStep(t)

	cases := []struct {
		name        string
		part        string
		goList      string
		goListExit  string
		wantTests   []string
		wantFailure bool
	}{
		{
			name:      "rest runs only the remainder",
			part:      "rest",
			goList:    unixResolvedGoList,
			wantTests: []string{unixRemainderInvocation},
		},
		{
			name:        "rest still fails closed on an empty listing",
			part:        "rest",
			wantFailure: true,
		},
		{
			name:       "steps runs only the steps package shards and needs no listing",
			part:       "steps",
			goListExit: "1",
			wantTests:  unixStepsShardInvocations,
		},
		{
			name:       "daemon runs only the daemon shards and needs no listing",
			part:       "daemon",
			goListExit: "1",
			wantTests:  unixDaemonShardInvocations,
		},
		{
			name:        "an unknown part runs nothing",
			part:        "all",
			goList:      unixResolvedGoList,
			wantFailure: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tests, err, output := runUnixTestStep(t, bash, step, tc.part, tc.goList, tc.goListExit)
			if tc.wantFailure {
				if err == nil {
					t.Fatalf("the Unix test step exited 0 for part %q; it must fail\n%s", tc.part, output)
				}
				if len(tests) != 0 {
					t.Fatalf("the Unix test step ran go %v for part %q, want nothing", tests, tc.part)
				}
				return
			}
			if err != nil {
				t.Fatalf("the Unix test step failed for part %q: %v\n%s", tc.part, err, output)
			}
			slices.Sort(tests)
			want := slices.Clone(tc.wantTests)
			slices.Sort(want)
			if !slices.Equal(tests, want) {
				t.Fatalf("Unix go test invocations for part %q = %v, want %v", tc.part, tests, want)
			}
		})
	}
}

// runUnixTestStep executes the Unix test step's script for one matrix part with
// a fake go on PATH that reports the given listing for `go list` and records
// every other invocation, so the guard observes the command the step really
// runs.
func runUnixTestStep(t *testing.T, bash string, step wfStep, part, goList, goListExit string) ([]string, error, string) {
	t.Helper()
	dir := t.TempDir()
	shimDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("create fake go directory: %v", err)
	}
	logDir := filepath.Join(dir, "go-log")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("create fake go log directory: %v", err)
	}
	// The step runs its go invocations concurrently, so each one records itself
	// in its own file: an append to a shared log can interleave with a sibling's
	// write and merge two invocations into one unparseable record.
	const shim = `#!/bin/sh
if [ "$1" = "list" ]; then
	printf '%s' "$FAKE_GO_LIST"
	exit "$FAKE_GO_LIST_EXIT"
fi
record=$(mktemp "$FAKE_GO_LOG/record.XXXXXXXX") || exit 1
printf '%s\n' "$@" > "$record"
`
	if err := os.WriteFile(filepath.Join(shimDir, "go"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}
	scriptPath := filepath.Join(dir, "step.sh")
	if err := os.WriteFile(scriptPath, []byte(step.Run), 0o644); err != nil {
		t.Fatalf("write Unix test step script: %v", err)
	}

	if goListExit == "" {
		goListExit = "0"
	}
	cmd := exec.Command(bash, "-e", scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_GO_LIST="+goList,
		"FAKE_GO_LIST_EXIT="+goListExit,
		"FAKE_GO_LOG="+logDir,
	)
	for name, value := range step.Env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	// The step reads its part from the matrix, which only Actions expands.
	cmd.Env = append(cmd.Env, "NM_CI_UNIX_PART="+part)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()

	records, readErr := os.ReadDir(logDir)
	if readErr != nil {
		t.Fatalf("read recorded go invocations: %v", readErr)
	}
	var tests []string
	for _, record := range records {
		data, readErr := os.ReadFile(filepath.Join(logDir, record.Name()))
		if readErr != nil {
			t.Fatalf("read recorded go invocation %s: %v", record.Name(), readErr)
		}
		args := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		if len(args) == 0 || (len(args) == 1 && args[0] == "") {
			continue
		}
		tests = append(tests, strings.Join(args, " "))
	}
	return tests, err, output.String()
}

// isNativeGitBash reports whether bash identifies as the MSYS shell that runs
// the Unix step, so a WSL launcher earlier on PATH cannot make the guard hand a
// checkout path to a Linux shell.
func isNativeGitBash(t *testing.T, bash string) bool {
	t.Helper()
	output, err := exec.Command(bash, "-c", `printf %s "$OSTYPE"`).Output()
	return err == nil && strings.TrimSpace(string(output)) == "msys"
}
