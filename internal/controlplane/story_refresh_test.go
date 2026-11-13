// This file tests bound-workspace Story refresh operations.
package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	gensurlatools "github.com/goadesign/surla/gen/surla/toolsets/surla"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/testmodule"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStoryRefreshPreconditionsAndExpectedAbsence verifies authentication,
// unbound-workspace errors, missing-go.mod errors without "pass design in the
// request" advice, expected absence ("none") before any refresh and after a
// server restart, and replaced-workspace rejection.
func TestStoryRefreshPreconditionsAndExpectedAbsence(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})

	t.Run("unauthenticated callers are rejected", func(t *testing.T) {
		cp := newControlPlane(t, Options{})
		_, err := cp.RefreshStory(context.Background())
		require.ErrorContains(t, err, "authenticated principal is required")
		_, err = cp.GetStoryRefresh(context.Background())
		require.ErrorContains(t, err, "authenticated principal is required")
	})

	t.Run("no bound workspace returns not_implemented naming the enabling step", func(t *testing.T) {
		cp := newControlPlane(t, Options{})
		_, err := cp.RefreshStory(alice)
		assertServiceError(t, err, "not_implemented")
		assert.ErrorContains(t, err, "start Surla from a project directory")
		assert.NotContains(t, err.Error(), "pass design in the request")

		_, err = cp.GetStoryRefresh(alice)
		assertServiceError(t, err, "not_implemented")
		assert.ErrorContains(t, err, "start Surla from a project directory")
		assert.NotContains(t, err.Error(), "pass design in the request")
	})

	t.Run("bound workspace without go.mod returns none on get and unprocessable on refresh", func(t *testing.T) {
		wsDir := t.TempDir()
		cp := newControlPlane(t, Options{WorkspaceDir: wsDir})
		srv := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{
			"alice-token": {ID: "alice@example.com"},
		})))
		t.Cleanup(srv.Close)

		noneRes, err := cp.GetStoryRefresh(alice)
		require.NoError(t, err)
		assert.Equal(t, storyRefreshStateNone, noneRes.State)
		assert.Nil(t, noneRes.OpID)
		assert.Nil(t, noneRes.SnapshotDigest)
		assert.Empty(t, noneRes.Stories)
		assert.Nil(t, noneRes.Diagnostic)
		assert.Contains(t, noneRes.Message, "refresh_story")

		req, err := http.NewRequestWithContext(alice, http.MethodGet, srv.URL+"/workspace/story/refresh", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer alice-token")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		_, err = cp.RefreshStory(alice)
		assertServiceError(t, err, "unprocessable")
		assert.ErrorContains(t, err, "go.mod")
		assert.ErrorContains(t, err, "init_project")
		assert.NotContains(t, err.Error(), "pass design in the request")

		status, body := postJSON(t, srv.URL+"/workspace/story/refresh", "alice-token", "")
		assert.Equal(t, http.StatusUnprocessableEntity, status, body)
		assert.Contains(t, body, "go.mod")
		assert.NotContains(t, body, "pass design in the request")
	})

	t.Run("invalid UTF-8 and replaced workspace directory are rejected as unprocessable", func(t *testing.T) {
		wsRoot := t.TempDir()
		wsDir := filepath.Join(wsRoot, "project")
		require.NoError(t, os.MkdirAll(filepath.Join(wsDir, "design"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(wsDir, "go.mod"), []byte("module example.com/orders\n\ngo 1.24\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(wsDir, "design", "design.go"), []byte("package design\n// \xff\n"), 0o600))

		cp := newControlPlane(t, Options{WorkspaceDir: wsDir})
		_, err := cp.RefreshStory(alice)
		assertServiceError(t, err, "unprocessable")
		assert.ErrorContains(t, err, "not valid UTF-8")

		// Replace the directory at wsDir with a new inode.
		require.NoError(t, os.RemoveAll(wsDir))
		require.NoError(t, os.MkdirAll(wsDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(wsDir, "go.mod"), []byte("module example.com/orders\n\ngo 1.24\n"), 0o600))

		_, err = cp.RefreshStory(alice)
		assertServiceError(t, err, "unprocessable")
		assert.ErrorContains(t, err, "was replaced or removed after startup")
	})

	t.Run("restart reports expected absence and writes nothing to disk", func(t *testing.T) {
		wsDir := t.TempDir()
		cardsDir := filepath.Join(t.TempDir(), "cards")
		writeWorkspaceFiles(t, wsDir, map[string]string{
			"go.mod": "module example.com/orders\n\ngo 1.24\n",
		})
		cp1, err := New(Options{CardsDir: cardsDir, WorkspaceDir: wsDir})
		require.NoError(t, err)
		started, err := cp1.RefreshStory(alice)
		require.NoError(t, err)
		require.NotNil(t, started.OpID)
		require.Eventually(t, func() bool {
			cur, getErr := cp1.GetStoryRefresh(alice)
			return getErr == nil && cur.State == storyRefreshStateFailed
		}, 5*time.Second, 10*time.Millisecond)
		require.NoError(t, cp1.Close())

		entries, err := os.ReadDir(cardsDir)
		require.NoError(t, err)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		assert.Equal(t, []string{"designeval", "lock"}, names, "Story refresh writes no card or refresh files to cardsDir")
		evalEntries, err := os.ReadDir(filepath.Join(cardsDir, "designeval"))
		require.NoError(t, err)
		assert.Empty(t, evalEntries, "Close removes the designeval build workspace")

		cp2, err := New(Options{CardsDir: cardsDir, WorkspaceDir: wsDir})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, cp2.Close()) })
		afterRestart, err := cp2.GetStoryRefresh(alice)
		require.NoError(t, err)
		assert.Equal(t, storyRefreshStateNone, afterRestart.State)
		assert.Contains(t, afterRestart.Message, "refresh_story")
	})
}

// TestSlowStoryRefreshDoesNotBlockGetChangeVerifyChangeOrProposeChange verifies
// that a slow Story evaluation holds no control-plane lock and blocks neither
// propose_change, get_change, nor verify_change, and that refreshing an
// unchanged snapshot while running reuses the running operation.
func TestSlowStoryRefreshDoesNotBlockGetChangeVerifyChangeOrProposeChange(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	wsDir := t.TempDir()
	files := validWorkspaceStub("package design\n")
	writeWorkspaceFiles(t, wsDir, files)
	wantDigest := designeval.SnapshotDigest(files)

	markerDir := t.TempDir()
	startedFile := filepath.Join(markerDir, "started")
	installFakeGo(t, "#!/bin/sh\n: > "+startedFile+"\nexec sleep 600\n")

	cp := newControlPlane(t, Options{WorkspaceDir: wsDir})

	first, err := cp.RefreshStory(alice)
	require.NoError(t, err)
	require.NotNil(t, first.OpID)
	require.NotNil(t, first.SnapshotDigest)
	assert.Equal(t, storyRefreshStateRunning, first.State)
	assert.Equal(t, wantDigest, *first.SnapshotDigest)
	assert.Empty(t, first.Stories)

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(startedFile)
		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)

	// Refreshing an unchanged snapshot while running returns the same operation.
	reused, err := cp.RefreshStory(alice)
	require.NoError(t, err)
	require.NotNil(t, reused.OpID)
	assert.Equal(t, *first.OpID, *reused.OpID)
	assert.Equal(t, storyRefreshStateRunning, reused.State)

	// propose_change, get_change, and verify_change do not block on the running evaluation.
	var (
		proposed *gensurla.ChangeCardResult
		propErr  error
		gotCard  *gensurla.ChangeCardResult
		getErr   error
		verErr   error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		proposed, propErr = cp.ProposeChange(alice, &gensurla.ProposeChangePayload{
			ChangeID: "chg-unblocked",
			Prompt:   "Refunds up to $50 are instant.",
			Analysis: refundAnalysis("Refunds up to $50 are instant."),
		})
		if propErr != nil {
			return
		}
		gotCard, getErr = cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: proposed.ChangeID})
		_, verErr = cp.VerifyChange(alice, &gensurla.VerifyChangePayload{
			ChangeID:        proposed.ChangeID,
			RequestRevision: proposed.RequestRevision,
			CandidateCommit: strings.Repeat("a", 40),
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("propose_change, get_change, or verify_change blocked on a slow Story refresh")
	}
	require.NoError(t, propErr)
	require.NoError(t, getErr)
	assert.Equal(t, proposed.RequestRevision, gotCard.RequestRevision)
	assertServiceError(t, verErr, "not_implemented")

	status, err := cp.GetStoryRefresh(alice)
	require.NoError(t, err)
	require.NotNil(t, status.OpID)
	assert.Equal(t, *first.OpID, *status.OpID)
	assert.Equal(t, storyRefreshStateRunning, status.State)
}

// TestStoryRefreshSurvivesClientDisconnect verifies that canceling the caller's
// request context after RefreshStory returns does not cancel the background
// Story evaluation.
func TestStoryRefreshSurvivesClientDisconnect(t *testing.T) {
	wsDir := t.TempDir()
	writeWorkspaceFiles(t, wsDir, validWorkspaceStub("package design\n"))

	gateDir := t.TempDir()
	startedFile := filepath.Join(gateDir, "started")
	releaseFile := filepath.Join(gateDir, "release")
	installFakeGo(t, "#!/bin/sh\n: > "+startedFile+"\nwhile [ ! -f "+releaseFile+" ]; do sleep 0.02; done\necho 'reached after client disconnect' >&2\nexit 1\n")

	cp := newControlPlane(t, Options{WorkspaceDir: wsDir})
	reqCtx, reqCancel := context.WithCancel(WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser}))

	started, err := cp.RefreshStory(reqCtx)
	require.NoError(t, err)
	require.NotNil(t, started.OpID)
	assert.Equal(t, storyRefreshStateRunning, started.State)

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(startedFile)
		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)

	// Simulate client disconnect by canceling the request context.
	reqCancel()

	readCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	stillRunning, err := cp.GetStoryRefresh(readCtx)
	require.NoError(t, err)
	assert.Equal(t, storyRefreshStateRunning, stillRunning.State)

	// Let the background command finish and verify it was not canceled by reqCancel.
	require.NoError(t, os.WriteFile(releaseFile, []byte("ok\n"), 0o600))
	require.Eventually(t, func() bool {
		cur, getErr := cp.GetStoryRefresh(readCtx)
		return getErr == nil && cur.State == storyRefreshStateFailed
	}, 5*time.Second, 10*time.Millisecond)
}

// TestStoryRefreshCloseCancelsWaitsAndWritesNothingAfterwards verifies that
// Close during a running Story refresh cancels the evaluation, waits for it to
// exit before returning, leaves no operation running or published, and writes
// nothing to disk afterwards.
func TestStoryRefreshCloseCancelsWaitsAndWritesNothingAfterwards(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	wsDir := t.TempDir()
	cardsDir := filepath.Join(t.TempDir(), "cards")
	writeWorkspaceFiles(t, wsDir, validWorkspaceStub("package design\n"))

	markerDir := t.TempDir()
	startedFile := filepath.Join(markerDir, "started")
	installFakeGo(t, "#!/bin/sh\n: > "+startedFile+"\nexec sleep 600\n")

	cp, err := New(Options{CardsDir: cardsDir, WorkspaceDir: wsDir})
	require.NoError(t, err)

	res, err := cp.RefreshStory(alice)
	require.NoError(t, err)
	require.NotNil(t, res.OpID)
	assert.Equal(t, storyRefreshStateRunning, res.State)

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(startedFile)
		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, cp.Close())

	// After Close returns, no Story refresh operation remains running or published.
	afterClose, err := cp.GetStoryRefresh(alice)
	require.NoError(t, err)
	assert.Equal(t, storyRefreshStateNone, afterClose.State)

	// Starting a new refresh after Close is refused as unavailable.
	_, err = cp.RefreshStory(alice)
	assertServiceError(t, err, "unavailable")

	// Nothing was written to cardsDir beyond the lock file and empty designeval parent.
	entries, err := os.ReadDir(cardsDir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"designeval", "lock"}, names)
	evalEntries, err := os.ReadDir(filepath.Join(cardsDir, "designeval"))
	require.NoError(t, err)
	assert.Empty(t, evalEntries)
}

// TestStoryRefreshReplacementFailureRetryAndExpiry verifies that:
//  1. refreshing a changed snapshot cancels and replaces the prior operation, and
//     the replaced or late evaluation publishes nothing;
//  2. the same snapshot after a failed run evaluates again (and withholds
//     operator proxy credentials from module commands);
//  3. the same snapshot after an expired run reads failed with a diagnostic
//     naming the deadline and evaluates again on the next refresh.
func TestStoryRefreshReplacementFailureRetryAndExpiry(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})

	t.Run("changed snapshot replaces running operation and replaced evaluation publishes nothing", func(t *testing.T) {
		wsDir := t.TempDir()
		v1Files := validWorkspaceStub("package design\n// v1\n")
		writeWorkspaceFiles(t, wsDir, v1Files)
		v1Digest := designeval.SnapshotDigest(v1Files)

		gateDir := t.TempDir()
		started1 := filepath.Join(gateDir, "started1")
		started2 := filepath.Join(gateDir, "started2")
		release2 := filepath.Join(gateDir, "release2")
		installFakeGo(t, "#!/bin/sh\nif [ ! -f "+started1+" ]; then\n  : > "+started1+"\n  exec sleep 600\nfi\n: > "+started2+"\nwhile [ ! -f "+release2+" ]; do sleep 0.02; done\nexit 1\n")

		cp := newControlPlane(t, Options{WorkspaceDir: wsDir})
		op1, err := cp.RefreshStory(alice)
		require.NoError(t, err)
		require.NotNil(t, op1.OpID)
		assert.Equal(t, v1Digest, *op1.SnapshotDigest)

		require.Eventually(t, func() bool {
			_, statErr := os.Stat(started1)
			return statErr == nil
		}, 5*time.Second, 10*time.Millisecond)

		// Change the workspace snapshot and refresh again.
		v2Files := validWorkspaceStub("package design\n// v2\n")
		writeWorkspaceFiles(t, wsDir, v2Files)
		v2Digest := designeval.SnapshotDigest(v2Files)
		require.NotEqual(t, v1Digest, v2Digest)

		op2, err := cp.RefreshStory(alice)
		require.NoError(t, err)
		require.NotNil(t, op2.OpID)
		assert.NotEqual(t, *op1.OpID, *op2.OpID)
		assert.Equal(t, v2Digest, *op2.SnapshotDigest)
		assert.Equal(t, storyRefreshStateRunning, op2.State)

		require.Eventually(t, func() bool {
			_, statErr := os.Stat(started2)
			return statErr == nil
		}, 5*time.Second, 10*time.Millisecond)

		// While op2 is still running, op1 (canceled on replacement) has not
		// overwritten op2 in the map.
		cur, err := cp.GetStoryRefresh(alice)
		require.NoError(t, err)
		require.NotNil(t, cur.OpID)
		assert.Equal(t, *op2.OpID, *cur.OpID)
		assert.Equal(t, storyRefreshStateRunning, cur.State)

		require.NoError(t, os.WriteFile(release2, []byte("ok\n"), 0o600))
		require.Eventually(t, func() bool {
			got, getErr := cp.GetStoryRefresh(alice)
			return getErr == nil && got.State == storyRefreshStateFailed && got.OpID != nil && *got.OpID == *op2.OpID
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("same snapshot after a failed run evaluates again", func(t *testing.T) {
		wsDir := t.TempDir()
		files := validWorkspaceStub("package design\n")
		writeWorkspaceFiles(t, wsDir, files)

		countDir := t.TempDir()
		run1 := filepath.Join(countDir, "run1")
		run2 := filepath.Join(countDir, "run2")
		installFakeGo(t, "#!/bin/sh\nif [ ! -f "+run1+" ]; then\n  : > "+run1+"\n  echo 'https://user:secret-token@proxy.internal.example/mod' >&2\n  exit 1\nfi\n: > "+run2+"\nexit 1\n")

		cp := newControlPlane(t, Options{WorkspaceDir: wsDir})
		first, err := cp.RefreshStory(alice)
		require.NoError(t, err)
		require.NotNil(t, first.OpID)

		var failed *gensurla.StoryRefreshResult
		require.Eventually(t, func() bool {
			cur, getErr := cp.GetStoryRefresh(alice)
			if getErr != nil {
				return false
			}
			failed = cur
			return cur.State == storyRefreshStateFailed
		}, 5*time.Second, 10*time.Millisecond)
		require.NotNil(t, failed.Diagnostic)
		assert.Contains(t, *failed.Diagnostic, "Story evaluation failed while")
		assert.NotContains(t, *failed.Diagnostic, "secret-token")
		assert.NotContains(t, *failed.Diagnostic, "proxy.internal.example")

		// Refreshing the exact same snapshot after failure starts a new evaluation.
		second, err := cp.RefreshStory(alice)
		require.NoError(t, err)
		require.NotNil(t, second.OpID)
		assert.NotEqual(t, *first.OpID, *second.OpID)

		require.Eventually(t, func() bool {
			_, statErr := os.Stat(run2)
			return statErr == nil
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("expired refresh reports deadline in diagnostic and re-evaluates on next refresh", func(t *testing.T) {
		wsDir := t.TempDir()
		writeWorkspaceFiles(t, wsDir, validWorkspaceStub("package design\n"))

		countDir := t.TempDir()
		run1 := filepath.Join(countDir, "run1")
		run2 := filepath.Join(countDir, "run2")
		installFakeGo(t, "#!/bin/sh\nif [ ! -f "+run1+" ]; then\n  : > "+run1+"\n  exec sleep 600\nfi\n: > "+run2+"\nexec sleep 600\n")

		var (
			deadlineCalls int
			expireFirst   context.CancelCauseFunc
		)
		cp := newControlPlane(t, Options{WorkspaceDir: wsDir})
		cp.storyRefreshWithTimeout = func(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
			assert.Equal(t, defaultStoryRefreshTimeout, limit)
			deadlineCalls++
			if deadlineCalls == 1 {
				evalCtx, cancelCause := context.WithCancelCause(ctx)
				expireFirst = cancelCause
				return evalCtx, func() { cancelCause(nil) }
			}
			return context.WithTimeout(ctx, limit)
		}

		first, err := cp.RefreshStory(alice)
		require.NoError(t, err)
		require.NotNil(t, first.OpID)

		require.Eventually(t, func() bool {
			_, statErr := os.Stat(run1)
			return statErr == nil
		}, 5*time.Second, 10*time.Millisecond)
		require.NotNil(t, expireFirst)
		expireFirst(context.DeadlineExceeded)

		var expired *gensurla.StoryRefreshResult
		require.Eventually(t, func() bool {
			cur, getErr := cp.GetStoryRefresh(alice)
			if getErr != nil {
				return false
			}
			expired = cur
			return cur.State == storyRefreshStateFailed
		}, 5*time.Second, 10*time.Millisecond)
		require.NotNil(t, expired.Diagnostic)
		assert.Contains(t, *expired.Diagnostic, "Story refresh timed out after 5m0s")

		// Refreshing the same snapshot after expiry starts a fresh evaluation.
		second, err := cp.RefreshStory(alice)
		require.NoError(t, err)
		require.NotNil(t, second.OpID)
		assert.NotEqual(t, *first.OpID, *second.OpID)

		require.Eventually(t, func() bool {
			_, statErr := os.Stat(run2)
			return statErr == nil
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("real context.WithTimeout expiry reports timed out diagnostic", func(t *testing.T) {
		wsDir := t.TempDir()
		writeWorkspaceFiles(t, wsDir, validWorkspaceStub("package design\n"))
		installFakeGo(t, "#!/bin/sh\nexec sleep 600\n")

		cp := newControlPlane(t, Options{WorkspaceDir: wsDir})
		cp.storyRefreshWithTimeout = func(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
			assert.Equal(t, defaultStoryRefreshTimeout, limit)
			return context.WithTimeout(ctx, 20*time.Millisecond)
		}

		started, err := cp.RefreshStory(alice)
		require.NoError(t, err)
		require.NotNil(t, started.OpID)

		var expired *gensurla.StoryRefreshResult
		require.Eventually(t, func() bool {
			cur, getErr := cp.GetStoryRefresh(alice)
			if getErr != nil {
				return false
			}
			expired = cur
			return cur.State == storyRefreshStateFailed
		}, 5*time.Second, 10*time.Millisecond)
		require.NotNil(t, expired.Diagnostic)
		assert.Contains(t, *expired.Diagnostic, "Story refresh timed out after 5m0s")
	})
}

// TestStoryRefreshOwnerIsolation verifies that callers acting for another
// project owner cannot read or replace this owner's Story refresh operation.
func TestStoryRefreshOwnerIsolation(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	bob := WithPrincipal(context.Background(), Principal{ID: "bob@example.com", CallerID: "bob@example.com", Kind: CallerUser})
	wsDir := t.TempDir()
	writeWorkspaceFiles(t, wsDir, validWorkspaceStub("package design\n"))

	markerDir := t.TempDir()
	startedFile := filepath.Join(markerDir, "started")
	installFakeGo(t, "#!/bin/sh\n: > "+startedFile+"\nexec sleep 600\n")

	cp := newControlPlane(t, Options{WorkspaceDir: wsDir})
	aliceOp, err := cp.RefreshStory(alice)
	require.NoError(t, err)
	require.NotNil(t, aliceOp.OpID)
	assert.Equal(t, storyRefreshStateRunning, aliceOp.State)

	// Bob sees expected absence ("none"), not Alice's operation.
	bobStatus, err := cp.GetStoryRefresh(bob)
	require.NoError(t, err)
	assert.Equal(t, storyRefreshStateNone, bobStatus.State)
	assert.Nil(t, bobStatus.OpID)

	// Bob starting a refresh gets his own operation ID and leaves Alice's untouched.
	bobOp, err := cp.RefreshStory(bob)
	require.NoError(t, err)
	require.NotNil(t, bobOp.OpID)
	assert.NotEqual(t, *aliceOp.OpID, *bobOp.OpID)

	aliceStatus, err := cp.GetStoryRefresh(alice)
	require.NoError(t, err)
	require.NotNil(t, aliceStatus.OpID)
	assert.Equal(t, *aliceOp.OpID, *aliceStatus.OpID)
}

// TestStoryRefreshEvaluatesStoryOnlyAndNonStoryDesignsWhilePreviewRejectsStoryOnly
// exercises the full sandboxed evaluator through MCP and service calls on a
// Story-only design and a design without the Story module, and verifies that
// preview_architecture still rejects a Story-only design.
func TestStoryRefreshEvaluatesStoryOnlyAndNonStoryDesignsWhilePreviewRejectsStoryOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	wsDir := t.TempDir()

	storyOnlyDesign := `package design

import . "github.com/goadesign/surla/story/dsl"

var _ = Story("Order Refunds", func() {
	Description("Domain timeline for customer refund requests.")
	Actor("customer", "Customer", func() {
		Description("Places orders and requests refunds.")
	})
	Actor("manager", "Support Manager")

	Flow("instant-refund", "Instant refund under the limit", func() {
		Description("Refunds under $50 complete automatically.")
		StartsWith("request-refund", "customer", "Customer requests a $25 refund")
		Outcome("refund-paid", "customer", "Store credit is issued immediately")
	})

	Flow("high-value-refund", "Refund over the limit", func() {
		StartsWith("request-high-refund", "customer", "Customer requests a $120 refund", func() {
			Question("limit-boundary", "Is $50.00 itself instant or manager-reviewed?", func() {
				Description("Boundary rule at $50.00.")
				Option("Instant at $50.00")
				Option("Manager review at $50.00")
			})
		})
		Step("review-refund", "manager", "Manager reviews the refund request")
	})
})
`
	storySnapshot := testmodule.Tidy(t, map[string]string{
		"go.mod": "module example.com/story-design\n\ngo 1.27.3\n\nrequire (\n" +
			"\tgithub.com/goadesign/surla/story v0.0.0-20261111183703-4568fcf5bb85\n" +
			"\tgoa.design/goa/v3 " + testmodule.SurlaVersion(t, "goa.design/goa/v3") + "\n)\n",
		"design/design.go": storyOnlyDesign,
	})
	writeWorkspaceFiles(t, wsDir, storySnapshot)
	wantStoryDigest := designeval.SnapshotDigest(storySnapshot)

	cp := newControlPlane(t, Options{WorkspaceDir: wsDir})
	endpoints := gensurla.NewEndpoints(cp)
	applyOperationPolicy(endpoints, cp.tracer("test/requests"))
	adapter := genmcpadapter.NewMCPAdapter(endpoints, nil)
	guide := WithPrincipal(t.Context(), Principal{ID: "alice@example.com", CallerID: "guide", Kind: CallerGuide})

	callMCP := func(toolName string) *gensurlatools.RefreshStoryResult {
		t.Helper()
		caller := alice
		if toolName == "refresh_story" {
			caller = guide
		}
		res, callErr := adapter.ToolsCall(caller, &genmcpadapter.ToolsCallPayload{
			Name:      toolName,
			Arguments: json.RawMessage(`{}`),
		})
		require.NoError(t, callErr)
		complete, ok := res.Outcome.AsComplete()
		require.True(t, ok)
		require.False(t, complete.IsError != nil && *complete.IsError, "unexpected MCP tool error: %+v", complete.Content)
		require.NotEmpty(t, complete.StructuredContent)
		out, decErr := gensurlatools.UnmarshalRefreshStoryResult(complete.StructuredContent)
		require.NoError(t, decErr)
		return out
	}

	started := callMCP("refresh_story")
	require.NotNil(t, started.OpID)
	require.NotNil(t, started.SnapshotDigest)
	assert.Equal(t, wantStoryDigest, *started.SnapshotDigest)

	var succeeded *gensurlatools.RefreshStoryResult
	require.Eventually(t, func() bool {
		cur := callMCP("get_story_refresh")
		succeeded = cur
		return cur.State == storyRefreshStateSucceeded || cur.State == storyRefreshStateFailed
	}, 2*time.Minute, 50*time.Millisecond)
	require.Equal(t, storyRefreshStateSucceeded, succeeded.State, "unexpected diagnostic: %v", succeeded.Diagnostic)

	require.NotNil(t, succeeded.OpID)
	assert.Equal(t, *started.OpID, *succeeded.OpID)
	assert.Equal(t, wantStoryDigest, *succeeded.SnapshotDigest)
	assert.Nil(t, succeeded.Diagnostic)
	require.Len(t, succeeded.Stories, 1)

	story := succeeded.Stories[0]
	assert.Equal(t, "Order Refunds", story.Name)
	require.NotNil(t, story.Description)
	assert.Equal(t, "Domain timeline for customer refund requests.", *story.Description)
	require.Len(t, story.Actors, 2)
	assert.Equal(t, "customer", story.Actors[0].Key)
	assert.Equal(t, "Customer", story.Actors[0].Title)
	require.NotNil(t, story.Actors[0].Description)
	assert.Equal(t, "Places orders and requests refunds.", *story.Actors[0].Description)
	assert.Equal(t, "manager", story.Actors[1].Key)
	assert.Nil(t, story.Actors[1].Description)

	require.Len(t, story.Flows, 2)
	assert.Equal(t, "instant-refund", story.Flows[0].Key)
	assert.True(t, story.Flows[0].Complete)
	require.Len(t, story.Flows[0].Steps, 2)
	assert.Equal(t, "start", story.Flows[0].Steps[0].Kind)
	assert.Equal(t, "outcome", story.Flows[0].Steps[1].Kind)

	assert.Equal(t, "high-value-refund", story.Flows[1].Key)
	assert.False(t, story.Flows[1].Complete)
	require.Len(t, story.Flows[1].Steps, 2)
	require.Len(t, story.Flows[1].Steps[0].Questions, 1)
	assert.Equal(t, "limit-boundary", story.Flows[1].Steps[0].Questions[0].Key)
	assert.Equal(t, []string{"Instant at $50.00", "Manager review at $50.00"}, story.Flows[1].Steps[0].Questions[0].Options)

	// Refreshing the unchanged succeeded snapshot returns the same operation immediately.
	reused := callMCP("refresh_story")
	require.NotNil(t, reused.OpID)
	assert.Equal(t, *succeeded.OpID, *reused.OpID)
	assert.Equal(t, storyRefreshStateSucceeded, reused.State)
	require.Len(t, reused.Stories, 1)

	// Adopting the succeeded Story refresh into a Change Card saves the
	// evaluated Stories and hashes them into request_revision; preview_architecture
	// still rejects a Story-only design.
	cardWithoutStory := mustPropose(t, cp, alice, "chg-story-only", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
	assert.Empty(t, cardWithoutStory.Stories)
	cardWithStory, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{
		ChangeID:         "chg-story-adopted",
		Prompt:           "Refunds up to $50 are instant.",
		StoryRefreshOpID: succeeded.OpID,
		Analysis:         refundAnalysis("Refunds up to $50 are instant."),
	})
	require.NoError(t, err)
	require.Len(t, cardWithStory.Stories, 1)
	assert.Equal(t, "Order Refunds", cardWithStory.Stories[0].Name)
	assert.NotEqual(t, cardWithoutStory.RequestRevision, cardWithStory.RequestRevision)

	cardWithStory, err = cp.ConfirmIntent(alice, &gensurla.ConfirmIntentPayload{ChangeID: cardWithStory.ChangeID, RequestRevision: cardWithStory.RequestRevision, Answers: map[string]string{}, Examples: cardWithStory.Examples})
	require.NoError(t, err)
	_, err = cp.PreviewArchitecture(alice, &gensurla.PreviewArchitecturePayload{
		ChangeID:        cardWithStory.ChangeID,
		RequestRevision: cardWithStory.RequestRevision,
	})
	assertServiceError(t, err, "unprocessable")
	assert.ErrorContains(t, err, "design must declare at least one Goa API or Service container")

	// A design that does not load the Story module succeeds with no Stories.
	nonStoryDesign := `package design

import . "goa.design/goa/v3/dsl"

var _ = API("orders", func() {})

var _ = Service("orders", func() {
	Method("health", func() {
		Result(String)
		HTTP(func() {
			GET("/health")
		})
	})
})
`
	nonStorySnapshot := testmodule.Snapshot(t, "", nonStoryDesign)
	writeWorkspaceFiles(t, wsDir, nonStorySnapshot)
	wantNonStoryDigest := designeval.SnapshotDigest(nonStorySnapshot)

	nonStoryStarted := callMCP("refresh_story")
	require.NotNil(t, nonStoryStarted.OpID)
	assert.NotEqual(t, *succeeded.OpID, *nonStoryStarted.OpID)
	assert.Equal(t, wantNonStoryDigest, *nonStoryStarted.SnapshotDigest)

	var nonStorySucceeded *gensurlatools.RefreshStoryResult
	require.Eventually(t, func() bool {
		cur := callMCP("get_story_refresh")
		nonStorySucceeded = cur
		return cur.State == storyRefreshStateSucceeded || cur.State == storyRefreshStateFailed
	}, 2*time.Minute, 50*time.Millisecond)
	require.Equal(t, storyRefreshStateSucceeded, nonStorySucceeded.State, "unexpected diagnostic: %v", nonStorySucceeded.Diagnostic)

	assert.Empty(t, nonStorySucceeded.Stories)
	assert.Contains(t, nonStorySucceeded.Message, "the design declares no Stories")

	// Adopting a succeeded refresh with zero Stories equals adopting none.
	adoptedZero, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{
		ChangeID:         "chg-zero-stories",
		Prompt:           "Refunds up to $50 are instant.",
		StoryRefreshOpID: nonStorySucceeded.OpID,
		Analysis:         refundAnalysis("Refunds up to $50 are instant."),
	})
	require.NoError(t, err)
	assert.Nil(t, adoptedZero.Stories)
	plainZero := mustPropose(t, newControlPlane(t, Options{WorkspaceDir: wsDir}), alice, "chg-zero-stories", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
	assert.Equal(t, plainZero.RequestRevision, adoptedZero.RequestRevision)
}

// writeWorkspaceFiles writes files into dir, creating parent directories.
func writeWorkspaceFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
}

// installFakeGo places a shell script named "go" first on PATH for the test.
func installFakeGo(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o700))
	t.Setenv("PATH", bin+":/usr/bin:/bin")
}

// validWorkspaceStub returns a minimal Go module snapshot that passes snapshot
// validation and reaches the toolchain resolution step in designeval.Evaluator.
func validWorkspaceStub(designBody string) map[string]string {
	if designBody == "" {
		designBody = "package design\n"
	}
	return map[string]string{
		"go.mod":           "module example.com/orders\n\ngo 1.24\n",
		"go.sum":           "",
		"design/design.go": designBody,
	}
}
