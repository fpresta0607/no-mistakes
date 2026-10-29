package agent

import (
	"context"
	"strings"
	"testing"
)

func TestServerAgentsRefuseAHiddenDesktopRatherThanStartOnTheUsersDesktop(t *testing.T) {
	opencode := &opencodeAgent{bin: "opencode"}
	rovodev := &rovodevAgent{bin: "rovodev"}
	for _, tc := range []struct {
		name      string
		agent     Agent
		isStarted func() bool
	}{
		{"opencode", opencode, func() bool { return opencode.server != nil }},
		{"rovodev", rovodev, func() bool { return rovodev.server != nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.agent.Run(context.Background(), RunOpts{Prompt: "check", CWD: t.TempDir(), Desktop: "no-mistakes-0123456789abcdef"})

			if err == nil || !strings.Contains(err.Error(), "cannot start on this invocation's hidden desktop") {
				t.Fatalf("Run() error = %v, want a refusal of the hidden desktop", err)
			}
			if tc.isStarted() {
				t.Error("the server started although the invocation was refused")
			}
		})
	}
}
