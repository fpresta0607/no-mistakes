package db

import (
	"path/filepath"
	"strings"
	"testing"
)

// The daemon's only connection serves every client, so a per-run lookup that
// scans the whole history stalls gate_context and get_run as history grows.
func TestRunHistoryLookupsUseIndexes(t *testing.T) {
	queries := map[string]string{
		"GetStepsByRun":   `SELECT * FROM step_results WHERE run_id = ? ORDER BY step_order, id`,
		"GetRoundsByStep": `SELECT * FROM step_rounds WHERE step_result_id = ? ORDER BY round`,
	}
	path := filepath.Join(t.TempDir(), "history.sqlite")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	assertIndexedPlans(t, "fresh database", database, queries)
	for _, statement := range []string{
		`DROP INDEX IF EXISTS idx_step_results_run_order`,
		`DROP INDEX IF EXISTS idx_step_rounds_step_round`,
	} {
		if _, err := database.sql.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	assertIndexedPlans(t, "existing database", reopened, queries)
}

func assertIndexedPlans(t *testing.T, label string, database *DB, queries map[string]string) {
	t.Helper()
	for name, query := range queries {
		rows, err := database.sql.Query(`EXPLAIN QUERY PLAN `+query, "id")
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(plan, "; ")
		if !strings.HasPrefix(joined, "SEARCH ") || !strings.Contains(joined, " INDEX ") || strings.Contains(joined, "TEMP B-TREE") {
			t.Errorf("%s: %s plan = %q, want an index search with no temporary sort", label, name, joined)
		}
	}
}
