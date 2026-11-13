// This file dispatches only test helper subprocesses through the real production entrypoints.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/project"
)

// herdrTestRuntime is bound only by the explicitly tagged, isolated Herdr test file.
var herdrTestRuntime func() projectRuntime

// testProjectRuntime fails closed for terminal and browser effects. Its root resolver
// uses the test's explicitly scoped config seam, which TestMain blocks by default.
func testProjectRuntime() projectRuntime {
	return projectRuntime{
		stateRoot: defaultOperatorRoot,
		startTerminal: func(context.Context, *project.State, []string) error {
			return errors.New("default tests cannot start real project terminals")
		},
		openViews: func(context.Context, string) error {
			return errors.New("default tests cannot open real project views")
		},
	}
}

// run exercises command dispatch with blocked external effects in the default suite.
func run(args []string, stdout io.Writer) error {
	return runWithRuntime(args, stdout, testProjectRuntime())
}

// discoverProject uses only the default suite's scoped configuration resolver.
func discoverProject() (*project.State, error) { return testProjectRuntime().discover() }

// runProjectSettings exercises local management under the test's owned config root.
func runProjectSettings(kind string, args []string, stdout io.Writer) error {
	return testProjectRuntime().settings(kind, args, stdout)
}

// configureServeWorkspace exercises startup setup under the test's owned config root.
func configureServeWorkspace(cfg serveConfig) (workspaceServeConfig, error) {
	return testProjectRuntime().configureWorkspace(cfg)
}

// serveOptions exercises server option composition under the test's owned config root.
func serveOptions(cfg serveConfig, getenv func(string) string, environ []string) (controlplane.Options, error) {
	return testProjectRuntime().serveOptions(cfg, getenv, environ)
}

// TestMain lets the test executable serve a project and run the real supervised helpers without a second binary.
func TestMain(m *testing.M) {
	userConfigDir = func() (string, error) {
		return "", errors.New("default tests must bind a test-owned configuration directory")
	}
	if directory := os.Getenv("SURLA_TEST_USER_CONFIG_DIR"); directory != "" {
		userConfigDir = func() (string, error) { return directory, nil }
	}
	if len(os.Args) > 1 {
		var runHelper func([]string) error
		switch os.Args[1] {
		case "test-project-start":
			if os.Getenv("SURLA_TEST_USER_CONFIG_DIR") == "" || herdrTestRuntime == nil {
				os.Exit(1)
			}
			runHelper = func(args []string) error { return runWithRuntime(args, io.Discard, herdrTestRuntime()) }
		case "serve":
			runHelper = testProjectRuntime().serve
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
