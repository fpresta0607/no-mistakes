package db

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
)

func TestLaunchAssertionPersistedBindingReopensAndFailsClosed(t *testing.T) {
	profile := agentcfg.Selection{Harness: "codex", Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	expected := &launchassert.Expectation{TrustedSHA: strings.Repeat("a", 40), Profiles: map[string][]agentcfg.Selection{
		"primary": {profile}, "reviewer": {profile}, "fixer": {profile},
	}}
	proof, err := expected.Verify(expected.TrustedSHA, expected.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("reopen_and_claim", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "reopen.db")
		database, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { database.Close() })
		repository, err := database.InsertRepo(t.TempDir(), "https://example.com/reopen.git", "main")
		if err != nil {
			t.Fatal(err)
		}
		asserted, err := database.InsertRunWithLaunchAssertion(repository.ID, "feature", "head", "base", nil, "asserted", "generation", "digest", "", false, nil, expected, proof)
		if err != nil {
			t.Fatal(err)
		}
		legacy, err := database.InsertRunWithLaunchAssertion(repository.ID, "feature", "head", "base", nil, "legacy", "generation", "digest", "", false, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		database = reopened
		stored, err := database.GetRun(asserted.ID)
		if err != nil || !reflect.DeepEqual(stored.LaunchAssertion, expected) || !reflect.DeepEqual(stored.LaunchAssertionProof, proof) || stored.LaunchReceiptClaimedAt != nil {
			t.Fatalf("reopened binding changed: %+v, error=%v", stored, err)
		}
		for _, run := range []*Run{asserted, legacy} {
			binding := expected
			if run.ID == legacy.ID {
				binding = nil
			}
			for observation := range 2 {
				claimed, isCreated, err := database.ClaimLaunchReceiptWithAssertion(repository.ID, "feature", *run.LaunchNonce, "head", "generation", "digest", "", false, binding)
				if err != nil || claimed == nil || claimed.ID != run.ID || isCreated != (observation == 0) || claimed.LaunchReceiptClaimedAt == nil || claimed.LaunchAssertionProof.Check(binding) != nil {
					t.Fatalf("reopened observer binding=%s observation=%d: %+v, created=%v, error=%v", *run.LaunchNonce, observation, claimed, isCreated, err)
				}
			}
		}
		for _, query := range []string{
			`UPDATE runs SET launch_assertion = NULL WHERE id = ?`,
			`UPDATE runs SET launch_assertion_proof = NULL WHERE id = ?`,
		} {
			if _, err := database.sql.Exec(query, asserted.ID); err == nil {
				t.Fatal("reopening removed the immutable proof trigger")
			}
		}
		beforeLegacy, err := database.GetRun(legacy.ID)
		if err != nil || beforeLegacy.LaunchAssertion != nil || beforeLegacy.LaunchAssertionProof != nil {
			t.Fatalf("legacy NULL binding premise changed: %+v, error=%v", beforeLegacy, err)
		}
		assertionJSON, err := json.Marshal(expected)
		if err != nil {
			t.Fatal(err)
		}
		_, updateError := database.sql.Exec(`UPDATE runs SET launch_assertion = ? WHERE id = ?`, string(assertionJSON), legacy.ID)
		if updateError == nil || !strings.Contains(updateError.Error(), "run launch assertion is immutable") {
			t.Fatalf("legacy NULL binding was not refused by the immutable trigger: %v", updateError)
		}
		afterLegacy, err := database.GetRun(legacy.ID)
		if err != nil || !reflect.DeepEqual(afterLegacy, beforeLegacy) {
			t.Fatalf("immutable-trigger refusal changed the legacy NULL row: %+v, error=%v", afterLegacy, err)
		}
	})
	t.Run("malformed_persisted_row", func(t *testing.T) {
		database := openTestDB(t)
		repository, err := database.InsertRepo(t.TempDir(), "https://example.com/malformed.git", "main")
		if err != nil {
			t.Fatal(err)
		}
		assertionJSON, err := json.Marshal(expected)
		if err != nil {
			t.Fatal(err)
		}
		proofJSON, err := json.Marshal(proof)
		if err != nil {
			t.Fatal(err)
		}
		for _, malformed := range []struct {
			name      string
			assertion string
			proof     string
		}{
			{"assertion_json", "{", string(proofJSON)},
			{"assertion_null", "null", string(proofJSON)},
			{"assertion_invalid", `{}`, string(proofJSON)},
			{"proof_json", string(assertionJSON), "{"},
			{"proof_null", string(assertionJSON), "null"},
			{"proof_invalid", string(assertionJSON), `{}`},
		} {
			if _, err := database.sql.Exec(`INSERT INTO runs
				(id, repo_id, branch, head_sha, base_sha, submitted_head_sha, launch_nonce, launch_validation_generation, launch_intent_digest, launch_assertion, launch_assertion_proof, created_at, updated_at)
				VALUES (?, ?, 'feature', 'head', 'base', 'head', ?, 'generation', 'digest', ?, ?, 1, 1)`, malformed.name, repository.ID, malformed.name, malformed.assertion, malformed.proof); err != nil {
				t.Fatalf("malformed fixture insert before behavior: %v", err)
			}
			if _, err := database.GetRun(malformed.name); err == nil {
				t.Errorf("malformed %s survived persisted decoding", malformed.name)
			}
			for _, binding := range []*launchassert.Expectation{expected, nil} {
				run, isCreated, err := database.ClaimLaunchReceiptWithAssertion(repository.ID, "feature", malformed.name, "head", "generation", "digest", "", false, binding)
				if err == nil || run != nil || isCreated {
					t.Errorf("malformed %s acquired receipt custody: %+v, created=%v, error=%v", malformed.name, run, isCreated, err)
				}
			}
			var claimedAt sql.NullInt64
			var storedAssertion, storedProof string
			if err := database.sql.QueryRow(`SELECT launch_receipt_claimed_at, launch_assertion, launch_assertion_proof FROM runs WHERE id = ?`, malformed.name).Scan(&claimedAt, &storedAssertion, &storedProof); err != nil {
				t.Fatal(err)
			}
			if claimedAt.Valid || storedAssertion != malformed.assertion || storedProof != malformed.proof {
				t.Errorf("malformed %s changed immutable row or claim timestamp", malformed.name)
			}
		}
	})
}
