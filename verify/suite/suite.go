// Package suite implements Surla's evaluator suite command contract: the
// protocol between `surla verify` and an evaluator suite program.
//
// `surla verify` builds the suite program from the suite commit and runs it
// twice:
//
//   - suite --describe prints the suite's Declaration as JSON and exits 0. It
//     needs no candidate binaries.
//   - suite --artifact name=path ... receives every candidate binary the
//     verification plan binds (`surla verify -binary name=package`). The suite
//     pins each one, runs its scenarios, and prints the goa-ai eval.Report as
//     JSON. It exits 0 when every scenario passed and the pinned binaries
//     were removed, 1 when a scenario did not pass, and 2 when it could not
//     run or could not remove the pinned binaries.
//
// A suite that sets Command.Live also supports a third mode, used by a CP3
// live target rather than by verification:
//
//   - suite --live --artifact name=path ... pins the binaries, calls Live
//     with the key in the SURLA_LIVE_TARGET_KEY environment variable, prints
//     one JSON line {"url": "http://127.0.0.1:<port>"} once the candidate is
//     listening, and serves until SIGINT or SIGTERM or until the target
//     ends. It exits 0 when it was asked to stop and the target and pinned
//     binaries were cleaned up, and 2 otherwise. A suite without Live
//     rejects --live with ExitError and changes nothing else.
//
// A scenario result must carry the evidence of every bound binary
// (candidate.Artifact.Evidence) so the verifier can compare the digests the
// suite reports with the ones it measured.
//
// An evaluator suite's main function only describes itself with a Command and
// calls Main; the verifier decodes --describe output with Declaration and
// compares the exit status with ExitPassed.
package suite

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"goa.design/goa-ai/eval"

	"github.com/goadesign/surla/verify/candidate"
)

type (
	// Command describes one evaluator suite program to Main.
	Command struct {
		// Environment names the dependency profile the suite runs candidates
		// with, such as "sim". It is part of the declaration, so reports name
		// the profile that actually ran.
		Environment string
		// Binaries lists the candidate binary names the suite runs. Every run
		// must pass exactly these as --artifact name=path.
		Binaries []string
		// MaxConcurrency is the default maximum number of scenarios in
		// flight; --max-concurrency overrides it. It must be positive.
		MaxConcurrency int
		// Declare returns the generated eval.Suite for --describe. Only its ID
		// and its scenarios' IDs and tags are read; its scenarios never run,
		// so it needs no binaries and builds no runtime state.
		Declare func() (eval.Suite, error)
		// Suite returns the generated eval.Suite whose scenarios run the given
		// pinned binaries, keyed by name. pinned holds exactly Binaries. The
		// verifier rejects a report naming a scenario Declare did not
		// declare, counts a declared scenario the report omits as not run,
		// and reads holdout tags from Declare alone.
		Suite func(pinned map[string]candidate.Artifact) (eval.Suite, error)
		// Live, when set, starts the given pinned binaries the way a scenario
		// does, for interactive use, and returns once the candidate is
		// listening. key is the credential the candidate must require on its
		// prompt-override operation; it comes from LiveKeyEnv and must reach
		// the candidate only through candidate.Spec.Env. ctx is canceled when
		// the suite is asked to stop; Live's processes must then stop, and
		// LiveTarget.Done must deliver once they have. Nil means the suite
		// supports no --live mode.
		Live func(ctx context.Context, pinned map[string]candidate.Artifact, key string) (LiveTarget, error)
	}

	// LiveTarget is a running candidate started by Command.Live.
	LiveTarget struct {
		// URL is the candidate's loopback HTTP origin, such as
		// http://127.0.0.1:8080.
		URL string
		// Done delivers once, after every process Live started has exited:
		// nil when they stopped because ctx was canceled, otherwise why the
		// target ended.
		Done <-chan error
	}

	// liveReady is the one line --live prints when its target listens.
	liveReady struct {
		// URL is LiveTarget.URL.
		URL string `json:"url"`
	}

	// Declaration is what a suite prints for --describe and what the
	// verifier reads to learn every scenario a report must account for.
	Declaration struct {
		// ID is the suite identifier.
		ID string `json:"id"`
		// Environment is the dependency profile the suite runs candidates
		// with.
		Environment string `json:"environment"`
		// Scenarios lists every declared scenario in declaration order.
		Scenarios []DeclaredScenario `json:"scenarios"`
	}

	// DeclaredScenario is one scenario of a Declaration.
	DeclaredScenario struct {
		// ID is the scenario identifier.
		ID string `json:"id"`
		// Tags are the scenario's declared tags. The tag "holdout" marks a
		// scenario that verify.Report.ForBuilders, the report view given to
		// builders, shows only as an anonymous outcome. That redaction is the
		// only protection: the suite's source and raw output still name the
		// scenario to anyone who can read them.
		Tags []string `json:"tags"`
	}

	// listFlag collects a repeatable string flag.
	listFlag []string
)

const (
	// ExitPassed is the exit status of a run in which every scenario passed,
	// and of a successful --describe. The verifier treats any other status
	// as a failed run.
	ExitPassed = 0
	// ExitFailed is the exit status of a run in which a scenario failed.
	ExitFailed = 1
	// ExitError is the exit status of a suite that could not run.
	ExitError = 2

	// LiveKeyEnv names the environment variable that carries the --live
	// target's credential to the suite. It is an environment variable, not a
	// flag, so the key never appears in a process listing.
	LiveKeyEnv = "SURLA_LIVE_TARGET_KEY"
)

// Main runs the suite command described by c with the process's arguments
// and exits with the contract's status. SIGINT and SIGTERM cancel the run.
func Main(c Command) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runCommand(ctx, os.Args[1:], os.Stdout, os.Stderr, c)
	stop()
	os.Exit(code)
}

// runCommand runs the suite command described by c with args and returns its
// exit status. --describe writes the Declaration to stdout. Otherwise it
// pins every --artifact into a temporary store, runs the scenarios selected
// by --scenario or --tag (all by default; the two cannot be combined) with at
// most --max-concurrency in flight, writes the eval.Report to stdout, and
// removes the store. Errors go to stderr. A report is written whenever the
// runner produced one, even when the run then failed; a store that cannot be
// removed is an error, so the status is then ExitError.
func runCommand(ctx context.Context, args []string, stdout, stderr io.Writer, c Command) int {
	var artifacts, scenarios, tags listFlag
	fs := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)
	fs.SetOutput(stderr)
	describe := fs.Bool("describe", false, "print the suite declaration as JSON and exit")
	live := fs.Bool("live", false, "start the candidate for interactive use, print its URL, and serve until stopped")
	fs.Var(&artifacts, "artifact", "candidate binary as name=path (repeatable)")
	fs.Var(&scenarios, "scenario", "run only this scenario (repeatable)")
	fs.Var(&tags, "tag", "run only scenarios with this tag (repeatable)")
	concurrency := fs.Int("max-concurrency", c.MaxConcurrency, "maximum scenarios in flight")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitPassed
		}
		return ExitError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected arguments %q\n", fs.Args())
		return ExitError
	}
	if *live {
		if *describe || len(scenarios) > 0 || len(tags) > 0 {
			fmt.Fprintln(stderr, "--live cannot be combined with --describe, --scenario or --tag")
			return ExitError
		}
		if err := runLive(ctx, c, artifacts, os.Getenv(LiveKeyEnv), stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return ExitError
		}
		return ExitPassed
	}
	if *describe {
		if err := writeDeclaration(stdout, c); err != nil {
			fmt.Fprintln(stderr, err)
			return ExitError
		}
		return ExitPassed
	}
	c.MaxConcurrency = *concurrency
	report, err := run(ctx, c, artifacts, scenarios, tags)
	if report != nil {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		err = errors.Join(err, enc.Encode(report))
	}
	switch {
	case err != nil:
		fmt.Fprintln(stderr, err)
		return ExitError
	case !report.Passed:
		return ExitFailed
	default:
		return ExitPassed
	}
}

// String returns the collected values.
func (l *listFlag) String() string {
	return strings.Join(*l, ",")
}

// Set appends one value.
func (l *listFlag) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// writeDeclaration writes the Declaration of c's suite to w as JSON.
func writeDeclaration(w io.Writer, c Command) error {
	s, err := c.Declare()
	if err != nil {
		return err
	}
	d := Declaration{ID: s.ID, Environment: c.Environment, Scenarios: []DeclaredScenario{}}
	for _, sc := range s.Scenarios {
		d.Scenarios = append(d.Scenarios, DeclaredScenario{ID: sc.ID, Tags: sc.Tags})
	}
	return json.NewEncoder(w).Encode(d)
}

// run pins the artifacts into a temporary store, builds the suite and runs
// the selected scenarios. The store is removed before run returns.
func run(ctx context.Context, c Command, artifacts, scenarios, tags []string) (_ *eval.Report, err error) {
	paths, err := parseArtifacts(artifacts, c.Binaries)
	if err != nil {
		return nil, err
	}
	if len(scenarios) > 0 && len(tags) > 0 {
		return nil, errors.New("--scenario and --tag cannot be combined")
	}
	store, err := os.MkdirTemp("", "surla-suite-artifacts-*")
	if err != nil {
		return nil, fmt.Errorf("create artifact store: %w", err)
	}
	defer func() { err = errors.Join(err, candidate.RemoveStore(store)) }()
	pinned := make(map[string]candidate.Artifact, len(paths))
	for name, path := range paths {
		a, err := candidate.Pin(store, path)
		if err != nil {
			return nil, err
		}
		pinned[name] = a
	}
	s, err := c.Suite(pinned)
	if err != nil {
		return nil, err
	}
	runner, err := eval.NewRunner(nil, eval.RunnerConfig{MaxConcurrency: c.MaxConcurrency})
	if err != nil {
		return nil, err
	}
	var report eval.Report
	switch {
	case len(scenarios) > 0:
		report, err = runner.RunScenarios(ctx, s, scenarios...)
	case len(tags) > 0:
		report, err = runner.RunTags(ctx, s, tags...)
	default:
		report, err = runner.Run(ctx, s)
	}
	return &report, err
}

// runLive pins the artifacts into a temporary store, starts them with
// c.Live, prints the ready line and waits until the target ends. A target
// that ends while ctx is still active is an error, as is one that does not
// stop cleanly after ctx is canceled; the store is removed only after Done
// reports that Live's processes have exited.
func runLive(ctx context.Context, c Command, artifacts []string, key string, stdout io.Writer) (err error) {
	if c.Live == nil {
		return errors.New("this evaluator suite does not support --live: its suite.Command sets no Live function")
	}
	if key == "" {
		return fmt.Errorf("--live requires the target key in %s", LiveKeyEnv)
	}
	paths, err := parseArtifacts(artifacts, c.Binaries)
	if err != nil {
		return err
	}
	store, err := os.MkdirTemp("", "surla-suite-artifacts-*")
	if err != nil {
		return fmt.Errorf("create artifact store: %w", err)
	}
	pinned := make(map[string]candidate.Artifact, len(paths))
	for name, path := range paths {
		a, err := candidate.Pin(store, path)
		if err != nil {
			return errors.Join(err, candidate.RemoveStore(store))
		}
		pinned[name] = a
	}
	liveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	target, err := c.Live(liveCtx, pinned, key)
	if err != nil {
		return errors.Join(fmt.Errorf("start live target: %w", err), candidate.RemoveStore(store))
	}
	if target.Done == nil {
		return errors.Join(errors.New("Live returned a target without a Done channel"), candidate.RemoveStore(store))
	}
	// From here Live's processes may use the store, so it is removed only
	// after Done reports that they have exited.
	defer func() { err = errors.Join(err, candidate.RemoveStore(store)) }()
	if err := checkLiveURL(target.URL); err != nil {
		cancel()
		return errors.Join(err, <-target.Done)
	}
	if err := json.NewEncoder(stdout).Encode(liveReady{URL: target.URL}); err != nil {
		cancel()
		return errors.Join(fmt.Errorf("print ready line: %w", err), <-target.Done)
	}
	select {
	case doneErr := <-target.Done:
		// Done and a stop request can be ready together; a stop was asked
		// for exactly when ctx is canceled.
		if ctx.Err() != nil {
			if doneErr != nil {
				return fmt.Errorf("stop live target: %w", doneErr)
			}
			return nil
		}
		if doneErr == nil {
			doneErr = errors.New("the live target stopped although it was not asked to")
		}
		return fmt.Errorf("live target ended: %w", doneErr)
	case <-ctx.Done():
		cancel()
		if doneErr := <-target.Done; doneErr != nil {
			return fmt.Errorf("stop live target: %w", doneErr)
		}
		return nil
	}
}

// checkLiveURL requires an http origin on the IPv4 loopback address, the
// only address a sandboxed candidate can listen on.
func checkLiveURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("live target URL %q is not an http://127.0.0.1:<port> origin", raw)
	}
	return nil
}

// parseArtifacts maps --artifact name=path flags and requires exactly the
// names in want, each once.
func parseArtifacts(flags, want []string) (map[string]string, error) {
	paths := make(map[string]string, len(flags))
	for _, f := range flags {
		name, path, ok := strings.Cut(f, "=")
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("--artifact %q must be name=path", f)
		}
		if _, dup := paths[name]; dup {
			return nil, fmt.Errorf("--artifact %s given twice", name)
		}
		if !slices.Contains(want, name) {
			return nil, fmt.Errorf("--artifact %s is not a binary this suite runs (expected %s)", name, strings.Join(want, ", "))
		}
		paths[name] = path
	}
	for _, name := range want {
		if _, ok := paths[name]; !ok {
			return nil, fmt.Errorf("--artifact %s=<path> is required", name)
		}
	}
	return paths, nil
}
