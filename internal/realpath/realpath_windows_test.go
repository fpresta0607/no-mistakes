//go:build windows

package realpath

import "testing"

func TestTrimExtendedPrefix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{`\\?\C:\Users\x`, `C:\Users\x`},
		{`\\?\UNC\server\share\x`, `\\server\share\x`},
		{`C:\already\plain`, `C:\already\plain`},
	} {
		if got := trimExtendedPrefix(tc.in); got != tc.want {
			t.Errorf("trimExtendedPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
