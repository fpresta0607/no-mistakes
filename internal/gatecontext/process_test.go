package gatecontext

import (
	"os"
	"runtime"
	"testing"
	"time"
)

func TestProcessInfoReportsTheLiveParentAndNeverCutsIt(t *testing.T) {
	self, err := processInfo(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if self.ParentPID != os.Getppid() {
		t.Fatalf("parent pid = %d, want %d", self.ParentPID, os.Getppid())
	}
	if runtime.GOOS != "windows" {
		return
	}
	if self.Started.IsZero() || self.Started.After(time.Now()) {
		t.Fatalf("own creation time = %v, want a past time", self.Started)
	}
	parent, err := processInfo(self.ParentPID)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Started.IsZero() || parent.Started.After(self.Started) {
		t.Fatalf("live parent created %v, after its child at %v", parent.Started, self.Started)
	}
	chain, err := ancestry(os.Getpid(), processInfo)
	if err != nil {
		t.Fatal(err)
	}
	if !chain[self.ParentPID] {
		t.Fatal("ancestry cut the live parent")
	}
}
