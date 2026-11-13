// Command surla runs the Surla MCP software factory control-plane server
// and CLI utilities (`surla init`, `surla serve`, `surla preview`,
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
	"strconv"
	"strings"
	"syscall"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/diff"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
	"github.com/goadesign/surla/verify/candidate"
)

// serveConfig holds the optional `surla serve` flags that choose the model
// live targets plan with. Zero values mean
// "not configured".
type serveConfig struct {
	// gatewayAddr is the host:port of a Goa-AI model gateway reached over
	// plaintext gRPC, passed to live targets as SURLA_LIVE_MODEL_GATEWAY. It
	// excludes provider and modelName.
	gatewayAddr string
	// provider is "anthropic" or "openai" for direct provider calls, passed
	// to live targets as SURLA_LIVE_MODEL_PROVIDER; it requires modelName and
	// the provider's API key environment variable.
	provider string
	// modelName is the provider model identifier used in direct mode, passed
	// to live targets as SURLA_LIVE_MODEL_NAME.
	modelName string
	// projectDir is the derived physical project folder supplied by the server composition root.
	projectDir string
	// checkTimeout bounds one whole check_project or update_dependencies
	// operation (copy, image pull and every container).
	checkTimeout time.Duration
}

// defaultCheckTimeout is the default -check-timeout.
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

// usage is the text `surla`, `surla help`, `surla -h` and `surla --help`
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

// liveModelProviderEnv, liveModelNameEnv and liveModelGatewayEnv name the
// model choice serve passes to live targets' evaluator suites (see
// liveTargetEnv and docs/REFERENCE.md).
const (
	liveModelProviderEnv  = "SURLA_LIVE_MODEL_PROVIDER"
	liveModelNameEnv      = "SURLA_LIVE_MODEL_NAME"
	liveModelGatewayEnv   = "SURLA_LIVE_MODEL_GATEWAY"
	defaultAnthropicModel = "claude-sonnet-4-6"
	defaultOpenAIModel    = "gpt-5.4-mini"
)

// apiKeyEnv maps each supported direct model provider to the environment
// variable that holds its API key. Keys are read only from the environment so
// they never appear in process listings or shell history.
var apiKeyEnv = map[string]string{
	"anthropic": "ANTHROPIC_API_KEY",
	"openai":    "OPENAI_API_KEY",
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "surla: %v\n", err)
		os.Exit(1)
	}
}

// run dispatches the surla subcommands. No arguments, `help`, `-h`, `-help`
// and `--help` print usage to stdout; `help <command>` and
// `<command> -h` print that command's flags to stderr (the flag package's
// output). A help request returns nil so the exit status is 0; an unknown
// command or a bad flag returns an error.
func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return runProjectStart(nil, stdout)
	}
	sub, rest := args[0], args[1:]
	if slices.Contains(helpWords, sub) {
		switch {
		case len(rest) == 0, sub == "help" && len(rest) == 1 && slices.Contains(helpWords, rest[0]):
			_, err := fmt.Fprint(stdout, usage)
			return err
		case sub == "help" && len(rest) == 1:
			return run([]string{rest[0], "-h"}, stdout)
		default:
			return fmt.Errorf("unexpected arguments %q after %s", rest, sub)
		}
	}
	err := dispatch(sub, rest, stdout)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

// dispatch runs the subcommand sub with its arguments. A `-h` flag makes the
// subcommand's flag parsing print its flags and return flag.ErrHelp.
func dispatch(sub string, args []string, stdout io.Writer) error {
	switch sub {
	case "init":
		return runInit(args, stdout)
	case "serve":
		return runServe(args)
	case "stop":
		return runProjectStop(args, stdout)
	case "verification", "promotion":
		return runProjectSettings(sub, args, stdout)
	case "preview":
		return runPreview(args, stdout, os.Stderr)
	case "diff":
		return runDiff(args)
	case "gen":
		return runGen(args, stdout, os.Stderr)
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

// runServe starts the Surla HTTP and MCP JSON-RPC server protected by a
// verified bearer-token authenticator.
// bindServeConfigFlags registers the workspace, model, verification,
// promotion, and check flags for `surla serve` on fs.
func bindServeConfigFlags(fs *flag.FlagSet, cfg *serveConfig) {
	fs.StringVar(&cfg.gatewayAddr, "model-gateway-addr", "", "model gateway host:port that live targets plan with (plaintext gRPC; excludes -model-provider and -model-name)")
	fs.StringVar(&cfg.provider, "model-provider", "", "model provider that live targets plan with: anthropic or openai (key from ANTHROPIC_API_KEY or OPENAI_API_KEY)")
	fs.StringVar(&cfg.modelName, "model-name", "", "model name for -model-provider")

	fs.DurationVar(&cfg.checkTimeout, "check-timeout", defaultCheckTimeout, "how long one check_project or update_dependencies operation may take, including copying the workspace, pulling the Go image and running every module's containers")
}

// parseServeConfigFlags parses supported server execution options and rejects positional arguments.
func parseServeConfigFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Flag parsing stops at the first non-flag argument, so anything left over
	// would silently drop the flags after it.
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	return nil
}

// writeServeStatus tells the operator of `surla serve`, once the listener is
// bound, where MCP clients connect, where Change Cards are saved, and which of
// the five checkpoints (CP1–CP5) are on or off, naming the flag or host option
// that enables each off capability. addr is the bound address, so port 0
// reports the port actually assigned; an unspecified IP such as [::] is shown
// as localhost so the printed URL works from this machine. project is the
// derived project path, or empty when verification comes from stored settings
// or is off; with it, the status also says that the evaluator suite commit is
// the project's HEAD at startup and that a restart selects a later one. The
// bearer token never appears.
func writeServeStatus(w io.Writer, addr *net.TCPAddr, opts controlplane.Options, project string) {
	host := addr.String()
	if addr.IP.IsUnspecified() {
		host = net.JoinHostPort("localhost", strconv.Itoa(addr.Port))
	}
	fmt.Fprintf(w, "surla: serving on http://%s\n", host)
	fmt.Fprintf(w, "surla: connect MCP clients to http://%s/mcp with the bearer token (architecture guide: surla://golden-path or http://%s/golden-path)\n", host, host)
	writeCheckpointStatus(w, opts, project)
}

// writeCheckpointStatus writes the Change Cards directory and the on/off
// status of checkpoints CP1–CP5 to w for `surla serve`.
func writeCheckpointStatus(w io.Writer, opts controlplane.Options, project string) {
	fmt.Fprintf(w, "surla: saving Change Cards in %s\n", opts.CardsDir)
	fmt.Fprintln(w, "surla: CP1 (propose_change, revise_change, confirm_intent, get_change): on")
	fmt.Fprintln(w, "surla: CP2 (preview_architecture): on")
	switch {
	case opts.TargetUpdater == nil && (opts.Verification != nil || opts.ResolveVerification != nil) && opts.Live != nil:
		root := controlplane.LiveRoot(opts.CardsDir)
		fmt.Fprintf(w, "surla: CP3 (draft_override, start_live_target): compiled_rule is on; prompt_template is on once start_live_target runs a candidate commit (live targets in %s)\n", root)
		writeLeftoverLiveDirs(w, root)
	case opts.TargetUpdater == nil:
		fmt.Fprintln(w, "surla: CP3 (draft_override, start_live_target): compiled_rule is on; prompt_template is off"+
			" (initialize a committed starter or use surla verification set, then call start_live_target with a commit; or edit the prompt's source text, commit, and verify the commit)")
	default:
		fmt.Fprintln(w, "surla: CP3 (draft_override): on")
	}
	switch {
	case opts.Verification != nil:
		fmt.Fprintf(w, "surla: CP4 (verify_change, get_verification, list_verifications): on (runs in %s, reports in %s, evaluator suite commit %s)\n",
			opts.Verification.RunsDir, opts.Verification.Plan.ReportDir, opts.Verification.Plan.SuiteCommit)
	case opts.ResolveVerification != nil:
		fmt.Fprintf(w, "surla: CP4 (verify_change, get_verification, list_verifications): workspace %s (runs in %s; activates after init_project or when started in a committed starter repository)\n",
			opts.WorkspaceDir, opts.RunsDir)
	default:
		fmt.Fprintln(w, "surla: CP4 (verify_change, get_verification, list_verifications): off"+
			" (initialize a committed starter or use surla verification set, or run surla verify on the CLI)")
	}
	if project != "" && opts.Verification != nil {
		fmt.Fprintf(w, "surla: project %s: the evaluator suite commit is the project's HEAD at startup;"+
			" commit and restart Surla to use a later suite\n", project)
	}
	if opts.PromotionTarget == nil {
		fmt.Fprintln(w, "surla: CP5 (approve_merge): off (use surla promotion set, or run surla approve on the CLI)")
	} else {
		fmt.Fprintln(w, "surla: CP5 (approve_merge): on")
	}
}

// runPreview reads the design module in -dir (default: the current directory)
// with designeval.ReadModuleSnapshot, evaluates it with a design evaluator it owns, and
// writes the result with writePreviewResult: indented JSON to w and a one-line
// summary to stderr. It records nothing and calls no model. SIGINT and SIGTERM
// cancel the evaluation. On every return it closes the evaluator, which
// removes its build workspace, and returns that error with any other.
func runPreview(args []string, w, stderr io.Writer) (err error) {
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
	evalWorkDir, err := localEvalWorkDir(*dir)
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

// runDiff compares two design modules on disk, each read into a snapshot with
// designeval.ReadModuleSnapshot, and writes the structured diff JSON report to stdout.
// Both evaluations share one design evaluator it owns. SIGINT and SIGTERM
// cancel them. On every return it closes the evaluator, which removes its
// build workspace, and returns that error with any other.
func runDiff(args []string) (err error) {
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
	evalWorkDir, err := localEvalWorkDir(*oldDir, *newDir)
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

// serveOptions builds the control-plane Options for `surla serve` from cfg.
// getenv supplies provider API keys. When all model flags are empty,
// resolveStartupModelConfig auto-detects anthropic/claude-sonnet-4-6 from
// ANTHROPIC_API_KEY or openai/gpt-5.4-mini from OPENAI_API_KEY. The chosen
// model only reaches live targets' environment; with neither flags nor API
// keys set, live targets get no model choice until configure_model sets one.
func serveOptions(
	cfg serveConfig,
	getenv func(string) string,
	environ []string,
) (controlplane.Options, error) {
	resolvedCfg := resolveStartupModelConfig(cfg, getenv)
	if err := validateModelConfig(resolvedCfg, getenv); err != nil {
		return controlplane.Options{}, err
	}

	ws, err := configureServeWorkspace(cfg)
	if err != nil {
		return controlplane.Options{}, err
	}
	self, err := selfCommand()
	if err != nil {
		return controlplane.Options{}, err
	}
	live := &controlplane.LiveConfig{
		Env: liveTargetEnv(environ, resolvedCfg),
	}
	if ws.verification != nil {
		live.Command = ws.verification.Command
		live.StartTimeout = ws.verification.Timeout
	} else if ws.resolveVerification != nil {
		live.Command = self
		live.StartTimeout = defaultProjectVerifyTimeout
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
		LiveModelEnv:           newLiveModelEnv(getenv, environ),
		Verification:           ws.verification,
		RunsDir:                ws.runsDir,
		ResolveVerification:    ws.resolveVerification,
		PromotionTarget:        ws.promotionTarget,
		ResolvePromotionTarget: ws.resolvePromotionTarget,
		Live:                   live,
		Checker:                candidate.NewContainerRunner(),
		CheckHelper:            checkHelperCommand(self),
		CheckTimeout:           cfg.checkTimeout,
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

// resolveStartupModelConfig selects the default direct provider and model when
// all model flags are empty and ANTHROPIC_API_KEY or OPENAI_API_KEY is set in
// getenv. Any explicit model flag returns cfg unchanged so validateModelConfig
// enforces flag validation.
func resolveStartupModelConfig(cfg serveConfig, getenv func(string) string) serveConfig {
	if cfg.gatewayAddr != "" || cfg.provider != "" || cfg.modelName != "" {
		return cfg
	}
	switch {
	case strings.TrimSpace(getenv("ANTHROPIC_API_KEY")) != "":
		cfg.provider = "anthropic"
		cfg.modelName = defaultAnthropicModel
	case strings.TrimSpace(getenv("OPENAI_API_KEY")) != "":
		cfg.provider = "openai"
		cfg.modelName = defaultOpenAIModel
	}
	return cfg
}

// newLiveModelEnv returns the LiveModelEnv callback configure_model uses to
// build the environment live targets start with for a new model choice. A
// direct provider whose API key is missing from getenv is refused as
// unprocessable, because the live target would inherit no key.
func newLiveModelEnv(getenv func(string) string, environ []string) controlplane.LiveModelEnv {
	return func(p *gensurla.ConfigureModelPayload) ([]string, error) {
		var nextCfg serveConfig
		if p.GatewayAddr != nil {
			nextCfg.gatewayAddr = *p.GatewayAddr
		}
		if p.Provider != nil {
			nextCfg.provider = *p.Provider
		}
		if p.ModelName != nil {
			nextCfg.modelName = *p.ModelName
		}
		if nextCfg.provider != "" {
			if env, ok := apiKeyEnv[nextCfg.provider]; ok && strings.TrimSpace(getenv(env)) == "" {
				return nil, gensurla.MakeUnprocessable(fmt.Errorf(
					"provider %s requires %s in surla serve's environment; set %s and restart surla serve, or call configure_model with gateway_addr to attach a running model gateway",
					nextCfg.provider,
					env,
					env,
				))
			}
		}
		return liveTargetEnv(environ, nextCfg), nil
	}
}

// liveTargetEnv returns the environment live targets start with: serve's own,
// which carries the provider key its model flags use, without serve's MCP
// bearer token, plus the model choice those flags made, so an evaluator
// suite's Live hook can plan with the same model: SURLA_LIVE_MODEL_PROVIDER
// and SURLA_LIVE_MODEL_NAME for -model-provider and -model-name, or
// SURLA_LIVE_MODEL_GATEWAY for -model-gateway-addr. Nothing is added when no
// model is configured; serve's own values replace inherited ones.
func liveTargetEnv(environ []string, cfg serveConfig) []string {
	derived := map[string]string{}
	switch {
	case cfg.gatewayAddr != "":
		derived[liveModelGatewayEnv] = cfg.gatewayAddr
	case cfg.provider != "":
		derived[liveModelProviderEnv] = cfg.provider
		derived[liveModelNameEnv] = cfg.modelName
	}
	out := make([]string, 0, len(environ)+len(derived))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case liveModelGatewayEnv, liveModelProviderEnv, liveModelNameEnv:
			continue
		}
		out = append(out, kv)
	}
	for _, name := range []string{liveModelProviderEnv, liveModelNameEnv, liveModelGatewayEnv} {
		if v, ok := derived[name]; ok {
			out = append(out, name+"="+v)
		}
	}
	return out
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
		fmt.Fprintf(w, "surla: CP3: an earlier surla serve using this cards directory left live target directory %s; its files were kept\n", dir)
	}
}

// validateModelConfig checks the model flags `surla serve` passes to live
// targets: a gateway address alone, or a provider with a model name and that
// provider's API key in getenv. No flags at all is valid and means live
// targets get no model choice. Mixed or partial model flags, an unknown
// provider and a missing provider API key are errors; key values never
// appear in them.
func validateModelConfig(cfg serveConfig, getenv func(string) string) error {
	switch {
	case cfg.gatewayAddr != "" && (cfg.provider != "" || cfg.modelName != ""):
		return errors.New("-model-gateway-addr cannot be combined with -model-provider or -model-name; the gateway chooses the model")
	case cfg.gatewayAddr != "":
		return nil
	case cfg.provider == "" && cfg.modelName == "":
		return nil
	case cfg.provider == "" || cfg.modelName == "":
		return errors.New("-model-provider and -model-name must be set together")
	}
	env, ok := apiKeyEnv[cfg.provider]
	if !ok {
		return fmt.Errorf("unsupported -model-provider %q (expected \"anthropic\" or \"openai\")", cfg.provider)
	}
	if getenv(env) == "" {
		return fmt.Errorf("-model-provider %s requires %s", cfg.provider, env)
	}
	return nil
}
