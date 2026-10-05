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
	if _, err := EffectiveSelection(types.AgentClaude, Profile{Model: "configured", Effort: EffortHigh}, nil); err == nil {
		t.Fatal("unsupported assertion harness was accepted")
	}
}
