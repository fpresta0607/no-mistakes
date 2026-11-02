//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeInitKeepsUserSkillsInFixtureHome(t *testing.T) {
	ambientHome := t.TempDir()
	t.Setenv("HOME", ambientHome)
	t.Setenv("USERPROFILE", ambientHome)
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if home, err := os.UserHomeDir(); err != nil || home != h.HomeDir {
		t.Fatalf("native init user home = %q, %v; want fixture %q", home, err, h.HomeDir)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("native init: %v\n%s", err, out)
	}
	for _, base := range []string{".agents", ".claude"} {
		relative := filepath.Join(base, "skills", "no-mistakes", "SKILL.md")
		generated, err := os.ReadFile(filepath.Join(h.HomeDir, relative))
		if err != nil || len(generated) == 0 {
			t.Fatalf("native init did not install fixture skill %s: %v", relative, err)
		}
		if _, err := os.Stat(filepath.Join(ambientHome, base)); !os.IsNotExist(err) {
			t.Fatalf("native init wrote to inherited user home: %s, %v", base, err)
		}
	}
	if invocations := h.AgentInvocations(); len(invocations) != 0 {
		t.Fatalf("native init invoked an agent: %#v", invocations)
	}
}
