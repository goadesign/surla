package designeval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// incomplete is a snapshot that evaluate rejects at once, after admission
// created the workspace.
var incomplete = map[string]string{"design/design.go": "package design\n"}

func TestNewEvaluatorCreatesNothingAndClosesCleanly(t *testing.T) {
	tmp := isolatedTempDir(t)
	ev := NewEvaluator()
	assert.Empty(t, entries(t, tmp), "a host that never evaluates creates no workspace")
	require.NoError(t, ev.Close())
	assert.Empty(t, entries(t, tmp))
}

func TestCloseRemovesTheWorkspaceAfterAFailedEvaluation(t *testing.T) {
	tmp := isolatedTempDir(t)
	ev := NewEvaluator()
	_, err := ev.Evaluate(t.Context(), incomplete)
	require.Error(t, err)
	require.Len(t, entries(t, tmp), 1, "the first admitted evaluation creates the workspace")
	require.NoError(t, ev.Close())
	assert.Empty(t, entries(t, tmp))
}

func TestEvaluateAfterCloseFailsAndCreatesNothing(t *testing.T) {
	tmp := isolatedTempDir(t)
	ev := NewEvaluator()
	require.NoError(t, ev.Close())
	_, err := ev.Evaluate(t.Context(), incomplete)
	require.ErrorIs(t, err, errEvaluatorClosed)
	assert.Empty(t, entries(t, tmp))
}

func TestEveryCloseReturnsTheOneCleanupResult(t *testing.T) {
	cases := []struct {
		name string
		// prepare makes the cleanup fail, or keep the workspace.
		prepare func(t *testing.T, ev *Evaluator)
		want    string
	}{
		{name: "removed", prepare: func(*testing.T, *Evaluator) {}},
		{name: "removal fails", prepare: func(t *testing.T, ev *Evaluator) {
			locked := filepath.Join(ev.workDir, "locked")
			require.NoError(t, os.MkdirAll(filepath.Join(locked, "inner"), 0o700))
			require.NoError(t, os.Chmod(locked, 0o500))
			t.Cleanup(func() { require.NoError(t, os.Chmod(locked, 0o700)) })
		}, want: "permission denied"},
		{name: "kept for a stuck build", prepare: func(_ *testing.T, ev *Evaluator) {
			ev.unsafe = true
		}, want: "is kept because build processes may still use it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatedTempDir(t)
			ev := NewEvaluator()
			_, err := ev.Evaluate(t.Context(), incomplete)
			require.Error(t, err)
			tc.prepare(t, ev)
			first, second := ev.Close(), ev.Close()
			if tc.want == "" {
				assert.NoError(t, first)
				assert.NoError(t, second)
				return
			}
			require.ErrorContains(t, first, tc.want)
			assert.Same(t, first, second, "a repeated Close returns the same completed result")
		})
	}
}

func TestCloseWaitsForAdmittedEvaluations(t *testing.T) {
	tmp := isolatedTempDir(t)
	ev := NewEvaluator()
	// An evaluation that has been admitted but has not finished its own
	// cleanup, as one still removing its temporary directory would be.
	_, err := ev.admit()
	require.NoError(t, err)

	results := make(chan error, 2)
	for range 2 {
		go func() { results <- ev.Close() }()
	}
	select {
	case err := <-results:
		t.Fatalf("Close returned %v while an evaluation was still active", err)
	case <-time.After(200 * time.Millisecond):
	}
	assert.Len(t, entries(t, tmp), 1, "the workspace is kept while an evaluation is active")
	ev.active.Done()
	for range 2 {
		assert.NoError(t, <-results)
	}
	assert.Empty(t, entries(t, tmp))
}

func TestAdmissionRacingCloseNeverOutlivesIt(t *testing.T) {
	tmp := isolatedTempDir(t)
	ev := NewEvaluator()
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if _, err := ev.admit(); err != nil {
				assert.ErrorIs(t, err, errEvaluatorClosed)
				return
			}
			ev.active.Done()
		})
	}
	wg.Go(func() { assert.NoError(t, ev.Close()) })
	wg.Wait()
	_, err := ev.admit()
	require.ErrorIs(t, err, errEvaluatorClosed)
	assert.Empty(t, entries(t, tmp), "nothing admitted after Close recreates the workspace")
}

func TestCloseCancelsARunningEvaluationAndAFailureIsNotRemembered(t *testing.T) {
	tmp := isolatedTempDir(t)
	// The go on PATH hangs, as a slow go env would, until it is killed.
	fakeGo(t, "#!/bin/sh\nexec sleep 600\n")
	ev := NewEvaluator()
	done := make(chan error, 1)
	go func() {
		_, err := ev.Evaluate(context.Background(), validSnapshotStub())
		done <- err
	}()
	require.Eventually(t, func() bool { return len(entries(t, tmp)) == 1 }, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, ev.Close(), "Close cancels the evaluation, waits for it, and removes the workspace")
	require.Error(t, <-done)
	assert.Empty(t, entries(t, tmp))

	// A second evaluator runs go env again: nothing about the first
	// failure is remembered.
	fakeGo(t, "#!/bin/sh\necho 'second go env reached' >&2\nexit 1\n")
	ev2 := NewEvaluator()
	_, err := ev2.Evaluate(t.Context(), validSnapshotStub())
	require.ErrorContains(t, err, "resolve go env")
	require.NoError(t, ev2.Close())
}

func TestACanceledEvaluationDoesNotAffectTheNext(t *testing.T) {
	isolatedTempDir(t)
	fakeGo(t, "#!/bin/sh\nexec sleep 600\n")
	ev := NewEvaluator()
	t.Cleanup(func() { require.NoError(t, ev.Close()) })
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := ev.Evaluate(ctx, validSnapshotStub())
	require.ErrorContains(t, err, "resolve go env")

	fakeGo(t, "#!/bin/sh\necho 'second go env reached' >&2\nexit 1\n")
	_, err = ev.Evaluate(t.Context(), validSnapshotStub())
	require.ErrorContains(t, err, "resolve go env", "the next evaluation resolves the toolchain again")
	var exitErr interface{ ExitCode() int }
	require.True(t, errors.As(err, &exitErr))
	assert.Equal(t, 1, exitErr.ExitCode(), "the second go env ran and failed on its own")
}

func TestTwoEvaluatorsOwnSeparateWorkspaces(t *testing.T) {
	tmp := isolatedTempDir(t)
	a, b := NewEvaluator(), NewEvaluator()
	_, err := a.Evaluate(t.Context(), incomplete)
	require.Error(t, err)
	_, err = b.Evaluate(t.Context(), incomplete)
	require.Error(t, err)
	require.Len(t, entries(t, tmp), 2)
	require.NoError(t, a.Close())
	assert.Equal(t, []string{filepath.Base(b.workDir)}, entries(t, tmp), "closing one evaluator leaves the other's workspace")
	require.NoError(t, b.Close())
	assert.Empty(t, entries(t, tmp))
}

func TestAStuckBuildMarksTheEvaluatorUnsafeForGood(t *testing.T) {
	isolatedTempDir(t)
	ev := NewEvaluator()
	_, err := ev.admit()
	require.NoError(t, err)
	// Evaluate's deferred bookkeeping, given a stuck build's error.
	ev.finish(errors.Join(&stuckBuildError{err: errors.New("group did not empty")}))
	_, err = ev.Evaluate(t.Context(), incomplete)
	require.Error(t, err, "a later evaluation still runs")
	require.ErrorContains(t, ev.Close(), "is kept because build processes may still use it", "a later evaluation does not clear the mark")
}

// isolatedTempDir points TMPDIR at an empty directory owned by the test and
// returns it, so the test sees exactly the workspaces it creates.
func isolatedTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

// entries lists dir's top-level names.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(list))
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

// fakeGo puts a go command running script first on PATH.
func fakeGo(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o700))
	t.Setenv("PATH", bin+":/usr/bin:/bin")
}

// validSnapshotStub passes snapshot validation, so evaluation reaches go env.
func validSnapshotStub() map[string]string {
	return map[string]string{
		"go.mod":           "module example.com/design\n\ngo 1.24\n",
		"go.sum":           "",
		"design/design.go": "package design\n",
	}
}
