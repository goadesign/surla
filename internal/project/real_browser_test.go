//go:build browser && (darwin || linux)

// This file starts and ends the headless browser that views_browser_test.go
// drives: it finds an installed Chrome or Chromium, launches it on a
// throwaway profile, reads the DevTools address it prints, and at the end
// shuts it down and checks that no process it started is left. The browser
// also ends when the test process ends any other way:
//
//   - It reads a pipe the test process holds open as its DevTools pipe
//     (--remote-debugging-pipe) and shuts down when that pipe closes, which
//     the operating system does when the test process ends for any reason,
//     including SIGKILL and a test timeout's panic.
//   - On SIGINT or SIGTERM the test process first ends the browser and
//     removes its profile, then lets the signal end the process. The browser
//     leads its own process group, so a terminal's interrupt never reaches it
//     directly.
//
// A test process killed outright cannot remove the profile; it stays in the
// test's temporary directory, as everything else such a test leaves does.
package project

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type (
	// realBrowser is one headless browser process the test started on a
	// throwaway profile. The test ends it and checks that nothing it started
	// is left.
	realBrowser struct {
		cmd      *exec.Cmd
		profile  string
		devtools string
		output   *browserOutput
		// closeLifeline closes the test's end of the pipe the browser reads
		// as its DevTools pipe, once; nothing is ever written to it. The
		// browser shuts down when it reads the pipe's end.
		closeLifeline func() error
		// exited is closed once cmd.Wait returned waitErr.
		exited  chan struct{}
		waitErr error
		// signals receives SIGINT and SIGTERM while the browser runs;
		// stopWatching stops that, once.
		signals      chan os.Signal
		watching     chan struct{}
		stopWatching func()
	}

	// browserOutput keeps the end of the browser's output for diagnostics
	// and hands over the DevTools address the browser prints once.
	browserOutput struct {
		mu       sync.Mutex
		tail     []byte
		line     []byte
		found    bool
		devtools chan string
	}

	// browserHelper is a running TestRealBrowserHelper process.
	browserHelper struct {
		cmd *exec.Cmd
		// stdout and stderr keep the helper's output; stdout is complete
		// once done is closed.
		stdout, stderr bytes.Buffer
		// done is closed once the helper's output ended, scanErr holding
		// why reading it failed, if it did.
		done    chan struct{}
		scanErr error
	}

	// listedProcess is one process as ps lists it.
	listedProcess struct {
		pid, ppid, pgid int
		command         string
	}
)

const (
	// browserEnv names the environment variable that selects the browser's
	// executable explicitly.
	browserEnv = "SURLA_TEST_BROWSER"
	// helperProfileEnv names the environment variable that makes
	// TestRealBrowserHelper start a browser on the profile it names.
	helperProfileEnv = "SURLA_TEST_BROWSER_HELPER_PROFILE"
	// helperReady starts the line TestRealBrowserHelper prints with its
	// browser's process ID once the browser listens.
	helperReady = "BROWSER-READY "
	// maxBrowserOutput bounds the browser output kept for diagnostics.
	maxBrowserOutput = 64 << 10
	// browserDeadline bounds each wait on the browser process.
	browserDeadline = 30 * time.Second
)

// wellKnownBrowsers are the executables tried, in order, when browserEnv is
// unset: macOS application bundles, then names on PATH.
var wellKnownBrowsers = []string{
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
	"google-chrome",
	"google-chrome-stable",
	"chromium",
	"chromium-browser",
}

// TestRealBrowserEndsWithItsTest starts the browser from a helper process,
// this test binary run as TestRealBrowserHelper, ends the helper with SIGINT,
// SIGTERM, SIGKILL or its own test timeout's panic, and checks within a
// bounded wait that no process of the browser remains. After SIGINT or
// SIGTERM the helper has also removed the profile; after SIGKILL or the
// panic the browser ended by itself when its pipe closed, and the profile is
// left for this test's temporary directory to remove.
func TestRealBrowserEndsWithItsTest(t *testing.T) {
	executable := findBrowser(t)
	cases := []struct {
		name string
		// signal ends the helper; without one, the helper's -test.timeout
		// of timeout ends it with a panic and exit status 2.
		signal  syscall.Signal
		timeout time.Duration
		// profileRemoved says the helper removes the profile before it ends.
		profileRemoved bool
	}{
		{name: "interrupt", signal: syscall.SIGINT, profileRemoved: true},
		{name: "terminate", signal: syscall.SIGTERM, profileRemoved: true},
		{name: "kill", signal: syscall.SIGKILL},
		{name: "test timeout", timeout: 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile := filepath.Join(t.TempDir(), "profile")
			helper, pid := startBrowserHelper(t, executable, profile, tc.timeout)
			running := false
			for _, p := range listProcesses(t) {
				running = running || p.pid == pid
			}
			require.True(t, running, "the helper's browser %d is not running", pid)
			ending := time.Now()
			if tc.signal != 0 {
				require.NoError(t, helper.cmd.Process.Signal(tc.signal))
			}
			select {
			case <-helper.done:
			case <-time.After(tc.timeout + browserDeadline):
				t.Fatalf("the helper did not end within %s", tc.timeout+browserDeadline)
			}
			require.NoError(t, errors.Join(helper.scanErr, ignoreExit(helper.cmd.Wait())))
			status, ok := helper.cmd.ProcessState.Sys().(syscall.WaitStatus)
			require.True(t, ok, "the helper's wait status")
			if tc.signal != 0 {
				assert.True(t, status.Signaled() && status.Signal() == tc.signal,
					"the helper did not end by %v: %s\n%s%s", tc.signal, helper.cmd.ProcessState, helper.stdout.String(), helper.stderr.String())
			} else {
				assert.True(t, status.Exited() && status.ExitStatus() == 2 && strings.Contains(helper.stdout.String()+helper.stderr.String(), "test timed out"),
					"the helper did not end by its test timeout: %s\n%s%s", helper.cmd.ProcessState, helper.stdout.String(), helper.stderr.String())
			}

			helperEnded := time.Since(ending)
			left := waitForNoBrowser(t, pid, profile)
			assert.Empty(t, left, "processes of the browser outlived the helper")
			t.Logf("%s: the helper ended after %s, the browser's last process after %s",
				tc.name, helperEnded.Round(time.Millisecond), time.Since(ending).Round(time.Millisecond))
			for _, p := range left {
				if err := syscall.Kill(p.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					t.Errorf("ending process %d: %v", p.pid, err)
				}
			}
			if tc.profileRemoved {
				assert.NoDirExists(t, profile, "the helper did not remove the browser's profile")
			} else {
				assert.DirExists(t, profile, "the killed helper's profile; this test's temporary directory removes it")
			}
		})
	}
}

// TestRealBrowserHelper starts a browser on the profile helperProfileEnv
// names, prints its process ID, and waits to be ended by
// TestRealBrowserEndsWithItsTest. It skips when run on its own.
func TestRealBrowserHelper(t *testing.T) {
	profile := os.Getenv(helperProfileEnv)
	if profile == "" {
		t.Skip("runs only as the helper process of TestRealBrowserEndsWithItsTest")
	}
	b := startBrowser(t, findBrowser(t), profile)
	_, err := fmt.Printf("%s%d\n", helperReady, b.cmd.Process.Pid)
	require.NoError(t, err)
	// The browser's end is not awaited here: on a signal it ends while the
	// signal handler still has the profile to remove.
	time.Sleep(browserDeadline)
	t.Fatalf("the helper was not ended within %s", browserDeadline)
}

// findBrowser returns the browser executable named by browserEnv, or the
// first well-known one installed, and skips the test when there is none.
func findBrowser(t *testing.T) string {
	t.Helper()
	if explicit := os.Getenv(browserEnv); explicit != "" {
		info, err := os.Stat(explicit)
		require.NoError(t, err, "%s names a browser that does not exist", browserEnv)
		require.False(t, info.IsDir(), "%s must name the browser's executable, not a directory", browserEnv)
		return explicit
	}
	for _, candidate := range wellKnownBrowsers {
		if filepath.IsAbs(candidate) {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
			continue
		}
		if found, err := exec.LookPath(candidate); err == nil {
			return found
		}
	}
	t.Skipf("no Chromium-family browser found; install Google Chrome or Chromium, or set %s to its executable", browserEnv)
	return ""
}

// startBrowser starts executable headless on profile, a directory the
// caller owns and the browser creates, and returns once the browser printed
// its DevTools address. The browser leads its own process group so the test
// can find and end everything it starts. When the test ends before its
// cleanup subtest ran, for example under -run or after a failure, the same
// shutdown and checks run as a cleanup.
func startBrowser(t *testing.T, executable, profile string) *realBrowser {
	t.Helper()
	commands, lifeline, err := os.Pipe()
	require.NoError(t, err)
	replies, replyEnd, err := os.Pipe()
	require.NoError(t, err)
	output := &browserOutput{devtools: make(chan string, 1)}
	cmd := exec.Command(executable,
		"--headless=new",
		"--remote-debugging-port=0",
		"--remote-debugging-pipe",
		"--user-data-dir="+profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--use-mock-keychain",
		"--password-store=basic",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-extensions",
		"--disable-sync",
		"--disable-updater-scheduler",
		"--disable-breakpad",
		"--mute-audio",
		"about:blank",
	)
	cmd.Stdout = output
	cmd.Stderr = output
	// The browser reads its DevTools pipe on descriptor 3 and writes on 4.
	cmd.ExtraFiles = []*os.File{commands, replyEnd}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	startErr := cmd.Start()
	// The browser holds its own copies of its pipe ends from here on.
	require.NoError(t, errors.Join(startErr, commands.Close(), replyEnd.Close()))
	b := &realBrowser{
		cmd:           cmd,
		profile:       profile,
		output:        output,
		closeLifeline: sync.OnceValue(lifeline.Close),
		exited:        make(chan struct{}),
		signals:       make(chan os.Signal, 1),
		watching:      make(chan struct{}),
	}
	b.stopWatching = sync.OnceFunc(func() {
		signal.Stop(b.signals)
		close(b.watching)
	})
	signal.Notify(b.signals, os.Interrupt, syscall.SIGTERM)
	go b.endOnSignal()
	go func() {
		b.waitErr = cmd.Wait()
		close(b.exited)
	}()
	go output.drain(replies)
	t.Cleanup(func() {
		if !b.ended() {
			b.stopAndCheck(t)
		}
		b.stopWatching()
	})
	select {
	case b.devtools = <-output.devtools:
	case <-b.exited:
		t.Fatalf("the browser exited before it listened for DevTools: %v\n%s", b.waitErr, output.String())
	case <-time.After(browserDeadline):
		t.Fatalf("the browser printed no DevTools address within %s:\n%s", browserDeadline, output.String())
	}
	return b
}

// startBrowserHelper runs this test binary as TestRealBrowserHelper with a
// browser on profile and returns it with its browser's process ID once the
// browser listens. A non-zero timeout becomes the helper's -test.timeout.
func startBrowserHelper(t *testing.T, executable, profile string, timeout time.Duration) (*browserHelper, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*browserDeadline)
	t.Cleanup(cancel)
	h := &browserHelper{done: make(chan struct{})}
	args := []string{"-test.run=^TestRealBrowserHelper$"}
	if timeout > 0 {
		args = append(args, "-test.timeout="+timeout.String())
	}
	h.cmd = exec.CommandContext(ctx, os.Args[0], args...)
	h.cmd.Env = append(os.Environ(), helperProfileEnv+"="+profile, browserEnv+"="+executable)
	h.cmd.Stderr = &h.stderr
	stdout, err := h.cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, h.cmd.Start())
	ready := make(chan int, 1)
	go func() {
		defer close(h.done)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			h.stdout.WriteString(scanner.Text() + "\n")
			if pid, ok := strings.CutPrefix(scanner.Text(), helperReady); ok {
				n, err := strconv.Atoi(pid)
				if err != nil {
					h.scanErr = err
					continue
				}
				ready <- n
			}
		}
		h.scanErr = errors.Join(h.scanErr, scanner.Err())
	}()
	select {
	case pid := <-ready:
		return h, pid
	case <-h.done:
		t.Fatalf("the helper ended before its browser listened: %v\n%s%s", ignoreExit(h.cmd.Wait()), h.stdout.String(), h.stderr.String())
	case <-time.After(browserDeadline):
		t.Fatalf("the helper's browser did not listen within %s", browserDeadline)
	}
	return nil, 0
}

// waitForNoBrowser waits up to browserDeadline until no process is the
// browser pid, in its process group, or names profile, and returns those
// still running at the deadline.
func waitForNoBrowser(t *testing.T, pid int, profile string) []listedProcess {
	t.Helper()
	deadline := time.Now().Add(browserDeadline)
	for {
		var left []listedProcess
		for _, p := range listProcesses(t) {
			if ofBrowser(p, pid, profile) {
				left = append(left, p)
			}
		}
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ofBrowser reports whether p is the browser pid, is in its process group,
// or names its profile.
func ofBrowser(p listedProcess, pid int, profile string) bool {
	if p.pid == pid || p.pgid == pid {
		return true
	}
	resolved, err := filepath.EvalSymlinks(profile)
	if err != nil {
		resolved = profile
	}
	return strings.Contains(p.command, profile) || strings.Contains(p.command, resolved)
}

// ignoreExit returns err unless it only reports how a process exited.
func ignoreExit(err error) error {
	if errors.As(err, new(*exec.ExitError)) {
		return nil
	}
	return err
}

// listProcesses lists every process on the machine with ps, which macOS and
// Linux both provide.
func listProcesses(t *testing.T) []listedProcess {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-ww", "-o", "pid=,ppid=,pgid=,command=").Output()
	require.NoError(t, err)
	var processes []listedProcess
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		var numbers [3]int
		for i := range numbers {
			numbers[i], err = strconv.Atoi(fields[i])
			require.NoError(t, err, "ps line %q", line)
		}
		processes = append(processes, listedProcess{pid: numbers[0], ppid: numbers[1], pgid: numbers[2], command: strings.Join(fields[3:], " ")})
	}
	return processes
}

// truncate shortens s to at most n bytes for logs.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Write keeps the end of the browser's output and hands over the first
// "DevTools listening on <address>" line's address.
func (o *browserOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tail = append(o.tail, p...)
	if len(o.tail) > maxBrowserOutput {
		o.tail = o.tail[len(o.tail)-maxBrowserOutput:]
	}
	if o.found {
		return len(p), nil
	}
	o.line = append(o.line, p...)
	for {
		end := bytes.IndexByte(o.line, '\n')
		if end < 0 {
			break
		}
		line := strings.TrimSpace(string(o.line[:end]))
		o.line = o.line[end+1:]
		if address, ok := strings.CutPrefix(line, "DevTools listening on "); ok {
			o.found = true
			o.line = nil
			o.devtools <- address
			break
		}
	}
	return len(p), nil
}

// String returns the kept end of the browser's output.
func (o *browserOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.tail)
}

// drain reads and discards what the browser writes on its DevTools pipe
// until the browser closes it, so the browser never blocks writing there,
// and keeps any failure with the browser's output.
func (o *browserOutput) drain(replies *os.File) {
	_, copyErr := io.Copy(io.Discard, replies)
	if err := errors.Join(copyErr, replies.Close()); err != nil {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.tail = append(o.tail, []byte("reading the DevTools pipe: "+err.Error()+"\n")...)
	}
}

// stopAndCheck closes the browser's pipe, as the test process ending would,
// waits for the browser to shut down, and checks that it used only the
// test's profile and that no process it started is left. A leftover process
// in the browser's group or naming its profile is ended; any other leftover,
// such as a system updater the browser woke, is only reported.
func (b *realBrowser) stopAndCheck(t *testing.T) {
	defer b.stopWatching()
	tempRoot, err := filepath.EvalSymlinks(os.TempDir())
	require.NoError(t, err)
	profile, err := filepath.EvalSymlinks(b.profile)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(profile, tempRoot+string(filepath.Separator)), "the profile %s is not inside the test's temporary directory", profile)
	active, err := os.ReadFile(filepath.Join(b.profile, "DevToolsActivePort"))
	require.NoError(t, err, "the browser did not write its state into the throwaway profile")
	devtools, err := url.Parse(b.devtools)
	require.NoError(t, err)
	assert.Equal(t, devtools.Port(), strings.SplitN(string(active), "\n", 2)[0], "the profile's DevTools port")

	started := b.processes(t)
	require.NotEmpty(t, started, "the browser process is not listed")
	for _, p := range started {
		t.Logf("browser process %d (group %d): %s", p.pid, p.pgid, truncate(p.command, 160))
	}
	require.NoError(t, b.shutdown(), "the browser's output:\n%s", b.output.String())
	t.Logf("browser exit: %s", b.cmd.ProcessState)
	assert.True(t, b.cmd.ProcessState.Exited(), "the browser did not shut down by itself: %s", b.cmd.ProcessState)
	if errors.Is(b.waitErr, exec.ErrWaitDelay) {
		t.Logf("a process the browser started kept its output open after the browser exited; it is checked below")
	} else if err := ignoreExit(b.waitErr); err != nil {
		t.Errorf("waiting for the browser: %v", err)
	}

	deadline := time.Now().Add(browserDeadline)
	var left []listedProcess
	for {
		left = b.leftover(t, started)
		if len(left) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, p := range left {
		t.Errorf("process %d (group %d) outlived the browser: %s", p.pid, p.pgid, truncate(p.command, 160))
		if !b.belongs(p) {
			continue
		}
		if err := syscall.Kill(p.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("ending process %d: %v", p.pid, err)
		}
	}
}

// shutdown closes the browser's pipe and waits for the browser to exit. If
// it has not exited within browserDeadline, shutdown ends the browser's
// process group and waits again. It reports any step that failed.
func (b *realBrowser) shutdown() error {
	if err := b.closeLifeline(); err != nil {
		return fmt.Errorf("closing the browser's pipe: %w", err)
	}
	select {
	case <-b.exited:
		return nil
	case <-time.After(browserDeadline):
	}
	failure := fmt.Errorf("the browser did not exit within %s of its pipe closing", browserDeadline)
	if err := syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		failure = errors.Join(failure, fmt.Errorf("ending its process group: %w", err))
	}
	select {
	case <-b.exited:
	case <-time.After(browserDeadline):
		failure = errors.Join(failure, fmt.Errorf("the browser %d did not exit after SIGKILL", b.cmd.Process.Pid))
	}
	return failure
}

// endOnSignal waits for SIGINT or SIGTERM while the browser runs. On one,
// it ends the browser and removes its profile, since an ended test runs no
// cleanup, then restores the signal's default action and sends the signal
// to this process again, so it ends the test as it would have.
func (b *realBrowser) endOnSignal() {
	var sig os.Signal
	select {
	case <-b.watching:
		return
	case sig = <-b.signals:
	}
	if err := errors.Join(b.shutdown(), os.RemoveAll(b.profile)); err != nil {
		fmt.Fprintf(os.Stderr, "ending the browser on %v: %v\n", sig, err)
	}
	signal.Reset(os.Interrupt, syscall.SIGTERM)
	if err := syscall.Kill(os.Getpid(), sig.(syscall.Signal)); err != nil {
		fmt.Fprintf(os.Stderr, "ending the test on %v: %v\n", sig, err)
		os.Exit(1)
	}
}

// processes lists the browser and every process it started: its
// descendants, its process group, and anything naming its profile.
func (b *realBrowser) processes(t *testing.T) []listedProcess {
	all := listProcesses(t)
	root := b.cmd.Process.Pid
	family := map[int]bool{root: true}
	for grew := true; grew; {
		grew = false
		for _, p := range all {
			if family[p.ppid] && !family[p.pid] {
				family[p.pid] = true
				grew = true
			}
		}
	}
	var out []listedProcess
	for _, p := range all {
		if family[p.pid] || b.belongs(p) {
			out = append(out, p)
		}
	}
	return out
}

// leftover returns the processes of started still running, and any other
// process that belongs to the browser.
func (b *realBrowser) leftover(t *testing.T, started []listedProcess) []listedProcess {
	alive := map[int]string{}
	for _, p := range started {
		alive[p.pid] = p.command
	}
	var out []listedProcess
	for _, p := range listProcesses(t) {
		if command, ok := alive[p.pid]; ok && command == p.command || b.belongs(p) {
			out = append(out, p)
		}
	}
	return out
}

// belongs reports whether p is in the browser's process group or names the
// browser's profile.
func (b *realBrowser) belongs(p listedProcess) bool {
	return ofBrowser(p, b.cmd.Process.Pid, b.profile)
}

// ended reports whether the browser process has exited and was waited for.
func (b *realBrowser) ended() bool {
	select {
	case <-b.exited:
		return true
	default:
		return false
	}
}
