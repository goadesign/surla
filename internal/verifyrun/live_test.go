package verifyrun

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/verify/suite"
)

const (
	// liveFakeEnv selects what the fake live preparation and suite do:
	//   - "ready": prepare succeeds and the suite starts a child, prints the
	//     ready line and serves;
	//   - "prepare-fails": prepare returns a StageError;
	//   - "prepare-hangs": prepare starts a child, as a build does, and blocks;
	//   - "suite-hangs": the suite starts a child and never prints the line;
	//   - "suite-exits": the suite exits before printing the line;
	//   - "target-ends": the suite prints the line and exits 2s later.
	// Each started process records "pgid pid" lines in TMPDIR/pids.
	liveFakeEnv = "VERIFYRUN_TEST_LIVE"
	// liveHostReady is printed by the fake host once StartLive returned.
	liveHostReady = "host-ready"
)

// fakePrepare stands in for verify.Prepare: it returns this test binary as the
// suite, which then acts as fakeSuiteLive.
func fakePrepare(ctx context.Context, _ verify.Plan) (verify.Prepared, error) {
	switch os.Getenv(liveFakeEnv) {
	case "prepare-fails":
		return verify.Prepared{}, &verify.StageError{Stage: verify.StageBuild, Summary: "The candidate did not build.",
			Err: errors.New("compile error in /private/secret/path")}
	case "prepare-hangs":
		if err := startRecordedChild(); err != nil {
			return verify.Prepared{}, err
		}
		<-ctx.Done()
		return verify.Prepared{}, ctx.Err()
	}
	self, err := os.Executable()
	if err != nil {
		return verify.Prepared{}, err
	}
	return verify.Prepared{SuiteBin: self, ArtifactFlags: []string{"--artifact", "app=/nonexistent"}}, nil
}

// fakeSuiteLive acts as `<suite> --live`: it checks that the key arrived
// through the environment, starts a child in its group as a candidate would
// be, and behaves according to liveFakeEnv.
func fakeSuiteLive() int {
	if os.Getenv(suite.LiveKeyEnv) != "secret-key" {
		fmt.Fprintln(os.Stderr, "missing live key")
		return 2
	}
	mode := os.Getenv(liveFakeEnv)
	if mode == "suite-exits" {
		return 2
	}
	if err := startRecordedChild(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if mode == "suite-hangs" {
		time.Sleep(time.Hour)
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"url": "http://127.0.0.1:4100"}); err != nil {
		return 2
	}
	if mode == "target-ends" {
		time.Sleep(2 * time.Second)
		return 2
	}
	time.Sleep(time.Hour)
	return 0
}

// liveHost acts as `surla serve`: it starts a live target under root, prints
// liveHostReady and the target's group once ready, and then blocks until it
// is killed. A start error is printed and ends it.
func liveHost(root string) int {
	self, err := os.Executable()
	if err != nil {
		return 2
	}
	p, err := StartLive(context.Background(), root, verify.Plan{}, "secret-key", []string{self}, os.Environ())
	if err != nil {
		fmt.Println("start-error", err)
		return 2
	}
	fmt.Println(liveHostReady, p.pgid)
	time.Sleep(time.Hour)
	return 0
}

// startRecordedChild starts a long sleep in the caller's process group and
// records the caller's and the child's group and PID in TMPDIR/pids.
func startRecordedChild() error {
	child := exec.Command("sleep", "300")
	if err := child.Start(); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(os.TempDir(), "pids"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "%d %d\n%d %d\n", syscall.Getpgrp(), os.Getpid(), syscall.Getpgrp(), child.Process.Pid)
	return errors.Join(err, f.Close())
}

// recordedPIDs returns the processes recorded under the live directory's
// TMPDIR, waiting until at least want are recorded.
func recordedPIDs(t *testing.T, dir string, want int) (pgid int, pids []int) {
	t.Helper()
	path := filepath.Join(dir, liveTmpDir, "pids")
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(path)
		return err == nil && strings.Count(string(b), "\n") >= want
	}, 30*time.Second, 20*time.Millisecond, "processes were not recorded")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var g, p int
		_, err := fmt.Sscan(line, &g, &p)
		require.NoError(t, err)
		pgid = g
		pids = append(pids, p)
	}
	return pgid, pids
}

// requireGroupGone waits until the process group no longer exists.
func requireGroupGone(t *testing.T, pgid int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
	}, 30*time.Second, 20*time.Millisecond, "process group %d still has processes", pgid)
}

// onlyLiveDir returns the single live directory under root.
func onlyLiveDir(t *testing.T, root string) string {
	t.Helper()
	var dirs []string
	require.Eventually(t, func() bool {
		var err error
		dirs, err = LeftoverLiveDirs(root)
		return err == nil && len(dirs) == 1
	}, 30*time.Second, 20*time.Millisecond)
	return dirs[0]
}

func TestStartLiveServesUntilStopAndThenRemovesEverything(t *testing.T) {
	t.Setenv(liveFakeEnv, "ready")
	root := t.TempDir()
	self, err := os.Executable()
	require.NoError(t, err)

	p, err := StartLive(t.Context(), root, verify.Plan{}, "secret-key", []string{self}, os.Environ())
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:4100", p.URL)
	pgid, pids := recordedPIDs(t, p.Dir(), 2)
	assert.Equal(t, p.pgid, pgid, "the suite and its child are in the executor's group")
	assert.Len(t, pids, 2)
	var req LiveRequest
	require.NoError(t, decodeLiveRequest(p.Dir(), &req))
	assert.Equal(t, filepath.Join(p.Dir(), liveWorkDir), req.Plan.WorkDir)
	raw, err := os.ReadFile(filepath.Join(p.Dir(), liveRequestFile))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "secret-key", "the key is never written to disk")

	require.NoError(t, p.Stop())
	requireGroupGone(t, pgid)
	assert.NoDirExists(t, p.Dir())
	require.NoError(t, p.Stop(), "Stop is idempotent")
	dirs, err := LeftoverLiveDirs(root)
	require.NoError(t, err)
	assert.Empty(t, dirs)
}

func TestStartLiveReportsAndCleansUpEveryFailedStart(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	cases := []struct {
		mode    string
		key     string
		wantErr string
	}{
		{mode: "prepare-fails", key: "secret-key", wantErr: "The candidate did not build."},
		{mode: "suite-exits", key: "secret-key", wantErr: summaryLiveSuite},
		{mode: "ready", key: "", wantErr: "live target key is required"},
	}
	for _, tc := range cases {
		t.Run(tc.mode+"/"+tc.wantErr, func(t *testing.T) {
			t.Setenv(liveFakeEnv, tc.mode)
			root := t.TempDir()
			_, err := StartLive(t.Context(), root, verify.Plan{}, tc.key, []string{self}, os.Environ())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), "/private/secret/path", "only the safe summary reaches the caller")
			dirs, err := LeftoverLiveDirs(root)
			require.NoError(t, err)
			assert.Empty(t, dirs)
		})
	}
}

func TestStartLiveStopsAStartThatIsCanceled(t *testing.T) {
	for _, mode := range []string{"prepare-hangs", "suite-hangs"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(liveFakeEnv, mode)
			root := t.TempDir()
			self, err := os.Executable()
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			errs := make(chan error, 1)
			go func() {
				_, err := StartLive(ctx, root, verify.Plan{}, "secret-key", []string{self}, os.Environ())
				errs <- err
			}()
			dir := onlyLiveDir(t, root)
			pgid, _ := recordedPIDs(t, dir, 2)
			cancel()
			err = <-errs
			require.ErrorIs(t, err, context.Canceled)
			requireGroupGone(t, pgid)
			assert.NoDirExists(t, dir)
		})
	}
}

func TestLiveTargetThatEndsIsReportedDone(t *testing.T) {
	t.Setenv(liveFakeEnv, "target-ends")
	root := t.TempDir()
	self, err := os.Executable()
	require.NoError(t, err)
	p, err := StartLive(t.Context(), root, verify.Plan{}, "secret-key", []string{self}, os.Environ())
	require.NoError(t, err)
	pgid, _ := recordedPIDs(t, p.Dir(), 2)
	select {
	case <-p.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("an ended target was not reported done")
	}
	require.NoError(t, p.Stop())
	requireGroupGone(t, pgid)
	assert.NoDirExists(t, p.Dir())
}

func TestLiveTargetDiesWithItsHostAtEveryStage(t *testing.T) {
	cases := []struct {
		mode string
		// ready reports whether the host sees the target ready before it is
		// killed.
		ready bool
	}{
		{mode: "prepare-hangs"},
		{mode: "suite-hangs"},
		{mode: "ready", ready: true},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv(liveFakeEnv, tc.mode)
			root := t.TempDir()
			self, err := os.Executable()
			require.NoError(t, err)
			host := exec.Command(self, "live-host", root)
			out, err := host.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, host.Start())
			t.Cleanup(func() {
				if err := host.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					t.Errorf("kill host: %v", err)
				}
				if err := host.Wait(); err == nil {
					t.Error("the killed host exited cleanly")
				}
			})
			if tc.ready {
				line, err := bufio.NewReader(out).ReadString('\n')
				require.NoError(t, err)
				require.True(t, strings.HasPrefix(line, liveHostReady), line)
			}
			dir := onlyLiveDir(t, root)
			pgid, pids := recordedPIDs(t, dir, 2)
			require.NoError(t, host.Process.Signal(syscall.SIGKILL))
			requireGroupGone(t, pgid)
			for _, pid := range pids {
				assert.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH, "process %d outlived its host", pid)
			}
			dirs, err := LeftoverLiveDirs(root)
			require.NoError(t, err)
			assert.Equal(t, []string{dir}, dirs, "the directory is left for the operator and reported")
			assert.FileExists(t, filepath.Join(dir, liveTmpDir, "pids"), "every file of the session is under the directory")
		})
	}
}

func TestLiveEnvReplacesTMPDIRAndKey(t *testing.T) {
	got := liveEnv([]string{"PATH=/bin", "TMPDIR=/tmp", suite.LiveKeyEnv + "=old", "HOME=/h"})
	assert.Equal(t, []string{"PATH=/bin", "HOME=/h"}, got)
}
