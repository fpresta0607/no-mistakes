package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A run whose test command starts behind a machine-local wrapper keeps the
// same record of its effective command configuration a run with any other
// machine-local override keeps, even when the wrapper is the only one.
func TestExecutor_CommandConfigurationRecordsATestCommandWrapper(t *testing.T) {
	database, p, run, repo := setupTest(t)
	global, err := config.LoadGlobalFromBytes([]byte(`test_command_wrapper:
  command: [turnline, --as, "{repo} {branch}", --]
  refusal_prefix: "turnline: "
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.MergeForRemote(global, &config.RepoConfig{Commands: config.Commands{Test: "team-tests"}}, repo.UpstreamURL)
	if len(cfg.CommandOverrides) != 0 {
		t.Fatalf("fixture has command overrides %v, want the wrapper alone", cfg.CommandOverrides)
	}
	path := filepath.Join(p.RunLogDir(run.ID), commandConfigurationFile)
	step := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("a run behind a wrapper left no record of its command configuration: %v", err)
		}
		var evidence struct {
			EffectiveConfig config.Config `json:"effective_config"`
		}
		if err := json.Unmarshal(payload, &evidence); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(evidence.EffectiveConfig.TestCommandWrapper, cfg.TestCommandWrapper) {
			t.Fatalf("recorded wrapper = %+v, want %+v", evidence.EffectiveConfig.TestCommandWrapper, cfg.TestCommandWrapper)
		}
		return &StepOutcome{}, nil
	}}
	executor := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

// With neither an override nor a wrapper, no record is written: today's
// behavior.
func TestExecutor_CommandConfigurationNotRecordedWithNoWrapperOrOverride(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := config.MergeForRemote(config.DefaultGlobalConfig(), &config.RepoConfig{Commands: config.Commands{Test: "team-tests"}}, repo.UpstreamURL)
	path := filepath.Join(p.RunLogDir(run.ID), commandConfigurationFile)
	step := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("command configuration record exists with nothing machine-local to record (stat error %v)", err)
		}
		return &StepOutcome{}, nil
	}}
	executor := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
