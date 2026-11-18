package main

import (
	"fmt"
	"os"
	"time"
)

func fakeGitCancellationHandler(args []string) {
	if len(args) == 2 && args[0] == "rev-parse" && args[1] == "HEAD" {
		ready, err := os.OpenFile(os.Getenv("FAKE_CLI_CANCELLATION_READY"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			if err := ready.Close(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			for {
				if _, err := os.Stat(os.Getenv("FAKE_CLI_CANCELLATION_RELEASE")); err == nil {
					break
				} else if !os.IsNotExist(err) {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				time.Sleep(10 * time.Millisecond)
			}
		} else if !os.IsExist(err) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fakeGitPassthroughHandler(args)
}
