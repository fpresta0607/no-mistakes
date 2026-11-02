//go:build e2e

package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func TestAxiExplicitOwnedTargetBinding(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	defer h.shutdown()
	const ownedURL = "https://github.com/fork-owner/fixture.git"
	const parentURL = "https://github.com/parent-owner/fixture.git"
	configureGitURLRewrite(t, h, ownedURL, h.UpstreamDir)
	for _, args := range [][]string{
		{"remote", "set-url", "origin", ownedURL},
		{"remote", "add", "fork", ownedURL},
		{"remote", "add", "upstream", parentURL},
	} {
		if out, err := h.runGit(t.Context(), h.WorkDir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	h.CommitChange("feature/explicit-binding", "feature.txt", "feature\n", "fixture")
	operator := h.AddWorktree("feature/explicit-binding")
	out, err := h.RunInDir(operator, "init", "--fork-url", ownedURL)
	if err != nil {
		t.Fatalf("explicit self-target init: %v\n%s", err, out)
	}
	database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.GetRepoByPath(h.WorkDir)
	if err != nil || repo == nil || repo.UpstreamURL != ownedURL || repo.ForkURL != ownedURL || repo.PushURL() != ownedURL {
		t.Fatalf("explicit target binding = %#v %v", repo, err)
	}
	parent, err := h.runGit(t.Context(), h.WorkDir, "config", "--get", "remote.upstream.url")
	if err != nil || strings.TrimSpace(string(parent)) != parentURL {
		t.Fatalf("parent metadata changed: %s %v", parent, err)
	}
	gateOrigin, err := h.runGit(t.Context(), filepath.Join(h.NMHome, "repos", repo.ID+".git"), "config", "--get", "remote.origin.url")
	if err != nil || strings.TrimSpace(string(gateOrigin)) != ownedURL {
		t.Fatalf("gate origin = %s %v", gateOrigin, err)
	}
	t.Logf("normal init --fork-url equal to origin: upstream_url=%s fork_url=%s push=%s; parent metadata retained=%s", repo.UpstreamURL, repo.ForkURL, repo.PushURL(), strings.TrimSpace(string(parent)))
}
