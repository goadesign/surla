package verifyrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/verify"
)

// TestPrepareSnapshotSkipsUnacceptedRunsAndPreservesCompletedInterruptedAndCorruptRecords verifies
// that PrepareSnapshot skips unaccepted run directories while copying completed, interrupted,
// and corrupt run records into the staged snapshot without modifying the legacy workspace.
func TestPrepareSnapshotSkipsUnacceptedRunsAndPreservesCompletedInterruptedAndCorruptRecords(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	wsRoot := filepath.Join(root, "workspace")
	legacyRunsDir := filepath.Join(wsRoot, ".surla", "runs")
	legacyReportsDir := filepath.Join(wsRoot, "reports")
	customReportsDir := filepath.Join(wsRoot, "custom-reports")
	require.NoError(t, os.MkdirAll(legacyRunsDir, 0o755))
	require.NoError(t, os.MkdirAll(legacyReportsDir, 0o755))
	require.NoError(t, os.MkdirAll(customReportsDir, 0o755))

	defaultReportName := strings.Repeat("a", 64) + ".json"
	customReportName := strings.Repeat("b", 64) + ".json"
	require.NoError(t, os.WriteFile(filepath.Join(legacyReportsDir, defaultReportName), []byte(`{"default":true}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(customReportsDir, customReportName), []byte(`{"custom":true}`), 0o600))

	// 1. Pre-request crash folder (lock exists, request.json absent): must be skipped.
	unacceptedID := strings.Repeat("0", 32)
	require.NoError(t, os.MkdirAll(filepath.Join(legacyRunsDir, unacceptedID), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(legacyRunsDir, unacceptedID, lockFile), nil, 0o600))

	// 2. Completed run with default workspace ReportDir and non-empty lock bytes: must preserve lock bytes and rewrite Plan.ReportDir to <publishedReportsRoot>/default.
	completedID := strings.Repeat("1", 32)
	completedDir := filepath.Join(legacyRunsDir, completedID)
	require.NoError(t, os.MkdirAll(completedDir, 0o700))
	lockPayload := []byte("supervisor-lock-bytes\n")
	require.NoError(t, os.WriteFile(filepath.Join(completedDir, lockFile), lockPayload, 0o600))
	completedReq := Request{
		RunID:      completedID,
		Owner:      "alice",
		ChangeID:   "CHG-1",
		AcceptedAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
		Timeout:    2 * time.Minute,
		Plan: verify.Plan{
			ReportDir: legacyReportsDir,
		},
	}
	reqBytes, err := json.Marshal(completedReq)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(completedDir, requestFile), reqBytes, 0o600))
	resBytes, err := json.Marshal(Result{
		Kind:    ResultNoReport,
		Failure: FailureExecution,
		Stage:   string(verify.StageBuild),
		Summary: "The candidate did not build.",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(completedDir, resultFile), resBytes, 0o600))

	// 3. Interrupted run with custom workspace ReportDir (request.json present, result.json absent, unlocked).
	interruptedID := strings.Repeat("2", 32)
	interruptedDir := filepath.Join(legacyRunsDir, interruptedID)
	require.NoError(t, os.MkdirAll(interruptedDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(interruptedDir, lockFile), nil, 0o600))
	interruptedReq := Request{
		RunID:      interruptedID,
		Owner:      "alice",
		ChangeID:   "CHG-1",
		AcceptedAt: time.Date(2026, 11, 8, 12, 2, 0, 0, time.UTC),
		Timeout:    2 * time.Minute,
		Plan: verify.Plan{
			ReportDir: customReportsDir,
		},
	}
	intReqBytes, err := json.Marshal(interruptedReq)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(interruptedDir, requestFile), intReqBytes, 0o600))

	// 4. Corrupt published run (request.json has trailing garbage): must be copied verbatim so Read/List surface the corruption error.
	corruptID := strings.Repeat("3", 32)
	corruptDir := filepath.Join(legacyRunsDir, corruptID)
	require.NoError(t, os.MkdirAll(corruptDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(corruptDir, lockFile), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(corruptDir, requestFile), []byte(`{"run_id":"bad"}trailing`), 0o600))

	// 5. Missing-lock accepted run with valid request.json and completed result.json: lock absence must be preserved so Read/List fail with open run lock error.
	missingLockWithResID := strings.Repeat("4", 32)
	missingLockWithResDir := filepath.Join(legacyRunsDir, missingLockWithResID)
	require.NoError(t, os.MkdirAll(missingLockWithResDir, 0o700))
	missingLockWithResReq := Request{
		RunID:      missingLockWithResID,
		Owner:      "alice",
		ChangeID:   "CHG-1",
		AcceptedAt: time.Date(2026, 11, 8, 12, 3, 0, 0, time.UTC),
		Timeout:    2 * time.Minute,
		Plan:       verify.Plan{ReportDir: legacyReportsDir},
	}
	mlResReqBytes, err := json.Marshal(missingLockWithResReq)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(missingLockWithResDir, requestFile), mlResReqBytes, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(missingLockWithResDir, resultFile), resBytes, 0o600))

	// 6. Missing-lock accepted run with valid request.json and missing result.json: lock absence must be preserved so Read/List fail with open run lock error instead of interrupted.
	missingLockNoResID := strings.Repeat("5", 32)
	missingLockNoResDir := filepath.Join(legacyRunsDir, missingLockNoResID)
	require.NoError(t, os.MkdirAll(missingLockNoResDir, 0o700))
	missingLockNoResReq := Request{
		RunID:      missingLockNoResID,
		Owner:      "alice",
		ChangeID:   "CHG-1",
		AcceptedAt: time.Date(2026, 11, 8, 12, 4, 0, 0, time.UTC),
		Timeout:    2 * time.Minute,
		Plan:       verify.Plan{ReportDir: legacyReportsDir},
	}
	mlNoResReqBytes, err := json.Marshal(missingLockNoResReq)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(missingLockNoResDir, requestFile), mlNoResReqBytes, 0o600))

	stageDir := filepath.Join(root, "stage")
	stageRunsDir := filepath.Join(stageDir, "runs")
	stageReportsRoot := filepath.Join(stageDir, "reports")
	publishedReportsRoot := filepath.Join(root, "published", "reports")

	releaseLocks, err := PrepareSnapshot(
		legacyRunsDir,
		legacyReportsDir,
		stageRunsDir,
		stageReportsRoot,
		publishedReportsRoot,
		wsRoot,
	)
	require.NoError(t, err)
	require.NotNil(t, releaseLocks)
	require.NoError(t, releaseLocks())

	// Unaccepted run folder is omitted from stageRunsDir while original folder remains untouched.
	_, err = os.Stat(filepath.Join(stageRunsDir, unacceptedID))
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(legacyRunsDir, unacceptedID, lockFile))
	assert.NoError(t, err)

	// Completed run is readable, preserves original lock bytes, and its Plan.ReportDir points to <publishedReportsRoot>/default.
	gotReq, gotState, err := Read(stageRunsDir, completedID)
	require.NoError(t, err)
	assert.Equal(t, StateNotCompleted, gotState.Kind)
	assert.Equal(t, FailureExecution, gotState.Result.Failure)
	assert.Equal(t, filepath.Join(publishedReportsRoot, "default"), gotReq.Plan.ReportDir)
	copiedLockBytes, err := os.ReadFile(filepath.Join(stageRunsDir, completedID, lockFile))
	require.NoError(t, err)
	assert.Equal(t, lockPayload, copiedLockBytes)

	// Legacy original request.json is untouched.
	origCompletedReqBytes, err := os.ReadFile(filepath.Join(completedDir, requestFile))
	require.NoError(t, err)
	assert.Equal(t, reqBytes, origCompletedReqBytes)

	// Interrupted run is readable and reports interrupted status.
	gotIntReq, gotIntState, err := Read(stageRunsDir, interruptedID)
	require.NoError(t, err)
	assert.Equal(t, StateNotCompleted, gotIntState.Kind)
	assert.Equal(t, FailureInterrupted, gotIntState.Result.Failure)
	assert.Equal(t, summarySupervisorDied, gotIntState.Result.Summary)
	assert.Contains(t, gotIntReq.Plan.ReportDir, "dir-")

	// Corrupt run is preserved verbatim and fails strict decoding on Read and List.
	_, _, err = Read(stageRunsDir, corruptID)
	require.ErrorContains(t, err, "unexpected trailing data")
	_, err = List(stageRunsDir, "alice", "CHG-1")
	require.ErrorContains(t, err, "unexpected trailing data")
	require.NoError(t, os.RemoveAll(filepath.Join(stageRunsDir, corruptID)))

	// Both missing-lock runs preserve lock absence and fail Read and List with open run lock (os.ErrNotExist).
	for _, mlID := range []string{missingLockWithResID, missingLockNoResID} {
		_, statErr := os.Stat(filepath.Join(stageRunsDir, mlID, lockFile))
		assert.ErrorIs(t, statErr, os.ErrNotExist)
		mlReq, _, readErr := Read(stageRunsDir, mlID)
		require.ErrorContains(t, readErr, "open run lock")
		assert.ErrorIs(t, readErr, os.ErrNotExist)
		assert.Equal(t, "alice", mlReq.Owner)
		_, listErr := List(stageRunsDir, "alice", "CHG-1")
		require.ErrorContains(t, listErr, "open run lock")
		assert.ErrorIs(t, listErr, os.ErrNotExist)
		require.NoError(t, os.RemoveAll(filepath.Join(stageRunsDir, mlID)))
	}

	// Removing the corrupt and missing-lock runs lets List return both valid imported runs in AcceptedAt descending order.
	runs, err := List(stageRunsDir, "alice", "CHG-1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, interruptedID, runs[0].Request.RunID)
	assert.Equal(t, completedID, runs[1].Request.RunID)

	// imported_paths.json records both copied report file paths.
	pathsRaw, err := os.ReadFile(filepath.Join(stageRunsDir, importedPathsFile))
	require.NoError(t, err)
	var paths importedWorkspacePaths
	require.NoError(t, json.Unmarshal(pathsRaw, &paths))
	assert.Equal(t, 1, paths.Version)
	assert.Len(t, paths.ReportFiles, 2)
}

// TestPrepareSnapshotRejectsActiveLegacyRunWriter verifies that PrepareSnapshot fails
// when a legacy run directory holds an active exclusive flock.
func TestPrepareSnapshotRejectsActiveLegacyRunWriter(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	wsRoot := filepath.Join(root, "workspace")
	legacyRunsDir := filepath.Join(wsRoot, ".surla", "runs")
	runID := strings.Repeat("f", 32)
	runDir := filepath.Join(legacyRunsDir, runID)
	require.NoError(t, os.MkdirAll(runDir, 0o700))

	lockPath := filepath.Join(runDir, lockFile)
	lockF, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	defer lockF.Close()
	require.NoError(t, syscall.Flock(int(lockF.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))

	stageDir := filepath.Join(root, "stage")
	_, err = PrepareSnapshot(
		legacyRunsDir,
		filepath.Join(wsRoot, "reports"),
		filepath.Join(stageDir, "runs"),
		filepath.Join(stageDir, "reports"),
		filepath.Join(root, "published", "reports"),
		wsRoot,
	)
	require.ErrorContains(t, err, "has an active writer")
}

// TestResolveImportedReportDirIgnoresPostImportWorkspaceSymlinks verifies that ResolveImportedReportDir
// resolves legacy workspace report directories without following post-import symlinks inside the workspace.
func TestResolveImportedReportDirIgnoresPostImportWorkspaceSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	wsRoot := filepath.Join(root, "workspace")
	publishedReportsRoot := filepath.Join(root, "imported", "reports")
	require.NoError(t, os.MkdirAll(wsRoot, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(publishedReportsRoot, "default"), 0o700))

	// Even if a builder later replaces <workspace>/reports with a symlink to an external directory,
	// ResolveImportedReportDir resolves <workspace>/reports without following symlinks inside wsRoot.
	externalDir := filepath.Join(root, "external-target")
	require.NoError(t, os.MkdirAll(externalDir, 0o755))
	require.NoError(t, os.Symlink(externalDir, filepath.Join(wsRoot, "reports")))

	resolved, err := ResolveImportedReportDir(publishedReportsRoot, wsRoot, filepath.Join(wsRoot, "reports"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(publishedReportsRoot, "default"), resolved)

	// External configured paths are returned unchanged.
	extResolved, err := ResolveImportedReportDir(publishedReportsRoot, wsRoot, externalDir)
	require.NoError(t, err)
	assert.Equal(t, externalDir, extResolved)

	// Non-directory entry under publishedReportsRoot returns an error.
	customWSReportDir := filepath.Join(wsRoot, "custom-reports")
	evalWS, err := evalExistingAncestor(wsRoot)
	require.NoError(t, err)
	dirKey := importedReportDirKey(evalWS, filepath.Join(evalWS, "custom-reports"))
	require.NoError(t, os.WriteFile(filepath.Join(publishedReportsRoot, dirKey), []byte("not-a-dir"), 0o600))
	_, err = ResolveImportedReportDir(publishedReportsRoot, wsRoot, customWSReportDir)
	require.ErrorContains(t, err, "is not a regular directory")
}
