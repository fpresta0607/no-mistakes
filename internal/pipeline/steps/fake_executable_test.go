package steps

import (
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// A fork holds syscall.ForkLock for writing while it copies this process's
// descriptors. A script written during that hold can leave the forked child
// with its write descriptor, and the exec of the script then fails with
// ETXTBSY (golang/go#22315), which is how a fake agent never starts.
func TestWriteFakeExecutableWaitsOutAForkInFlight(t *testing.T) {
	var hasForkEnded atomic.Bool
	released := make(chan struct{})
	syscall.ForkLock.Lock()
	time.AfterFunc(50*time.Millisecond, func() {
		hasForkEnded.Store(true)
		syscall.ForkLock.Unlock()
		close(released)
	})

	writeFakeExecutable(t, filepath.Join(t.TempDir(), "fake"), "#!/bin/sh\n")
	wroteDuringFork := !hasForkEnded.Load()
	<-released

	if wroteDuringFork {
		t.Fatal("the script was written while a fork held syscall.ForkLock")
	}
}
