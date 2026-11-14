package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestParseReceiveUpdates(t *testing.T) {
	old, new := strings.Repeat("1", 40), strings.Repeat("2", 40)
	for _, test := range []struct {
		name        string
		input       string
		want        []ipc.GateRefUpdate
		wantErrorIs string
	}{
		{name: "no input", input: ""},
		{name: "one update", input: old + " " + new + " refs/heads/feature\n", want: []ipc.GateRefUpdate{{Old: old, New: new, Ref: "refs/heads/feature"}}},
		{name: "several updates without a final newline", input: old + " " + new + " refs/heads/a\n" + new + " " + old + " refs/heads/b", want: []ipc.GateRefUpdate{{Old: old, New: new, Ref: "refs/heads/a"}, {Old: new, New: old, Ref: "refs/heads/b"}}},
		{name: "a missing field", input: old + " refs/heads/feature\n", wantErrorIs: "malformed proposed ref update"},
		{name: "an extra field", input: old + " " + new + " refs/heads/feature extra\n", wantErrorIs: "malformed proposed ref update"},
		{name: "a blank line between updates", input: old + " " + new + " refs/heads/a\n\n" + old + " " + new + " refs/heads/b\n", wantErrorIs: "malformed proposed ref update"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseReceiveUpdates(strings.NewReader(test.input))

			if test.wantErrorIs != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrorIs) {
					t.Fatalf("error = %v, want one naming %q", err, test.wantErrorIs)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("updates = %+v, %v; want %+v", got, err, test.want)
			}
		})
	}
}
