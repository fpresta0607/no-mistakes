package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

func fakeCIRevalidationHandler(args []string) {
	binaryName := strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0]))
	if binaryName == "baseline-check" || binaryName == "lint-check" {
		if binaryName == "baseline-check" {
			file, err := os.OpenFile(os.Getenv("FAKE_CLI_BASELINE_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			_, err = fmt.Fprintln(file, "baseline")
			closeErr := file.Close()
			if err != nil || closeErr != nil {
				os.Exit(1)
			}
		}
		os.Exit(0)
	}
	realGit := os.Getenv("FAKE_CLI_REAL_GIT")
	remote := os.Getenv("FAKE_CLI_REVALIDATION_REMOTE")
	branch := os.Getenv("FAKE_CLI_REVALIDATION_BRANCH")
	readGit := func(arguments ...string) (string, error) {
		command := exec.Command(realGit, arguments...)
		winproc.Harden(command)
		output, err := command.Output()
		return strings.TrimSpace(string(output)), err
	}
	if binaryName == "git" {
		if len(args) > 1 && args[0] == "push" && args[1] == os.Getenv("FAKE_CLI_REVALIDATION_URL") {
			if _, err := os.Stat(os.Getenv("FAKE_CLI_REVALIDATION_REJECT")); err == nil {
				source := strings.SplitN(args[len(args)-1], ":", 2)[0]
				if _, err := readGit("cat-file", "-e", source+":ci-repair.txt"); err == nil {
					fmt.Fprintln(os.Stderr, "refusing to allow an OAuth App to update workflow without workflow scope")
					os.Exit(1)
				}
			}
		}
		command := exec.Command(realGit, args...)
		winproc.Harden(command)
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := command.Run(); err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				os.Exit(exit.ExitCode())
			}
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	head, err := readGit("--git-dir", remote, "rev-parse", "--verify", "refs/heads/"+branch)
	if err == nil {
		_ = os.Setenv("FAKE_CLI_PR_HEAD_SHA", head)
		list, _ := json.Marshal([]struct {
			Number      int    `json:"number"`
			URL         string `json:"url"`
			BaseRefName string `json:"baseRefName"`
		}{{42, "https://github.com/example/revalidation/pull/42", "main"}})
		_ = os.Setenv("FAKE_CLI_PR_LIST_JSON", string(list))
	} else {
		_ = os.Setenv("FAKE_CLI_PR_LIST_JSON", "[]")
	}
	if len(args) > 1 && args[0] == "pr" && args[1] == "create" {
		fmt.Println("https://github.com/example/revalidation/pull/42")
		os.Exit(0)
	}
	_ = os.Setenv("FAKE_CLI_STATE", "OPEN")
	_ = os.Setenv("FAKE_CLI_CHECKS", `[{"name":"build","state":"FAILURE","bucket":"fail","link":"https://github.com/example/revalidation/actions/runs/1","completedAt":"2026-01-01T00:00:00Z"}]`)
	if _, err := readGit("--git-dir", remote, "cat-file", "-e", head+":ci-repair.txt"); err == nil {
		_ = os.Setenv("FAKE_CLI_CHECKS", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)
	}
	fakeCIGHHandler(args)
}
