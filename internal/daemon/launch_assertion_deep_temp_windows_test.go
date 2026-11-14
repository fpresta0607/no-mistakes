package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #152: under a nested Test scratch TMP the authored-admission fixture
// failed its setup push with "Filename too long", because its gate root sat in
// a t.TempDir() that carries the test's long name. A 100-character TMP is past
// where that failed (about 78) and short of where this test's own
// t.TempDir() work repositories reach MAX_PATH (about 120), so a deeper
// caller TMP only adds one character.
func TestAuthoredAdmissionFixtureValidatesUnderADeepTemporaryRoot(t *testing.T) {
	// Arrange
	const depth = 100
	parent := shortTempDir(t)
	deep := filepath.Join(parent, strings.Repeat("t", max(1, depth-len(parent)-1)))
	if err := os.Mkdir(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	// Set before this test's first t.TempDir(), which reads GOTMPDIR and then TMP.
	for _, key := range []string{"TMP", "TEMP", "GOTMPDIR"} {
		t.Setenv(key, deep)
	}
	step := &mockPassStep{name: types.StepReview}
	manager, database, _, request, _ := authoredAdmissionFixture(t, func() []pipeline.Step { return []pipeline.Step{step} })

	// Act
	result, err := manager.validateAuthored(t.Context(), request, os.Getpid())

	// Assert
	if err != nil || !result.Started {
		t.Fatalf("admission under a %d-character TMP: %+v %v", len(deep), result, err)
	}
	if child := waitForRunTerminalState(t, database, result.Receipt.RunID); child.Status != types.RunCompleted || step.execCnt.Load() != 1 {
		t.Fatalf("validation under a deep TMP: status=%s executions=%d", child.Status, step.execCnt.Load())
	}
}
