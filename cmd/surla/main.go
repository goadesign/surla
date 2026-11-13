// Command surla runs the Surla MCP software factory control-plane server
// and CLI utilities (`surla init`, `surla preview`,
// `surla diff`, `surla gen`, `surla verify`,
// `surla approve` and `surla version`).
// Run `surla help` for usage.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/diff"
	"github.com/goadesign/surla/internal/project"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
	"github.com/goadesign/surla/verify/candidate"
)

// defaultCheckTimeout bounds one project check, including copy and containers.
const defaultCheckTimeout = 20 * time.Minute

// binaryFlags collects repeated `-binary name=pkg` flags for `surla verify`
// into verify.Plan.Binaries, rejecting malformed entries and repeated names.
type binaryFlags map[string]string

// artifactFlags collects repeated `-artifact name=sha256` flags for
// `surla approve` into verify.Binding.Artifacts: the digest of each candidate
// binary being promoted, measured by whoever runs the promotion.
type artifactFlags map[string]string

// approveSummary is what `surla approve` prints when the report matches the
// change being promoted.
type approveSummary struct {
	// ReportDigest identifies the approved report.
	ReportDigest string `json:"report_digest"`
	// Binding is the change the report was checked against.
	Binding verify.Binding `json:"binding"`
	// Counts covers every declared scenario, all of which passed.
	Counts verify.Counts `json:"counts"`
}

// usage is the text `surla help`, `surla -h` and `surla --help`
// print. Each subcommand prints its own flags with `surla <subcommand> -h`.
const usage = `Surla checks changes to a Goa/Goa-AI service design before code is
written, and checks the built service against an evaluator suite before it is
promoted.

Usage:

  surla
  surla <command> [flags]

Commands:

  init            create a starter service with an agent and an evaluator suite
  stop            stop this project's server in order
  verification    set, show or remove this project's verification settings
  promotion       set, show or remove this project's promotion settings

Running surla starts or attaches to this folder's project and opens its views.
The server runs in its own parentless Herdr terminal.

Other commands:
  preview         print the architecture, agent policies and tool contracts a design
                  module declares; needs no server and no model key
  diff            compare the designs in two module directories
  gen             regenerate a design module's gen/ inside Surla's sandbox and
                  replace it atomically
  verify          build a candidate commit and run an evaluator suite against it,
                  printing a verification report
  approve         check that a saved verification report passed and matches the
                  change being promoted
  version         print the module version this surla binary was built from
  help            print this text, or "surla help <command>" for a command's flags
`

// helpWords are the arguments that ask for top-level usage: `surla help`,
// `surla -h`, `surla -help` and `surla --help`, also after `help`.
var helpWords = []string{"help", "-h", "-help", "--help"}

func main() {
	if err := runWithRuntime(os.Args[1:], os.Stdout, productionProjectRuntime()); err != nil {
		fmt.Fprintf(os.Stderr, "surla: %v\n", err)
		os.Exit(1)
	}
}

// runWithRuntime dispatches CLI commands using the composition root's project effects.
// No arguments starts the project; `help`, `-h`, `-help` and `--help` print usage.
// `help <command>` and `<command> -h` print that command's flags to stderr (the flag package's
// output). A help request returns nil so the exit status is 0; an unknown
// command or a bad flag returns an error.
func runWithRuntime(args []string, stdout io.Writer, runtime projectRuntime) error {
	if len(args) == 0 {
		return runtime.start(stdout)
	}
	sub, rest := args[0], args[1:]
	if slices.Contains(helpWords, sub) {
		switch {
		case len(rest) == 0, sub == "help" && len(rest) == 1 && slices.Contains(helpWords, rest[0]):
			_, err := fmt.Fprint(stdout, usage)
			return err
		case sub == "help" && len(rest) == 1:
			return runWithRuntime([]string{rest[0], "-h"}, stdout, runtime)
		default:
			return fmt.Errorf("unexpected arguments %q after %s", rest, sub)
		}
	}
	err := dispatch(sub, rest, stdout, runtime)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

// dispatch runs the subcommand sub with its arguments. A `-h` flag makes the
// subcommand's flag parsing print its flags and return flag.ErrHelp.
func dispatch(sub string, args []string, stdout io.Writer, runtime projectRuntime) error {
	switch sub {
	case "init":
		return runtime.init(args, stdout)
	case "serve":
		stateDir, err := parseServeArgs(args)
		if err != nil {
			return err
		}
		return runtime.serve(stateDir)
	case "stop":
		return runtime.stop(args, stdout)
	case "verification", "promotion":
		return runtime.settings(sub, args, stdout)
	case "preview":
		return runtime.preview(args, stdout, os.Stderr)
	case "diff":
		return runtime.diff(args)
	case "gen":
		return runtime.gen(args, stdout, os.Stderr)
	case "verify":
		return runVerify(args)
	case "approve":
		return runApprove(args, stdout)
	case "version":
		return runVersion(args, stdout)
	case "verify-run":
		return runVerifyRun(args)
	case "check-run":
		return runCheckRun(args)
	default:
		return fmt.Errorf("unknown command %q; run \"surla help\" to list the commands", sub)
	}
}

// writeServeStatus reports the bound numeric loopback address and checkpoint capabilities.
func writeServeStatus(w io.Writer, addr *net.TCPAddr, opts controlplane.Options) {
	host := addr.String()
	fmt.Fprintf(w, "surla: serving on http://%s\n", host)
	writeCheckpointStatus(w, opts)
}

// writeCheckpointStatus writes the Change Cards directory and the on/off
// status of checkpoints CP1–CP5 to w for the project server, naming how CP4
// verifies: the stored evaluator, or the tests accepted for each change.
func writeCheckpointStatus(w io.Writer, opts controlplane.Options) {
	fmt.Fprintf(w, "surla: saving Change Cards in %s\n", opts.CardsDir)
	fmt.Fprintln(w, "surla: CP1 (propose_change, revise_change, confirm_intent, decline_revision, get_change): on")
	fmt.Fprintln(w, "surla: CP2 (preview_architecture): on")
	if (opts.Verification != nil || opts.WrittenTestRuns != nil) && opts.Live != nil {
		root := controlplane.LiveRoot(opts.CardsDir)
		fmt.Fprintf(w, "surla: CP3 (draft_override, start_live_target): compiled_rule is on; prompt_template is on once start_live_target runs a candidate commit (live targets in %s)\n", root)
		writeLeftoverLiveDirs(w, root)
	} else {
		fmt.Fprintln(w, "surla: CP3 (draft_override, start_live_target): compiled_rule is on; prompt_template is off"+
			" (use surla verification set, then call start_live_target with a commit; or edit the prompt's source text, commit, and verify the commit)")
	}
	switch {
	case opts.Verification != nil:
		fmt.Fprintf(w, "surla: CP4 (verify_change, get_verification, list_verifications): on with the stored evaluator (runs in %s, reports in %s, evaluator suite commit %s)\n",
			opts.Verification.RunsDir, opts.Verification.Plan.ReportDir, opts.Verification.Plan.SuiteCommit)
	case opts.WrittenTestRuns != nil:
		fmt.Fprintf(w, "surla: CP4 (verify_change, get_verification, list_verifications): on with the tests accepted for each change's build (runs in %s, reports in %s)\n",
			opts.RunsDir, opts.WrittenTestRuns.ReportDir)
	default:
		fmt.Fprintln(w, "surla: CP4 (verify_change, get_verification, list_verifications): off"+
			" (use surla verification set, or run surla verify on the CLI)")
	}
	if opts.PromotionTarget == nil {
		fmt.Fprintln(w, "surla: CP5 (approve_merge): off (use surla promotion set, or run surla approve on the CLI)")
	} else {
		fmt.Fprintln(w, "surla: CP5 (approve_merge): on")
	}
}

// preview reads the design module in -dir (default: the current directory)
// with designeval.ReadModuleSnapshot, evaluates it with a design evaluator it owns, and
// writes the result with writePreviewResult: indented JSON to w and a one-line
// summary to stderr. It records nothing and calls no model. SIGINT and SIGTERM
// cancel the evaluation. On every return it closes the evaluator, which
// removes its build workspace, and returns that error with any other.
func (runtime projectRuntime) preview(args []string, w, stderr io.Writer) (err error) {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	dir := fs.String("dir", ".", "root directory of the design module (holding go.mod, go.sum and design/)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	files, err := designeval.ReadModuleSnapshot(*dir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	evalWorkDir, err := runtime.localEvalWorkDir(*dir)
	if err != nil {
		return err
	}
	ev := designeval.NewEvaluatorInDir(evalWorkDir)
	defer func() { err = errors.Join(err, ev.Close()) }()
	res, err := ev.Evaluate(ctx, files)
	if err != nil {
		return err
	}
	return writePreviewResult(w, stderr, res)
}

// writePreviewResult writes res to stdout as indented JSON, the only thing
// surla preview prints there, so the output stays machine-readable. After
// the JSON it writes one line to stderr for a person:
// `surla: "orders": 3 container(s), ...` from Result.Summary. It writes the
// summary only after the JSON was written, and returns the JSON write error.
func writePreviewResult(stdout, stderr io.Writer, res designeval.Result) error {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "surla: %s\n", res.Summary())
	return nil
}

// diff compares two design modules on disk, each read into a snapshot with
// designeval.ReadModuleSnapshot, and writes the structured diff JSON report to stdout.
// Both evaluations share one design evaluator it owns. SIGINT and SIGTERM
// cancel them. On every return it closes the evaluator, which removes its
// build workspace, and returns that error with any other.
func (runtime projectRuntime) diff(args []string) (err error) {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	oldDir := fs.String("old-dir", "", "root directory of the previous design module (holding go.mod and design/)")
	newDir := fs.String("new-dir", "", "root directory of the candidate design module (holding go.mod and design/)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if *oldDir == "" || *newDir == "" {
		return errors.New("--old-dir and --new-dir are required")
	}
	oldFiles, err := designeval.ReadModuleSnapshot(*oldDir)
	if err != nil {
		return err
	}
	newFiles, err := designeval.ReadModuleSnapshot(*newDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	evalWorkDir, err := runtime.localEvalWorkDir(*oldDir, *newDir)
	if err != nil {
		return err
	}
	ev := designeval.NewEvaluatorInDir(evalWorkDir)
	defer func() { err = errors.Join(err, ev.Close()) }()
	rep, err := diff.Compare(ctx, ev, oldFiles, newFiles)
	if err != nil {
		return err
	}
	res := &gensurla.DiffResult{
		HasChanges:           rep.HasChanges,
		AddedDeclarations:    rep.AddedDeclarations,
		RemovedDeclarations:  rep.RemovedDeclarations,
		ModifiedDeclarations: rep.ModifiedDeclarations,
		VersionDeltas:        rep.VersionDeltas,
		Summary:              rep.Summary,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

// writeVerifyReport writes report to w as indented JSON, or its builder view
// when builderView is set, and then returns an error when the report did not
// pass so `surla verify` exits 1 after printing it.
func writeVerifyReport(w io.Writer, report verify.Report, builderView bool) error {
	var out any = report
	if builderView {
		out = report.ForBuilders()
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return err
	}
	if !report.Passed {
		return fmt.Errorf("verification did not pass (report %s)", report.Digest)
	}
	return nil
}

// runApprove checks, with verify.Approve, that the saved report at -report
// passed and describes exactly the change given by the other flags, and
// prints an approveSummary to w. It saves nothing: no Change Card approval is
// recorded and nothing is merged. A report that is stale, did not pass, or
// cannot be read prints nothing and returns the error, so the exit status is 1.
func runApprove(args []string, w io.Writer) error {
	reportPath, current, err := parseApproveFlags(args)
	if err != nil {
		return err
	}
	f, err := os.Open(reportPath)
	if err != nil {
		return fmt.Errorf("open report: %w", err)
	}
	defer f.Close()
	report, err := verify.DecodeReport(f)
	if err != nil {
		return err
	}
	if err := verify.Approve(current, report); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(approveSummary{ReportDigest: report.Digest, Binding: current, Counts: report.Counts})
}

// parseApproveFlags turns `surla approve` arguments into the report path and
// the binding of the change being promoted. Every flag is required and must
// be given explicitly, because each one is a fact the report is compared with.
func parseApproveFlags(args []string) (string, verify.Binding, error) {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	artifacts := artifactFlags{}
	var reportPath string
	var b verify.Binding
	fs.StringVar(&reportPath, "report", "", "path of the full verification report written by surla verify")
	fs.StringVar(&b.CandidateCommit, "candidate-commit", "", "full commit ID of the candidate being promoted")
	fs.StringVar(&b.RequestRevision, "request-revision", "", "current request_revision of the confirmed change")
	fs.StringVar(&b.SuiteID, "suite-id", "", "evaluator suite the promotion requires")
	fs.StringVar(&b.SuiteCommit, "suite-commit", "", "evaluator suite commit the promotion requires")
	fs.StringVar(&b.Environment, "environment", "", "dependency profile the promotion requires, such as sim")
	fs.Var(artifacts, "artifact", "candidate binary being promoted as name=sha256 (repeatable)")
	if err := fs.Parse(args); err != nil {
		return "", verify.Binding{}, err
	}
	if fs.NArg() > 0 {
		return "", verify.Binding{}, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	required := []struct {
		flag  string
		value string
	}{
		{"-report", reportPath},
		{"-candidate-commit", b.CandidateCommit},
		{"-request-revision", b.RequestRevision},
		{"-suite-id", b.SuiteID},
		{"-suite-commit", b.SuiteCommit},
		{"-environment", b.Environment},
	}
	var missing []string
	for _, r := range required {
		if r.value == "" {
			missing = append(missing, r.flag)
		}
	}
	if len(artifacts) == 0 {
		missing = append(missing, "-artifact")
	}
	if len(missing) > 0 {
		return "", verify.Binding{}, fmt.Errorf("approve requires %s", strings.Join(missing, ", "))
	}
	b.Artifacts = artifacts
	return reportPath, b, nil
}

// String renders the collected artifacts as sorted name=sha256 pairs.
func (a artifactFlags) String() string {
	pairs := make([]string, 0, len(a))
	for name, digest := range a {
		pairs = append(pairs, name+"="+digest)
	}
	slices.Sort(pairs)
	return strings.Join(pairs, ",")
}

// Set records one name=sha256 entry. The name must be non-empty and appear
// only once, and the digest must be 64 lowercase hex characters, the form
// surla verify records.
func (a artifactFlags) Set(v string) error {
	name, digest, ok := strings.Cut(v, "=")
	if !ok || name == "" || !isSHA256Hex(digest) {
		return fmt.Errorf("artifact %q must be name=<64 lowercase hex sha256>", v)
	}
	if _, dup := a[name]; dup {
		return fmt.Errorf("artifact name %q is repeated", name)
	}
	a[name] = digest
	return nil
}

// String renders the collected binaries as sorted name=pkg pairs.
func (b binaryFlags) String() string {
	pairs := make([]string, 0, len(b))
	for name, pkg := range b {
		pairs = append(pairs, name+"="+pkg)
	}
	slices.Sort(pairs)
	return strings.Join(pairs, ",")
}

// Set records one name=pkg entry. Both parts must be non-empty, and a name may
// appear only once.
func (b binaryFlags) Set(v string) error {
	name, pkg, ok := strings.Cut(v, "=")
	if !ok || name == "" || pkg == "" {
		return fmt.Errorf("binary %q must be name=package", v)
	}
	if _, dup := b[name]; dup {
		return fmt.Errorf("binary name %q is repeated", name)
	}
	b[name] = pkg
	return nil
}

// parseVerifyFlags turns `surla verify` arguments into a verify.Plan, the
// run's time limit, and whether to print the builder view (the default).
// -candidate-module and -suite-module default to ".", the repository root;
// every other plan field is required and must be given explicitly. Commit IDs
// and repositories are checked by verify.Run.
func parseVerifyFlags(args []string) (verify.Plan, time.Duration, bool, error) {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	binaries := binaryFlags{}
	var p verify.Plan
	fs.StringVar(&p.CandidateRepo, "candidate-repo", "", "git repository holding the candidate")
	fs.StringVar(&p.CandidateCommit, "candidate-commit", "", "full commit ID of the candidate to build")
	fs.StringVar(&p.CandidateModule, "candidate-module", ".", "candidate Go module directory inside -candidate-repo")
	fs.Var(binaries, "binary", "candidate binary as name=package inside -candidate-module (repeatable)")
	fs.StringVar(&p.SuiteRepo, "suite-repo", "", "git repository holding the evaluator suite")
	fs.StringVar(&p.SuiteCommit, "suite-commit", "", "full commit ID of the evaluator suite")
	fs.StringVar(&p.SuiteModule, "suite-module", ".", "suite Go module directory inside -suite-repo")
	fs.StringVar(&p.SuiteCommand, "suite-command", "", "suite command package inside -suite-module")
	fs.StringVar(&p.RequestRevision, "request-revision", "", "request_revision of the confirmed change being verified")
	fs.StringVar(&p.WorkDir, "work-dir", "", "private directory for clones, builds (candidate-bin/<name>) and evidence; it must be missing or empty and is kept after the run; keep it away from builders")
	fs.StringVar(&p.ReportDir, "report-dir", "", "directory that receives the full report as <digest>.json")
	timeout := fs.Duration("timeout", 0, "how long the run may take before it is stopped; 0, the default, means no limit")
	builderView := fs.Bool("builder-view", true, "hide holdout scenario details in the printed report and summary; set to false for the full report")
	if err := fs.Parse(args); err != nil {
		return verify.Plan{}, 0, false, err
	}
	if fs.NArg() > 0 {
		return verify.Plan{}, 0, false, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	required := []struct {
		flag  string
		value string
	}{
		{"-candidate-repo", p.CandidateRepo},
		{"-candidate-commit", p.CandidateCommit},
		{"-suite-repo", p.SuiteRepo},
		{"-suite-commit", p.SuiteCommit},
		{"-suite-command", p.SuiteCommand},
		{"-request-revision", p.RequestRevision},
		{"-work-dir", p.WorkDir},
		{"-report-dir", p.ReportDir},
	}
	var missing []string
	for _, r := range required {
		if r.value == "" {
			missing = append(missing, r.flag)
		}
	}
	if len(binaries) == 0 {
		missing = append(missing, "-binary")
	}
	if len(missing) > 0 {
		return verify.Plan{}, 0, false, fmt.Errorf("verify requires %s", strings.Join(missing, ", "))
	}
	p.Binaries = binaries
	// The operator names the suite on the command line, so it keeps the
	// operator's environment as a stored evaluator does.
	p.Tests = verify.TestsEvaluator
	return p, *timeout, *builderView, nil
}

// isSHA256Hex reports whether s is a lowercase hex SHA-256 digest.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// projectServeOptions derives private stores and process helpers from the
// initialized project and environ, the server's environment after serve
// removed every provider family from it. Live targets start from environ; the
// model choice and provider settings come later, from hand_over_model_settings
// and configure_model. No caller supplies project identity or storage paths.
func projectServeOptions(state *project.State, environ []string) (controlplane.Options, error) {
	ws, err := configureProjectWorkspace(state)
	if err != nil {
		return controlplane.Options{}, err
	}
	self, err := selfCommand()
	if err != nil {
		return controlplane.Options{}, err
	}
	live := &controlplane.LiveConfig{
		Env:          environ,
		Command:      self,
		StartTimeout: defaultProjectVerifyTimeout,
	}
	if ws.verification != nil {
		live.Command = ws.verification.Command
		live.StartTimeout = ws.verification.Timeout
	}
	goProxyEnv, err := operatorGoProxyEnv(ws.evalWorkDir)
	if err != nil {
		return controlplane.Options{}, err
	}
	return controlplane.Options{
		CardsDir:               ws.cardsDir,
		WorkspaceDir:           ws.workspaceDir,
		InitStageDir:           ws.initStageDir,
		Evaluator:              designeval.NewEvaluatorInDir(ws.evalWorkDir),
		NewGenerator:           func() scaffold.Generator { return designeval.NewEvaluatorInDir(ws.evalWorkDir) },
		SurlaVersion:           surlaBuildVersion(),
		Verification:           ws.verification,
		RunsDir:                ws.runsDir,
		WrittenTestRuns:        ws.writtenTestRuns,
		PromotionTarget:        ws.promotionTarget,
		ResolvePromotionTarget: ws.resolvePromotionTarget,
		Live:                   live,
		Checker:                candidate.NewContainerRunner(),
		CheckHelper:            checkHelperCommand(self),
		CheckTimeout:           defaultCheckTimeout,
		GoProxyEnv:             goProxyEnv,
	}, nil
}

// operatorGoProxyEnv returns the operator's module proxy and checksum
// settings as KEY=value for dependency-download containers. It runs the
// trusted go command in dir, an operator-owned directory with no Go files,
// so no workspace go.mod, go.work or go.env influences the values. Empty
// values are omitted so the container's Go uses its defaults.
func operatorGoProxyEnv(dir string) ([]string, error) {
	names := []string{"GOPROXY", "GOSUMDB", "GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOINSECURE"}
	cmd := exec.Command("go", append([]string{"env", "-json"}, names...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read the operator's Go proxy settings with go env: %w", err)
	}
	var values map[string]string
	if err := json.Unmarshal(out, &values); err != nil {
		return nil, fmt.Errorf("decode go env output: %w", err)
	}
	var env []string
	for _, name := range names {
		if v := values[name]; v != "" {
			env = append(env, name+"="+v)
		}
	}
	return env, nil
}

// writeLeftoverLiveDirs names the live target directories an earlier serve
// using the same cards directory left without removing them. It states only
// what it observed: whether their processes have stopped is not checked.
func writeLeftoverLiveDirs(w io.Writer, root string) {
	dirs, err := verifyrun.LeftoverLiveDirs(root)
	if err != nil {
		fmt.Fprintf(w, "surla: CP3: could not list earlier live target directories: %v\n", err)
		return
	}
	for _, dir := range dirs {
		fmt.Fprintf(w, "surla: CP3: an earlier project server using this cards directory left live target directory %s; its files were kept\n", dir)
	}
}
