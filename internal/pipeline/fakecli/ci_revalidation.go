package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
			command := exec.Command(os.Getenv("FAKE_CLI_REAL_GIT"), "rev-parse", "HEAD")
			winproc.Harden(command)
			head, readErr := command.Output()
			if readErr != nil {
				os.Exit(1)
			}
			_, err = fmt.Fprintln(file, strings.TrimSpace(string(head)))
			closeErr := file.Close()
			if err != nil || closeErr != nil {
				os.Exit(1)
			}
			command = exec.Command(os.Getenv("FAKE_CLI_REAL_GIT"), "cat-file", "-e", "HEAD:ci-repair.txt")
			winproc.Harden(command)
			if command.Run() == nil {
				if once, err := os.OpenFile(os.Getenv("FAKE_CLI_PUBLICATION_LOST")+".once", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
					_ = once.Close()
					if err := os.WriteFile(os.Getenv("FAKE_CLI_PUBLICATION_LOST"), nil, 0o600); err != nil {
						os.Exit(1)
					}
				}
			}
		}
		os.Exit(0)
	}
	recordBoundary := func(name string) {
		if path := os.Getenv("FAKE_CLI_PUBLICATION_TRACE"); path != "" {
			file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				os.Exit(1)
			}
			_, err = fmt.Fprintf(file, "%d %s\n", time.Now().UnixMilli(), name)
			if closeErr := file.Close(); err != nil || closeErr != nil {
				os.Exit(1)
			}
		}
	}
	joined := strings.Join(args, " ")
	if binaryName == "gh" && strings.Contains(joined, "auth git-credential") {
		recordBoundary("Git credential proof")
		if os.Getenv("GH_CONFIG_DIR") != os.Getenv("FAKE_CLI_PUBLICATION_CONFIG") {
			os.Exit(1)
		}
		if _, err := os.Stat(os.Getenv("FAKE_CLI_PUBLICATION_LOST")); err == nil {
			os.Exit(1)
		}
		fmt.Printf("protocol=https\nhost=github.com\nusername=fixture-operator\npassword=%s\n\n", os.Getenv("FAKE_CLI_PUBLICATION_SECRET"))
		return
	}
	if binaryName == "gh" && len(args) > 0 && args[0] == "api" {
		for _, arg := range args {
			if arg == "user" || arg == "/user" {
				recordBoundary("forge credential proof")
				if strings.Contains(joined, "--jq") {
					fmt.Println("fixture-operator")
				} else {
					fmt.Println(`{"login":"fixture-operator"}`)
				}
				return
			}
		}
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
		if len(args) == 3 && args[0] == "remote" && args[1] == "get-url" && args[2] == "origin" {
			origin, err := readGit("config", "--get", "remote.origin.url")
			if err != nil {
				os.Exit(1)
			}
			fmt.Println(origin)
			return
		}
		if len(args) > 1 && args[0] == "push" && args[1] == os.Getenv("FAKE_CLI_REVALIDATION_URL") {
			recordBoundary("Git push entered")
			if os.Getenv("GH_CONFIG_DIR") == os.Getenv("FAKE_CLI_PUBLICATION_CONFIG") && os.Remove(os.Getenv("FAKE_CLI_PUBLICATION_REJECT")) == nil {
				fmt.Fprintln(os.Stderr, "selected publication transport rejected")
				os.Exit(1)
			}
			if _, err := os.Stat(os.Getenv("FAKE_CLI_REVALIDATION_REJECT")); err == nil && os.Getenv("GH_CONFIG_DIR") != os.Getenv("FAKE_CLI_PUBLICATION_CONFIG") {
				source := strings.SplitN(args[len(args)-1], ":", 2)[0]
				if _, err := readGit("cat-file", "-e", source+":ci-repair.txt"); err == nil {
					fmt.Fprintln(os.Stderr, "refusing to allow an OAuth App to update workflow without workflow scope")
					os.Exit(1)
				}
			}
		}
		transportArgs := args
		for _, argument := range args {
			if argument == "clone" || argument == "fetch" || argument == "ls-remote" || argument == "push" {
				target := (&url.URL{Scheme: "file", Path: "/" + strings.TrimPrefix(filepath.ToSlash(remote), "/")}).String()
				transportArgs = append([]string{"-c", "protocol.allow=never", "-c", "protocol.file.allow=always", "-c", "url." + target + ".insteadOf=" + os.Getenv("FAKE_CLI_REVALIDATION_URL")}, args...)
				break
			}
		}
		command := exec.Command(realGit, transportArgs...)
		winproc.Harden(command)
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := command.Run(); err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				os.Exit(exit.ExitCode())
			}
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if len(args) > 1 && args[0] == "push" && args[1] == os.Getenv("FAKE_CLI_REVALIDATION_URL") && os.Getenv("GH_CONFIG_DIR") == os.Getenv("FAKE_CLI_PUBLICATION_CONFIG") {
			recordBoundary("selected Git push confirmed")
			if err := os.Remove(os.Getenv("FAKE_CLI_PUBLICATION_CRASH")); err == nil {
				root := os.Getenv("NM_HOME")
				if !strings.Contains(filepath.Base(filepath.Dir(root)), "nm-e2e-") {
					os.Exit(1)
				}
				data, err := os.ReadFile(filepath.Join(root, "daemon.pid"))
				if err != nil {
					os.Exit(1)
				}
				var record struct {
					PID int `json:"pid"`
				}
				if json.Unmarshal(data, &record) != nil {
					record.PID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
				}
				if record.PID <= 0 || record.PID == os.Getpid() {
					os.Exit(1)
				}
				process, err := os.FindProcess(record.PID)
				if err != nil || process.Kill() != nil {
					os.Exit(1)
				}
				_ = process.Release()
			}
		}
		return
	}
	head, err := readGit("--git-dir", remote, "rev-parse", "--verify", "refs/heads/"+branch)
	if binaryName == "gh" && strings.Contains(joined, "repos/example/revalidation/pulls/42") {
		recordBoundary("live PR proof")
		proofPath := os.Getenv("FAKE_CLI_PUBLICATION_PROOF")
		if proofPath != "" && os.Remove(proofPath+".arm") == nil {
			if err := os.WriteFile(proofPath+".pending", nil, 0o600); err != nil {
				os.Exit(1)
			}
			deadline := time.Now().Add(15 * time.Second)
			for {
				if os.Remove(proofPath+".release") == nil {
					break
				}
				if time.Now().After(deadline) {
					os.Exit(1)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		fmt.Printf(`{"html_url":"https://github.com/example/revalidation/pull/42","state":"open","head":{"sha":%q,"ref":%q,"repo":{"full_name":"example/revalidation"}},"base":{"ref":"main","repo":{"full_name":"example/revalidation"}}}`, head, branch)
		return
	}
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
	if _, err := readGit("--git-dir", remote, "merge-tree", "--write-tree", head, "refs/heads/main"); err != nil {
		_ = os.Setenv("FAKE_CLI_MERGEABLE", "CONFLICTING")
	}
	_ = os.Setenv("FAKE_CLI_CHECKS", `[{"name":"build","state":"FAILURE","bucket":"fail","link":"https://github.com/example/revalidation/actions/runs/1","completedAt":"2026-01-01T00:00:00Z"}]`)
	if _, err := readGit("--git-dir", remote, "cat-file", "-e", head+":ci-repair.txt"); err == nil {
		_ = os.Setenv("FAKE_CLI_CHECKS", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)
	}
	fakeCIGHHandler(args)
}
