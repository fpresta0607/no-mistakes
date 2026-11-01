package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestTerminalContinuationRequiresExplicitReason(t *testing.T) {
	cmd := newAxiSyncCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--recover", "--continue"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(output.String(), "--reason") {
		t.Fatalf("continuation must require an operator reason: %v %s", err, output.String())
	}
}
