//go:build linux

package shellenv

import (
	"strings"
	"testing"
)

func TestParseProcStat(t *testing.T) {
	tail := " 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 987654 18 19"
	for _, tc := range []struct {
		name   string
		line   string
		want   procStat
		wantOK bool
	}{
		{name: "plain name", line: "42 (sleep) S 1 77" + tail, want: procStat{state: 'S', pgid: 77, startTicks: 987654}, wantOK: true},
		{name: "name with spaces and parentheses", line: "42 (a (b) c) Z 1 77" + tail, want: procStat{state: 'Z', pgid: 77, startTicks: 987654}, wantOK: true},
		{name: "no closing parenthesis", line: "42 (sleep S 1 77" + tail},
		{name: "too few fields", line: "42 (sleep) S 1 77 1 2"},
		{name: "group not a number", line: "42 (sleep) S 1 x" + tail},
		{name: "start not a number", line: "42 (sleep) S 1 77" + strings.Replace(tail, "987654", "x", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseProcStat(tc.line)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("parseProcStat() = %+v, %t; want %+v, %t", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
