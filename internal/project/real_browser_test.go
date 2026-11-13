//go:build browser && (darwin || linux)

// This file starts and ends the headless browser that views_browser_test.go
// drives: it finds an installed Chrome or Chromium, launches it on a
// throwaway profile inside the test's temporary directory, reads the
// DevTools address it prints, and at the end shuts it down and checks that
// no process it started is left.
package project

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
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
		// exited is closed once cmd.Wait returned waitErr.
		exited  chan struct{}
		waitErr error
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

// startBrowser starts executable headless on a new profile inside the test's
// temporary directory and returns once the browser printed its DevTools
// address. The browser leads its own process group so the test can find and
// end everything it starts. When the test ends before its cleanup subtest
// ran, for example under -run or after a failure, the same shutdown and
// checks run as a cleanup.
func startBrowser(t *testing.T, executable string) *realBrowser {
	t.Helper()
	profile := t.TempDir()
	output := &browserOutput{devtools: make(chan string, 1)}
	cmd := exec.Command(executable,
		"--headless=new",
		"--remote-debugging-port=0",
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	require.NoError(t, cmd.Start())
	b := &realBrowser{cmd: cmd, profile: profile, output: output, exited: make(chan struct{})}
	go func() {
		b.waitErr = cmd.Wait()
		close(b.exited)
	}()
	t.Cleanup(func() {
		if !b.ended() {
			b.stopAndCheck(t)
		}
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

// stopAndCheck asks the browser to shut down with SIGTERM, waits for it,
// and checks that it used only the test's profile and that no process it
// started is left. A leftover process in the browser's group or naming its
// profile is ended; any other leftover, such as a system updater the browser
// woke, is only reported.
func (b *realBrowser) stopAndCheck(t *testing.T) {
	pid := b.cmd.Process.Pid
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
	require.NoError(t, syscall.Kill(pid, syscall.SIGTERM))
	select {
	case <-b.exited:
	case <-time.After(browserDeadline):
		b.kill(t)
		t.Fatalf("the browser did not exit within %s of SIGTERM:\n%s", browserDeadline, b.output.String())
	}
	t.Logf("browser exit: %s", b.cmd.ProcessState)
	assert.True(t, b.cmd.ProcessState.Exited(), "the browser did not shut down by itself: %s", b.cmd.ProcessState)
	if errors.Is(b.waitErr, exec.ErrWaitDelay) {
		t.Logf("a process the browser started kept its output open after the browser exited; it is checked below")
	} else if b.waitErr != nil && !errors.As(b.waitErr, new(*exec.ExitError)) {
		t.Errorf("waiting for the browser: %v", b.waitErr)
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
	if p.pgid == b.cmd.Process.Pid {
		return true
	}
	resolved, err := filepath.EvalSymlinks(b.profile)
	if err != nil {
		resolved = b.profile
	}
	return strings.Contains(p.command, b.profile) || strings.Contains(p.command, resolved)
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

// kill ends the browser's whole process group when it did not shut down
// when asked, so a failed test leaves no browser behind.
func (b *realBrowser) kill(t *testing.T) {
	if b.ended() {
		return
	}
	t.Errorf("the browser did not shut down; ending its process group")
	if err := syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Errorf("ending the browser's process group: %v", err)
	}
	select {
	case <-b.exited:
	case <-time.After(browserDeadline):
		t.Errorf("the browser %d did not exit after SIGKILL", b.cmd.Process.Pid)
	}
}
