package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	goa "goa.design/goa/v3/pkg"
	"golang.org/x/sys/unix"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/scaffold"
)

// fakeInitGenerator is an in-memory scaffold.Generator test double that returns a minimal generated file.
type fakeInitGenerator struct {
	// pkg is the Go package name emitted in the stub generated file.
	pkg string
}

// testSurlaVersion is a canonical Surla module pseudo-version used in workspace unit tests.
const testSurlaVersion = "v0.0.0-20261104215521-580509970fb6"

// TestNewValidatesSurlaVersionAndWorkspaceSeparation verifies that New rejects non-canonical
// SurlaVersion values and CardsDir paths inside WorkspaceDir.
func TestNewValidatesSurlaVersionAndWorkspaceSeparation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	wsDir := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(wsDir, 0o755))

	// Invalid SurlaVersion is rejected at startup.
	_, err := New(Options{
		CardsDir:     filepath.Join(root, "cards-bad-ver"),
		WorkspaceDir: wsDir,
		SurlaVersion: "(devel)",
	})
	require.ErrorContains(t, err, "must be a canonical semantic version")

	// CardsDir inside WorkspaceDir is rejected at startup.
	_, err = New(Options{
		CardsDir:     filepath.Join(wsDir, ".surla", "cards"),
		WorkspaceDir: wsDir,
		SurlaVersion: testSurlaVersion,
	})
	require.ErrorContains(t, err, "must be outside the workspace")
}

// TestWorkspaceWALReconciliationStates verifies startup reconciliation of workspace.json
// across staged roll-forward, missing-stage cleanup, and published-inode finalization states.
func TestWorkspaceWALReconciliationStates(t *testing.T) {
	t.Parallel()

	t.Run("case_3_staged_dir_with_matching_inode_is_recovered_and_published", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		wsDir := filepath.Join(root, "workspace")
		cardsDir := filepath.Join(root, "cards")
		stageParent := filepath.Join(root, "init-stage")
		require.NoError(t, os.MkdirAll(wsDir, 0o755))
		require.NoError(t, os.MkdirAll(cardsDir, 0o700))
		require.NoError(t, os.MkdirAll(stageParent, 0o700))

		evalStageParent, err := abspath.Eval(stageParent)
		require.NoError(t, err)
		stagedDir := filepath.Join(evalStageParent, ".surla-init-123")
		require.NoError(t, os.MkdirAll(stagedDir, 0o700))
		var st unix.Stat_t
		require.NoError(t, unix.Lstat(stagedDir, &st))

		walBytes, err := json.Marshal(workspaceFile{
			Dir:      "shop",
			StageDir: evalStageParent,
			Staging:  ".surla-init-123",
			Dev:      uint64(st.Dev),
			Ino:      st.Ino,
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(cardsDir, workspaceRecordFile), walBytes, 0o600))

		cp, err := New(Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			InitStageDir: stageParent,
			SurlaVersion: testSurlaVersion,
		})
		require.NoError(t, err)
		defer cp.Close()

		assert.Equal(t, "shop", filepath.Base(cp.activeWorkspaceDir))
		_, err = os.Stat(stagedDir)
		assert.ErrorIs(t, err, os.ErrNotExist)
		var wf workspaceFile
		rawWAL, err := os.ReadFile(filepath.Join(cardsDir, workspaceRecordFile))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(rawWAL, &wf))
		assert.Equal(t, "shop", wf.Dir)
		assert.Empty(t, wf.Staging)
	})

	t.Run("case_4_uncommitted_wal_with_missing_stage_removes_wal", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		wsDir := filepath.Join(root, "workspace")
		cardsDir := filepath.Join(root, "cards")
		stageParent := filepath.Join(root, "init-stage")
		require.NoError(t, os.MkdirAll(wsDir, 0o755))
		require.NoError(t, os.MkdirAll(cardsDir, 0o700))
		require.NoError(t, os.MkdirAll(stageParent, 0o700))

		evalStageParent, err := abspath.Eval(stageParent)
		require.NoError(t, err)
		walBytes, err := json.Marshal(workspaceFile{
			Dir:      "shop",
			StageDir: evalStageParent,
			Staging:  ".surla-init-missing",
			Dev:      1,
			Ino:      1,
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(cardsDir, workspaceRecordFile), walBytes, 0o600))

		cp, err := New(Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			InitStageDir: stageParent,
			SurlaVersion: testSurlaVersion,
		})
		require.NoError(t, err)
		defer cp.Close()

		assert.Equal(t, filepath.Base(wsDir), filepath.Base(cp.activeWorkspaceDir))
		_, err = os.Stat(filepath.Join(cardsDir, workspaceRecordFile))
		assert.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("state_2_published_target_matches_wal_inode_finalizes_record", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		wsDir := filepath.Join(root, "workspace")
		cardsDir := filepath.Join(root, "cards")
		stageParent := filepath.Join(root, "init-stage")
		targetDir := filepath.Join(wsDir, "shop")
		require.NoError(t, os.MkdirAll(targetDir, 0o755))
		require.NoError(t, os.MkdirAll(cardsDir, 0o700))
		require.NoError(t, os.MkdirAll(stageParent, 0o700))

		var st unix.Stat_t
		require.NoError(t, unix.Lstat(targetDir, &st))
		walBytes, err := json.Marshal(workspaceFile{
			Dir:      "shop",
			StageDir: stageParent,
			Staging:  ".surla-init-456",
			Dev:      uint64(st.Dev),
			Ino:      st.Ino,
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(cardsDir, workspaceRecordFile), walBytes, 0o600))

		cp, err := New(Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			InitStageDir: stageParent,
			SurlaVersion: testSurlaVersion,
		})
		require.NoError(t, err)
		defer cp.Close()

		assert.Equal(t, "shop", filepath.Base(cp.activeWorkspaceDir))
		var wf workspaceFile
		rawWAL, err := os.ReadFile(filepath.Join(cardsDir, workspaceRecordFile))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(rawWAL, &wf))
		assert.Equal(t, "shop", wf.Dir)
		assert.Empty(t, wf.Staging)
	})
}

// TestInitProjectErrorAndSpanClassificationAndEndToEndTransition verifies InitProject's
// error contracts, OpenTelemetry span status codes, and in-place workspace transition.
func TestInitProjectErrorAndSpanClassificationAndEndToEndTransition(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})

	withTrackedSpans := func(t *testing.T) *tracetest.InMemoryExporter {
		t.Helper()
		exp := tracetest.NewInMemoryExporter()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
		prev := otel.GetTracerProvider()
		otel.SetTracerProvider(tp)
		t.Cleanup(func() {
			otel.SetTracerProvider(prev)
			_ = tp.Shutdown(context.Background())
		})
		return exp
	}

	t.Run("capability_unavailable_when_workspace_or_version_unset", func(t *testing.T) {
		exp := withTrackedSpans(t)
		root := t.TempDir()
		wsDir := filepath.Join(root, "ws")
		require.NoError(t, os.MkdirAll(wsDir, 0o755))

		cp, err := New(Options{
			CardsDir:     filepath.Join(root, "cards"),
			WorkspaceDir: wsDir,
		})
		require.NoError(t, err)
		defer cp.Close()

		_, err = cp.InitProject(aliceCtx, &gensurla.InitProjectPayload{
			ModulePath: "example.com/shop",
		})
		var svcErr *goa.ServiceError
		require.ErrorAs(t, err, &svcErr)
		assert.Equal(t, "not_implemented", svcErr.Name)
		spans := exp.GetSpans()
		require.Len(t, spans, 1)
		assert.Equal(t, codes.Unset, spans[0].Status.Code)
	})

	t.Run("invalid_input_returns_unprocessable_with_unset_span_status", func(t *testing.T) {
		exp := withTrackedSpans(t)
		root := t.TempDir()
		wsDir := filepath.Join(root, "ws")
		require.NoError(t, os.MkdirAll(wsDir, 0o755))
		cp, err := New(Options{
			CardsDir:     filepath.Join(root, "cards"),
			WorkspaceDir: wsDir,
			InitStageDir: filepath.Join(root, "stage"),
			SurlaVersion: testSurlaVersion,
		})
		require.NoError(t, err)
		defer cp.Close()

		badDir := "../escape"
		_, err = cp.InitProject(aliceCtx, &gensurla.InitProjectPayload{
			ModulePath: "example.com/shop",
			Dir:        &badDir,
		})
		var svcErr *goa.ServiceError
		require.ErrorAs(t, err, &svcErr)
		assert.Equal(t, "unprocessable", svcErr.Name)
		spans := exp.GetSpans()
		require.Len(t, spans, 1)
		assert.Equal(t, codes.Unset, spans[0].Status.Code)
	})

	t.Run("corrupt_card_in_precondition_scan_fails_with_internal_fault", func(t *testing.T) {
		exp := withTrackedSpans(t)
		root := t.TempDir()
		wsDir := filepath.Join(root, "ws")
		cardsDir := filepath.Join(root, "cards")
		require.NoError(t, os.MkdirAll(wsDir, 0o755))
		cp, err := New(Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			InitStageDir: filepath.Join(root, "stage"),
			SurlaVersion: testSurlaVersion,
		})
		require.NoError(t, err)
		defer cp.Close()

		corruptCardName := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.json"
		require.NoError(t, os.WriteFile(filepath.Join(cardsDir, corruptCardName), []byte(`{"card":{}}trailing`), 0o600))

		_, err = cp.InitProject(aliceCtx, &gensurla.InitProjectPayload{
			ModulePath: "example.com/shop",
		})
		require.Error(t, err)
		var svcErr *goa.ServiceError
		assert.False(t, errors.As(err, &svcErr), "corrupt card must return an internal fault, not a 4xx ServiceError")
		spans := exp.GetSpans()
		require.Len(t, spans, 1)
		assert.Equal(t, codes.Error, spans[0].Status.Code)
	})

	t.Run("happy_path_publishes_starter_and_updates_active_workspace_in_place", func(t *testing.T) {
		exp := withTrackedSpans(t)
		root := t.TempDir()
		wsDir := filepath.Join(root, "ws")
		cardsDir := filepath.Join(root, "cards")
		stageDir := filepath.Join(root, "stage")
		require.NoError(t, os.MkdirAll(wsDir, 0o755))

		cp, err := New(Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			InitStageDir: stageDir,
			SurlaVersion: testSurlaVersion,
			InitRunner:   fakeInitRunner,
			NewGenerator: func() scaffold.Generator {
				return &fakeInitGenerator{pkg: "shop"}
			},
		})
		require.NoError(t, err)
		defer cp.Close()

		res, err := cp.InitProject(aliceCtx, &gensurla.InitProjectPayload{
			ModulePath: "example.com/shop",
		})
		require.NoError(t, err)
		assert.Equal(t, "shop", res.ServiceName)
		assert.Equal(t, "example.com/shop", res.ModulePath)
		assert.Equal(t, "shop", filepath.Base(res.Dir))
		assert.Equal(t, res.Dir, cp.activeWorkspaceDir)

		var wf workspaceFile
		rawWAL, err := os.ReadFile(filepath.Join(cardsDir, workspaceRecordFile))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(rawWAL, &wf))
		assert.Equal(t, "shop", wf.Dir)
		assert.Empty(t, wf.Staging)

		spans := exp.GetSpans()
		require.Len(t, spans, 1)
		assert.Equal(t, codes.Unset, spans[0].Status.Code)
	})
}

// TestCloseDrainsAndCancelsInFlightInitProjectBeforeReleasingLocks verifies that Close cancels
// and waits for an in-flight InitProject before releasing the project lock.
func TestCloseDrainsAndCancelsInFlightInitProjectBeforeReleasingLocks(t *testing.T) {
	t.Parallel()
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})

	t.Run("close_during_prepare_cancels_context_and_waits_for_init_cleanup", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		wsDir := filepath.Join(root, "ws")
		cardsDir := filepath.Join(root, "cards")
		stageDir := filepath.Join(root, "stage")
		require.NoError(t, os.MkdirAll(wsDir, 0o755))

		runnerStarted := make(chan struct{})
		var runnerDone atomic.Bool
		var lockReleasedBeforeRunnerDone atomic.Bool

		cp, err := New(Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			InitStageDir: stageDir,
			SurlaVersion: testSurlaVersion,
			InitRunner: func(ctx context.Context, dir, logPath string, args ...string) error {
				select {
				case <-runnerStarted:
				default:
					close(runnerStarted)
				}
				<-ctx.Done()
				time.Sleep(20 * time.Millisecond)
				runnerDone.Store(true)
				return ctx.Err()
			},
			NewGenerator: func() scaffold.Generator {
				return &fakeInitGenerator{pkg: "shop"}
			},
			CloseProjectLock: func() error {
				if !runnerDone.Load() {
					lockReleasedBeforeRunnerDone.Store(true)
				}
				return nil
			},
		})
		require.NoError(t, err)

		initDone := make(chan error, 1)
		go func() {
			_, initErr := cp.InitProject(aliceCtx, &gensurla.InitProjectPayload{
				ModulePath: "example.com/shop",
			})
			initDone <- initErr
		}()

		<-runnerStarted
		require.NoError(t, cp.Close())
		initErr := <-initDone
		require.Error(t, initErr)
		assert.True(t, runnerDone.Load(), "Close must wait for InitProject to finish before returning")
		assert.False(t, lockReleasedBeforeRunnerDone.Load(), "Close must not release project lock while InitProject is still running")
	})

	t.Run("close_at_after_wal_waits_for_publication_and_wal_finalization", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		wsDir := filepath.Join(root, "ws")
		cardsDir := filepath.Join(root, "cards")
		stageDir := filepath.Join(root, "stage")
		require.NoError(t, os.MkdirAll(wsDir, 0o755))

		atAfterWAL := make(chan struct{})
		releaseHook := make(chan struct{})
		var closeReturned atomic.Bool

		cp, err := New(Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			InitStageDir: stageDir,
			SurlaVersion: testSurlaVersion,
			InitRunner:   fakeInitRunner,
			NewGenerator: func() scaffold.Generator {
				return &fakeInitGenerator{pkg: "shop"}
			},
		})
		require.NoError(t, err)
		cp.initStepHook = func(step string) {
			if step == "after_wal" {
				close(atAfterWAL)
				<-releaseHook
			}
		}

		initDone := make(chan error, 1)
		go func() {
			_, initErr := cp.InitProject(aliceCtx, &gensurla.InitProjectPayload{
				ModulePath: "example.com/shop",
			})
			initDone <- initErr
		}()

		<-atAfterWAL
		closeDone := make(chan error, 1)
		go func() {
			err := cp.Close()
			closeReturned.Store(true)
			closeDone <- err
		}()

		// Give Close() a moment to block on workspaceMu.
		time.Sleep(25 * time.Millisecond)
		assert.False(t, closeReturned.Load(), "Close must block on workspaceMu while InitProject holds it")

		close(releaseHook)
		require.NoError(t, <-initDone)
		require.NoError(t, <-closeDone)

		var wf workspaceFile
		rawWAL, err := os.ReadFile(filepath.Join(cardsDir, workspaceRecordFile))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(rawWAL, &wf))
		assert.Equal(t, "shop", wf.Dir)
		assert.Empty(t, wf.Staging)
	})
}

// Generate returns a single stub generated Go source file for g.pkg.
func (g *fakeInitGenerator) Generate(_ context.Context, _ map[string]string) (map[string][]byte, error) {
	return map[string][]byte{
		"gen/" + g.pkg + "/service.go": []byte("package " + g.pkg + "\n"),
	}, nil
}

// Close satisfies scaffold.Generator.
func (g *fakeInitGenerator) Close() error {
	return nil
}

// fakeInitRunner writes a synthetic command log and creates go.sum on "go mod tidy" invocations.
func fakeInitRunner(_ context.Context, dir, logPath string, args ...string) error {
	if err := os.WriteFile(logPath, []byte("ok\n"), 0o600); err != nil {
		return err
	}
	if len(args) > 2 && args[2] == "tidy" {
		return os.WriteFile(filepath.Join(dir, "go.sum"), []byte("sum\n"), 0o644)
	}
	return nil
}
