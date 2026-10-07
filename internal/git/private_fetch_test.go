package git

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateBranchFetchPreservesTrackingRefsAndFetchHead(t *testing.T) {
	upstream := initTestRepo(t)
	branch := run(t, upstream, "git", "symbolic-ref", "--short", "HEAD")
	oldHead := run(t, upstream, "git", "rev-parse", "HEAD")
	repository := filepath.Join(t.TempDir(), "gate.git")
	run(t, upstream, "git", "clone", "--bare", upstream, repository)
	run(t, repository, "git", "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	run(t, repository, "git", "update-ref", "refs/remotes/origin/"+branch, oldHead)
	writeFile(t, filepath.Join(upstream, "README.md"), "# advanced upstream\n")
	run(t, upstream, "git", "add", "README.md")
	run(t, upstream, "git", "commit", "-m", "advance default branch")
	newHead := run(t, upstream, "git", "rev-parse", "HEAD")
	beforeRefs := run(t, repository, "git", "for-each-ref", "--format=%(refname) %(objectname)")
	fetchHeadPath := filepath.Join(repository, "FETCH_HEAD")
	fetchHead := []byte("existing run fetch custody\n")
	if err := os.WriteFile(fetchHeadPath, fetchHead, 0o600); err != nil {
		t.Fatal(err)
	}

	const privateRef = "refs/no-mistakes/assertion-fixture/default"
	if err := FetchRemoteBranchToPrivateRef(context.Background(), repository, "origin", branch, privateRef); err != nil {
		t.Fatal(err)
	}
	if fetched := run(t, repository, "git", "rev-parse", privateRef); fetched != newHead {
		t.Fatalf("private fetch = %s, want fresh %s", fetched, newHead)
	}
	run(t, repository, "git", "update-ref", "-d", privateRef)
	if afterRefs := run(t, repository, "git", "for-each-ref", "--format=%(refname) %(objectname)"); afterRefs != beforeRefs {
		t.Errorf("private fetch changed prior refs: before=%s after=%s", beforeRefs, afterRefs)
	}
	if after, err := os.ReadFile(fetchHeadPath); err != nil || !bytes.Equal(after, fetchHead) {
		t.Errorf("private fetch changed FETCH_HEAD: %q, error=%v", after, err)
	}
}
