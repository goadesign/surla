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
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/modelsettings"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/verify/suite"
)

const (
	// liveFakeEnv selects what the fake live preparation and suite do:
	//   - "ready": prepare succeeds and the suite starts a child, prints the
	//     ready line and serves;
	//   - "prepare-fails": prepare returns a StageError;
	//   - "prepare-fails-child": prepare starts a child, as a build does, and
	//     then returns a StageError;
	//   - "prepare-hangs": prepare starts a child, as a build does, and blocks;
	//   - "suite-hangs": the suite starts a child and never prints the line;
	//   - "suite-exits": the suite exits before printing the line;
	//   - "suite-bad-line": the suite starts a child, prints a diagnostic
	//     instead of the ready line and keeps serving;
	//   - "target-ends": the suite prints the line and exits 1s later,
	//     leaving its child running, as a candidate a suite did not stop;
	//   - "leak": the suite prints every provider variable it was given to
	//     stderr and a first line whose unknown field is its anthropic key,
	//     then waits to be killed.
	// Each started process records "pgid pid" lines in TMPDIR/pids. Prepare
	// and the suite always report, by names and booleans only, which
	// provider variables they see (writeSettingsReport).
	liveFakeEnv = "VERIFYRUN_TEST_LIVE"
	// liveHostReady is printed by the fake host once its live target was ready, and
	// liveHostEnded once the target reported that it ended.
	liveHostReady = "host-ready"
	liveHostEnded = "host-ended"
)

// fakePrepare stands in for verify.Prepare: it returns this test binary as the
// suite, which then acts as fakeSuiteLive.
func fakePrepare(ctx context.Context, _ verify.Plan) (verify.Prepared, error) {
	if err := writeSettingsReport(filepath.Join(os.TempDir(), "prepare-report"), os.Environ()); err != nil {
		return verify.Prepared{}, err
	}
	switch os.Getenv(liveFakeEnv) {
	case "prepare-fails-child":
		if err := startRecordedChild(); err != nil {
			return verify.Prepared{}, err
		}
		return verify.Prepared{}, &verify.StageError{Stage: verify.StageBuild, Summary: "The candidate did not build.",
			Err: errors.New("compile error")}
	case "prepare-fails":
		return verify.Prepared{}, &verify.StageError{Stage: verify.StageBuild, Summary: "The candidate did not build.",
			Err: errors.New("compile error in /private/secret/path")}
	case "prepare-fails-exit71":
		return verify.Prepared{}, &verify.StageError{
			Stage:   verify.StageBuild,
			Summary: "The sandboxed command exited with status 71. If Surla is running inside a restricted coding-agent shell, its sandbox may have been blocked; run Surla from an unrestricted host shell.",
			Err:     errors.New("build candidate app: exit status 71 in /private/secret/path"),
		}
	case "prepare-hangs":
		if err := startRecordedChild(); err != nil {
			return verify.Prepared{}, err
		}
		fmt.Fprintln(os.Stderr, "preparing: downloading modules")
		<-ctx.Done()
		return verify.Prepared{}, ctx.Err()
	}
	self, err := os.Executable()
	if err != nil {
		return verify.Prepared{}, err
	}
	return verify.Prepared{SuiteBin: self, ArtifactFlags: []string{"--artifact", "app=/nonexistent"}}, nil
}

// fakeSuiteLive acts as `<suite> --live`: it writes the environment it was
// started with to TMPDIR/suite-env, checks that the key arrived through the
// environment, starts a child in its group as a candidate would be, and
// behaves according to liveFakeEnv, which a test program agents wrote does
// not receive, so it then serves.
func fakeSuiteLive() int {
	env := strings.Join(os.Environ(), "\n") + "\n"
	block, err := parentStartingBlock(os.Getppid())
	if err != nil {
		fmt.Fprintln(os.Stderr, "read parent starting block:", err)
		return 1
	}
	parentFamilies := map[string]bool{}
	for _, provider := range modelsettings.Providers() {
		parentFamilies[provider.Prefix] = strings.Contains(string(block), provider.Prefix)
	}
	encoded, err := json.Marshal(parentFamilies)
	if err != nil {
		return 1
	}
	if err := os.WriteFile(filepath.Join(os.TempDir(), "parent-families.json"), encoded, 0o600); err != nil {
		return 1
	}
	if err := os.WriteFile(filepath.Join(os.TempDir(), "suite-env"), []byte(env), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := writeSettingsReport(filepath.Join(os.TempDir(), "suite-report"), os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if os.Getenv(suite.LiveKeyEnv) != "secret-key" {
		fmt.Fprintln(os.Stderr, "missing live key")
		return 2
	}
	mode := os.Getenv(liveFakeEnv)
	switch mode {
	case "leak":
		for _, kv := range os.Environ() {
			if name, _, _ := strings.Cut(kv, "="); modelsettings.InFamily(name) {
				fmt.Fprintln(os.Stderr, "suite configuration:", kv)
			}
		}
		fmt.Printf("{%q:true}\n", os.Getenv("ANTHROPIC_API_KEY"))
		time.Sleep(time.Hour)
		return 0
	case "suite-exits":
		return 2
	case "suite-unsupported":
		fmt.Fprintln(os.Stderr, "this evaluator suite does not support --live: its suite.Command sets no Live function")
		return 2
	case "suite-provider":
		fmt.Fprintln(os.Stderr, "start live target: model gateway: ANTHROPIC_API_KEY is not set")
		return 2
	}
	// Suite setup can change its directory before it uses TMPDIR again.
	if err := os.Chdir(os.TempDir()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := startRecordedChild(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if mode == "suite-bad-line" {
		fmt.Println("starting gateway...")
		time.Sleep(time.Hour)
		return 0
	}
	if mode == "suite-hangs" {
		time.Sleep(time.Hour)
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"url": "http://127.0.0.1:4100"}); err != nil {
		return 2
	}
	if mode == "target-ends" {
		time.Sleep(time.Second)
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
	p, err := startLive(context.Background(), root, evaluatorPlan, "secret-key", []string{self}, os.Environ())
	if err != nil {
		fmt.Println("start-error", err)
		return 2
	}
	fmt.Println(liveHostReady, p.pgid)
	<-p.Done()
	fmt.Println(liveHostEnded)
	time.Sleep(time.Hour)
	return 0
}

// startLive launches a live target without provider settings and waits until
// it is ready, stopping it when it does not become ready, as an owner of
// LaunchLive's process must.
func startLive(ctx context.Context, root string, plan verify.Plan, key string, command, env []string) (*LiveProcess, error) {
	return startLiveWith(ctx, root, plan, key, command, env)
}

// startLiveWith is startLive giving the target settings.
func startLiveWith(
	ctx context.Context,
	root string,
	plan verify.Plan,
	key string,
	command, env []string,
	settings ...modelsettings.Settings,
) (*LiveProcess, error) {
	p, err := LaunchLive(root, plan, key, command, env, settings)
	if err != nil {
		return nil, err
	}
	if err := p.AwaitReady(ctx); err != nil {
		return nil, errors.Join(err, p.Stop())
	}
	return p, nil
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

// TestLiveGivesTheTestProgramOnlyItsSourcesEnvironment starts a live copy
// from a server environment holding a variable of its own, inherited
// provider family values and the chosen direct provider, with the chosen
// provider's handed-over settings, and reads the environment the test
// program started with. No inherited family value reaches it. A program
// agents wrote sees only PATH, its TMPDIR, the live key and the gateway,
// with no provider family in its own or its parent's starting block. The
// operator's evaluator also sees the server's variable and chosen settings.
func TestLiveGivesTheTestProgramOnlyItsSourcesEnvironment(t *testing.T) {
	cases := []struct {
		name       string
		tests      verify.TestsSource
		sees       []string
		doesNotSee []string
	}{
		{
			name: "operator's evaluator", tests: verify.TestsEvaluator,
			sees: []string{
				"SURLA_TEST_SERVER_VAR=present", "ANTHROPIC_API_KEY=ant-k", "ANTHROPIC_BASE_URL=https://gateway.invalid",
				suite.LiveKeyEnv + "=secret-key",
			},
			doesNotSee: []string{"OPENAI_API_KEY", "ANTHROPIC_ADMIN_KEY"},
		},
		{
			name: "test module agents wrote", tests: verify.TestsWritten,
			sees: []string{
				"PATH=" + os.Getenv("PATH"), suite.LiveKeyEnv + "=secret-key",
			},
			doesNotSee: []string{"SURLA_TEST_SERVER_VAR", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_ADMIN_KEY", verify.LiveModelProviderEnv, verify.LiveModelNameEnv, liveFakeEnv, "HOME"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(liveFakeEnv, "ready")
			root := t.TempDir()
			self, err := os.Executable()
			require.NoError(t, err)
			env := append(os.Environ(),
				"SURLA_TEST_SERVER_VAR=present", "ANTHROPIC_API_KEY=inherited-k", "ANTHROPIC_ADMIN_KEY=inherited-admin", "OPENAI_API_KEY=oai-k",
				verify.LiveModelProviderEnv+"=anthropic", verify.LiveModelNameEnv+"=claude",
			)
			anthropic, err := modelsettings.New("anthropic", map[string]string{"ANTHROPIC_API_KEY": "ant-k", "ANTHROPIC_BASE_URL": "https://gateway.invalid"})
			require.NoError(t, err)

			p, err := startLiveWith(t.Context(), root, verify.Plan{Tests: tc.tests}, "secret-key", []string{self}, env, anthropic)
			require.NoError(t, err)
			raw, readErr := os.ReadFile(filepath.Join(p.Dir(), liveTmpDir, "suite-env"))
			tmp := filepath.Join(p.Dir(), liveTmpDir)
			parentRaw, parentErr := os.ReadFile(filepath.Join(tmp, "parent-families.json"))
			require.NoError(t, p.Stop())
			require.NoError(t, parentErr)
			var parentFamilies map[string]bool
			require.NoError(t, json.Unmarshal(parentRaw, &parentFamilies))
			assert.Equal(t, tc.tests == verify.TestsEvaluator, parentFamilies["ANTHROPIC_"])
			assert.False(t, parentFamilies["OPENAI_"])
			require.NoError(t, readErr)
			seen := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if tc.tests == verify.TestsWritten {
				gateway, ok := lookupEnv(seen, verify.LiveModelGatewayEnv)
				assert.True(t, ok)
				assert.True(t, strings.HasPrefix(gateway, "127.0.0.1:"))
			}
			for _, want := range append(tc.sees, "TMPDIR="+tmp) {
				assert.Contains(t, seen, want)
			}
			for _, name := range tc.doesNotSee {
				for _, kv := range seen {
					assert.False(t, strings.HasPrefix(kv, name+"="), "the test program sees %s", kv)
				}
			}
		})
	}
}

// TestDecodeLiveRequestRefusesAPlanWithoutADeclaredTestSource checks that a
// live.json whose plan names no test source, or one that is not declared, is
// refused, and that each declared source is read.
func TestDecodeLiveRequestRefusesAPlanWithoutADeclaredTestSource(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "operator's evaluator", body: `{"plan":{"Tests":"evaluator"}}`},
		{name: "written tests", body: `{"plan":{"Tests":"written_tests"}}`},
		{name: "no test source", body: `{"plan":{}}`, wantErr: `decode live request: unknown test source ""`},
		{name: "unknown test source", body: `{"plan":{"Tests":"agents"}}`, wantErr: `decode live request: unknown test source "agents"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, liveRequestFile), []byte(tc.body), 0o600))
			var req LiveRequest
			err := decodeLiveRequest(dir, &req)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.EqualError(t, err, tc.wantErr)
		})
	}
}

func TestStartLiveServesUntilStopAndThenRemovesEverything(t *testing.T) {
	t.Setenv(liveFakeEnv, "ready")
	root := t.TempDir()
	self, err := os.Executable()
	require.NoError(t, err)

	p, err := startLive(t.Context(), root, evaluatorPlan, "secret-key", []string{self}, os.Environ())
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

// TestLaunchLiveResolvesItsRoot lets a child change directories before using
// TMPDIR, while relative and symlinked roots still name the caller's directory.
func TestLaunchLiveResolvesItsRoot(t *testing.T) {
	t.Setenv(liveFakeEnv, "ready")
	base := t.TempDir()
	t.Chdir(base)
	require.NoError(t, os.MkdirAll("real-cards/nested", 0o700))
	require.NoError(t, os.Mkdir("live", 0o700))
	require.NoError(t, os.Symlink("real-cards/nested", "alias"))
	physicalBase, err := filepath.EvalSymlinks(base)
	require.NoError(t, err)
	self, err := os.Executable()
	require.NoError(t, err)
	cases := []struct {
		name string
		// root preserves the caller's path before symlink resolution.
		root string
		// physicalRoot is the directory where the live target must be created.
		physicalRoot string
	}{
		{"relative", ".surla/cards/live", filepath.Join(physicalBase, ".surla", "cards", "live")},
		{"symlink", "alias/live", filepath.Join(physicalBase, "real-cards", "nested", "live")},
		{"relative_parent", "alias/../live", filepath.Join(physicalBase, "real-cards", "live")},
		{"absolute_parent", base + "/alias/../live", filepath.Join(physicalBase, "real-cards", "live")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			p, err := startLive(ctx, tc.root, evaluatorPlan, "secret-key", []string{self}, os.Environ())
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, p.Stop()) })
			assert.True(t, filepath.IsAbs(p.Dir()))
			assert.Equal(t, tc.physicalRoot, filepath.Dir(p.Dir()))
			var request LiveRequest
			require.NoError(t, decodeLiveRequest(p.Dir(), &request))
			assert.Equal(t, filepath.Join(p.Dir(), liveWorkDir), request.Plan.WorkDir)
			pgid, pids := recordedPIDs(t, p.Dir(), 2)
			assert.Len(t, pids, 2)
			require.NoError(t, p.Stop())
			requireGroupGone(t, pgid)
			assert.NoDirExists(t, p.Dir())
		})
	}
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
		{mode: "prepare-fails-exit71", key: "secret-key", wantErr: "The sandboxed command exited with status 71. If Surla is running inside a restricted coding-agent shell, its sandbox may have been blocked; run Surla from an unrestricted host shell."},
		{mode: "suite-exits", key: "secret-key", wantErr: summaryLiveSuite},
		{mode: "ready", key: "", wantErr: "live target key is required"},
	}
	for _, tc := range cases {
		t.Run(tc.mode+"/"+tc.wantErr, func(t *testing.T) {
			t.Setenv(liveFakeEnv, tc.mode)
			root := t.TempDir()
			_, err := startLive(t.Context(), root, evaluatorPlan, tc.key, []string{self}, os.Environ())
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
				_, err := startLive(ctx, root, evaluatorPlan, "secret-key", []string{self}, os.Environ())
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
	p, err := startLive(t.Context(), root, evaluatorPlan, "secret-key", []string{self}, os.Environ())
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

func TestLiveTargetDiesWithItsHostAfterItEndedAndBeforeStop(t *testing.T) {
	t.Setenv(liveFakeEnv, "target-ends")
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
	r := bufio.NewReader(out)
	for _, want := range []string{liveHostReady, liveHostEnded} {
		line, err := r.ReadString('\n')
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(line, want), line)
	}
	dir := onlyLiveDir(t, root)
	pgid, pids := recordedPIDs(t, dir, 2)
	// The suite exited and left its child; the host saw Done and has not
	// called Stop. The terminal event itself already ended every process,
	// and the host's death leaves none behind.
	requireGroupGone(t, pgid)
	require.NoError(t, host.Process.Signal(syscall.SIGKILL))
	for _, pid := range pids {
		assert.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH, "process %d outlived its host", pid)
	}
}

func TestLiveExecutorEndsItsWholeGroupOnATerminalEvent(t *testing.T) {
	cases := []struct {
		mode      string
		wantLines []string
	}{
		{mode: "prepare-fails-child", wantLines: []string{`{"error":"The candidate did not build."}`}},
		{mode: "suite-bad-line", wantLines: []string{`{"error":"` + summaryLiveSuite + `"}`}},
		{mode: "target-ends", wantLines: []string{`{"url":"http://127.0.0.1:4100"}`}},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv(liveFakeEnv, tc.mode)
			self, err := os.Executable()
			require.NoError(t, err)
			dir := t.TempDir()
			tmp := filepath.Join(dir, liveTmpDir)
			require.NoError(t, os.Mkdir(tmp, 0o700))
			require.NoError(t, writeJSON(dir, liveRequestFile, LiveRequest{Plan: evaluatorPlan}))
			control, keep, err := os.Pipe()
			require.NoError(t, err)
			defer keep.Close()
			cmd := exec.Command(self, "verify-run", "live", dir)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.ExtraFiles = []*os.File{control}
			cmd.Env = append(liveEnv(os.Environ()), "TMPDIR="+tmp, suite.LiveKeyEnv+"=secret-key")
			out, err := cmd.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, cmd.Start())
			require.NoError(t, control.Close())
			// The pipe stays open: the starter is alive and calls no Stop,
			// so only the terminal event can end the group.
			b, err := io.ReadAll(out)
			require.NoError(t, err, "the executor's output ends")
			assert.Equal(t, tc.wantLines, strings.Split(strings.TrimSpace(string(b)), "\n"))
			assert.Error(t, cmd.Wait(), "the executor ended with its group")
			pgid, pids := recordedPIDs(t, dir, 2)
			requireGroupGone(t, pgid)
			for _, pid := range pids {
				assert.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH, "process %d outlived the terminal event", pid)
			}
		})
	}
}

func TestStartLiveFailsPromptlyOnABadReadyLine(t *testing.T) {
	t.Setenv(liveFakeEnv, "suite-bad-line")
	root := t.TempDir()
	self, err := os.Executable()
	require.NoError(t, err)
	started := time.Now()
	_, err = startLive(t.Context(), root, evaluatorPlan, "secret-key", []string{self}, os.Environ())
	require.Error(t, err)
	assert.Contains(t, err.Error(), summaryLiveSuite)
	assert.Less(t, time.Since(started), 10*time.Second, "a protocol failure is reported without waiting for the suite to exit")
	dirs, err := LeftoverLiveDirs(root)
	require.NoError(t, err)
	assert.Empty(t, dirs)
}

func TestRepeatedLiveTargetsRetainNoDescriptors(t *testing.T) {
	t.Setenv(liveFakeEnv, "ready")
	root := t.TempDir()
	self, err := os.Executable()
	require.NoError(t, err)
	start := openDescriptors(t)
	for range 5 {
		p, err := startLive(t.Context(), root, evaluatorPlan, "secret-key", []string{self}, os.Environ())
		require.NoError(t, err)
		require.NoError(t, p.Stop())
	}
	assert.Equal(t, start, openDescriptors(t), "every pipe is closed by its owner, not by the garbage collector")
}

// openDescriptors returns how many descriptors this process has open.
func openDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	require.NoError(t, err)
	return len(entries)
}

func TestLiveStartupKeepsTheSafeSummaryWhenTheOutputHasAlsoEnded(t *testing.T) {
	cases := []struct {
		name    string
		line    liveLine
		wantErr string
		wantURL string
	}{
		{name: "error line", line: liveLine{Error: "The candidate did not build."}, wantErr: "The candidate did not build."},
		{name: "no line", line: liveLine{}, wantErr: summaryLiveSuite},
		{name: "ready line", line: liveLine{URL: "http://127.0.0.1:4100"}, wantURL: "http://127.0.0.1:4100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for range 100 {
				// The line is queued and the output has already ended, as
				// when the executor writes its line and kills its group.
				p := &LiveProcess{lines: make(chan liveLine, 1), ended: make(chan struct{}), summary: summaryLiveSuite}
				p.lines <- tc.line
				close(p.ended)
				err := p.AwaitReady(t.Context())
				if tc.wantErr == "" {
					require.NoError(t, err)
					assert.Equal(t, tc.wantURL, p.URL)
					continue
				}
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLiveStartFailuresStayDiagnosableAfterCleanup(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	cases := []struct {
		mode       string
		wantDetail string
	}{
		{mode: "suite-unsupported", wantDetail: "does not support --live"},
		{mode: "suite-provider", wantDetail: "ANTHROPIC_API_KEY is not set"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv(liveFakeEnv, tc.mode)
			root := t.TempDir()
			_, err := startLive(t.Context(), root, evaluatorPlan, "secret-key", []string{self}, os.Environ())
			var start *LiveStartError
			require.ErrorAs(t, err, &start)
			assert.Equal(t, summaryLiveSuite, start.Summary, "the public summary is the same safe text")
			assert.Contains(t, start.Detail, tc.wantDetail, "the private detail tells the causes apart")
			dirs, err := LeftoverLiveDirs(root)
			require.NoError(t, err)
			assert.Empty(t, dirs, "the detail outlives the removed directory")
		})
	}
}

func TestAwaitReadyCapturesDetailOnTimeoutAndLeavesTheTargetToItsOwner(t *testing.T) {
	t.Setenv(liveFakeEnv, "prepare-hangs")
	self, err := os.Executable()
	require.NoError(t, err)
	p, err := LaunchLive(t.TempDir(), evaluatorPlan, "secret-key", []string{self}, os.Environ(), nil)
	require.NoError(t, err)
	pgid, _ := recordedPIDs(t, p.Dir(), 2)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err = p.AwaitReady(ctx)
	var start *LiveStartError
	require.ErrorAs(t, err, &start)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, start.Detail, "preparing: downloading modules", "the detail is read before any cleanup")
	require.NoError(t, syscall.Kill(-pgid, 0), "AwaitReady left the target running for its owner")
	assert.DirExists(t, p.Dir())
	require.NoError(t, p.Stop())
	requireGroupGone(t, pgid)
	assert.NoDirExists(t, p.Dir())
}
