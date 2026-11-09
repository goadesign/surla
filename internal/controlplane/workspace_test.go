package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	goahttp "goa.design/goa/v3/http"
	goa "goa.design/goa/v3/pkg"
	"golang.org/x/sys/unix"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/scaffold"
)

type (
	// fakeInitGenerator is an in-memory scaffold.Generator test double that returns a minimal generated file.
	fakeInitGenerator struct {
		// pkg is the Go package name emitted in the stub generated file.
		pkg string
	}
)

const (
	// testSurlaVersion is a canonical Surla module pseudo-version used in workspace unit tests.
	testSurlaVersion = "v0.0.0-20261104215521-580509970fb6"
	// fakeInitialSuiteCommit is the synthetic initial Git commit hash returned by fakeInitRunner.
	fakeInitialSuiteCommit = "1111111111111111111111111111111111111111"
)

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

	t.Run("staged_dir_with_matching_inode_is_recovered_and_published", func(t *testing.T) {
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
			Dir:         "shop",
			SuiteCommit: fakeInitialSuiteCommit,
			StageDir:    evalStageParent,
			Staging:     ".surla-init-123",
			Dev:         uint64(st.Dev),
			Ino:         st.Ino,
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
		assert.Equal(t, fakeInitialSuiteCommit, cp.boundSuiteCommit)
		_, err = os.Stat(stagedDir)
		assert.ErrorIs(t, err, os.ErrNotExist)
		var wf workspaceFile
		rawWAL, err := os.ReadFile(filepath.Join(cardsDir, workspaceRecordFile))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(rawWAL, &wf))
		assert.Equal(t, "shop", wf.Dir)
		assert.Equal(t, fakeInitialSuiteCommit, wf.SuiteCommit)
		assert.Empty(t, wf.Staging)
	})

	t.Run("uncommitted_wal_with_missing_stage_removes_wal", func(t *testing.T) {
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
			Dir:         "shop",
			SuiteCommit: fakeInitialSuiteCommit,
			StageDir:    evalStageParent,
			Staging:     ".surla-init-missing",
			Dev:         1,
			Ino:         1,
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

	t.Run("published_target_matches_wal_inode_finalizes_record", func(t *testing.T) {
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
			Dir:         "shop",
			SuiteCommit: fakeInitialSuiteCommit,
			StageDir:    stageParent,
			Staging:     ".surla-init-456",
			Dev:         uint64(st.Dev),
			Ino:         st.Ino,
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
		assert.Equal(t, fakeInitialSuiteCommit, cp.boundSuiteCommit)
		var wf workspaceFile
		rawWAL, err := os.ReadFile(filepath.Join(cardsDir, workspaceRecordFile))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(rawWAL, &wf))
		assert.Equal(t, "shop", wf.Dir)
		assert.Equal(t, fakeInitialSuiteCommit, wf.SuiteCommit)
		assert.Empty(t, wf.Staging)
	})

	t.Run("uncommitted_wal_with_foreign_target_and_foreign_staging_preserves_both_and_removes_wal", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		wsDir := filepath.Join(root, "workspace")
		cardsDir := filepath.Join(root, "cards")
		stageParent := filepath.Join(root, "init-stage")
		targetDir := filepath.Join(wsDir, "shop")
		require.NoError(t, os.MkdirAll(targetDir, 0o755))
		require.NoError(t, os.MkdirAll(cardsDir, 0o700))
		require.NoError(t, os.MkdirAll(stageParent, 0o700))

		evalStageParent, err := abspath.Eval(stageParent)
		require.NoError(t, err)
		foreignStageDir := filepath.Join(evalStageParent, ".surla-init-foreign")
		require.NoError(t, os.MkdirAll(foreignStageDir, 0o700))
		targetKeep := filepath.Join(targetDir, "keep-target.txt")
		stageKeep := filepath.Join(foreignStageDir, "keep-stage.txt")
		require.NoError(t, os.WriteFile(targetKeep, []byte("user-target"), 0o644))
		require.NoError(t, os.WriteFile(stageKeep, []byte("foreign-stage"), 0o644))

		var wsStat unix.Stat_t
		require.NoError(t, unix.Lstat(wsDir, &wsStat))
		walBytes, err := json.Marshal(workspaceFile{
			Dir:         "shop",
			SuiteCommit: fakeInitialSuiteCommit,
			StageDir:    evalStageParent,
			Staging:     ".surla-init-foreign",
			Dev:         uint64(wsStat.Dev),
			Ino:         wsStat.Ino, // matches neither targetDir nor foreignStageDir
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
		gotTarget, err := os.ReadFile(targetKeep)
		require.NoError(t, err)
		assert.Equal(t, "user-target", string(gotTarget))
		gotStage, err := os.ReadFile(stageKeep)
		require.NoError(t, err)
		assert.Equal(t, "foreign-stage", string(gotStage))
	})

	t.Run("committed_wal_with_replaced_target_inode_fails_closed_without_deleting_target", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		wsDir := filepath.Join(root, "workspace")
		cardsDir := filepath.Join(root, "cards")
		targetDir := filepath.Join(wsDir, "shop")
		require.NoError(t, os.MkdirAll(targetDir, 0o755))
		require.NoError(t, os.MkdirAll(cardsDir, 0o700))
		targetKeep := filepath.Join(targetDir, "keep.txt")
		require.NoError(t, os.WriteFile(targetKeep, []byte("important"), 0o644))

		var wsStat unix.Stat_t
		require.NoError(t, unix.Lstat(wsDir, &wsStat))
		walBytes, err := json.Marshal(workspaceFile{
			Dir:         "shop",
			SuiteCommit: fakeInitialSuiteCommit,
			Dev:         uint64(wsStat.Dev),
			Ino:         wsStat.Ino, // different inode from targetDir
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(cardsDir, workspaceRecordFile), walBytes, 0o600))

		_, err = New(Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			SurlaVersion: testSurlaVersion,
		})
		require.ErrorContains(t, err, "no longer matches its initialized (dev, ino) identity")
		gotTarget, readErr := os.ReadFile(targetKeep)
		require.NoError(t, readErr)
		assert.Equal(t, "important", string(gotTarget))
	})

	t.Run("committed_wal_with_missing_target_fails_closed", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		wsDir := filepath.Join(root, "workspace")
		cardsDir := filepath.Join(root, "cards")
		require.NoError(t, os.MkdirAll(wsDir, 0o755))
		require.NoError(t, os.MkdirAll(cardsDir, 0o700))

		var wsStat unix.Stat_t
		require.NoError(t, unix.Lstat(wsDir, &wsStat))
		walBytes, err := json.Marshal(workspaceFile{
			Dir:         "shop",
			SuiteCommit: fakeInitialSuiteCommit,
			Dev:         uint64(wsStat.Dev),
			Ino:         wsStat.Ino,
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(cardsDir, workspaceRecordFile), walBytes, 0o600))

		_, err = New(Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			SurlaVersion: testSurlaVersion,
		})
		require.ErrorContains(t, err, "does not exist")
	})

	t.Run("uncommitted_wal_with_foreign_staging_preserves_foreign_staging_and_removes_wal", func(t *testing.T) {
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
		foreignStage := filepath.Join(evalStageParent, ".surla-init-replaced")
		require.NoError(t, os.MkdirAll(foreignStage, 0o700))
		foreignFile := filepath.Join(foreignStage, "foreign.txt")
		require.NoError(t, os.WriteFile(foreignFile, []byte("keep-foreign"), 0o644))

		var wsStat unix.Stat_t
		require.NoError(t, unix.Lstat(wsDir, &wsStat))
		walBytes, err := json.Marshal(workspaceFile{
			Dir:         "shop",
			SuiteCommit: fakeInitialSuiteCommit,
			StageDir:    evalStageParent,
			Staging:     ".surla-init-replaced",
			Dev:         uint64(wsStat.Dev),
			Ino:         wsStat.Ino, // does not match foreignStage
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

		_, err = os.Stat(filepath.Join(cardsDir, workspaceRecordFile))
		assert.ErrorIs(t, err, os.ErrNotExist)
		gotForeign, err := os.ReadFile(foreignFile)
		require.NoError(t, err)
		assert.Equal(t, "keep-foreign", string(gotForeign))
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
			require.NoError(t, tp.Shutdown(context.Background()))
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
		var svcErr *goa.ServiceError
		require.ErrorAs(t, err, &svcErr)
		assert.True(t, svcErr.Fault, "corrupt card must return a Goa Fault ServiceError")
		spans := exp.GetSpans()
		require.Len(t, spans, 1)
		assert.Equal(t, codes.Error, spans[0].Status.Code)
	})

	t.Run("init_fault_returns_http_500_and_mcp_internal_error_without_leaking_log_path_or_tail", func(t *testing.T) {
		exp := withTrackedSpans(t)
		root := t.TempDir()
		wsDir := filepath.Join(root, "ws")
		cardsDir := filepath.Join(root, "cards")
		stageDir := filepath.Join(root, "stage")
		require.NoError(t, os.MkdirAll(wsDir, 0o755))

		const (
			secretLogPath = "/private/operator/init-stage/secret-tidy.log"
			secretTail    = "SECRET_COMPILER_TAIL_TOKEN_98765"
		)
		cp, err := New(Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			InitStageDir: stageDir,
			SurlaVersion: testSurlaVersion,
			InitRunner: func(_ context.Context, _, _ string, args ...string) error {
				return &scaffold.StepError{
					Args:   args,
					Module: ".",
					Log:    secretLogPath,
					Tail:   secretTail,
					Err:    errors.New("exit status 1"),
				}
			},
			NewGenerator: func() scaffold.Generator {
				return &fakeInitGenerator{pkg: "shop"}
			},
		})
		require.NoError(t, err)
		defer cp.Close()

		handler := NewHTTPHandler(cp, func(_ *http.Request) (Principal, error) {
			return Principal{ID: "alice@example.com"}, nil
		})

		// 1. Verify HTTP POST /workspace/init returns 500 Fault without leaking Log or Tail.
		httpReq := httptest.NewRequest(http.MethodPost, "/workspace/init", strings.NewReader(`{"module_path":"example.com/shop"}`))
		httpReq.Header.Set("Content-Type", "application/json")
		httpRec := httptest.NewRecorder()
		handler.ServeHTTP(httpRec, httpReq)
		assert.Equal(t, http.StatusInternalServerError, httpRec.Code)
		rawHTTPBody, err := io.ReadAll(httpRec.Body)
		require.NoError(t, err)
		assert.NotContains(t, string(rawHTTPBody), secretLogPath)
		assert.NotContains(t, string(rawHTTPBody), secretTail)
		var errResp goahttp.ErrorResponse
		require.NoError(t, json.Unmarshal(rawHTTPBody, &errResp))
		assert.True(t, errResp.Fault)
		assert.Contains(t, errResp.Message, "go mod tidy")

		// 2. Verify MCP tools/call init_project returns JSON-RPC -32603 internal error without leaking Log or Tail.
		mcpBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"init_project","arguments":{"module_path":"example.com/shop"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
		mcpReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(mcpBody))
		mcpReq.Header.Set("Content-Type", "application/json")
		mcpReq.Header.Set("Accept", "application/json, text/event-stream")
		mcpReq.Header.Set("MCP-Protocol-Version", "2026-07-28")
		mcpReq.Header.Set("Mcp-Method", "tools/call")
		mcpReq.Header.Set("Mcp-Name", "init_project")
		mcpRec := httptest.NewRecorder()
		handler.ServeHTTP(mcpRec, mcpReq)
		assert.Equal(t, http.StatusInternalServerError, mcpRec.Code)
		rawMCPBody, err := io.ReadAll(mcpRec.Body)
		require.NoError(t, err)
		assert.NotContains(t, string(rawMCPBody), secretLogPath)
		assert.NotContains(t, string(rawMCPBody), secretTail)
		var rpcResp struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rawMCPBody, &rpcResp))
		require.NotNil(t, rpcResp.Error, "MCP response must contain a JSON-RPC error object: %s", string(rawMCPBody))
		assert.Equal(t, -32603, rpcResp.Error.Code)

		// 3. Verify the raw diagnostic error is recorded on the server trace spans.
		var initSpans []tracetest.SpanStub
		for _, sp := range exp.GetSpans() {
			if sp.Name == "surla.controlplane.init_project" {
				initSpans = append(initSpans, sp)
			}
		}
		require.Len(t, initSpans, 2)
		for _, sp := range initSpans {
			assert.Equal(t, codes.Error, sp.Status.Code)
			require.NotEmpty(t, sp.Events)
		}
	})

	t.Run("after_rename_concurrent_target_replacement_fails_closed_and_preserves_replacement_tree", func(t *testing.T) {
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

		replacementFile := filepath.Join(wsDir, "shop", "builder.txt")
		cp.initStepHook = func(step string) {
			if step == "after_rename" {
				published := filepath.Join(wsDir, "shop")
				movedAside := filepath.Join(wsDir, "shop-moved-aside")
				require.NoError(t, os.Rename(published, movedAside))
				require.NoError(t, os.MkdirAll(published, 0o755))
				require.NoError(t, os.WriteFile(replacementFile, []byte("builder-owned"), 0o644))
			}
		}

		_, err = cp.InitProject(aliceCtx, &gensurla.InitProjectPayload{
			ModulePath: "example.com/shop",
		})
		var svcErr *goa.ServiceError
		require.ErrorAs(t, err, &svcErr)
		assert.True(t, svcErr.Fault)
		gotReplacement, readErr := os.ReadFile(replacementFile)
		require.NoError(t, readErr)
		assert.Equal(t, "builder-owned", string(gotReplacement))
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
		assert.Equal(t, fakeInitialSuiteCommit, res.Commit)
		assert.Equal(t, res.Dir, cp.activeWorkspaceDir)
		assert.Equal(t, fakeInitialSuiteCommit, cp.boundSuiteCommit)

		var wf workspaceFile
		rawWAL, err := os.ReadFile(filepath.Join(cardsDir, workspaceRecordFile))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(rawWAL, &wf))
		assert.Equal(t, "shop", wf.Dir)
		assert.Equal(t, fakeInitialSuiteCommit, wf.SuiteCommit)
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

		select {
		case <-runnerStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for InitRunner to start")
		}
		require.NoError(t, cp.Close())
		var initErr error
		select {
		case initErr = <-initDone:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for InitProject to finish")
		}
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
		var releaseClosed atomic.Bool
		releaseBarrier := func() {
			if releaseClosed.CompareAndSwap(false, true) {
				close(releaseHook)
			}
		}
		t.Cleanup(releaseBarrier)

		var lockSawFinalizedWAL atomic.Bool
		var lockSawPublishedDir atomic.Bool

		cp, err := New(Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			InitStageDir: stageDir,
			SurlaVersion: testSurlaVersion,
			InitRunner:   fakeInitRunner,
			NewGenerator: func() scaffold.Generator {
				return &fakeInitGenerator{pkg: "shop"}
			},
			CloseProjectLock: func() error {
				rawWAL, readErr := os.ReadFile(filepath.Join(cardsDir, workspaceRecordFile))
				if readErr == nil {
					var wf workspaceFile
					if json.Unmarshal(rawWAL, &wf) == nil &&
						wf.Dir == "shop" &&
						wf.SuiteCommit == fakeInitialSuiteCommit &&
						wf.Staging == "" {
						lockSawFinalizedWAL.Store(true)
					}
				}
				if info, statErr := os.Stat(filepath.Join(wsDir, "shop")); statErr == nil && info.IsDir() {
					lockSawPublishedDir.Store(true)
				}
				return nil
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

		select {
		case <-atAfterWAL:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for after_wal hook")
		}
		closeDone := make(chan error, 1)
		go func() {
			closeDone <- cp.Close()
		}()

		// Release the after_wal barrier so InitProject finishes rename + WAL finalization
		// while Close waits to acquire workspaceMu before calling CloseProjectLock.
		releaseBarrier()
		select {
		case err := <-initDone:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for InitProject to complete")
		}
		select {
		case err := <-closeDone:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for Close to complete")
		}

		assert.True(t, lockSawPublishedDir.Load(), "CloseProjectLock must run only after InitProject publishes the project directory")
		assert.True(t, lockSawFinalizedWAL.Load(), "CloseProjectLock must run only after InitProject finalizes workspace.json")
	})
}

// TestCP1WorkspaceDesignSourceProvenanceAcrossProposeReviseAndReopen verifies that
// workspace-derived CP1 Change Cards re-read updated workspace design files on
// ReviseChange after reopening ControlPlane, while caller-supplied DesignSource
// cards preserve their supplied source across ReviseChange.
func TestCP1WorkspaceDesignSourceProvenanceAcrossProposeReviseAndReopen(t *testing.T) {
	t.Parallel()
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	root := t.TempDir()
	wsDir := filepath.Join(root, "ws")
	cardsDir := filepath.Join(root, "cards")
	designDir := filepath.Join(wsDir, "design")
	require.NoError(t, os.MkdirAll(designDir, 0o755))

	const designV1 = "package design\n// v1 workspace design\n"
	const designV2 = "package design\n// v2 workspace design\n"
	const suppliedDesign = "package design\n// explicit caller-supplied design\n"
	require.NoError(t, os.WriteFile(filepath.Join(designDir, "design.go"), []byte(designV1), 0o644))

	var capturedSources []string
	analyzer := &stubCP1Analyzer{
		analyzeFn: func(_ context.Context, prompt, designSource string) (CP1SemanticAnalysis, error) {
			capturedSources = append(capturedSources, designSource)
			return CP1SemanticAnalysis{
				Summary: "Summary for " + prompt,
				Examples: []CP1ModelExample{
					{
						Scenario:         "Price an item",
						InputSummary:     "1 item",
						ExpectedOutcome:  "Returns total",
						VerificationKind: "deterministic",
					},
				},
				PreservedBehaviors: []string{"existing orders continue to work"},
			}, nil
		},
		reviseFn: func(_ context.Context, prior CP1PriorCard, prompt, designSource string) (CP1RevisionSemanticAnalysis, error) {
			capturedSources = append(capturedSources, designSource)
			revExamples := make([]CP1RevisionModelExample, 0, len(prior.Examples))
			for _, ex := range prior.Examples {
				revExamples = append(revExamples, CP1RevisionModelExample{
					PriorID:          ex.ID,
					Scenario:         ex.Scenario,
					InputSummary:     ex.InputSummary,
					ExpectedOutcome:  ex.ExpectedOutcome,
					VerificationKind: ex.VerificationKind,
				})
			}
			return CP1RevisionSemanticAnalysis{
				Summary:            "Revised summary for " + prompt,
				Examples:           revExamples,
				PreservedBehaviors: prior.PreservedBehaviors,
			}, nil
		},
	}

	cp1, err := New(Options{
		CardsDir:     cardsDir,
		WorkspaceDir: wsDir,
		Analyzer:     analyzer,
	})
	require.NoError(t, err)

	// 1. ProposeChange with nil DesignSource reads design/design.go (v1) from the bound workspace.
	wsCard, err := cp1.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-workspace-origin",
		Prompt:   "Add loyalty discount",
	})
	require.NoError(t, err)
	require.Len(t, capturedSources, 1)
	assert.Contains(t, capturedSources[0], "v1 workspace design")

	// 2. ProposeChange with explicit DesignSource records caller-supplied provenance.
	suppliedCopy := suppliedDesign
	supCard, err := cp1.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID:     "chg-supplied-origin",
		Prompt:       "Add tax breakdown",
		DesignSource: &suppliedCopy,
	})
	require.NoError(t, err)
	require.Len(t, capturedSources, 2)
	assert.Equal(t, suppliedDesign, capturedSources[1])

	require.NoError(t, cp1.Close())

	// Update workspace design/design.go on disk to v2 and reopen ControlPlane.
	require.NoError(t, os.WriteFile(filepath.Join(designDir, "design.go"), []byte(designV2), 0o644))
	cp2, err := New(Options{
		CardsDir:     cardsDir,
		WorkspaceDir: wsDir,
		Analyzer:     analyzer,
	})
	require.NoError(t, err)
	defer cp2.Close()

	// 3. ReviseChange with nil DesignSource on the workspace-origin card re-reads v2 from disk.
	_, err = cp2.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:        wsCard.ChangeID,
		RequestRevision: wsCard.RequestRevision,
		Prompt:          "Add loyalty discount with cap",
	})
	require.NoError(t, err)
	require.Len(t, capturedSources, 3)
	assert.Contains(t, capturedSources[2], "v2 workspace design")

	// 4. ReviseChange with nil DesignSource on the supplied-origin card preserves the caller-supplied design.
	_, err = cp2.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:        supCard.ChangeID,
		RequestRevision: supCard.RequestRevision,
		Prompt:          "Add tax breakdown with rounding",
	})
	require.NoError(t, err)
	require.Len(t, capturedSources, 4)
	assert.Equal(t, suppliedDesign, capturedSources[3])
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

// fakeInitRunner writes a synthetic command log, creates go.sum on "go mod tidy"
// invocations, and writes fakeInitialSuiteCommit on "git rev-parse".
func fakeInitRunner(_ context.Context, dir, logPath string, args ...string) error {
	if gitSubcommand(args) == "rev-parse" {
		return os.WriteFile(logPath, []byte(fakeInitialSuiteCommit+"\n"), 0o600)
	}
	if err := os.WriteFile(logPath, []byte("ok\n"), 0o600); err != nil {
		return err
	}
	if len(args) > 2 && args[2] == "tidy" {
		return os.WriteFile(filepath.Join(dir, "go.sum"), []byte("sum\n"), 0o644)
	}
	return nil
}
