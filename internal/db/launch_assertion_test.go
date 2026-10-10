package db

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
)

func TestLaunchAssertionImmutableAndReceiptClaimFailsClosed(t *testing.T) {
	database := openTestDB(t)
	repository, err := database.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	profile := agentcfg.Selection{Harness: "codex", Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	expected := &launchassert.Expectation{TrustedSHA: strings.Repeat("a", 40), Profiles: map[string][]agentcfg.Selection{
		"primary": {profile}, "reviewer": {profile}, "fixer": {profile},
	}}
	proof, err := expected.Verify(expected.TrustedSHA, expected.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRunWithLaunchAssertion(repository.ID, "feature", "head", "base", nil, "nonce", "generation", "digest", "", false, nil, expected, proof)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.sql.Exec(`UPDATE runs SET launch_assertion = NULL WHERE id = ?`, run.ID); err == nil {
		t.Fatal("launch assertion could be cleared")
	}
	if _, err := database.sql.Exec(`UPDATE runs SET launch_assertion_proof = NULL WHERE id = ?`, run.ID); err == nil {
		t.Fatal("launch proof could be cleared")
	}
	if _, err := database.sql.Exec(`INSERT INTO runs
		(id, repo_id, branch, head_sha, base_sha, submitted_head_sha, launch_nonce, launch_validation_generation, launch_intent_digest, launch_assertion, created_at, updated_at)
		VALUES ('unproved', ?, 'feature', 'head', 'base', 'head', 'unproved', 'generation', 'digest', ?, 1, 1)`, repository.ID, expected); err != nil {
		t.Fatal(err)
	}
	if _, _, err := database.ClaimLaunchReceiptWithAssertion(repository.ID, "feature", "unproved", "head", "generation", "digest", "", false, expected); err == nil {
		t.Fatal("missing proof consumed the launch receipt")
	}
	changed := *expected
	changed.TrustedSHA = strings.Repeat("b", 40)
	if _, _, err := database.ClaimLaunchReceiptWithAssertion(repository.ID, "feature", "nonce", "head", "generation", "digest", "", false, &changed); err == nil {
		t.Fatal("conflicting expectation consumed the receipt")
	}
	if _, _, err := database.ClaimLaunchReceipt(repository.ID, "feature", "nonce", "head", "generation", "digest", "", false); err == nil {
		t.Fatal("omitting the captured expectation weakened a replay")
	}
	stored, isClaimed, err := database.ClaimLaunchReceiptWithAssertion(repository.ID, "feature", "nonce", "head", "generation", "digest", "", false, expected)
	if err != nil || !isClaimed || stored.LaunchAssertionProof.Check(expected) != nil {
		t.Fatalf("matching first observer = %+v %v %v", stored, isClaimed, err)
	}
	legacy, err := database.InsertRun(repository.ID, "legacy", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.sql.Exec(`UPDATE runs SET launch_assertion = ? WHERE id = ?`, expected, legacy.ID); err == nil {
		t.Fatal("legacy run could acquire an assertion retroactively")
	}
}

func TestLaunchAssertionReceiptClaimRaceKeepsOneOwner(t *testing.T) {
	database := openTestDB(t)
	repository, err := database.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	profile := agentcfg.Selection{Harness: "codex", Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	expected := &launchassert.Expectation{TrustedSHA: strings.Repeat("a", 40), Profiles: map[string][]agentcfg.Selection{
		"primary": {profile}, "reviewer": {profile}, "fixer": {profile},
	}}
	proof, err := expected.Verify(expected.TrustedSHA, expected.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRunWithLaunchAssertion(repository.ID, "feature", "head", "base", nil, "race", "generation", "digest", "", false, nil, expected, proof)
	if err != nil {
		t.Fatal(err)
	}
	changed := *expected
	changed.TrustedSHA = strings.Repeat("b", 40)
	type claimResult struct {
		run        *Run
		isClaimed  bool
		isMatching bool
		err        error
	}
	const callers = 12
	start := make(chan struct{})
	results := make(chan claimResult, callers)
	for caller := range callers {
		go func() {
			assertion := expected
			if caller%3 == 1 {
				assertion = &changed
			} else if caller%3 == 2 {
				assertion = nil
			}
			<-start
			stored, isClaimed, err := database.ClaimLaunchReceiptWithAssertion(repository.ID, "feature", "race", "head", "generation", "digest", "", false, assertion)
			results <- claimResult{stored, isClaimed, assertion == expected, err}
		}()
	}
	close(start)
	created := 0
	for range callers {
		result := <-results
		if !result.isMatching {
			if result.err == nil || result.isClaimed || result.run != nil {
				t.Errorf("conflicting or omitted assertion acquired custody: %+v", result)
			}
			continue
		}
		if result.err != nil || result.run == nil || result.run.ID != run.ID || result.run.LaunchAssertionProof.Check(expected) != nil {
			t.Errorf("matching observer = %+v", result)
		}
		if result.isClaimed {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created receipts = %d, want 1", created)
	}
}

func TestLaunchAssertionRowIsNeverVisibleWithoutItsProof(t *testing.T) {
	database := openTestDB(t)
	repository, err := database.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	profile := agentcfg.Selection{Harness: "codex", Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	expected := &launchassert.Expectation{TrustedSHA: strings.Repeat("a", 40), Profiles: map[string][]agentcfg.Selection{
		"primary": {profile}, "reviewer": {profile}, "fixer": {profile},
	}}
	proof, err := expected.Verify(expected.TrustedSHA, expected.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		expected *launchassert.Expectation
		proof    *launchassert.Proof
	}{
		{name: "assertion without proof", expected: expected},
		{name: "proof without assertion", proof: proof},
	} {
		if _, err := database.InsertRunWithLaunchAssertion(repository.ID, "feature", "head", "base", nil, "partial", "generation", "digest", "", false, nil, test.expected, test.proof); err == nil {
			t.Errorf("%s was inserted", test.name)
		}
	}
	if runs, err := database.GetRunsByRepo(repository.ID); err != nil || len(runs) != 0 {
		t.Fatalf("refused insert left a row: %+v, error=%v", runs, err)
	}

	run, err := database.InsertRunWithLaunchAssertion(repository.ID, "feature", "head", "base", nil, "atomic", "generation", "digest", "", false, nil, expected, proof)
	if err != nil {
		t.Fatal(err)
	}
	claimed, isCreated, err := database.ClaimLaunchReceiptWithAssertion(repository.ID, "feature", "atomic", "head", "generation", "digest", "", false, expected)
	if err != nil || !isCreated || claimed.ID != run.ID || claimed.LaunchAssertionProof.Check(expected) != nil {
		t.Fatalf("claim right after the insert = %+v, created=%v, error=%v", claimed, isCreated, err)
	}
}

// A launch selection is stored and claimed as the assertion it extends, and
// the claim keeps it apart from the same profiles asserted without apply.
func TestLaunchSelectionReceiptIsClaimedOnlyAsTheSelectionItWasStoredAs(t *testing.T) {
	database := openTestDB(t)
	repository, err := database.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	profile := agentcfg.Selection{Harness: "claude", Model: "fixture", Effort: agentcfg.EffortHigh}
	selected := &launchassert.Expectation{TrustedSHA: strings.Repeat("a", 40), Apply: true, Profiles: map[string][]agentcfg.Selection{
		"primary": {profile}, "reviewer": {profile}, "fixer": {profile},
	}}
	proof, err := selected.Verify(selected.TrustedSHA, selected.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRunWithLaunchAssertion(repository.ID, "feature", "head", "base", nil, "selection", "generation", "digest", "", false, nil, selected, proof)
	if err != nil {
		t.Fatalf("a launch selection could not be stored with its run: %v", err)
	}
	asserted := *selected
	asserted.Apply = false
	if _, _, err := database.ClaimLaunchReceiptWithAssertion(repository.ID, "feature", "selection", "head", "generation", "digest", "", false, &asserted); err == nil {
		t.Fatal("the same profiles without apply claimed a selection's receipt")
	}

	claimed, isCreated, err := database.ClaimLaunchReceiptWithAssertion(repository.ID, "feature", "selection", "head", "generation", "digest", "", false, selected)

	if err != nil || !isCreated || claimed.ID != run.ID {
		t.Fatalf("selection claim = %+v, created=%v, error=%v", claimed, isCreated, err)
	}
	if !claimed.LaunchAssertion.Apply || !claimed.LaunchAssertionProof.Apply || claimed.LaunchAssertionProof.Check(selected) != nil {
		t.Fatalf("the stored selection lost its apply: assertion=%+v proof=%+v", claimed.LaunchAssertion, claimed.LaunchAssertionProof)
	}
}
