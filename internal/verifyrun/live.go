package verifyrun

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/procgroup"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/verify/candidate"
	"github.com/goadesign/surla/verify/suite"
)

// A live target is a candidate commit an evaluator suite runs for interactive
// CP3 prompt drafts. It lives no longer than the process that started it.
//
// LaunchLive, called by `surla serve`, creates a directory under the live root
// and starts `surla verify-run live <dir>` as the leader of a new process
// group, with the read end of a control pipe on its descriptor 3 and TMPDIR
// set inside the directory. The executor (Live) watches that pipe before it
// does anything else, then runs verify.Prepare and the suite's --live mode in
// its own group and relays the suite's ready line. Every process the session
// starts is in that one group, and every file it writes, apart from the
// shared Go module and toolchain caches verify.Prepare inherits, is under the
// directory.
//
// Any terminal event ends the whole group at once: when preparation fails,
// when the suite prints no valid ready line, or when the suite exits, the
// executor prints at most one safe line and kills its own group, itself
// included. It never removes anything. LiveProcess.Stop, run by serve outside
// the group, is the only cleanup: it kills the group if it still exists,
// waits until it is empty and the executor is reaped, and only then removes
// the directory. If serve dies, the kernel closes the pipe and the
// executor kills its whole group, itself included, so no process outlives
// serve; the directory is then left for the operator. LaunchLive returns as
// soon as the executor runs, so its caller owns the process group from then
// on; AwaitReady waits for the ready line and never cleans anything up.

type (
	// LiveRequest is the record of one live target, saved as live.json in
	// its directory before the executor starts. It holds no credential.
	LiveRequest struct {
		// Plan is the build plan: the candidate and suite repositories and
		// exact commits, the binaries and the suite command. Its WorkDir is
		// inside the live directory; ReportDir and RequestRevision are unused.
		Plan verify.Plan `json:"plan"`
	}

	// LiveProcess is a started live target, owned by the process that
	// called LaunchLive.
	LiveProcess struct {
		// URL is the candidate's loopback origin from the suite's ready line.
		URL string
		// dir is the live directory Stop removes.
		dir string
		// pgid is the executor's process group.
		pgid int
		// control is serve's end of the control pipe; closing it makes the
		// executor kill its group.
		control *os.File
		// exited is closed once the executor has been reaped; waitErr is
		// its exit error.
		exited  chan struct{}
		waitErr error
		// ended is closed when the executor's output ends, which happens
		// only when its group has been killed.
		ended chan struct{}
		// lines delivers the executor's first line to AwaitReady.
		lines chan liveLine
		// readerDone is closed once the reader of the executor's output has
		// closed the pipe.
		readerDone chan struct{}
		// stopOnce makes Stop idempotent; stopErr is its result.
		stopOnce sync.Once
		stopErr  error
	}

	// liveLine is the one line the executor prints: the target's URL once it
	// listens, or a summary safe to show builders when it could not start.
	liveLine struct {
		// URL is the ready target's origin.
		URL string `json:"url,omitempty"`
		// Error is why the target could not start.
		Error string `json:"error,omitempty"`
	}
)

const (
	// liveRequestFile, liveWorkDir and liveTmpDir are entries of a live
	// directory.
	liveRequestFile = "live.json"
	liveWorkDir     = "work"
	liveTmpDir      = "tmp"
	// liveDirPrefix names live directories inside the live root.
	liveDirPrefix = "live-"
	// liveStopTimeout bounds how long Stop waits for the group to empty.
	liveStopTimeout = 30 * time.Second
	// maxLiveLine bounds the executor's line.
	maxLiveLine = 4 << 10
	// summaryLiveSuite describes a suite that did not report a ready target
	// and what the operator can check.
	summaryLiveSuite = "The evaluator suite did not start a live target. Check that its suite.Command sets Live " +
		"and that the project server's environment has the model provider settings the suite reads."
	// maxLiveDetail bounds the executor log excerpt kept in a
	// LiveStartError after its directory is removed.
	maxLiveDetail = 8 << 10
)

// ErrLiveEnded reports a live target whose processes ended before Stop.
var ErrLiveEnded = errors.New("live target ended")

// LiveStartError is why a launched live target did not become ready. Summary
// is safe to show builders. Detail is the end of the executor's log, read
// while the live directory still existed; it may quote command output, paths
// and provider errors, so it is only for the operator's private diagnostics.
// Err is the cause: ctx.Err() when the wait was canceled or timed out,
// ErrLiveEnded when the executor ended first, or nil when the executor
// reported the failure itself.
type LiveStartError struct {
	// Summary describes the failure in one sentence.
	Summary string
	// Detail is at most maxLiveDetail bytes from the end of executor.log.
	Detail string
	// Err is the underlying cause, or nil.
	Err error
}

// Error returns the summary.
func (e *LiveStartError) Error() string {
	return e.Summary
}

// Unwrap returns the cause.
func (e *LiveStartError) Unwrap() error {
	return e.Err
}

// LeftoverLiveDirs returns the names of the live directories under root. It
// only lists names: it does not check whether a directory belongs to a
// running target or whether its processes have stopped.
func LeftoverLiveDirs(root string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(root, liveDirPrefix+"*"))
	if err != nil {
		return nil, fmt.Errorf("list live directories: %w", err)
	}
	return matches, nil
}

// LaunchLive starts a live target for plan under root and returns as soon as
// its executor runs; AwaitReady then waits until the suite reports the target
// listening. From the moment LaunchLive returns a process, the caller owns it
// and must eventually call Stop. key is the target's credential: it
// reaches the executor and the suite only through the environment
// (suite.LiveKeyEnv), and the suite passes it to the candidate. env is the
// executor's environment without TMPDIR or the key, which LaunchLive sets.
// command is the argument list that runs this program. When LaunchLive fails,
// no process was started and the directory it created is removed.
// The allocated directory and child TMPDIR are absolute and symlink-free, so
// preparation and suite processes can change directories without changing them.
func LaunchLive(root string, plan verify.Plan, key string, command, env []string) (*LiveProcess, error) {
	if key == "" {
		return nil, errors.New("live target key is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create live root: %w", err)
	}
	resolvedRoot, err := abspath.Eval(root)
	if err != nil {
		return nil, fmt.Errorf("resolve live root symlinks: %w", err)
	}
	dir, err := os.MkdirTemp(resolvedRoot, liveDirPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("create live directory: %w", err)
	}
	plan.WorkDir = filepath.Join(dir, liveWorkDir)
	plan.ReportDir, plan.RequestRevision = "", ""
	tmp := filepath.Join(dir, liveTmpDir)
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return nil, errors.Join(fmt.Errorf("create live temporary directory: %w", err), os.RemoveAll(dir))
	}
	if err := writeJSON(dir, liveRequestFile, LiveRequest{Plan: plan}); err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "executor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create executor log: %w", err), os.RemoveAll(dir))
	}
	defer logFile.Close()
	control, keep, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create control pipe: %w", err), os.RemoveAll(dir))
	}
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create executor output pipe: %w", err), control.Close(), keep.Close(), os.RemoveAll(dir))
	}
	cmd := exec.Command(command[0], append(command[1:], "verify-run", "live", dir)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{control}
	cmd.Env = append(liveEnv(env), "TMPDIR="+tmp, suite.LiveKeyEnv+"="+key)
	cmd.Stdout = stdoutW
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("start live executor: %w", err), control.Close(), keep.Close(),
			stdout.Close(), stdoutW.Close(), os.RemoveAll(dir))
	}
	control.Close()
	stdoutW.Close()
	p := &LiveProcess{
		dir: dir, pgid: cmd.Process.Pid, control: keep, exited: make(chan struct{}),
		ended: make(chan struct{}), lines: make(chan liveLine, 1), readerDone: make(chan struct{}),
	}
	go p.readOutput(stdout)
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// Live runs as `surla verify-run live <dir>`, the leader of the target's
// process group. Its first action watches the control pipe on descriptor 3,
// so the whole group dies with the process that started it at any stage. It
// then prepares the plan saved in live.json with prepare, starts the suite in
// --live mode in the same group, relays the suite's ready line to out and
// waits for the suite to exit. A failure prints a builder-safe summary as the
// line and the full error to errLog. Live returns an error whenever the
// target is not running, so its exit status is never 0 after a start.
func Live(dir string, prepare func(context.Context, verify.Plan) (verify.Prepared, error), out, errLog io.Writer) error {
	syscall.CloseOnExec(passedFD)
	control := os.NewFile(passedFD, "control")
	go watchSupervisor(control)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	var req LiveRequest
	if err := decodeLiveRequest(dir, &req); err != nil {
		return reportLive(out, errLog, "The live target request could not be read.", err)
	}
	prep, err := prepare(ctx, req.Plan)
	if err != nil {
		summary := summaryLiveSuite
		var stage *verify.StageError
		if errors.As(err, &stage) {
			summary = stage.Summary
		}
		return reportLive(out, errLog, summary, err)
	}
	suiteOut, suiteOutW, err := os.Pipe()
	if err != nil {
		return reportLive(out, errLog, summaryLiveSuite, err)
	}
	cmd := exec.CommandContext(ctx, prep.SuiteBin, append([]string{"--live"}, prep.ArtifactFlags...)...)
	cmd.Stdout = suiteOutW
	cmd.Stderr = errLog
	// The suite and everything it starts stay in this process group, which
	// the starting process kills to stop the target.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	if err := cmd.Start(); err != nil {
		return errors.Join(reportLive(out, errLog, summaryLiveSuite, err), suiteOutW.Close(), suiteOut.Close())
	}
	suiteOutW.Close()
	first := make(chan liveLine, 1)
	go func() {
		line, err := readFirstLine(suiteOut)
		if err != nil {
			fmt.Fprintln(errLog, err)
		}
		first <- line
	}()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case line := <-first:
		if line.URL == "" {
			return reportLive(out, errLog, summaryLiveSuite, errors.New("suite printed no valid ready line"))
		}
		if err := json.NewEncoder(out).Encode(liveLine{URL: line.URL}); err != nil {
			fmt.Fprintln(errLog, err)
			return endGroup()
		}
		fmt.Fprintln(errLog, "suite exited:", <-exited)
		return endGroup()
	case err := <-exited:
		return reportLive(out, errLog, summaryLiveSuite, fmt.Errorf("suite exited before it was ready: %v", err))
	}
}

// AwaitReady waits until the target reports it is listening and sets URL.
// Its one source is the executor's first line, which readOutput always
// delivers (a zero line when the output ends first), so the end of the output
// can never replace the safe summary the executor wrote before it ended. ctx
// bounds the wait. Every failure is a *LiveStartError whose Detail was read
// before any cleanup; AwaitReady never stops the target, which stays the
// caller's to stop.
func (p *LiveProcess) AwaitReady(ctx context.Context) error {
	select {
	case line := <-p.lines:
		switch {
		case line.Error != "":
			return p.startError(line.Error, nil)
		case line.URL == "":
			return p.startError(summaryLiveSuite, fmt.Errorf("%w before it was ready", ErrLiveEnded))
		}
		p.URL = line.URL
		return nil
	case <-ctx.Done():
		return p.startError(summaryLiveSuite, ctx.Err())
	}
}

// startError returns a *LiveStartError with the executor log's current tail.
func (p *LiveProcess) startError(summary string, cause error) *LiveStartError {
	return &LiveStartError{Summary: summary, Detail: logTail(filepath.Join(p.dir, "executor.log"), maxLiveDetail), Err: cause}
}

// Dir returns the live directory, which Stop removes.
func (p *LiveProcess) Dir() string {
	return p.dir
}

// Done is closed once the target has ended or been stopped. The session's
// processes may still exist until Stop, which the owner must then call.
func (p *LiveProcess) Done() <-chan struct{} {
	return p.ended
}

// Stop ends the target: it closes the control pipe, kills the executor's
// whole group, waits until the group is empty and the executor is reaped,
// and only then removes the live directory. A group that does not empty in
// time keeps the directory, and the error names it. Stop is idempotent.
func (p *LiveProcess) Stop() error {
	p.stopOnce.Do(func() {
		closeErr := p.control.Close()
		if err := procgroup.Stop(p.pgid, liveStopTimeout); err != nil {
			p.stopErr = errors.Join(closeErr, fmt.Errorf("stop live target; kept %s: %w", p.dir, err))
			return
		}
		<-p.exited
		<-p.readerDone
		p.stopErr = errors.Join(closeErr, candidate.RemoveStore(p.dir))
	})
	return p.stopErr
}

// reportLive prints summary as the executor's one line, writes the full error
// to errLog and ends the group, so nothing the failed step started remains.
func reportLive(out, errLog io.Writer, summary string, err error) error {
	fmt.Fprintln(errLog, err)
	if err := json.NewEncoder(out).Encode(liveLine{Error: summary}); err != nil {
		fmt.Fprintln(errLog, err)
	}
	return endGroup()
}

// endGroup kills the executor's whole process group, the executor included.
// The line already written stays readable in the pipe. It returns only if
// the kill failed.
func endGroup() error {
	if err := syscall.Kill(0, syscall.SIGKILL); err != nil {
		return fmt.Errorf("end live process group: %w", err)
	}
	// SIGKILL to the own group is delivered before the kill call returns to
	// a running process, so this is not reached.
	return errors.New("live process group survived SIGKILL")
}

// readOutput owns the executor's stdout: it delivers the first line to
// AwaitReady, a zero line when the output ends first or the line does not
// decode, closes ended when the output ends, and closes the pipe before
// closing readerDone.
func (p *LiveProcess) readOutput(r *os.File) {
	defer close(p.readerDone)
	defer r.Close()
	defer close(p.ended)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, maxLiveLine), maxLiveLine)
	var line liveLine
	if sc.Scan() {
		decoded, err := decodeLiveLine(sc.Text())
		if err == nil {
			line = decoded
		}
	}
	p.lines <- line
	// The executor writes nothing after its line; read to EOF, which comes
	// when its group is killed.
	for sc.Scan() {
		continue
	}
}

// readFirstLine reads the suite's ready line from r and then keeps draining r
// in the background until the suite closes it, so the suite never blocks on a
// full pipe. A missing or malformed line is a zero line and an error.
func readFirstLine(r *os.File) (liveLine, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, maxLiveLine), maxLiveLine)
	if !sc.Scan() {
		return liveLine{}, errors.Join(errors.New("suite printed no ready line"), sc.Err(), r.Close())
	}
	line, err := decodeLiveLine(sc.Text())
	go func() {
		// Discard the suite's later output; its lines carry nothing the
		// executor reports.
		for sc.Scan() {
			continue
		}
		r.Close()
	}()
	return line, err
}

// decodeLiveLine decodes one line, rejecting unknown fields.
func decodeLiveLine(text string) (liveLine, error) {
	var line liveLine
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&line); err != nil {
		return liveLine{}, fmt.Errorf("decode live line: %w", err)
	}
	return line, nil
}

// logTail returns at most n bytes from the end of the file at path, or a
// note saying why it could not be read.
func logTail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("(executor log unreadable: %v)", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Sprintf("(executor log unreadable: %v)", err)
	}
	start := max(info.Size()-n, 0)
	b := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(b, start); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Sprintf("(executor log unreadable: %v)", err)
	}
	return string(b)
}

// decodeLiveRequest reads live.json, rejecting unknown fields and trailing
// data.
func decodeLiveRequest(dir string, req *LiveRequest) error {
	f, err := os.Open(filepath.Join(dir, liveRequestFile))
	if err != nil {
		return fmt.Errorf("open live request: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(req); err != nil {
		return fmt.Errorf("decode live request: %w", err)
	}
	if dec.More() {
		return errors.New("decode live request: trailing data")
	}
	return nil
}

// liveEnv returns env without TMPDIR or the live key, which LaunchLive sets.
func liveEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if name == "TMPDIR" || name == suite.LiveKeyEnv {
			continue
		}
		out = append(out, kv)
	}
	return out
}
