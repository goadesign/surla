package verifyrun

import (
	"bufio"
	"bytes"
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
	"unicode/utf8"

	"goa.design/goa-ai/runtime/agent/model"

	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/modelsettings"
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
		// ended is closed when executor output ends. Its descendants may
		// still exist until the owner calls Stop and confirms group cleanup.
		ended chan struct{}
		// lines delivers the executor's first line to AwaitReady.
		lines chan liveLine
		// readerDone is closed once the reader of the executor's output has
		// closed the pipe.
		readerDone chan struct{}
		// settings are the provider request settings the target was given;
		// startError protects the log excerpt it returns against them.
		settings []modelsettings.Settings
		// summary is the fixed startup guidance for this target's test source.
		summary string
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
		"and that a model is configured in Surla."
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
	// Detail is the protected tail of executor.log, bounded to maxLiveDetail
	// bytes after modelsettings.ProtectExcerpt replaces held settings. A
	// character cut at either edge is left out, so exporters receive UTF-8.
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
// executor's environment; LaunchLive replaces its TMPDIR and key and drops
// every provider family from it. For written tests settings reach only
// the executor's standard input as NUL-terminated NAME=value entries; the
// executor serves that model through its loopback gateway, which has no
// authentication: any local process can use it while this target runs,
// without receiving the provider key. An operator's
// evaluator keeps the environment delivery of its settings. Neither path
// writes settings in arguments or live.json. command runs this program. When
// LaunchLive fails, no process was started and the directory it created is
// removed. The allocated directory and child TMPDIR are absolute and
// symlink-free, so preparation and suite processes can change directories
// without changing them.
func LaunchLive(
	root string,
	plan verify.Plan,
	key string,
	command, env []string,
	settings []modelsettings.Settings,
) (*LiveProcess, error) {
	if key == "" {
		return nil, errors.New("live target key is required")
	}
	if err := plan.Tests.Check(); err != nil {
		return nil, err
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
	if plan.Tests == verify.TestsWritten {
		var input bytes.Buffer
		for _, held := range settings {
			for _, entry := range held.Environ() {
				input.WriteString(entry)
				input.WriteByte(0)
			}
		}
		cmd.Stdin = &input
	} else {
		for _, held := range settings {
			cmd.Env = append(cmd.Env, held.Environ()...)
		}
	}
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
		settings: settings, summary: liveSuiteSummary(plan),
	}
	go p.readOutput(stdout)
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// Live runs as `surla verify-run live <dir>`, the target's process-group
// leader. It starts watching its parent's control pipe before reading stdin,
// then removes provider families so preparation receives no held setting.
// Written tests receive a gateway: after preparation the executor applies
// stdin's settings to itself and calls newProvider, supplied by the command
// composition root. An operator's evaluator keeps the held starting settings
// in its environment and needs no constructor. The suite's --live mode and
// gateway live only as long as this executor; every terminal event ends the
// whole group. Only its owner removes the directory after confirmed cleanup.
// out gets one builder-safe ready/failure line; errLog keeps private details.
func Live(
	dir string,
	prepare func(context.Context, verify.Plan) (verify.Prepared, error),
	newProvider func(string, string, []modelsettings.Settings) (model.Provider, error),
	out, errLog io.Writer,
) error {
	syscall.CloseOnExec(passedFD)
	control := os.NewFile(passedFD, "control")
	go watchSupervisor(control)

	input, err := readLiveSettings(os.Stdin)
	if err != nil {
		return reportLive(out, errLog, "The live target's model settings could not be read; restart Surla.", err)
	}
	providers, err := takeProviderFamilies()
	if err != nil {
		return reportLive(out, errLog, summaryLiveSuite, err)
	}
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
		if req.Plan.Tests == verify.TestsWritten {
			summary = writtenLivePreparationSummary(stage)
		}
		return reportLive(out, errLog, summary, err)
	}
	programEnv := append(os.Environ(), providers...)
	if req.Plan.Tests == verify.TestsWritten {
		if provider := os.Getenv(verify.LiveModelProviderEnv); provider != "" {
			for _, held := range input {
				for _, entry := range held.Environ() {
					name, value, _ := strings.Cut(entry, "=")
					if err := os.Setenv(name, value); err != nil {
						return reportLive(out, errLog, liveSettingsSummary(provider), err)
					}
				}
			}
			p, err := newProvider(provider, os.Getenv(verify.LiveModelNameEnv), input)
			if err != nil {
				return reportLive(out, errLog, liveSettingsSummary(provider), err)
			}
			address, err := serveLiveModel(p, provider)
			if err != nil {
				return reportLive(out, errLog, liveSettingsSummary(provider), err)
			}
			programEnv = append(programEnv, verify.LiveModelGatewayEnv+"="+address)
		}
	}
	suiteOut, suiteOutW, err := os.Pipe()
	if err != nil {
		return reportLive(out, errLog, liveSuiteSummary(req.Plan), err)
	}
	cmd := exec.CommandContext(ctx, prep.SuiteBin, append([]string{"--live"}, prep.ArtifactFlags...)...)
	cmd.Env = verify.LiveProgramEnv(req.Plan, programEnv)
	cmd.Stdout = suiteOutW
	cmd.Stderr = errLog
	// The suite and everything it starts stay in this process group, which
	// the starting process kills to stop the target.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	if err := cmd.Start(); err != nil {
		return errors.Join(reportLive(out, errLog, liveSuiteSummary(req.Plan), err), suiteOutW.Close(), suiteOut.Close())
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
			return reportLive(out, errLog, liveSuiteSummary(req.Plan), errors.New("suite printed no valid ready line"))
		}
		if err := json.NewEncoder(out).Encode(liveLine{URL: line.URL}); err != nil {
			fmt.Fprintln(errLog, err)
			return endGroup()
		}
		fmt.Fprintln(errLog, "suite exited:", <-exited)
		return endGroup()
	case err := <-exited:
		return reportLive(out, errLog, liveSuiteSummary(req.Plan), fmt.Errorf("suite exited before it was ready: %v", err))
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
			return p.startError(p.summary, fmt.Errorf("%w before it was ready", ErrLiveEnded))
		}
		p.URL = line.URL
		return nil
	case <-ctx.Done():
		return p.startError(p.summary, ctx.Err())
	}
}

// startError returns a *LiveStartError with the executor log's current tail,
// read with the context before it that modelsettings.ProtectExcerpt needs,
// which replaces every byte of a setting the target was given, whoever
// wrote the line, including one the tail's start cuts or the log's end
// leaves unfinished. The log itself is the suite's own output and stays as
// written.
func (p *LiveProcess) startError(summary string, cause error) *LiveStartError {
	text, start := logTail(filepath.Join(p.dir, "executor.log"), maxLiveDetail, modelsettings.ExcerptContext)
	detail := modelsettings.ProtectExcerpt(text, start, p.settings...)
	if len(detail) > maxLiveDetail {
		start := len(detail) - maxLiveDetail
		for start < len(detail) && !utf8.RuneStart(detail[start]) {
			start++
		}
		detail = detail[start:]
	}
	return &LiveStartError{Summary: summary, Detail: detail, Err: cause}
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

// logTail reads the tail of the file at path, its last n bytes, together
// with up to context bytes before it, bounded by the file's start. It
// returns what it read and where the tail starts in it, or a note saying
// why the file could not be read, which is all tail.
func logTail(path string, n, context int64) (string, int) {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("(executor log unreadable: %v)", err), 0
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Sprintf("(executor log unreadable: %v)", err), 0
	}
	tail := max(info.Size()-n, 0)
	from := max(tail-context, 0)
	b := make([]byte, info.Size()-from)
	if _, err := f.ReadAt(b, from); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Sprintf("(executor log unreadable: %v)", err), 0
	}
	return string(b), int(tail - from)
}

// decodeLiveRequest reads live.json, rejecting unknown fields, trailing data
// and a plan without a declared test source.
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
	if err := req.Plan.Tests.Check(); err != nil {
		return fmt.Errorf("decode live request: %w", err)
	}
	return nil
}

// liveEnv returns env without TMPDIR, the live key or any provider family,
// which LaunchLive sets from its own arguments.
func liveEnv(env []string) []string {
	_, rest := modelsettings.Split(env)
	out := make([]string, 0, len(rest))
	for _, kv := range rest {
		name, _, _ := strings.Cut(kv, "=")
		if name == "TMPDIR" || name == suite.LiveKeyEnv {
			continue
		}
		out = append(out, kv)
	}
	return out
}
