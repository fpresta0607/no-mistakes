//go:build e2e

package e2e

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func TestScaledTimeout(t *testing.T) {
	for _, test := range []struct {
		name    string
		scale   string
		timeout time.Duration
		want    time.Duration
	}{
		{name: "default", timeout: time.Minute, want: time.Minute},
		{name: "unchanged", scale: "1", timeout: 5 * time.Second, want: 5 * time.Second},
		{name: "ref read", scale: "4", timeout: 5 * time.Second, want: 20 * time.Second},
		{name: "worktree", scale: "4", timeout: 10 * time.Second, want: 40 * time.Second},
		{name: "push", scale: "4", timeout: 30 * time.Second, want: 2 * time.Minute},
		{name: "command", scale: "4", timeout: time.Minute, want: 4 * time.Minute},
		{name: "run wait", scale: "4", timeout: 90 * time.Second, want: 6 * time.Minute},
		{name: "maximum", scale: "1", timeout: math.MaxInt64, want: math.MaxInt64},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := scaledTimeout(test.timeout, test.scale)
			if err != nil || got != test.want {
				t.Fatalf("scaledTimeout(%v, %q) = %v, %v; want %v", test.timeout, test.scale, got, err, test.want)
			}
		})
	}
}

func TestScaledTimeoutRejectsInvalidScale(t *testing.T) {
	for _, scale := range []string{"0", "-1", "1.5", "invalid", " 4 ", "9223372036854775808", "9223372036854775807"} {
		t.Run(scale, func(t *testing.T) {
			if _, err := scaledTimeout(time.Second, scale); err == nil {
				t.Fatalf("scaledTimeout accepted invalid scale %q", scale)
			}
		})
	}
}

func TestRunGitReportsContextDeadline(t *testing.T) {
	h := &Harness{t: t}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := h.runGit(ctx, t.TempDir(), "--version")

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runGit error = %v; want context deadline exceeded", err)
	}
}
