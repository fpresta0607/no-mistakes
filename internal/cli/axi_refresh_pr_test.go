package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestAxiRefreshPR(t *testing.T) {
	for _, testCase := range []struct {
		name, requested string
		isRefused       bool
	}{
		{"exact-run-outside-repository", "selected", false},
		{"missing-run", "", true},
		{"unknown-run", "unknown", true},
		{"unsupported-daemon", "unsupported", true},
		{"mismatched-response", "mismatched", true},
		{"nested-caller", "nested", true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("NM_HOME", makeSocketSafeTempDir(t))
			p, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { database.Close() })
			repo, err := database.InsertRepo(t.TempDir(), "https://github.com/owner/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			selected, err := database.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.InsertRun(repo.ID, "other", strings.Repeat("c", 40), strings.Repeat("b", 40)); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunPushBinding(selected.ID, db.PushBinding{HeadSHA: selected.HeadSHA}); err != nil {
				t.Fatal(err)
			}
			url := "https://github.com/owner/repo/pull/17"
			if err := database.UpdateRunPRURL(selected.ID, url); err != nil {
				t.Fatal(err)
			}
			server := ipc.NewServer()
			server.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.HealthResult{Status: "ok"}, nil
			})
			server.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) {
				return &ipc.GateContextResult{Nested: testCase.requested == "nested"}, nil
			})
			calls := 0
			server.Handle("refresh_pr", func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				calls++
				if testCase.requested == "unsupported" {
					return nil, &ipc.RPCError{Code: ipc.ErrMethodNotFound, Message: "method not found"}
				}
				var wire struct {
					RunID          string `json:"run_id"`
					RepoID         string `json:"repo_id"`
					Branch         string `json:"branch"`
					HeadSHA        string `json:"head_sha"`
					PRURL          string `json:"pr_url"`
					PushGeneration int64  `json:"push_generation"`
				}
				if err := json.Unmarshal(raw, &wire); err != nil {
					t.Error(err)
					return nil, err
				}
				if wire.RunID != selected.ID || wire.RepoID != repo.ID || wire.Branch != selected.Branch || wire.HeadSHA != selected.HeadSHA || wire.PRURL != url || wire.PushGeneration != 1 {
					return nil, errors.New("wrong exact-run selection")
				}
				responseID := selected.ID
				if testCase.requested == "mismatched" {
					responseID = "another-run"
				}
				return struct {
					RunID   string `json:"run_id"`
					HeadSHA string `json:"head_sha"`
					PRURL   string `json:"pr_url"`
					Updated bool   `json:"updated"`
				}{responseID, selected.HeadSHA, url, true}, nil
			})
			if err := server.Listen(p.Socket()); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- server.ServeReady() }()
			t.Cleanup(func() {
				server.Close()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("fixture IPC server did not stop")
				}
			})
			chdir(t, t.TempDir())
			arguments := []string{"axi", "refresh-pr"}
			if testCase.requested != "" {
				id := testCase.requested
				if id != "unknown" {
					id = selected.ID
				}
				arguments = append(arguments, "--run", id)
			}
			output, err := executeCmd(arguments...)
			if testCase.isRefused {
				if err == nil || strings.Contains(output, "updated: true") {
					t.Fatalf("refusal reported success: %v, %s", err, output)
				}
			} else if err != nil || calls != 1 || !strings.Contains(output, selected.ID) || !strings.Contains(output, "updated: true") {
				t.Fatalf("exact refresh: error=%v calls=%d output=%s", err, calls, output)
			}
			if (testCase.requested == "" || testCase.requested == "unknown" || testCase.requested == "nested") && calls != 0 {
				t.Fatalf("invalid selection reached publication: calls=%d", calls)
			}
		})
	}
}
