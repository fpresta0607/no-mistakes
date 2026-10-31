package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestCIRevalidationRefusesInvalidOperatorInputsBeforeReadingRunState(t *testing.T) {
	for _, name := range []string{"automatic decisions", "finding action", "candidate", "reason", "relative profile", "login"} {
		t.Run(name, func(t *testing.T) {
			arguments := respondArgs{candidate: strings.Repeat("a", 40), reason: "validate this retained work", ghConfigDir: t.TempDir(), expectedLogin: "operator"}
			switch name {
			case "automatic decisions":
				arguments.autoYes = true
			case "finding action":
				arguments.findings = "f1"
			case "candidate":
				arguments.candidate = "HEAD"
			case "reason":
				arguments.reason = " "
			case "relative profile":
				arguments.ghConfigDir = "relative"
			case "login":
				arguments.expectedLogin = ""
			}
			var output bytes.Buffer
			command := &cobra.Command{}
			command.SetOut(&output)
			command.SetErr(&output)
			if err := runAxiCIRevalidation(command, nil, t.Context(), "unread-run", runView{}, arguments); err == nil {
				t.Fatal("invalid operator request reached the run-state boundary")
			}
		})
	}
}
