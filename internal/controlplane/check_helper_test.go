// This file proves that a check operation's helper process finishes the
// operation on its own when the server that started it dies or cancels it:
// containers are removed or kept recorded, the output is finished, the
// directory lock is released and no manifest is published. It also covers
// the current owner.json format, the helper's spec handling and its spans.
package controlplane

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
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"golang.org/x/sys/unix"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/verify/candidate"
)

type (
	// spanFileExporter appends one JSON line per ended span to path, so a
	// test can read the spans a check helper process recorded.
	spanFileExporter struct {
		// path is the file the lines are appended to.
		path string
	}

	// exportedSpan is one line spanFileExporter writes.
	exportedSpan struct {
		// Name is the span name.
		Name string
		// TraceID, SpanID and ParentSpanID link the span into its trace.
		TraceID, SpanID, ParentSpanID string
		// Events are the names of the span's events.
		Events []string
	}

	// publishGate holds a check helper of this test binary inside its first
	// manifest write, while it holds that directory's lock, until the test
	// releases it. TestMain's check-run role joins the gate whose directory
	// checkHelperPublishGateEnv names: it opens dir/release for reading,
	// opens dir/in-write for writing, which returns once held's reader is
	// there, and waits for one byte on dir/release.
	publishGate struct {
		// dir holds the two FIFOs.
		dir string
		// held is closed once a helper reports that it is held.
		held chan struct{}
		// release sends the byte a held helper waits for; it is idempotent.
		release func() error
	}

	// killCase is one moment at which TestCheckHelperFinishesTheOperationWhenItsServerIsKilled
	// kills the serving process.
	killCase struct {
		// name describes the moment.
		name string
		// tool is the operation the server runs.
		tool string
		// verb is the docker command held at the fake daemon when the server
		// is killed.
		verb string
		// helperStopsIt reports that the helper stops that command when it is
		// canceled, so the command's end proves the helper saw the server die.
		helperStopsIt bool
		// wantCreated is how many containers the operation creates, or -1
		// when that depends on how soon the helper sees the cancellation.
		wantCreated int
	}
)

const (
	// checkHelperSpansEnv names the file TestMain's check-run role writes
	// its spans to, when set.
	checkHelperSpansEnv = "CONTROLPLANE_TEST_HELPER_SPANS"
	// checkHelperPublishGateEnv names the directory of the publishGate
	// TestMain's check-run role joins, when set.
	checkHelperPublishGateEnv = "CONTROLPLANE_TEST_PUBLISH_GATE"
)

// TestCheckHelperFinishesTheOperationWhenItsServerIsKilled kills the
// serving process with SIGKILL while a docker command of its operation is
// held, at each phase, then releases the command and, without starting
// another server, checks that the helper removed every container it
// created, left no recorded name, finished the output, released the
// operation's lock, removed the scratch and wrote no manifest.
//
// Before create, while running and while tidying, the held command is one
// the helper stops when it is canceled, so the test waits for that command's
// sender to die: these cases prove the helper saw its server die before the
// release. During create and during removal the held command is one the
// helper deliberately keeps (an answered create, a removal with its own
// deadline), so nothing outside the helper shows when it saw its server die:
// these cases prove the cleanup outcome and that the held command was not
// abandoned, but not that the cancellation was seen before the release. That
// order is proven by TestEngineCreateIsNotAbandonedWhenItsContextEnds and
// TestAnswerContextEndsGraceAfterItsParent in verify/candidate.
func TestCheckHelperFinishesTheOperationWhenItsServerIsKilled(t *testing.T) {
	installFakeDocker(t)
	self, err := os.Executable()
	require.NoError(t, err)
	for _, tc := range []killCase{
		{name: "before create", tool: checkToolProject, verb: "image", helperStopsIt: true, wantCreated: 0},
		{name: "during create", tool: checkToolProject, verb: "create", wantCreated: -1},
		{name: "while running", tool: checkToolProject, verb: "start", helperStopsIt: true, wantCreated: 1},
		{name: "during removal", tool: checkToolProject, verb: "rm", wantCreated: -1},
		{name: "while tidying, before any manifest", tool: checkToolDependencies, verb: "start", helperStopsIt: true, wantCreated: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runKillCase(t, self, tc)
		})
	}
}

// TestCheckHelperWithoutASpecMakesNoDockerCall starts the helper with an
// empty or cut-off spec, as when its server dies before sending one: it
// exits non-zero before any docker command.
func TestCheckHelperWithoutASpecMakesNoDockerCall(t *testing.T) {
	installFakeDocker(t)
	daemon := newFakeDockerDaemon(t)
	t.Setenv("DOCKER_HOST", daemon.hostURI)
	for _, stdin := range []string{"", `{"tool":"check_project","op_id":`} {
		cmd := testCheckHelper(t)()
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr, string(out))
		assert.Equal(t, 1, exitErr.ExitCode())
		assert.Contains(t, string(out), "read the check helper spec")
	}
	assert.Zero(t, daemon.calls.Load())
}

// TestNewSkipsAnOperationWhoseHelperStillHoldsItsLock starts a server while
// an earlier server's helper still holds an operation directory: the server
// starts, and its recovery leaves that operation alone until the lock is
// released.
func TestNewSkipsAnOperationWhoseHelperStillHoldsItsLock(t *testing.T) {
	dockerBin := installFakeDocker(t)
	daemon := newFakeDockerDaemon(t)
	t.Setenv("DOCKER_HOST", daemon.hostURI)
	cards := filepath.Join(realTempDir(t), "cards")
	id := strings.Repeat("cd", 16)
	name := "surla-check-" + id + "-0"
	opDir := filepath.Join(cards, "check-work", id)
	daemon.addRunning(name, "surla.check="+id)
	writeTree(t, opDir, map[string]string{
		"src/go.mod": "module example.com/shop\n",
		outputFile:   "out\n",
		ownerFile:    fmt.Sprintf(`{"version":2,"docker":%q,"host":%q,"containers":[{"name":%q,"state":"creation_answered"}]}`, dockerBin, daemon.hostURI, name),
	})
	fd, err := unix.Open(opDir, openDirFlags, 0)
	require.NoError(t, err)
	require.NoError(t, unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB))

	cp := openCheckCP(t, cards, newShopModule(t, nil))
	assert.Zero(t, daemon.removeCount(name), "recovery left the locked operation to its helper")
	assert.DirExists(t, filepath.Join(opDir, "src"))

	require.NoError(t, unix.Close(fd))
	require.NoError(t, cp.Close())
	assert.Equal(t, 1, daemon.removeCount(name))
	c, ok := daemon.container(name)
	require.True(t, ok)
	assert.True(t, c.Removed)
	assert.NoDirExists(t, filepath.Join(opDir, "src"))
	assert.Empty(t, readOwnerRecord(t, opDir).Containers)
}

// TestCanceledCreateIsAnsweredAndItsContainerRemoved cancels a request while
// its create is in flight. The create is not abandoned: once the engine
// answers, the container is removed and no name is left recorded, so no
// container can appear unrecorded later. The test proves that outcome and
// that the create's sender was never killed; it cannot prove that the helper
// saw the cancellation before the engine answered, because nothing outside
// the helper shows that moment. That order is proven by
// TestEngineCreateIsNotAbandonedWhenItsContextEnds in verify/candidate.
func TestCanceledCreateIsAnsweredAndItsContainerRemoved(t *testing.T) {
	installFakeDocker(t)
	daemon := newFakeDockerDaemon(t)
	t.Setenv("DOCKER_HOST", daemon.hostURI)
	cards := filepath.Join(realTempDir(t), "cards")
	cp := openCheckCP(t, cards, newShopModule(t, nil))
	defer func() { assert.NoError(t, cp.Close()) }()
	wait, cancelWait := context.WithTimeout(t.Context(), lifecycleWaitTimeout)
	defer cancelWait()
	blocked, release, senderGone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	var first atomic.Bool
	daemon.setBeforeCall(func(reqCtx context.Context, verb string) {
		if verb != "create" || !first.CompareAndSwap(false, true) {
			return
		}
		close(blocked)
		select {
		case <-release:
		case <-reqCtx.Done():
			close(senderGone)
		}
	})
	ctx, cancel := context.WithCancel(WithPrincipal(t.Context(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser}))
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := cp.CheckProject(ctx)
		done <- err
	}()
	select {
	case <-blocked:
	case <-wait.Done():
		require.FailNow(t, "the operation never sent create")
	}
	cancel()
	releaseOnce()
	var err error
	select {
	case err = <-done:
	case <-wait.Done():
		require.FailNow(t, "the canceled operation did not return")
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "or was canceled")
	select {
	case <-senderGone:
		assert.Fail(t, "the in-flight create was abandoned")
	default:
	}
	created := daemon.createdContainers()
	require.NotEmpty(t, created)
	for _, c := range created {
		assert.True(t, c.Removed, c.Name)
	}
	opDir := onlyOperationDir(t, cards)
	assert.Empty(t, readOwnerRecord(t, opDir).Containers)
	assert.NoDirExists(t, filepath.Join(opDir, "src"))
}

// TestUnansweredCreateKeepsItsPendingRecord fails create: the engine did not
// confirm a container and no listing shows one, so its name stays recorded
// as creation_pending, on the call and at the next recovery, without any
// removal. Such a container never ran, so the scratch is reclaimed while
// the name and the output stay. Once a listing shows the container (a late
// create), recovery removes it, sees it gone and clears the record.
func TestUnansweredCreateKeepsItsPendingRecord(t *testing.T) {
	installFakeDocker(t)
	daemon := newFakeDockerDaemon(t)
	t.Setenv("DOCKER_HOST", daemon.hostURI)
	daemon.setFailCreate(true)
	cards := filepath.Join(realTempDir(t), "cards")
	authCtx := WithPrincipal(t.Context(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	cp := openCheckCP(t, cards, newShopModule(t, nil))
	_, err := cp.CheckProject(authCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mounts denied")
	assert.Contains(t, err.Error(), "may still appear")
	opDir := onlyOperationDir(t, cards)
	id := filepath.Base(opDir)
	name := "surla-check-" + id + "-0"
	pending := []ownedContainer{{Name: name, State: stateCreationPending}}
	assert.Equal(t, pending, readOwnerRecord(t, opDir).Containers)
	assert.ElementsMatch(t, []string{outputFile, outputMetaFile, ownerFile}, dirEntryNames(t, opDir), "the unused scratch is reclaimed")
	page, err := cp.ReadCheckOutput(authCtx, &gensurla.ReadCheckOutputPayload{OpID: id})
	require.NoError(t, err)
	assert.Contains(t, page.Text, "==> modules: .")
	assert.Zero(t, daemon.removeCount(name), "nothing is removed before a listing shows it")

	require.NoError(t, cp.Close(), "an entry no listing shows is kept without a recovery failure")
	assert.Equal(t, pending, readOwnerRecord(t, opDir).Containers)

	daemon.addCreated(name, "surla.check="+id)
	cp = openCheckCP(t, cards, newShopModule(t, nil))
	defer func() { assert.NoError(t, cp.Close()) }()
	assert.Equal(t, 1, daemon.removeCount(name))
	c, ok := daemon.container(name)
	require.True(t, ok)
	assert.True(t, c.Removed)
	assert.Empty(t, readOwnerRecord(t, opDir).Containers)
}

// TestRecoveryAfterACrashInEachWindow recovers the record and engine state
// a helper leaves when it dies between its steps: recording a name, sending
// create, recording the answer, and sending start. Each window is
// reconstructed exactly, with the container absent, created or running on
// the fake engine, so recovery's decision is checked without timing.
func TestRecoveryAfterACrashInEachWindow(t *testing.T) {
	dockerBin := installFakeDocker(t)
	cases := []struct {
		name        string
		state       containerState
		engine      string
		wantRemoves int
		wantKept    bool
	}{
		{name: "recorded, create not sent", state: stateCreationPending, wantKept: true},
		{name: "created, answer not recorded", state: stateCreationPending, engine: "created", wantRemoves: 1},
		{name: "answer recorded, start not sent", state: stateCreationAnswered, engine: "created", wantRemoves: 1},
		{name: "started and running", state: stateCreationAnswered, engine: "running", wantRemoves: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newFakeDockerDaemon(t)
			t.Setenv("DOCKER_HOST", daemon.hostURI)
			cards := filepath.Join(realTempDir(t), "cards")
			id := strings.Repeat("ab", 16)
			name := "surla-check-" + id + "-0"
			opDir := filepath.Join(cards, "check-work", id)
			switch tc.engine {
			case "created":
				daemon.addCreated(name, "surla.check="+id)
			case "running":
				daemon.addRunning(name, "surla.check="+id)
			}
			writeTree(t, opDir, map[string]string{
				"src/go.mod":   "module example.com/shop\n",
				"work-0/tmp/x": "",
				outputFile:     "",
				ownerFile:      fmt.Sprintf(`{"version":2,"docker":%q,"host":%q,"containers":[{"name":%q,"state":%q}]}`, dockerBin, daemon.hostURI, name, tc.state),
			})
			cp := openCheckCP(t, cards, newShopModule(t, nil))
			defer func() { assert.NoError(t, cp.Close()) }()
			assert.Equal(t, tc.wantRemoves, daemon.removeCount(name))
			if tc.wantKept {
				assert.Equal(t, []ownedContainer{{Name: name, State: tc.state}}, readOwnerRecord(t, opDir).Containers)
			} else {
				assert.Empty(t, readOwnerRecord(t, opDir).Containers)
				c, ok := daemon.container(name)
				require.True(t, ok)
				assert.Equal(t, tc.engine, c.State, "the container was %s when it was removed", tc.engine)
				assert.True(t, c.Removed)
			}
			assert.NoDirExists(t, filepath.Join(opDir, "src"), "no entry left can be using the scratch")
		})
	}
}

// TestOwnerRecordIsWrittenBeforeEachStep reads owner.json at the moment
// each docker command arrives: a name is recorded creation_pending before
// its create is sent and creation_answered before its start is sent, and is
// dropped once its removal is confirmed. When create fails, the name stays
// creation_pending.
func TestOwnerRecordIsWrittenBeforeEachStep(t *testing.T) {
	installFakeDocker(t)
	for _, failCreate := range []bool{false, true} {
		t.Run(fmt.Sprintf("create_fails=%t", failCreate), func(t *testing.T) {
			daemon := newFakeDockerDaemon(t)
			t.Setenv("DOCKER_HOST", daemon.hostURI)
			daemon.setFailCreate(failCreate)
			cards := filepath.Join(realTempDir(t), "cards")
			var (
				mu   sync.Mutex
				seen []string
			)
			daemon.setBeforeCall(func(_ context.Context, verb string) {
				if verb != "create" && verb != "start" && verb != "rm" {
					return
				}
				// This runs on the fake daemon's goroutine, so it reports with
				// assert and returns.
				workDir := filepath.Join(cards, "check-work")
				entries, err := os.ReadDir(workDir)
				if !assert.NoError(t, err) || !assert.Len(t, entries, 1) {
					return
				}
				id := entries[0].Name()
				raw, err := os.ReadFile(filepath.Join(workDir, id, ownerFile))
				if !assert.NoError(t, err) {
					return
				}
				owner, err := decodeCheckOwner(raw, id)
				if !assert.NoError(t, err) {
					return
				}
				var recorded []string
				for _, c := range owner.Containers {
					recorded = append(recorded, strings.TrimPrefix(c.Name, "surla-check-"+id+"-")+"="+string(c.State))
				}
				mu.Lock()
				seen = append(seen, verb+" "+strings.Join(recorded, ","))
				mu.Unlock()
			})
			cp := openCheckCP(t, cards, newShopModule(t, nil))
			defer func() { assert.NoError(t, cp.Close()) }()
			_, err := cp.CheckProject(WithPrincipal(t.Context(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser}))
			opDir := onlyOperationDir(t, cards)
			mu.Lock()
			defer mu.Unlock()
			if failCreate {
				require.Error(t, err)
				assert.Equal(t, []string{"create 0=creation_pending"}, seen)
				assert.Equal(t, []ownedContainer{{Name: "surla-check-" + filepath.Base(opDir) + "-0", State: stateCreationPending}}, readOwnerRecord(t, opDir).Containers)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []string{
				"create 0=creation_pending", "start 0=creation_answered", "rm 0=creation_answered",
				"create 1=creation_pending", "start 1=creation_answered", "rm 1=creation_answered",
			}, seen)
			assert.Empty(t, readOwnerRecord(t, opDir).Containers)
		})
	}
}

// TestConfirmRemovedNeedsAListingThatShowsItGone checks the one removal
// decision against the fake engine: rm's exit status is never proof, a
// failed listing proves nothing, an answered entry clears once a listing no
// longer shows it, and a pending or unknown entry clears only after a
// listing showed it, it was removed and a later listing no longer shows it.
func TestConfirmRemovedNeedsAListingThatShowsItGone(t *testing.T) {
	dockerBin := installFakeDocker(t)
	const id = "op1"
	name := "surla-check-op1-0"
	cases := []struct {
		name        string
		state       containerState
		exists      bool
		failRemove  bool
		failList    bool
		wantCleared bool
		wantRemoves int
		wantErr     string
	}{
		{name: "answered, removed, listing no longer shows it", state: stateCreationAnswered, exists: true, wantCleared: true, wantRemoves: 1},
		{name: "answered and already gone", state: stateCreationAnswered, wantCleared: true, wantRemoves: 1},
		{name: "answered, removal fails, still listed", state: stateCreationAnswered, exists: true, failRemove: true, wantRemoves: 1, wantErr: "still shows it"},
		{name: "answered, listing fails", state: stateCreationAnswered, exists: true, failList: true, wantRemoves: 1, wantErr: "list the containers of operation op1"},
		{name: "pending, never listed", state: stateCreationPending, wantErr: errCreateUnconfirmed.Error()},
		{name: "pending, listed, removed and gone", state: stateCreationPending, exists: true, wantCleared: true, wantRemoves: 1},
		{name: "pending, listed, removal fails", state: stateCreationPending, exists: true, failRemove: true, wantRemoves: 1, wantErr: "still shows it"},
		{name: "pending, listing fails", state: stateCreationPending, exists: true, failList: true, wantErr: "list the containers of operation op1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newFakeDockerDaemon(t)
			if tc.exists {
				daemon.addRunning(name, "surla.check="+id)
			}
			daemon.setFailRemove(name, tc.failRemove)
			daemon.setFailList(tc.failList)
			engine := candidate.NewContainerRunner().Reopen(dockerBin, daemon.hostURI, realTempDir(t))
			err := confirmRemoved(engine, id, ownedContainer{Name: name, State: tc.state})
			if tc.wantCleared {
				require.NoError(t, err)
			} else {
				var cleanup *candidate.CleanupError
				require.ErrorAs(t, err, &cleanup)
				assert.Equal(t, name, cleanup.Name)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
			assert.Equal(t, tc.wantRemoves, daemon.removeCount(name))
		})
	}
}

// TestCheckHelperSpansJoinTheServerTrace runs a passing check and reads the
// helper's spans: its operation span is a child of the server's request
// span, and every container has create, run and remove spans under it.
func TestCheckHelperSpansJoinTheServerTrace(t *testing.T) {
	installFakeDocker(t)
	daemon := newFakeDockerDaemon(t)
	t.Setenv("DOCKER_HOST", daemon.hostURI)
	spansFile := filepath.Join(realTempDir(t), "spans.jsonl")
	t.Setenv(checkHelperSpansEnv, spansFile)
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
	cp := newControlPlane(t, Options{
		CardsDir: filepath.Join(realTempDir(t), "cards"), WorkspaceDir: newShopModule(t, nil),
		Checker: candidate.NewContainerRunner(), CheckHelper: testCheckHelper(t), CheckTimeout: time.Minute, TracerProvider: tp,
	})
	res, err := cp.CheckProject(WithPrincipal(t.Context(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser}))
	require.NoError(t, err)
	require.True(t, res.Passed)

	var serve trace.SpanContext
	for _, s := range exporter.GetSpans() {
		if s.Name == "surla.controlplane.check_project" {
			serve = s.SpanContext
		}
	}
	require.True(t, serve.IsValid())
	spans := readExportedSpans(t, spansFile)
	byName := map[string][]exportedSpan{}
	for _, s := range spans {
		byName[s.Name] = append(byName[s.Name], s)
	}
	require.Len(t, byName["surla.check.helper"], 1)
	root := byName["surla.check.helper"][0]
	assert.Equal(t, serve.TraceID().String(), root.TraceID)
	assert.Equal(t, serve.SpanID().String(), root.ParentSpanID)
	require.Len(t, byName["surla.check.container"], 2, "download and test")
	containers := map[string]bool{}
	for _, c := range byName["surla.check.container"] {
		assert.Equal(t, root.SpanID, c.ParentSpanID)
		containers[c.SpanID] = true
	}
	for _, phase := range []string{"surla.check.container.create", "surla.check.container.run", "surla.check.container.remove"} {
		require.Len(t, byName[phase], 2, phase)
		for _, s := range byName[phase] {
			assert.True(t, containers[s.ParentSpanID], "%s is a child of its container span", phase)
		}
	}
	for _, s := range byName["surla.check.container.remove"] {
		assert.Contains(t, s.Events, "container_removed")
	}
}

// runKillCase runs one case of TestCheckHelperFinishesTheOperationWhenItsServerIsKilled.
func runKillCase(t *testing.T, self string, tc killCase) {
	daemon := newFakeDockerDaemon(t)
	t.Setenv("DOCKER_HOST", daemon.hostURI)
	workspace := newShopModule(t, map[string]string{"go.sum": "old-sum\n"})
	cards := filepath.Join(realTempDir(t), "cards")
	wait, cancelWait := context.WithTimeout(t.Context(), lifecycleWaitTimeout)
	defer cancelWait()
	blocked, release, senderGone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	var first atomic.Bool
	daemon.setBeforeCall(func(reqCtx context.Context, verb string) {
		if verb != tc.verb || !first.CompareAndSwap(false, true) {
			return
		}
		if tc.tool == checkToolDependencies {
			// go mod tidy changed go.sum in the copy, so only cancellation
			// can keep it from being published.
			created := daemon.createdContainers()
			src := findMount(t, &created[len(created)-1], workspace).Source
			assert.NoError(t, os.WriteFile(filepath.Join(src, "go.sum"), []byte("new-sum\n"), 0o644))
		}
		close(blocked)
		select {
		case <-release:
		case <-reqCtx.Done():
			close(senderGone)
		}
	})
	hostLog, err := os.Create(filepath.Join(realTempDir(t), "host.log"))
	require.NoError(t, err)
	host := exec.Command(self, "check-host", cards, workspace, tc.tool)
	host.Stdout, host.Stderr = hostLog, hostLog
	require.NoError(t, host.Start())
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			assert.NoError(t, host.Process.Kill())
			assert.Error(t, host.Wait())
		}
		assert.NoError(t, hostLog.Close())
	})
	select {
	case <-blocked:
	case <-wait.Done():
		log, err := os.ReadFile(hostLog.Name())
		require.FailNow(t, "the operation never reached docker "+tc.verb, "%s %v", log, err)
	}
	opDir := onlyOperationDir(t, cards)

	require.NoError(t, host.Process.Signal(syscall.SIGKILL))
	assert.Error(t, host.Wait(), "the server was killed")
	reaped = true
	if tc.helperStopsIt {
		select {
		case <-senderGone:
		case <-wait.Done():
			require.FailNow(t, "the helper never stopped docker "+tc.verb+" after its server died")
		}
	}
	releaseOnce()
	waitForUnlock(t, wait, opDir)

	// The output reads back by its op_id without any server running: the
	// reader only opens the operation's files.
	reader := &ControlPlane{checkWorkDir: filepath.Join(cards, "check-work")}
	page, err := reader.ReadCheckOutput(WithPrincipal(t.Context(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser}), &gensurla.ReadCheckOutputPayload{OpID: filepath.Base(opDir)})
	require.NoError(t, err, "the helper finished the output")
	assert.Contains(t, page.Text, "==> modules: .")
	if raw, err := os.ReadFile(filepath.Join(opDir, ownerFile)); err == nil {
		owner, err := decodeCheckOwner(raw, filepath.Base(opDir))
		require.NoError(t, err)
		assert.Empty(t, owner.Containers)
	} else {
		require.ErrorIs(t, err, os.ErrNotExist)
		assert.Zero(t, tc.wantCreated, "only an operation that created nothing has no record")
	}
	created := daemon.createdContainers()
	for _, c := range created {
		assert.True(t, c.Removed, "container %s was removed without another server", c.Name)
	}
	if tc.wantCreated >= 0 {
		assert.Len(t, created, tc.wantCreated)
	} else {
		assert.NotEmpty(t, created)
	}
	if !tc.helperStopsIt {
		select {
		case <-senderGone:
			assert.Fail(t, "the helper abandoned docker "+tc.verb+" in flight")
		default:
		}
	}
	assert.NoDirExists(t, filepath.Join(opDir, "src"))
	assertFileContent(t, filepath.Join(workspace, "go.sum"), "old-sum\n")
}

// onlyOperationDir returns the single operation directory under cards.
func onlyOperationDir(t *testing.T, cards string) string {
	t.Helper()
	workDir := filepath.Join(cards, "check-work")
	names := dirEntryNames(t, workDir)
	require.Len(t, names, 1)
	return filepath.Join(workDir, names[0])
}

// waitForUnlock waits, bounded by ctx, until no process holds dir's lock,
// which happens once the helper that owns the operation has exited.
func waitForUnlock(t *testing.T, ctx context.Context, dir string) {
	t.Helper()
	fd, err := unix.Open(dir, openDirFlags, 0)
	require.NoError(t, err)
	locked := make(chan error, 1)
	go func() { locked <- unix.Flock(fd, unix.LOCK_EX) }()
	select {
	case err := <-locked:
		require.NoError(t, err)
		require.NoError(t, unix.Close(fd))
	case <-ctx.Done():
		require.FailNow(t, "the check helper still holds "+dir)
	}
}

// newPublishGate creates a publish gate and points the check helpers this
// test starts at it. Its cleanup releases a held helper and ends the wait
// for one that never came.
func newPublishGate(t *testing.T) *publishGate {
	t.Helper()
	dir := realTempDir(t)
	for _, name := range []string{"in-write", "release"} {
		require.NoError(t, unix.Mkfifo(filepath.Join(dir, name), 0o600))
	}
	t.Setenv(checkHelperPublishGateEnv, dir)
	g := &publishGate{dir: dir, held: make(chan struct{})}
	go func() {
		// Opening the FIFO for reading returns once a helper opens it to
		// report that it is held.
		f, err := os.Open(filepath.Join(dir, "in-write"))
		if err != nil {
			return
		}
		if err := f.Close(); err != nil {
			return
		}
		close(g.held)
	}()
	g.release = sync.OnceValue(func() error {
		// A held helper has the release FIFO open for reading; without one,
		// a non-blocking open finds no reader and there is nothing to do.
		f, err := os.OpenFile(filepath.Join(dir, "release"), os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if errors.Is(err, syscall.ENXIO) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = f.Write([]byte{1})
		return errors.Join(err, f.Close())
	})
	t.Cleanup(func() {
		assert.NoError(t, g.release())
		// A helper that never came leaves the reader of in-write waiting.
		if f, err := os.OpenFile(filepath.Join(dir, "in-write"), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			assert.NoError(t, f.Close())
		}
	})
	return g
}

// holdAtPublishGate holds this check helper at the publish gate in dir; see
// publishGate.
func holdAtPublishGate(dir string) error {
	release, err := os.OpenFile(filepath.Join(dir, "release"), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer release.Close()
	inWrite, err := os.OpenFile(filepath.Join(dir, "in-write"), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if err := inWrite.Close(); err != nil {
		return err
	}
	_, err = release.Read(make([]byte, 1))
	return err
}

// readExportedSpans reads the lines spanFileExporter wrote to path.
func readExportedSpans(t *testing.T, path string) []exportedSpan {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	var spans []exportedSpan
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var s exportedSpan
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &s))
		spans = append(spans, s)
	}
	require.NoError(t, scanner.Err())
	return spans
}

// runTestCheckHelper is TestMain's check-run role: the check helper with the
// real Docker runner, recording spans to checkHelperSpansEnv and joining the
// publish gate of checkHelperPublishGateEnv when they are set.
func runTestCheckHelper() int {
	var tp trace.TracerProvider = noop.NewTracerProvider()
	if path := os.Getenv(checkHelperSpansEnv); path != "" {
		tp = sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanFileExporter{path: path}))
	}
	if gate := os.Getenv(checkHelperPublishGateEnv); gate != "" {
		prior := fsyncDir
		var once sync.Once
		fsyncDir = func(fd int) error {
			var err error
			once.Do(func() { err = holdAtPublishGate(gate) })
			return errors.Join(err, prior(fd))
		}
	}
	if err := RunCheckHelper(candidate.NewContainerRunner(), tp, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// checkHost is TestMain's check-host role: a server on cardsDir bound to
// workspace, as surla serve is, that runs tool once. Tests kill it while the
// operation runs.
func checkHost(cardsDir, workspace, tool string) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Println("executable:", err)
		return 2
	}
	cp, err := New(Options{
		CardsDir: cardsDir, WorkspaceDir: workspace, Checker: candidate.NewContainerRunner(),
		CheckHelper:  func() *exec.Cmd { return exec.Command(self, "check-run") },
		CheckTimeout: time.Minute,
	})
	if err != nil {
		fmt.Println("new:", err)
		return 2
	}
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	switch tool {
	case checkToolProject:
		_, err = cp.CheckProject(ctx)
	case checkToolDependencies:
		_, err = cp.UpdateDependencies(ctx)
	}
	fmt.Println("result:", errors.Join(err, cp.Close()))
	return 0
}

// ExportSpans appends spans to e.path, one JSON line each.
func (e spanFileExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	f, err := os.OpenFile(e.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, s := range spans {
		line := exportedSpan{
			Name:         s.Name(),
			TraceID:      s.SpanContext().TraceID().String(),
			SpanID:       s.SpanContext().SpanID().String(),
			ParentSpanID: s.Parent().SpanID().String(),
		}
		for _, ev := range s.Events() {
			line.Events = append(line.Events, ev.Name)
		}
		if err := enc.Encode(line); err != nil {
			return errors.Join(err, f.Close())
		}
	}
	return f.Close()
}

// Shutdown implements sdktrace.SpanExporter; there is nothing to flush.
func (spanFileExporter) Shutdown(context.Context) error {
	return nil
}
