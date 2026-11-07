//go:build e2e

package e2e

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"time"
)

func scaledTimeout(timeout time.Duration, scale string) (time.Duration, error) {
	if scale == "" {
		return timeout, nil
	}
	factor, err := strconv.ParseInt(scale, 10, 64)
	if err != nil || factor < 1 {
		return 0, fmt.Errorf("NM_E2E_TIMEOUT_SCALE must be a positive integer")
	}
	if int64(timeout) > math.MaxInt64/factor {
		return 0, fmt.Errorf("NM_E2E_TIMEOUT_SCALE overflows timeout %v", timeout)
	}
	return timeout * time.Duration(factor), nil
}

func (h *Harness) timeout(timeout time.Duration) time.Duration {
	h.t.Helper()
	scaled, err := scaledTimeout(timeout, os.Getenv("NM_E2E_TIMEOUT_SCALE"))
	if err != nil {
		h.t.Fatal(err)
	}
	return scaled
}
