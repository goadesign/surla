package controlplane

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sys/unix"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/verify"
)

type (
	// capturedProgressEvent records one progress notification emitted during a tool call.
	capturedProgressEvent struct {
		// progress is the completed step count.
		progress float64
		// total is the total step count.
		total float64
		// message is the human-readable phase summary.
		message string
	}
)

// TestBoundControlPlaneDefaultEvaluatorAndGeneratorIgnoreWorkspaceTMPDIRAndCleanOwnWork
// verifies that when a workspace-bound ControlPlane is constructed with nil
// Evaluator and nil NewGenerator while TMPDIR points inside WorkspaceDir, both
// default preview/diff evaluations and default InitProject generation allocate
// their work directories under <CardsDir>/designeval outside WorkspaceDir, and
// closing a per-call generator does not remove the long-lived evaluator's work.
func TestBoundControlPlaneDefaultEvaluatorAndGeneratorIgnoreWorkspaceTMPDIRAndCleanOwnWork(t *testing.T) {
	root := t.TempDir()
	wsDir := filepath.Join(root, "workspace")
	wsTmpDir := filepath.Join(wsDir, "tmp")
	cardsDir := filepath.Join(root, "cards")
	stageDir := filepath.Join(root, "stage")
	require.NoError(t, os.MkdirAll(wsTmpDir, 0o755))
	t.Setenv("TMPDIR", wsTmpDir)

	badStageDir := filepath.Join(root, "bad-stage-0755")
	require.NoError(t, os.MkdirAll(badStageDir, 0o755))
	require.NoError(t, os.Chmod(badStageDir, 0o755))
	var wsStat unix.Stat_t
	require.NoError(t, unix.Stat(wsDir, &wsStat))
	require.NoError(t, os.Chmod(badStageDir, 0o755))
	err := validateInitStageParentDir(badStageDir, uint64(wsStat.Dev), wsDir)
	require.ErrorContains(t, err, "has mode 0755, want 0700")

	cp, err := New(Options{
		CardsDir:     cardsDir,
		WorkspaceDir: wsDir,
		InitStageDir: stageDir,
		SurlaVersion: testSurlaVersion,
	})
	require.NoError(t, err)

	_, err = cp.evaluator.Evaluate(context.Background(), map[string]string{})
	require.Error(t, err)

	physCards, err := abspath.Eval(cardsDir)
	require.NoError(t, err)
	evalParent := filepath.Join(physCards, "designeval")
	entriesAfterEval, err := os.ReadDir(evalParent)
	require.NoError(t, err)
	require.Len(t, entriesAfterEval, 1, "default evaluator must allocate its workDir under <CardsDir>/designeval")
	evalWorkName := entriesAfterEval[0].Name()
	assert.True(t, strings.HasPrefix(evalWorkName, "surla-designeval-work-"))

	initGen := cp.makeInitGenerator(evalParent)
	_, err = initGen.Generate(context.Background(), map[string]string{})
	require.Error(t, err)
	entriesDuringInitGen, err := os.ReadDir(evalParent)
	require.NoError(t, err)
	require.Len(t, entriesDuringInitGen, 2, "per-call init generator must allocate its own workDir under <CardsDir>/designeval")
	require.NoError(t, initGen.Close())

	entriesAfterInitGenClose, err := os.ReadDir(evalParent)
	require.NoError(t, err)
	require.Len(t, entriesAfterInitGenClose, 1, "closing per-call init generator must keep cp.evaluator's workDir intact")
	assert.Equal(t, evalWorkName, entriesAfterInitGenClose[0].Name())

	wsTmpEntries, err := os.ReadDir(wsTmpDir)
	require.NoError(t, err)
	assert.Empty(t, wsTmpEntries, "bound default evaluator and generator must never allocate inside workspace TMPDIR")

	require.NoError(t, cp.Close())
	entriesAfterCPClose, err := os.ReadDir(evalParent)
	require.NoError(t, err)
	assert.Empty(t, entriesAfterCPClose, "ControlPlane.Close must remove cp.evaluator's workDir")
}

// TestNewAndInitProjectNormalizeSymlinkBeforeParentPaths verifies that New and
// InitProject resolve symlinks before parent ("..") components on CardsDir,
// RunsDir, InitStageDir, and Verification paths, rejecting paths that land
// inside WorkspaceDir through a symlink.
func TestNewAndInitProjectNormalizeSymlinkBeforeParentPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	wsDir := filepath.Join(root, "workspace")
	wsSub := filepath.Join(wsDir, "sub")
	require.NoError(t, os.MkdirAll(wsSub, 0o755))

	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(wsSub, alias))

	// 1. CardsDir = alias + "/../cards" resolves to wsDir/cards and is rejected before openCardStore creates it.
	_, err := New(Options{
		CardsDir:     alias + "/../cards",
		WorkspaceDir: wsDir,
		SurlaVersion: testSurlaVersion,
	})
	require.ErrorContains(t, err, "must be outside the workspace")
	_, statErr := os.Lstat(filepath.Join(wsDir, "cards"))
	assert.ErrorIs(t, statErr, os.ErrNotExist, "rejected CardsDir inside workspace must not be created")

	// 2. RunsDir = alias + "/../runs" is rejected at startup.
	_, err = New(Options{
		CardsDir:     filepath.Join(root, "cards-ok-1"),
		WorkspaceDir: wsDir,
		RunsDir:      alias + "/../runs",
		SurlaVersion: testSurlaVersion,
	})
	require.ErrorContains(t, err, "must be outside the workspace")

	// 3. InitStageDir = alias + "/../stage" is rejected at startup.
	_, err = New(Options{
		CardsDir:     filepath.Join(root, "cards-ok-2"),
		WorkspaceDir: wsDir,
		InitStageDir: alias + "/../stage",
		SurlaVersion: testSurlaVersion,
	})
	require.ErrorContains(t, err, "must be outside workspace")

	// 4. Verification.Plan.ReportDir = alias + "/../reports" is rejected at startup.
	_, err = New(Options{
		CardsDir:     filepath.Join(root, "cards-ok-3"),
		WorkspaceDir: wsDir,
		SurlaVersion: testSurlaVersion,
		Verification: &VerificationConfig{
			RunsDir: filepath.Join(root, "runs-ok"),
			Plan: verify.Plan{
				ReportDir: alias + "/../reports",
			},
		},
	})
	require.ErrorContains(t, err, "must be outside the workspace")
}

// TestSyncDirTreeFDFollowedByRemoveOwnedStagingDirAndFailedWALCleanup verifies
// that syncDirTreeFD does not advance the open directory stream on stageFD, so
// calling removeOwnedStagingDir immediately afterward removes the entire nested
// staging tree, and a failed writeWorkspaceRecord during InitProject cleans up
// both the staging directory and any workspace.json state.
func TestSyncDirTreeFDFollowedByRemoveOwnedStagingDirAndFailedWALCleanup(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stageParent := filepath.Join(root, "stage-parent")
	require.NoError(t, os.MkdirAll(stageParent, 0o700))
	evalParent, err := abspath.Eval(stageParent)
	require.NoError(t, err)

	stageName := ".surla-init-sync-remove"
	stageDir := filepath.Join(evalParent, stageName)
	require.NoError(t, os.MkdirAll(filepath.Join(stageDir, "nested", "sub"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(stageDir, "go.mod"), []byte("module example.com/shop\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(stageDir, "nested", "sub", "file.go"), []byte("package sub\n"), 0o600))

	parentFD, err := unix.Open(evalParent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	defer unix.Close(parentFD)

	stageFD, err := unix.Openat(parentFD, stageName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	require.NoError(t, err)

	var st unix.Stat_t
	require.NoError(t, unix.Fstat(stageFD, &st))
	require.NoError(t, syncDirTreeFD(stageFD, uint64(st.Dev)))
	require.NoError(t, removeOwnedStagingDir(parentFD, stageFD, evalParent, stageName, uint64(st.Dev), st.Ino))
	_, statErr := os.Lstat(stageDir)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "removeOwnedStagingDir must remove the staged tree after syncDirTreeFD")

	// Also exercise InitProject's WAL write failure path end-to-end.
	wsDir := filepath.Join(root, "ws")
	cardsDir := filepath.Join(root, "cards")
	initStage := filepath.Join(root, "init-stage")
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	cp, err := New(Options{
		CardsDir:     cardsDir,
		WorkspaceDir: wsDir,
		InitStageDir: initStage,
		SurlaVersion: testSurlaVersion,
		InitRunner:   fakeInitRunner,
		NewGenerator: func() scaffold.Generator {
			return &fakeInitGenerator{pkg: "shop"}
		},
	})
	require.NoError(t, err)
	defer cp.Close()

	// Create a non-empty directory at workspace.json.tmp so writeWorkspaceRecord fails.
	tmpObstacle := filepath.Join(cp.cards.dir, workspaceRecordFile+".tmp")
	require.NoError(t, os.MkdirAll(filepath.Join(tmpObstacle, "keep"), 0o700))

	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	_, err = cp.InitProject(aliceCtx, &gensurla.InitProjectPayload{
		ModulePath: "example.com/shop",
	})
	require.Error(t, err)

	stageEntries, err := os.ReadDir(cp.initStageDir)
	require.NoError(t, err)
	assert.Empty(t, stageEntries, "failed writeWorkspaceRecord must remove the staged project directory")
	_, walStatErr := os.Lstat(filepath.Join(cp.cards.dir, workspaceRecordFile))
	assert.ErrorIs(t, walStatErr, os.ErrNotExist)
}

// TestInitProjectEmitsMCPProgress verifies that InitProject sends progress
// notifications at each phase boundary and returns the initial starter commit.
func TestInitProjectEmitsMCPProgress(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	wsDir := filepath.Join(root, "ws")
	cardsDir := filepath.Join(root, "cards")
	stageDir := filepath.Join(root, "stage")
	runsDir := filepath.Join(root, "runs")
	reportsDir := filepath.Join(root, "reports")
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	require.NoError(t, os.MkdirAll(runsDir, 0o700))
	require.NoError(t, os.MkdirAll(reportsDir, 0o700))

	cp, err := New(Options{
		CardsDir:     cardsDir,
		WorkspaceDir: wsDir,
		InitStageDir: stageDir,
		RunsDir:      runsDir,
		SurlaVersion: testSurlaVersion,
		InitRunner:   fakeInitRunner,
		NewGenerator: func() scaffold.Generator {
			return &fakeInitGenerator{pkg: "shop"}
		},
		WrittenTestRuns: &WrittenTestRuns{ReportDir: reportsDir, Timeout: time.Minute},
	})
	require.NoError(t, err)
	defer cp.Close()

	srv := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{
		"alice-token": {ID: "alice@example.com"},
	})))
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{` +
		`"name":"init_project","arguments":{"module_path":"example.com/shop"},` +
		`"_meta":{"progressToken":"p-init","io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	reqCtx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer alice-token-guide")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "init_project")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.True(t, strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"), "Content-Type %q", resp.Header.Get("Content-Type"))

	var events []capturedProgressEvent
	var final *mcpResultFrame
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var msg struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		require.NoError(t, json.Unmarshal([]byte(data), &msg))
		if msg.Method == "notifications/progress" {
			require.Nil(t, final, "progress must arrive before the final result")
			assert.Equal(t, "p-init", msg.Params["progressToken"])
			events = append(events, capturedProgressEvent{
				progress: msg.Params["progress"].(float64),
				total:    msg.Params["total"].(float64),
				message:  msg.Params["message"].(string),
			})
			continue
		}
		require.Nil(t, final, "the stream must carry exactly one final result")
		final = new(mcpResultFrame)
		require.NoError(t, json.Unmarshal([]byte(data), final))
	}
	require.NoError(t, scanner.Err())
	require.NotNil(t, final)
	require.False(t, final.Result.IsError, "init_project failed: %s", final.Result.StructuredContent)

	var res struct {
		Dir    string `json:"dir"`
		Commit string `json:"commit"`
	}
	require.NoError(t, json.Unmarshal(final.Result.StructuredContent, &res))
	assert.Equal(t, fakeInitialSuiteCommit, res.Commit)
	require.Len(t, events, 2)
	assert.Equal(t, capturedProgressEvent{
		progress: 0,
		total:    2,
		message:  "Generating, building, testing, and committing the starter project",
	}, events[0])
	assert.Equal(t, capturedProgressEvent{
		progress: 1,
		total:    2,
		message:  "Publishing the initialized starter workspace",
	}, events[1])
}

// TestNewAndInitProjectPreserveSignificantPathWhitespace verifies that
// whitespace in filesystem paths is never stripped before boundary checks or
// directory resolution: an interior path with trailing spaces ("ws /cards") is
// rejected when the workspace is "ws ", an exterior path with trailing spaces
// ("ws /cards ") outside workspace "ws" is accepted and opened at its exact
// path, and init_project rejects a child directory name with leading/trailing
// whitespace (" shop ").
func TestNewAndInitProjectPreserveSignificantPathWhitespace(t *testing.T) {
	t.Parallel()

	t.Run("rejects_cards_runs_and_stage_dirs_inside_workspace_with_trailing_space", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		wsWithSpace := filepath.Join(root, "ws ")
		require.NoError(t, os.MkdirAll(wsWithSpace, 0o755))

		_, err := New(Options{
			WorkspaceDir: wsWithSpace,
			CardsDir:     filepath.Join(wsWithSpace, "cards"),
		})
		require.ErrorContains(t, err, "must be outside the workspace")

		extCards := filepath.Join(root, "cards")
		_, err = New(Options{
			WorkspaceDir: wsWithSpace,
			CardsDir:     extCards,
			RunsDir:      filepath.Join(wsWithSpace, "runs"),
		})
		require.ErrorContains(t, err, "must be outside the workspace")

		_, err = New(Options{
			WorkspaceDir: wsWithSpace,
			CardsDir:     extCards,
			InitStageDir: filepath.Join(wsWithSpace, "stage"),
		})
		require.ErrorContains(t, err, "must be outside workspace")
	})

	t.Run("accepts_exterior_paths_with_trailing_and_interior_spaces", func(t *testing.T) {
		t.Parallel()
		root, err := abspath.Eval(t.TempDir())
		require.NoError(t, err)
		ws := filepath.Join(root, "ws")
		extParentWithSpace := filepath.Join(root, "ws ")
		cardsWithSpace := filepath.Join(extParentWithSpace, "cards ")
		runsWithSpace := filepath.Join(extParentWithSpace, "runs ")
		reportsWithSpace := filepath.Join(extParentWithSpace, "reports ")
		stageWithSpace := filepath.Join(extParentWithSpace, "stage ")
		require.NoError(t, os.MkdirAll(ws, 0o755))
		require.NoError(t, os.MkdirAll(stageWithSpace, 0o700))

		cp, err := New(Options{
			WorkspaceDir: ws,
			CardsDir:     cardsWithSpace,
			RunsDir:      runsWithSpace,
			InitStageDir: stageWithSpace,
			SurlaVersion: testSurlaVersion,
			InitRunner:   fakeInitRunner,
			NewGenerator: func() scaffold.Generator {
				return &fakeInitGenerator{pkg: "shop"}
			},
			Verification: &VerificationConfig{
				RunsDir: runsWithSpace,
				Plan: verify.Plan{
					CandidateRepo:   ws,
					CandidateModule: ".",
					Binaries:        map[string]string{"shop": "./cmd/shop"},
					SuiteRepo:       ws,
					SuiteCommit:     fakeInitialSuiteCommit,
					SuiteModule:     "eval",
					SuiteCommand:    "./cmd/shop-eval",
					ReportDir:       reportsWithSpace,
				},
			},
		})
		require.NoError(t, err)
		defer cp.Close()

		assert.DirExists(t, cardsWithSpace)
		assert.Equal(t, runsWithSpace, cp.runsDir)

		alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
		paddedDir := " shop "
		_, err = cp.InitProject(alice, &gensurla.InitProjectPayload{
			ModulePath: "example.com/shop",
			Dir:        &paddedDir,
		})
		require.ErrorContains(t, err, "single child directory name")

		initRes, err := cp.InitProject(alice, &gensurla.InitProjectPayload{
			ModulePath: "example.com/shop",
		})
		require.NoError(t, err)
		assert.Equal(t, "shop", filepath.Base(initRes.Dir))
	})
}

// TestNewProtectsMonorepoAndPreservesValidLayouts verifies that New rejects
// CardsDir, RunsDir, InitStageDir, Verification.Plan.ReportDir and
// WrittenTestRuns.ReportDir when they lie inside an enclosing Git monorepo
// root R (outside the bound subdirectory R/services/shop),
// while preserving valid parent-directory operator stores (InitStageDir=/ops
// with WorkspaceDir=/ops/project), separate verification source repositories B,
// and no-WorkspaceDir embeddings.
func TestNewProtectsMonorepoAndPreservesValidLayouts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	monorepo := filepath.Join(root, "monorepo")
	wsDir := filepath.Join(monorepo, "services", "shop")
	candidateB := filepath.Join(root, "candidate-b")
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	require.NoError(t, os.MkdirAll(candidateB, 0o755))
	initGitWorktreeForBoundaryTest(t, monorepo)

	// 1. CardsDir, RunsDir, InitStageDir, or ReportDir inside enclosing monorepo R (outside wsDir) is rejected.
	_, err := New(Options{
		WorkspaceDir: wsDir,
		CardsDir:     filepath.Join(monorepo, ".surla", "cards"),
		SurlaVersion: testSurlaVersion,
	})
	require.ErrorContains(t, err, "must be outside the workspace")

	_, err = New(Options{
		WorkspaceDir: wsDir,
		CardsDir:     filepath.Join(root, "cards-ok-1"),
		RunsDir:      filepath.Join(monorepo, ".surla", "runs"),
		SurlaVersion: testSurlaVersion,
	})
	require.ErrorContains(t, err, "must be outside the workspace")

	_, err = New(Options{
		WorkspaceDir: wsDir,
		CardsDir:     filepath.Join(root, "cards-ok-2"),
		InitStageDir: filepath.Join(monorepo, ".surla-stage"),
		SurlaVersion: testSurlaVersion,
	})
	require.ErrorContains(t, err, "must be outside workspace")

	_, err = New(Options{
		WorkspaceDir: wsDir,
		CardsDir:     filepath.Join(root, "cards-ok-3"),
		SurlaVersion: testSurlaVersion,
		Verification: &VerificationConfig{
			RunsDir: filepath.Join(root, "runs-ok-3"),
			Plan: verify.Plan{
				CandidateRepo: ".",
				ReportDir:     filepath.Join(monorepo, "reports"),
			},
		},
	})
	require.ErrorContains(t, err, "must be outside the workspace")

	// 2. The report folder of runs of each change's accepted tests inside enclosing monorepo R is rejected.
	_, err = New(Options{
		WorkspaceDir:    wsDir,
		CardsDir:        filepath.Join(root, "cards-ok-4"),
		RunsDir:         filepath.Join(root, "runs-ok-4"),
		SurlaVersion:    testSurlaVersion,
		WrittenTestRuns: &WrittenTestRuns{ReportDir: filepath.Join(monorepo, "reports"), Timeout: time.Minute},
	})
	require.ErrorContains(t, err, "must be outside the workspace")

	// 3. Valid parent-directory InitStageDir=/ops with WorkspaceDir=/ops/project (outside any Git working tree) is accepted.
	opsParent := filepath.Join(root, "ops")
	opsProject := filepath.Join(opsParent, "project")
	require.NoError(t, os.MkdirAll(opsProject, 0o755))
	require.NoError(t, os.Chmod(opsParent, 0o700))
	cpParent, err := New(Options{
		WorkspaceDir: opsProject,
		CardsDir:     filepath.Join(root, "cards-parent"),
		InitStageDir: opsParent,
		SurlaVersion: testSurlaVersion,
	})
	require.NoError(t, err)
	require.NoError(t, cpParent.Close())

	// 4. Separate verification source repository B (e.g. read-only bare mirror) and no-WorkspaceDir embedding remain accepted.
	cpSeparate, err := New(Options{
		WorkspaceDir: wsDir,
		CardsDir:     filepath.Join(root, "cards-sep"),
		SurlaVersion: testSurlaVersion,
		Verification: &VerificationConfig{
			RunsDir: filepath.Join(candidateB, "runs"),
			Plan: verify.Plan{
				CandidateRepo: candidateB,
				ReportDir:     filepath.Join(candidateB, "reports"),
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, cpSeparate.Close())

	// 5. failInitFault with an empty StepError.Summary falls through to the generic safe message without leaking raw StepError.Args.
	faultErr := failInitFault(
		trace.SpanFromContext(context.Background()),
		"prepare starter project failed",
		&scaffold.StepError{
			Module: ".",
			Args:   []string{"git", "-C", "/private/operator/path", "commit"},
			Log:    "/private/operator/path/01.log",
			Err:    errors.New("exit status 1"),
		},
	)
	require.Error(t, faultErr)
	assert.NotContains(t, faultErr.Error(), "/private/operator/path")
	assert.Contains(t, faultErr.Error(), "prepare starter project failed; inspect the server trace span for diagnostic details")
}

// initGitWorktreeForBoundaryTest initializes a real Git working tree at dir.
func initGitWorktreeForBoundaryTest(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "init", "-q")
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git init %s: %s", dir, string(out))
}

// TestInitProjectAcceptsAnotherSpellingOfTheWorkspace verifies that a server
// started with another spelling of an empty workspace on a case-insensitive
// volume initializes it instead of reporting it replaced.
func TestInitProjectAcceptsAnotherSpellingOfTheWorkspace(t *testing.T) {
	root, err := abspath.Eval(t.TempDir())
	require.NoError(t, err)
	ws := filepath.Join(root, "ws")
	require.NoError(t, os.MkdirAll(ws, 0o755))
	if _, err := os.Stat(filepath.Join(root, "WS")); err != nil {
		t.Skip("needs a case-insensitive volume")
	}
	runs := filepath.Join(root, "ops", "runs")
	cp, err := New(Options{
		WorkspaceDir: filepath.Join(root, "WS"),
		CardsDir:     filepath.Join(root, "ops", "cards"),
		RunsDir:      runs,
		InitStageDir: filepath.Join(root, "ops", "stage"),
		SurlaVersion: testSurlaVersion,
		InitRunner:   fakeInitRunner,
		NewGenerator: func() scaffold.Generator {
			return &fakeInitGenerator{pkg: "shop"}
		},
		Verification: &VerificationConfig{
			RunsDir: runs,
			Plan: verify.Plan{
				CandidateRepo:   ws,
				CandidateModule: ".",
				Binaries:        map[string]string{"shop": "./cmd/shop"},
				SuiteRepo:       ws,
				SuiteCommit:     fakeInitialSuiteCommit,
				SuiteModule:     "eval",
				SuiteCommand:    "./cmd/shop-eval",
				ReportDir:       filepath.Join(root, "ops", "reports"),
			},
		},
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, cp.Close()) }()
	assert.Equal(t, ws, cp.initialWorkspaceRoot, "the stored spelling")

	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	initRes, err := cp.InitProject(alice, &gensurla.InitProjectPayload{ModulePath: "example.com/shop"})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(ws, "shop"), initRes.Dir)
}
