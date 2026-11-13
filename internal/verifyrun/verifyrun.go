// Package verifyrun runs one Checkpoint 4 verification in processes that
// outlive the API that accepted it, and reads its state back from disk.
//
// Every run owns one directory under the runs root:
//
//	<root>/<run_id>/request.json  immutable: owner, Change Card revision,
//	                              candidate commit and the full verify.Plan
//	<root>/<run_id>/lock          held exclusively by the run's supervisor
//	<root>/<run_id>/result.json   terminal outcome, written by the supervisor
//	<root>/<run_id>/*.log         supervisor and executor output
//
// The run's work directory, where verify.Run clones, builds and keeps
// evidence, is chosen by the caller in the plan and is never removed: the
// caller owns it, and its built binaries and evidence stay readable after
// the run.
//
// Start, called by the API, takes the lock before it writes request.json and
// hands the locked file to `surla verify-run supervise <dir>`, a new session
// that keeps the lock for its whole life. The supervisor starts
// `surla verify-run execute <dir>` as the leader of a new process group, so
// the builds, the evaluator suite and the candidates it runs all stay in that
// one group. When the executor ends, for any reason, the supervisor kills the
// group, waits until it is empty, and only then writes result.json. If the
// supervisor dies instead, the executor sees its control pipe close and kills
// its own group.
//
// Read classifies a run by the kernel lock alone: a held lock means the run is
// in progress; otherwise result.json is its outcome, and a missing result
// means the run's processes ended before it finished. Nothing else records a
// run's state.
package verifyrun

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/goadesign/surla/internal/modelsettings"
	"github.com/goadesign/surla/internal/procgroup"
	"github.com/goadesign/surla/internal/verify"
)

type (
	// Request is the immutable record of one accepted verification run.
	Request struct {
		// RunID identifies the run and names its directory.
		RunID string `json:"run_id"`
		// Owner is the authenticated principal that started the run; only it
		// may read the run.
		Owner string `json:"owner"`
		// ChangeID is the Change Card the run verifies.
		ChangeID string `json:"change_id"`
		// AcceptedAt is when the API accepted the run.
		AcceptedAt time.Time `json:"accepted_at"`
		// Timeout is how long the run may take before its supervisor stops it
		// and records it as interrupted. Zero means the run has no time limit,
		// as for surla verify unless -timeout is given; verify_change always
		// sets the operator's limit.
		Timeout time.Duration `json:"timeout"`
		// Plan is the complete verification plan: the operator's candidate
		// and suite configuration, the caller's candidate commit, the Change
		// Card revision, and the run's own work directory.
		Plan verify.Plan `json:"plan"`
	}

	// Result is the terminal outcome of a run, written once by its
	// supervisor and, for a run that finished, first by its executor.
	Result struct {
		// Kind says whether the run produced a report.
		Kind ResultKind `json:"kind"`
		// ReportDigest names the report under Plan.ReportDir when Kind is
		// ResultReport.
		ReportDigest string `json:"report_digest,omitempty"`
		// Failure says how a run without a report ended.
		Failure FailureKind `json:"failure,omitempty"`
		// Stage is where a run without a report stopped.
		Stage string `json:"stage,omitempty"`
		// Summary describes how a run without a report ended, without command
		// output.
		Summary string `json:"summary,omitempty"`
		// PartialReportDigest names, for an interrupted run, the report the
		// suite saved while it was being stopped, if it saved one. Such a
		// report never passes; surla verify prints it as evidence, and
		// get_verification never returns it.
		PartialReportDigest string `json:"partial_report_digest,omitempty"`
	}

	// ResultKind says whether a run produced a report.
	ResultKind string

	// FailureKind says how a run without a report ended.
	FailureKind string

	// State is a run's current state as Read classifies it.
	State struct {
		// Kind is the state.
		Kind StateKind
		// Report is the saved report when Kind is StateCompleted.
		Report verify.Report
		// Result is the terminal outcome when Kind is StateNotCompleted.
		Result Result
	}

	// StateKind is one of the three run states.
	StateKind string

	// Run pairs an accepted verification request with its current state as
	// classified by Read or List.
	Run struct {
		// Request is the immutable record written when the run was accepted.
		Request Request
		// State is the run's current state on disk.
		State State
	}

	// limitedWriter keeps at most n bytes written to it and drops the rest.
	limitedWriter struct {
		// w receives the kept bytes.
		w io.Writer
		// n is how many more bytes may be kept.
		n int
	}
)

const (
	// ResultReport is a run that produced a report.
	ResultReport ResultKind = "report"
	// ResultNoReport is a run that ended without one.
	ResultNoReport ResultKind = "no_report"

	// FailureExecution is a run whose verification step failed.
	FailureExecution FailureKind = "execution_failed"
	// FailureInterrupted is a run whose processes ended before it finished.
	FailureInterrupted FailureKind = "interrupted"

	// StageSupervise is the stage of a run interrupted outside verify.Run:
	// its executor or supervisor ended, it was canceled or timed out, or its
	// request could not be read.
	StageSupervise = "supervise"

	// StateRunning is a run still in progress.
	StateRunning StateKind = "running"
	// StateCompleted is a run that produced a report.
	StateCompleted StateKind = "completed"
	// StateNotCompleted is a run that ended without a report.
	StateNotCompleted StateKind = "not_completed"

	// requestFile, lockFile, resultFile and workDir are the entries of a
	// run directory.
	requestFile = "request.json"
	lockFile    = "lock"
	resultFile  = "result.json"

	// passedFD is the descriptor a supervisor and an executor receive from
	// their parent: the locked run file and the control pipe respectively.
	passedFD = 3

	// groupStopTimeout bounds how long the supervisor waits for the
	// executor's process group to empty after killing it.
	groupStopTimeout = 30 * time.Second
	// stopGrace is how long a canceled or timed-out executor gets to stop
	// its suite, which removes its pinned binaries, before the supervisor
	// kills the whole group. It exceeds verify.Run's suite stop grace.
	stopGrace = 45 * time.Second
	// executorWaitDelay bounds how long the supervisor waits for the
	// executor's output after it exits.
	executorWaitDelay = 5 * time.Second
	// maxExecutorOutput bounds the executor's result line.
	maxExecutorOutput = 64 << 10

	// summaryExecutorDied, summarySupervisorDied, summaryCanceled and
	// summaryTimedOut describe interrupted runs.
	summaryExecutorDied   = "The verification process ended before it finished."
	summarySupervisorDied = "The process supervising the verification ended before it finished."
	summaryCanceled       = "The verification was canceled."
	summaryTimedOut       = "The verification did not finish within its time limit."
	// summaryUnreadableRequest describes a run whose saved request could not
	// be read, so nothing was started.
	summaryUnreadableRequest = "The verification request could not be read."
)

var (
	// runIDPattern is the form of a run ID, which also names its directory.
	runIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// digestPattern is the form of a report digest.
	digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// NewRunID returns a new random run ID.
func NewRunID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// Start accepts req under root and starts its supervisor, then returns the
// supervisor's PID without waiting for the run. command is the argument list
// that runs this program, such as the path of the surla executable; Start
// appends "verify-run supervise <dir>". The supervisor gets this process's
// environment plus settings, the provider request settings the run's test
// program may use; they pass to the executor only through the environment,
// never an argument or request.json, so the run keeps this snapshot whatever
// its caller holds later. The supervisor, which starts nothing but the
// executor, thus holds the snapshot in its environment for the run's life,
// as the executor does until it hands it to the test program alone. The run directory must not exist,
// req.Plan.WorkDir must name the run's work directory, and req.Plan.Tests
// must name one of the declared test sources.
func Start(root string, req Request, command []string, settings []modelsettings.Settings) (int, error) {
	if !runIDPattern.MatchString(req.RunID) {
		return 0, fmt.Errorf("run ID %q is not 32 lowercase hex characters", req.RunID)
	}
	if req.Timeout < 0 {
		return 0, fmt.Errorf("run timeout %s must not be negative", req.Timeout)
	}
	if req.Plan.WorkDir == "" {
		return 0, errors.New("run work directory is required")
	}
	if strings.TrimSpace(req.Owner) == "" {
		return 0, errors.New("run owner is required")
	}
	if req.AcceptedAt.IsZero() {
		return 0, errors.New("run accepted_at time is required")
	}
	if err := req.Plan.Tests.Check(); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return 0, fmt.Errorf("create runs directory: %w", err)
	}
	dir := filepath.Join(root, req.RunID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return 0, fmt.Errorf("create run directory: %w", err)
	}
	// The lock is taken before request.json exists and is passed, still
	// held, to the supervisor, so no reader ever sees an accepted run that
	// nobody holds.
	lock, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create run lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return 0, fmt.Errorf("lock run: %w", err)
	}
	if err := writeJSON(dir, requestFile, req); err != nil {
		return 0, err
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "supervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create supervisor log: %w", err)
	}
	defer logFile.Close()
	cmd := exec.Command(command[0], append(command[1:], "verify-run", "supervise", dir)...)
	cmd.Env = os.Environ()
	for _, s := range settings {
		cmd.Env = append(cmd.Env, s.Environ()...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = []*os.File{lock}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start verification supervisor: %w", err)
	}
	go reap(cmd)
	return cmd.Process.Pid, nil
}

// Supervise runs as `surla verify-run supervise <dir>`. It holds the run lock
// it inherited on descriptor 3, starts the executor as the leader of a new
// process group with the read end of a control pipe on its descriptor 3, and
// waits for it. SIGTERM, SIGINT or SIGHUP cancels the run, and the request's
// Timeout ends it: either way the executor gets SIGTERM and stopGrace to stop
// its suite before the whole group is killed. When the executor ends, the
// group is killed and emptied before result.json is written: the executor's
// own result when it reported one, and otherwise an interrupted outcome. A
// run the supervisor stopped is always interrupted with the reason, canceled
// or timed out: a report the suite saved while stopping is partial and would
// read as failed scenarios, so it is kept only as the outcome's
// PartialReportDigest.
// command is the argument list that runs this program.
func Supervise(dir string, command []string) error {
	syscall.CloseOnExec(passedFD)
	lock := os.NewFile(passedFD, lockFile)
	defer lock.Close()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)

	req, err := readRequest(dir)
	if err != nil {
		return errors.Join(err, writeJSON(dir, resultFile, interrupted(summaryUnreadableRequest)))
	}
	control, keep, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create control pipe: %w", err)
	}
	defer keep.Close()
	var out bytes.Buffer
	logFile, err := os.OpenFile(filepath.Join(dir, "executor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("create executor log: %w", err)
	}
	defer logFile.Close()
	cmd := exec.Command(command[0], append(command[1:], "verify-run", "execute", dir)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{control}
	cmd.Stdout = &limitedWriter{w: &out, n: maxExecutorOutput}
	cmd.Stderr = logFile
	cmd.WaitDelay = executorWaitDelay
	if err := cmd.Start(); err != nil {
		control.Close()
		return errors.Join(fmt.Errorf("start verification executor: %w", err),
			writeJSON(dir, resultFile, interrupted(summaryExecutorDied)))
	}
	control.Close()
	pgid := cmd.Process.Pid

	// stopped receives why the supervisor stopped the run, at most once.
	stopped := make(chan string, 1)
	exited := make(chan struct{})
	go stopOnRequest(pgid, req.Timeout, signals, exited, stopped)
	waitErr := cmd.Wait()
	close(exited)
	// Whatever ended the executor, nothing it started may outlive the run.
	if err := procgroup.Stop(pgid, groupStopTimeout); err != nil {
		return fmt.Errorf("stop verification processes; result not written: %w", err)
	}
	result := interrupted(summaryExecutorDied)
	if waitErr == nil {
		if r, err := decodeResult(out.Bytes()); err == nil {
			result = r
		}
	}
	select {
	case reason := <-stopped:
		partial := ""
		if result.Kind == ResultReport {
			partial = result.ReportDigest
		}
		result = interrupted(reason)
		result.PartialReportDigest = partial
	default:
	}
	return writeJSON(dir, resultFile, result)
}

// Execute runs as `surla verify-run execute <dir>`, the leader of the run's
// process group. It watches the control pipe on descriptor 3 and kills its
// whole group when the supervisor dies. It then takes every provider family
// out of its own process (takeProviderFamilies), so the run's preparation
// never sees one, and runs the plan saved in request.json with run, passing
// the process environment plus what it took, from which only the test
// program's environment is derived. It cancels the run on SIGTERM and
// writes the run's Result as JSON to out. The full error of a failed step
// goes to errLog, which stays with the evaluator.
func Execute(
	dir string,
	run func(context.Context, verify.Plan, []string) (verify.Report, error),
	out, errLog io.Writer,
) error {
	syscall.CloseOnExec(passedFD)
	control := os.NewFile(passedFD, "control")
	go watchSupervisor(control)

	providers, err := takeProviderFamilies()
	if err != nil {
		return err
	}
	req, err := readRequest(dir)
	if err != nil {
		return err
	}
	// The supervisor cancels or times out a run with SIGTERM, so the suite
	// can stop its candidates and remove its pinned binaries.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	report, err := run(ctx, req.Plan, append(os.Environ(), providers...))
	result := Result{Kind: ResultReport, ReportDigest: report.Digest}
	if err != nil {
		var stage *verify.StageError
		if !errors.As(err, &stage) {
			return fmt.Errorf("verification failed without a stage: %w", err)
		}
		fmt.Fprintln(errLog, err)
		result = Result{Kind: ResultNoReport, Failure: FailureExecution, Stage: string(stage.Stage), Summary: stage.Summary}
	}
	return json.NewEncoder(out).Encode(result)
}

// Read returns the run's request and current state. It takes a shared lock on
// the run before it looks for result.json, so a run that finishes meanwhile is
// never reported as interrupted. A missing run is an error. Once request.json
// is read, it is returned with any later error, so a caller can check who owns
// the run before reporting that error.
func Read(root, runID string) (Request, State, error) {
	if !runIDPattern.MatchString(runID) {
		return Request{}, State{}, fmt.Errorf("run ID %q is not 32 lowercase hex characters", runID)
	}
	dir := filepath.Join(root, runID)
	req, err := readRequest(dir)
	if err != nil {
		return req, State{}, err
	}
	state, err := readState(dir, req)
	if err != nil {
		return req, State{}, err
	}
	return req, state, nil
}

// List returns the saved verification runs under root whose request.json names
// owner and changeID, ordered by AcceptedAt descending and tie-broken by RunID
// ascending. A missing root directory returns nil, nil because Start creates
// root on the first accepted run. A run directory without request.json has no
// published request in this enumeration snapshot and is skipped; any corrupt
// or unreadable published request.json returns an error so List never claims
// complete coverage after skipping a damaged record. Only matching
// (owner, changeID) runs have their lock, result.json, or report inspected.
func List(root, owner, changeID string) ([]Run, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("run owner is required")
	}
	if strings.TrimSpace(changeID) == "" {
		return nil, errors.New("change_id is required")
	}
	reqs, err := publishedRequests(root, func(req Request) bool {
		return req.Owner == owner && req.ChangeID == changeID
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(reqs, newestFirst)
	runs := make([]Run, 0, len(reqs))
	for _, req := range reqs {
		state, err := readState(filepath.Join(root, req.RunID), req)
		if err != nil {
			return nil, err
		}
		runs = append(runs, Run{Request: req, State: state})
	}
	return runs, nil
}

// LatestByChange returns, keyed by change ID, the newest saved run of each of
// owner's changes under root: the run List would return first for that
// change. It reads root once, however many changes owner has, so a caller
// listing every change does not scan root per change. Missing root, runs
// without request.json, and damaged requests behave as in List. Only the
// returned runs have their lock, result.json, or report inspected.
func LatestByChange(root, owner string) (map[string]Run, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("run owner is required")
	}
	reqs, err := publishedRequests(root, func(req Request) bool { return req.Owner == owner })
	if err != nil {
		return nil, err
	}
	newest := make(map[string]Request)
	for _, req := range reqs {
		if current, ok := newest[req.ChangeID]; !ok || newestFirst(req, current) < 0 {
			newest[req.ChangeID] = req
		}
	}
	latest := make(map[string]Run, len(newest))
	for changeID, req := range newest {
		state, err := readState(filepath.Join(root, req.RunID), req)
		if err != nil {
			return nil, err
		}
		latest[changeID] = Run{Request: req, State: state}
	}
	return latest, nil
}

// LoadReport reads the report saved under reportDir with digest, such as a
// completed run's report or an interrupted run's PartialReportDigest, and
// checks that it is the report that digest names.
func LoadReport(reportDir, digest string) (verify.Report, error) {
	f, err := os.Open(filepath.Join(reportDir, digest+".json"))
	if err != nil {
		return verify.Report{}, fmt.Errorf("open verification report: %w", err)
	}
	defer f.Close()
	report, err := verify.DecodeReport(f)
	if err != nil {
		return verify.Report{}, err
	}
	if report.Digest != digest {
		return verify.Report{}, fmt.Errorf("report %s has digest %s", digest, report.Digest)
	}
	return report, nil
}

// Wait blocks until the run's supervisor has finished, by taking a shared
// lock on the run.
func Wait(root, runID string) error {
	lock, err := os.Open(filepath.Join(root, runID, lockFile))
	if err != nil {
		return fmt.Errorf("open run lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
		return fmt.Errorf("wait for run: %w", err)
	}
	return nil
}

// Write keeps what fits and reports every byte as written, so the writer
// never blocks or fails the executor.
func (l *limitedWriter) Write(p []byte) (int, error) {
	keep := p[:min(len(p), l.n)]
	l.n -= len(keep)
	if _, err := l.w.Write(keep); err != nil {
		return 0, err
	}
	return len(p), nil
}

// readState classifies the run in dir by its kernel lock and, once unlocked,
// its result.json and saved report.
func readState(dir string, req Request) (State, error) {
	lock, err := os.Open(filepath.Join(dir, lockFile))
	if err != nil {
		return State{}, fmt.Errorf("open run lock: %w", err)
	}
	defer lock.Close()
	switch err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); {
	case errors.Is(err, syscall.EWOULDBLOCK):
		return State{Kind: StateRunning}, nil
	case err != nil:
		return State{}, fmt.Errorf("lock run for reading: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, resultFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return State{Kind: StateNotCompleted, Result: interrupted(summarySupervisorDied)}, nil
	case err != nil:
		return State{}, fmt.Errorf("read run result: %w", err)
	}
	result, err := decodeResult(raw)
	if err != nil {
		return State{}, err
	}
	if result.Kind == ResultNoReport {
		return State{Kind: StateNotCompleted, Result: result}, nil
	}
	report, err := LoadReport(req.Plan.ReportDir, result.ReportDigest)
	if err != nil {
		return State{}, err
	}
	return State{Kind: StateCompleted, Report: report}, nil
}

// interrupted returns the outcome of a run whose processes ended before it
// finished.
func interrupted(summary string) Result {
	return Result{Kind: ResultNoReport, Failure: FailureInterrupted, Stage: StageSupervise, Summary: summary}
}

// decodeResult strictly decodes one Result and checks that its fields match
// its kind.
func decodeResult(raw []byte) (Result, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r Result
	if err := dec.Decode(&r); err != nil {
		return Result{}, fmt.Errorf("decode run result: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Result{}, errors.New("decode run result: unexpected trailing data")
	}
	switch {
	case r.Kind == ResultReport && digestPattern.MatchString(r.ReportDigest) &&
		r.Failure == "" && r.Stage == "" && r.Summary == "" && r.PartialReportDigest == "":
		return r, nil
	case r.Kind == ResultNoReport && r.ReportDigest == "" && r.Failure == FailureExecution &&
		r.Stage != "" && r.Summary != "" && r.PartialReportDigest == "":
		return r, nil
	case r.Kind == ResultNoReport && r.ReportDigest == "" && r.Failure == FailureInterrupted &&
		r.Stage != "" && r.Summary != "" &&
		(r.PartialReportDigest == "" || digestPattern.MatchString(r.PartialReportDigest)):
		return r, nil
	}
	return Result{}, fmt.Errorf("run result %s is inconsistent", raw)
}

// readRequest strictly decodes the run's request.json and validates its
// directory identity, owner, and acceptance time. Once Decode succeeds, req is
// returned alongside any validation error so callers can still check req.Owner
// before reporting read failures.
func readRequest(dir string) (Request, error) {
	raw, err := os.ReadFile(filepath.Join(dir, requestFile))
	if err != nil {
		return Request{}, fmt.Errorf("read run request: %w", err)
	}
	return decodeRequest(filepath.Base(dir), raw)
}

// decodeRequest strictly decodes raw request.json bytes for wantRunID and
// validates its directory identity, owner, acceptance time and test source.
// Once Decode succeeds, req is returned alongside any validation error so
// callers can still check req.Owner before reporting read failures.
func decodeRequest(wantRunID string, raw []byte) (Request, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var req Request
	if err := dec.Decode(&req); err != nil {
		return Request{}, fmt.Errorf("decode run request: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return req, errors.New("decode run request: unexpected trailing data")
	}
	if !runIDPattern.MatchString(req.RunID) || req.RunID != wantRunID {
		return req, fmt.Errorf("decode run request: run_id %q does not match directory %q", req.RunID, wantRunID)
	}
	if strings.TrimSpace(req.Owner) == "" {
		req.Owner = ""
		return req, errors.New("decode run request: owner is required")
	}
	if req.AcceptedAt.IsZero() {
		return req, errors.New("decode run request: accepted_at is required")
	}
	if err := req.Plan.Tests.Check(); err != nil {
		return req, fmt.Errorf("decode run request: %w", err)
	}
	return req, nil
}

// writeJSON writes v to dir/name atomically: a temporary file is written,
// synced and renamed, and the directory is synced.
func writeJSON(dir, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	tmp, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if _, err := tmp.Write(b); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", name, err), tmp.Close(), os.Remove(tmp.Name()))
	}
	if err := errors.Join(tmp.Sync(), tmp.Close()); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", name, err), os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", name, err), os.Remove(tmp.Name()))
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return errors.Join(d.Sync(), d.Close())
}

// stopGroup kills the executor's whole process group when it did not stop
// within stopGrace; Supervise then waits for the group to empty.
func stopGroup(pgid int) {
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		fmt.Fprintf(os.Stderr, "cancel verification: kill process group %d: %v\n", pgid, err)
	}
}

// takeProviderFamilies removes every provider family variable from this
// process's environment and returns them as NAME=value entries. An executor
// calls it at entry: what it received are the provider request settings its
// caller handed it, which only its test program may get back.
func takeProviderFamilies() ([]string, error) {
	family, _ := modelsettings.Split(os.Environ())
	for _, kv := range family {
		name, _, _ := strings.Cut(kv, "=")
		if err := os.Unsetenv(name); err != nil {
			return nil, fmt.Errorf("remove provider settings from the executor's environment: %w", err)
		}
	}
	return family, nil
}

// watchSupervisor reads the control pipe until the supervisor's end closes,
// which happens only when the supervisor exits, and then kills the executor's
// whole process group, the executor included.
func watchSupervisor(control *os.File) {
	if _, err := io.Copy(io.Discard, control); err != nil {
		fmt.Fprintf(os.Stderr, "read control pipe: %v\n", err)
	}
	if err := syscall.Kill(0, syscall.SIGKILL); err != nil {
		fmt.Fprintf(os.Stderr, "kill verification process group: %v\n", err)
	}
}

// reap waits for the supervisor so it does not linger as a zombie of the API
// process. Its exit status is not needed: the run's outcome is result.json.
func reap(cmd *exec.Cmd) {
	if err := cmd.Wait(); err != nil {
		fmt.Fprintf(os.Stderr, "verification supervisor %d: %v\n", cmd.Process.Pid, err)
	}
}

// stopOnRequest waits for a cancel signal or the run's timeout, if it has
// one, whichever comes first, unless the executor exits before either. It records why in
// stopped, sends the executor SIGTERM, and kills the whole group if the
// executor has not exited after stopGrace.
func stopOnRequest(pgid int, timeout time.Duration, signals <-chan os.Signal, exited <-chan struct{}, stopped chan<- string) {
	// A zero timeout leaves expired nil, so only a signal or the executor's
	// exit can end the wait.
	var expired <-chan time.Time
	if timeout > 0 {
		deadline := time.NewTimer(timeout)
		defer deadline.Stop()
		expired = deadline.C
	}
	select {
	case <-exited:
		return
	case <-signals:
		stopped <- summaryCanceled
	case <-expired:
		stopped <- summaryTimedOut
	}
	select {
	case <-exited:
		// The executor exited as the stop was decided; its PID may already
		// belong to another process.
		return
	default:
	}
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		fmt.Fprintf(os.Stderr, "stop verification: signal executor %d: %v\n", pgid, err)
	}
	grace := time.NewTimer(stopGrace)
	defer grace.Stop()
	select {
	case <-exited:
	case <-grace.C:
		stopGroup(pgid)
	}
}

// publishedRequests reads every published request.json under root, in one
// pass, and returns those keep accepts. A missing root returns nil, nil; a
// run directory without request.json is skipped as not yet published; any
// other unreadable or invalid request.json fails the read.
func publishedRequests(root string, keep func(Request) bool) ([]Request, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read runs directory: %w", err)
	}
	var reqs []Request
	for _, entry := range entries {
		if !entry.IsDir() || !runIDPattern.MatchString(entry.Name()) {
			continue
		}
		req, err := readRequest(filepath.Join(root, entry.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if keep(req) {
			reqs = append(reqs, req)
		}
	}
	return reqs, nil
}

// newestFirst orders requests by AcceptedAt descending, tie-broken by RunID
// ascending.
func newestFirst(a, b Request) int {
	if byTime := b.AcceptedAt.Compare(a.AcceptedAt); byTime != 0 {
		return byTime
	}
	return strings.Compare(a.RunID, b.RunID)
}
