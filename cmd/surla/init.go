package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/scaffold"
)

// initNextSteps is what `surla init` prints after creating a project; the
// verbs are the shell-quoted project directory and the service name.
const initNextSteps = `Created the %[2]s service and its starter evaluator suite (eval/).

Next:
  cd %[1]s
  git init
  git add .gitignore README.md AGENTS_QUICKSTART.md go.mod go.sum design gen internal cmd eval
  git commit -m "Start %[2]s from surla init"
  surla preview -dir .
  VERIFY_DIR="$(mktemp -d)"
  WORK_DIR="$VERIFY_DIR/work"
  REPORT_DIR="$VERIFY_DIR/reports"
  surla verify -candidate-repo . -candidate-commit "$(git rev-parse HEAD)" -binary %[2]s=./cmd/%[2]s \
    -suite-repo . -suite-commit "$(git rev-parse HEAD)" -suite-module eval -suite-command ./cmd/%[2]s-evals \
    -request-revision starter -work-dir "$WORK_DIR" -report-dir "$REPORT_DIR" -builder-view

surla verify keeps $WORK_DIR, including its build logs in $WORK_DIR/evidence/,
writes the full report to $REPORT_DIR (used by surla approve), and with
-builder-view hides holdout scenario names and checks in the completed report
JSON and summary line (if a run fails before writing a report, stderr prints
operator setup/execution failure diagnostics). Do not share $WORK_DIR,
$REPORT_DIR, or setup/execution failure output with coding agents that build the
service.

README.md in the project explains the change checkpoints, building and testing
with check_project before committing, and verifying a candidate commit against
the confirmed Change Card's request_revision. Connected agents can read the
architecture and evaluator guide at surla://golden-path. Tool inputs and results:
https://github.com/goadesign/surla/blob/main/docs/REFERENCE.md#mcp-tools
`

// runInit creates a starter project with scaffold.Init and prints the
// commands that preview and verify it. The evaluator module requires the
// Surla version this binary was built from unless -surla-version names
// another exact version. SIGINT and SIGTERM cancel it, leaving -dir unchanged.
func runInit(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory to create; it must not exist (required)")
	modulePath := fs.String("module", "", "Go module path of the new service, such as example.com/shop; its last element names the service (required)")
	version := fs.String("surla-version", "", "exact github.com/goadesign/surla version the evaluator suite requires (default: the version of this surla binary)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if *dir == "" || *modulePath == "" {
		return errors.New("init requires -dir and -module")
	}
	if *version == "" {
		*version = surlaBuildVersion()
		if !scaffold.ValidVersion(*version) {
			return fmt.Errorf("this surla binary has no exact module version (%q); pass -surla-version with a published version such as v0.0.0-20261110163737-d9beac313142", buildVersion())
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	target, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	workDir, err := localInitWorkDir(filepath.Dir(target))
	if err != nil {
		return err
	}
	evalWorkDir, err := localEvalWorkDir(filepath.Dir(target))
	if err != nil {
		return err
	}
	name, err := scaffold.Init(ctx, scaffold.Options{
		Dir: target, Module: *modulePath, SurlaVersion: *version, WorkDir: workDir,
	}, scaffold.RunCommand, designeval.NewEvaluatorInDir(evalWorkDir))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, initNextSteps, shellQuote(*dir), name)
	return err
}

// runVersion prints the module version this surla binary was built from to
// stdout. It accepts -h/--help through its FlagSet and rejects positional
// arguments.
func runVersion(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	v := buildVersion()
	if v == "" {
		v = "(devel)"
	}
	_, err := fmt.Fprintln(stdout, v)
	return err
}

// shellQuote quotes s as one POSIX shell word: it wraps s in single quotes
// and writes each single quote inside s as a closing quote, an escaped quote
// and an opening quote. For example, my dir becomes 'my dir'.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildVersion returns the module version this binary was built from, such
// as a pseudo-version stamped from a clean checkout, or "(devel)".
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return info.Main.Version
}

// surlaBuildVersion returns the exact github.com/goadesign/surla module version
// from runtime build metadata when available, or "" for development builds.
func surlaBuildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return scaffold.SurlaVersion(info)
}
