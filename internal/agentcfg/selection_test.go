package agentcfg

import (
	"fmt"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestEffectiveSelectionUsesNativeOverrides(t *testing.T) {
	for _, test := range []struct {
		name   string
		args   []string
		model  string
		effort Effort
		tier   string
	}{
		{"typed", []string{"-c", `service_tier="default"`}, "configured", EffortHigh, "default"},
		{"native model", []string{"--model=native", "-c", `service_tier="default"`}, "native", EffortHigh, "default"},
		{"native config", []string{"--config=model='native'", "-c=model_reasoning_effort='xhigh'", "-c", `service_tier="priority"`}, "native", EffortXHigh, "priority"},
		{"last config wins", []string{"-c", `service_tier="priority"`, "-c", `service_tier="default"`}, "configured", EffortHigh, "default"},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual, err := EffectiveSelection(types.AgentCodex, Profile{Model: "configured", Effort: EffortHigh}, test.args)
			if err != nil {
				t.Fatal(err)
			}
			want := Selection{Harness: types.AgentCodex, Model: test.model, Effort: test.effort, ServiceTier: test.tier}
			if actual != want {
				t.Fatalf("selection = %+v, want %+v", actual, want)
			}
		})
	}
}

func TestEffectiveSelectionRefusesUnprovableNativeSettings(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"-c", "service_tier=null"},
		{"--profile", "hidden", "-c", `service_tier="default"`},
		{"--fast", "-c", `service_tier="default"`},
		{"-m", "one", "-c", `model="two"`, "-c", `service_tier="default"`},
		{"-c", `model_reasoning_effort="unknown"`, "-c", `service_tier="default"`},
		{"-c", `model_provider="hidden"`, "-c", `service_tier="default"`},
		{"-m"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			if _, err := EffectiveSelection(types.AgentCodex, Profile{Model: "configured", Effort: EffortHigh}, args); err == nil {
				t.Fatal("unprovable native selection was accepted")
			}
		})
	}
	if _, err := EffectiveSelection(types.AgentPi, Profile{Model: "configured", Effort: EffortHigh}, nil); err == nil {
		t.Fatal("unsupported assertion harness was accepted")
	}
}

func TestEffectiveSelectionReadsClaudeFlags(t *testing.T) {
	for _, test := range []struct {
		name   string
		args   []string
		model  string
		effort Effort
	}{
		{"typed", nil, "configured", EffortHigh},
		{"native model", []string{"--model=native"}, "native", EffortHigh},
		{"native effort", []string{"--effort", "max"}, "configured", EffortMax},
		{"execution flags", []string{"--setting-sources", "user", "--permission-mode=acceptEdits", "--dangerously-skip-permissions"}, "configured", EffortHigh},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual, err := EffectiveSelection(types.AgentClaude, Profile{Model: "configured", Effort: EffortHigh}, test.args)
			if err != nil {
				t.Fatal(err)
			}
			want := Selection{Harness: types.AgentClaude, Model: test.model, Effort: test.effort}
			if actual != want {
				t.Fatalf("selection = %+v, want %+v", actual, want)
			}
		})
	}
}

func TestEffectiveSelectionRefusesUnprovableClaudeSettings(t *testing.T) {
	for _, test := range []struct {
		name    string
		profile Profile
		args    []string
	}{
		{"implicit effort", Profile{Model: "configured"}, nil},
		{"implicit model", Profile{Effort: EffortHigh}, nil},
		{"repeated model", Profile{Effort: EffortHigh}, []string{"--model", "one", "--model", "two"}},
		{"repeated effort", Profile{Model: "configured"}, []string{"--effort", "low", "--effort=high"}},
		{"unknown effort", Profile{Model: "configured"}, []string{"--effort", "fastest"}},
		{"fallback model", Profile{Model: "configured", Effort: EffortHigh}, []string{"--fallback-model", "other"}},
		{"settings override", Profile{Model: "configured", Effort: EffortHigh}, []string{"--settings", "hidden.json"}},
		{"incomplete flag", Profile{Effort: EffortHigh}, []string{"--model"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := EffectiveSelection(types.AgentClaude, test.profile, test.args); err == nil {
				t.Fatal("unprovable Claude selection was accepted")
			}
		})
	}
}

func TestSelectionValidateKeepsEachHarnessShape(t *testing.T) {
	for _, test := range []struct {
		name      string
		selection Selection
		isValid   bool
	}{
		{"codex", Selection{Harness: types.AgentCodex, Model: "m", Effort: EffortHigh, ServiceTier: "default"}, true},
		{"codex without tier", Selection{Harness: types.AgentCodex, Model: "m", Effort: EffortHigh}, false},
		{"claude", Selection{Harness: types.AgentClaude, Model: "m", Effort: EffortHigh}, true},
		{"claude with tier", Selection{Harness: types.AgentClaude, Model: "m", Effort: EffortHigh, ServiceTier: "default"}, false},
		{"claude without effort", Selection{Harness: types.AgentClaude, Model: "m"}, false},
		{"other harness", Selection{Harness: types.AgentPi, Model: "m", Effort: EffortHigh}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.selection.Validate(); (err == nil) != test.isValid {
				t.Fatalf("Validate() = %v, want valid=%v", err, test.isValid)
			}
		})
	}
}
