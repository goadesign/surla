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
		preflight: func() error { return errors.New("default tests cannot start real project terminals") },
		startTerminal: func(context.Context, *project.State, []string) (string, error) {
			return "", errors.New("default tests cannot start real project terminals")
		},
		waitTerminal: func(context.Context, string) (bool, error) {
			return false, errors.New("default tests cannot observe real project terminals")
		},
		removeTerminal: func(context.Context, string) error {
			return errors.New("default tests cannot remove real project terminals")
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

// fixtureProjectState creates a project through its state owner before testing startup setup.
func fixtureProjectState(directory string) (*project.State, error) {
	if directory == "" {
		var err error
		directory, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	root, err := testProjectRuntime().stateRoot(directory)
	if err != nil {
		return nil, err
	}
	state, err := project.Discover(root, directory)
	if err != nil {
		return nil, err
	}
	lock, err := state.StartLock(context.Background())
	if err != nil {
		return nil, err
	}
	_, err = state.Initialize(fmt.Sprintf("uid:%d", os.Geteuid()))
	return state, errors.Join(err, lock.Close())
}

// configureServeWorkspace exercises startup setup for an initialized test project.
func configureServeWorkspace(directory ...string) (workspaceServeConfig, error) {
	dir := ""
	if len(directory) != 0 {
		dir = directory[0]
	}
	state, err := fixtureProjectState(dir)
	if err != nil {
		return workspaceServeConfig{}, err
	}
	return configureProjectWorkspace(state)
}

// serveOptions exercises server option composition for an initialized test project.
func serveOptions(cfg serveConfig, getenv func(string) string, environ []string, directory ...string) (controlplane.Options, error) {
	dir := ""
	if len(directory) != 0 {
		dir = directory[0]
	}
	state, err := fixtureProjectState(dir)
	if err != nil {
		return controlplane.Options{}, err
	}
	return projectServeOptions(state, cfg, getenv, environ)
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
			if os.Getenv("SURLA_TEST_USER_CONFIG_DIR") == "" {
				os.Exit(1)
			}
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
