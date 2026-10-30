package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

func fakeCIHandbackGit(args []string) {
	command := exec.Command(os.Getenv("FAKE_CLI_REAL_GIT"), args...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	marker := os.Getenv("FAKE_CLI_HANDBACK_MOVED")
	if len(args) > 1 && args[0] == "merge" && args[1] == "--ff-only" {
		head, err := exec.Command(os.Getenv("FAKE_CLI_REAL_GIT"), "rev-parse", "HEAD").Output()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(marker, head, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if os.Getenv("FAKE_CLI_HANDBACK_PAUSE") == "merge" {
			for {
				time.Sleep(time.Second)
			}
		}
	}
	if len(args) == 2 && args[0] == "rev-parse" && args[1] == "HEAD" && os.Getenv("FAKE_CLI_HANDBACK_PAUSE") == "head" {
		if _, err := os.Stat(marker); err == nil {
			if _, err := os.Stat(marker + ".waiting"); os.IsNotExist(err) {
				if err := os.WriteFile(marker+".waiting", []byte("waiting"), 0o600); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				for {
					time.Sleep(time.Second)
				}
			}
		}
	}
}
