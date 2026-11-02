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

	"github.com/goadesign/surla/internal/procgroup"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/verify/candidate"
	"github.com/goadesign/surla/verify/suite"
)

// A live target is a candidate commit an evaluator suite runs for interactive
// CP3 prompt drafts. It lives no longer than the process that started it.
//
// StartLive, called by `surla serve`, creates a directory under the live root
// and starts `surla verify-run live <dir>` as the leader of a new process
// group, with the read end of a control pipe on its descriptor 3 and TMPDIR
// set inside the directory. The executor (Live) watches that pipe before it
// does anything else, then runs verify.Prepare and the suite's --live mode in
// its own group and relays the suite's ready line. Every process the session
// starts is in that one group, and every file it writes, apart from the
// shared Go module and toolchain caches verify.Prepare inherits, is under the
// directory.
//
// The executor never removes anything. While serve lives, LiveProcess.Stop is
// the only cleanup: it kills the group, waits until it is empty and only then
// removes the directory. If serve dies, the kernel closes the pipe and the
// executor kills its whole group, itself included, so no process outlives
// serve; the directory is then left for the operator, and the next StartLive
// in the same root reports it.

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
	// called StartLive.
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
	// summaryLiveSuite describes a suite that did not report a ready target.
	summaryLiveSuite = "The evaluator suite did not start a live target."
)

// ErrLiveEnded reports a live target whose processes ended before Stop.
var ErrLiveEnded = errors.New("live target ended")

// LeftoverLiveDirs returns the live directories under root, left there when
// the process that started them died. Their processes are gone; the operator
// removes the directories.
func LeftoverLiveDirs(root string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(root, liveDirPrefix+"*"))
	if err != nil {
		return nil, fmt.Errorf("list live directories: %w", err)
	}
	return matches, nil
}

// StartLive starts a live target for plan under root and returns once its
// suite reports the target listening. key is the target's credential: it
// reaches the executor and the suite only through the environment
// (suite.LiveKeyEnv), and the suite passes it to the candidate. env is the
// executor's environment without TMPDIR or the key, which StartLive sets.
// command is the argument list that runs this program. ctx bounds only the
// start: canceling it before the ready line stops the target. On any error
// the target is stopped and its directory removed, unless its processes could
// not be stopped, in which case the error names the kept directory.
func StartLive(ctx context.Context, root string, plan verify.Plan, key string, command, env []string) (_ *LiveProcess, err error) {
	if key == "" {
		return nil, errors.New("live target key is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create live root: %w", err)
	}
	dir, err := os.MkdirTemp(root, liveDirPrefix+"*")
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
	p := &LiveProcess{dir: dir, pgid: cmd.Process.Pid, control: keep, exited: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()
	defer func() {
		if err != nil {
			err = errors.Join(err, p.Stop())
		}
	}()
	lines := make(chan liveLine, 1)
	go readLiveLine(stdout, lines)
	select {
	case line := <-lines:
		switch {
		case line.Error != "":
			return nil, errors.New(line.Error)
		case line.URL == "":
			return nil, errors.New(summaryLiveSuite)
		}
		p.URL = line.URL
		return p, nil
	case <-p.exited:
		return nil, fmt.Errorf("%w before it was ready: %v", ErrLiveEnded, p.waitErr)
	case <-ctx.Done():
		return nil, fmt.Errorf("start live target: %w", ctx.Err())
	}
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
	defer suiteOut.Close()
	cmd := exec.CommandContext(ctx, prep.SuiteBin, append([]string{"--live"}, prep.ArtifactFlags...)...)
	cmd.Stdout = suiteOutW
	cmd.Stderr = errLog
	// The suite and everything it starts stay in this process group, which
	// the starting process kills to stop the target.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	if err := cmd.Start(); err != nil {
		suiteOutW.Close()
		return reportLive(out, errLog, summaryLiveSuite, err)
	}
	suiteOutW.Close()
	lines := make(chan liveLine, 1)
	go readLiveLine(suiteOut, lines)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case line := <-lines:
		if line.URL == "" {
			return errors.Join(reportLive(out, errLog, summaryLiveSuite, errors.New("suite printed no URL")), <-exited)
		}
		if err := json.NewEncoder(out).Encode(liveLine{URL: line.URL}); err != nil {
			return errors.Join(err, <-exited)
		}
		return fmt.Errorf("%w: suite exited: %v", ErrLiveEnded, <-exited)
	case err := <-exited:
		return reportLive(out, errLog, summaryLiveSuite, fmt.Errorf("suite exited before it was ready: %v", err))
	}
}

// Dir returns the live directory, which Stop removes.
func (p *LiveProcess) Dir() string {
	return p.dir
}

// Done is closed once the executor has exited, which happens only when the
// target ended or Stop stopped it.
func (p *LiveProcess) Done() <-chan struct{} {
	return p.exited
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
		p.stopErr = errors.Join(closeErr, candidate.RemoveStore(p.dir))
	})
	return p.stopErr
}

// reportLive prints summary as the executor's line, writes the full error to
// errLog and returns it.
func reportLive(out, errLog io.Writer, summary string, err error) error {
	fmt.Fprintln(errLog, err)
	return errors.Join(err, json.NewEncoder(out).Encode(liveLine{Error: summary}))
}

// readLiveLine reads one bounded JSON line from r and sends it, or a zero
// line when r ends first or the line does not decode.
func readLiveLine(r io.Reader, lines chan<- liveLine) {
	var line liveLine
	sc := bufio.NewScanner(io.LimitReader(r, maxLiveLine))
	if sc.Scan() {
		dec := json.NewDecoder(strings.NewReader(sc.Text()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&line); err != nil {
			line = liveLine{}
		}
	}
	lines <- line
	// Keep draining so a writer never blocks on a full pipe.
	if _, err := io.Copy(io.Discard, r); err != nil {
		return
	}
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

// liveEnv returns env without TMPDIR or the live key, which StartLive sets.
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
