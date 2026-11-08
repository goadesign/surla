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
  WORK_DIR="$(mktemp -d)/work"
  surla verify -candidate-repo . -candidate-commit "$(git rev-parse HEAD)" -binary %[2]s=./cmd/%[2]s \
    -suite-repo . -suite-commit "$(git rev-parse HEAD)" -suite-module eval -suite-command ./cmd/%[2]s-evals \
    -request-revision starter -work-dir "$WORK_DIR" -report-dir reports -builder-view

surla verify keeps $WORK_DIR, including its build logs in $WORK_DIR/evidence/,
writes the full report to reports/ (used by surla approve), and with
-builder-view hides holdout scenario names and checks in the completed report
JSON and summary line (if a run fails before writing a report, stderr prints
operator setup/execution failure diagnostics). Do not share $WORK_DIR,
reports/, or setup/execution failure output with coding agents that build the
service.

To connect a coding agent and run a live target, start surla serve from this
directory after the first commit, with SURLA_AUTH_TOKEN exported as a token of
your choice, ANTHROPIC_API_KEY or OPENAI_API_KEY exported, and MODEL_NAME set
to a model that provider offers (-project . pins the evaluator suite to the
current HEAD commit at startup and needs Git 2.45 or newer; restart surla serve
to trust a later HEAD commit, or pass -verification-config for a separate suite
repository):

surla serve -principal you@example.com -model-provider anthropic -model-name "$MODEL_NAME" \
  -project .

# Or with OpenAI (OPENAI_API_KEY):
surla serve -principal you@example.com -model-provider openai -model-name "$MODEL_NAME" \
  -project .

Leave surla serve running. Open another terminal in this directory, export
the same SURLA_AUTH_TOKEN there, and register http://127.0.0.1:8090/mcp with
your coding agent, sending "Authorization: Bearer $SURLA_AUTH_TOKEN". Connected
agents can read the Surla Golden Path architecture and evaluator guide at the
MCP resource surla://golden-path (or GET http://127.0.0.1:8090/golden-path).
Setup for Claude Code, Cursor, Gemini CLI and Codex:
https://github.com/goadesign/surla/blob/main/docs/REFERENCE.md#register-surla-in-your-mcp-client

README.md in the project explains each surla verify part, -project .,
-verification-config, and the -request-revision value.
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
		*version = buildVersion()
		if !scaffold.ValidVersion(*version) {
			return fmt.Errorf("this surla binary has no exact module version (%q); pass -surla-version with a published version such as v0.0.0-20261104215521-580509970fb6", *version)
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
