// This file dispatches only test helper subprocesses through the real production entrypoints.
package main

import (
	"fmt"
	"io"
	"os"
	"testing"
)

// TestMain lets the test executable serve a project and run the real supervised helpers without a second binary.
func TestMain(m *testing.M) {
	if directory := os.Getenv("SURLA_TEST_USER_CONFIG_DIR"); directory != "" {
		userConfigDir = func() (string, error) { return directory, nil }
	}
	if len(os.Args) > 1 {
		var runHelper func([]string) error
		switch os.Args[1] {
		case "test-project-start":
			if os.Getenv("SURLA_TEST_USER_CONFIG_DIR") == "" {
				os.Exit(1)
			}
			runHelper = func(args []string) error { return run(args, io.Discard) }
		case "serve":
			runHelper = runServe
		case "verify-run":
			runHelper = runVerifyRun
		case "check-run":
			runHelper = runCheckRun
		}
		if runHelper != nil {
			if err := runHelper(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}
