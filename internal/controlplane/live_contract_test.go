// This file exercises live-target entry refusals, model choices and the
// retained observation of an unexpectedly ended target through their consumers.
package controlplane

import (
	"encoding/json"
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

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/verifyrun"
)

// TestLiveStartRefusesUnauthorizedCardsWithoutWaiting proves entry checks
// precede liveMu: an unrelated start cannot make these callers wait for it.
func TestLiveStartRefusesUnauthorizedCardsWithoutWaiting(t *testing.T) {
	cp, root := newLiveControlPlane(t, "serve")
	saveStoryCard(t, cp, "chg-unconfirmed", StatusNeedsConfirmation, refundStory("Refund waits"))
	cases := []struct {
		name, owner, changeID, refusal string
	}{
		{name: "unknown", owner: "alice@example.com", changeID: "chg-missing", refusal: "not_found"},
		{name: "another owner", owner: "bob@example.com", changeID: liveTestChangeID, refusal: "forbidden"},
		{name: "not confirmed", owner: "alice@example.com", changeID: "chg-unconfirmed", refusal: "conflict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp.liveMu.Lock()
			defer cp.liveMu.Unlock()
			result := make(chan error, 1)
			go func() {
				_, err := cp.StartLiveTarget(asCaller(tc.owner, CallerBuilder), &gensurla.StartLiveTargetPayload{
					ChangeID: tc.changeID, CandidateCommit: strings.Repeat("a", 40),
				})
				result <- err
			}()
			select {
			case err := <-result:
				assert.Equal(t, tc.refusal, goaErrorName(err))
			case <-time.After(2 * time.Second):
				t.Fatal("card refusal waited for a live start's lock")
			}
			dirs, err := verifyrun.LeftoverLiveDirs(root)
			require.NoError(t, err)
			assert.Empty(t, dirs, "a refused start creates no live directory")
		})
	}
}

// TestLiveStartRefusalsPreserveTheRunningTarget checks a failed caller cannot
// unbind the target or discard the drafts already applied to it.
func TestLiveStartRefusalsPreserveTheRunningTarget(t *testing.T) {
	cp, root := newLiveControlPlane(t, "serve")
	builder := asCaller("alice@example.com", CallerBuilder)
	started, err := cp.StartLiveTarget(builder, &gensurla.StartLiveTargetPayload{
		ChangeID: liveTestChangeID, CandidateCommit: strings.Repeat("a", 40),
	})
	require.NoError(t, err)
	_, err = draft(builder, cp, fakePromptID, "Keep this draft.")
	require.NoError(t, err)
	before := readRunning(t, cp)
	owned := boundTargetForTest(cp)
	saveStoryCard(t, cp, "chg-unconfirmed", StatusNeedsConfirmation, refundStory("Refund waits"))
	for _, tc := range []struct {
		owner, changeID, refusal string
	}{
		{owner: "alice@example.com", changeID: "chg-missing", refusal: "not_found"},
		{owner: "bob@example.com", changeID: liveTestChangeID, refusal: "forbidden"},
		{owner: "alice@example.com", changeID: "chg-unconfirmed", refusal: "conflict"},
	} {
		_, err := cp.StartLiveTarget(asCaller(tc.owner, CallerBuilder), &gensurla.StartLiveTargetPayload{
			ChangeID: tc.changeID, CandidateCommit: strings.Repeat("b", 40),
		})
		assert.Equal(t, tc.refusal, goaErrorName(err))
		assert.Same(t, owned, boundTargetForTest(cp))
		assert.Equal(t, before, readRunning(t, cp))
		assert.Equal(t, "Keep this draft.", lastText(t, started.URL))
		dirs, err := verifyrun.LeftoverLiveDirs(root)
		require.NoError(t, err)
		assert.Equal(t, []string{owned.proc.Dir()}, dirs)
	}
}

// TestWrittenLiveStartNeedsAModelBeforeLaunching proves accepted tests alone
// cannot trigger compilation without a usable model; stored fixtures above
// continue to start with no choice and no build.
func TestWrittenLiveStartNeedsAModelBeforeLaunching(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	root := realTempDir(t)
	repo := newLayoutRepo(t, filepath.Join(root, "repo"))
	cp := newBuildControlPlane(t, Options{
		WorkspaceDir: repo.workspace,
		RunsDir:      filepath.Join(root, "runs"),
		WrittenTestRuns: &WrittenTestRuns{
			ReportDir: filepath.Join(root, "reports"),
			Command:   []string{"/bin/sh", "-c", "exit 0", "sh"},
			Timeout:   time.Minute,
		},
		Live: &LiveConfig{Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=serve")},
	})
	card := confirmedChange(t, cp, "chg")
	_, err = cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{
		ChangeID: "chg", RequestRevision: card.RequestRevision,
	})
	require.NoError(t, err)
	acceptTests(t, cp, "chg", repo.tests)
	_, err = cp.StartLiveTarget(asCaller("alice@example.com", CallerBuilder), &gensurla.StartLiveTargetPayload{
		ChangeID: "chg", CandidateCommit: repo.candidate,
	})
	assert.Equal(t, "live_target_unavailable", goaErrorName(err))
	assert.ErrorContains(t, err, "model")
	assert.ErrorContains(t, err, "views page")
	dirs, err := verifyrun.LeftoverLiveDirs(cp.liveRoot)
	require.NoError(t, err)
	assert.Empty(t, dirs)
	assert.Nil(t, boundTargetForTest(cp))
}

// TestAQueuedLiveStartUsesTheCurrentAcceptedTestsAndModel observes the first
// plan read before changing the accepted tests and model while liveMu is held.
// The queued start must reread both when it can finally accept the launch.
func TestAQueuedLiveStartUsesTheCurrentAcceptedTestsAndModel(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	root := realTempDir(t)
	repo := newLayoutRepo(t, filepath.Join(root, "repo"))
	cp := newBuildControlPlane(t, Options{
		WorkspaceDir: repo.workspace,
		RunsDir:      filepath.Join(root, "runs"),
		WrittenTestRuns: &WrittenTestRuns{
			ReportDir: filepath.Join(root, "reports"),
			Command:   []string{"/bin/sh", "-c", "exit 0", "sh"},
			Timeout:   time.Minute,
		},
		Live: &LiveConfig{Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=serve")},
	})
	card := confirmedChange(t, cp, "chg")
	user := asCaller("alice@example.com", CallerUser)
	builder := asCaller("alice@example.com", CallerBuilder)
	_, err = cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	acceptTests(t, cp, "chg", repo.tests)
	firstGateway := "127.0.0.1:9090"
	_, err = cp.ConfigureModel(user, &gensurla.ConfigureModelPayload{GatewayAddr: &firstGateway})
	require.NoError(t, err)

	// The real read-only Git invocation is a synchronization point: plan
	// selection already captured T before its first Git query. This wrapper
	// only marks that observation and forwards the exact arguments unchanged.
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	bin := t.TempDir()
	marker := filepath.Join(bin, "read-started")
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(
		"#!/bin/sh\n: > \"$SURLA_LIVE_READ_MARKER\"\nexec \"$SURLA_LIVE_REAL_GIT\" \"$@\"\n"), 0o700))
	t.Setenv("SURLA_LIVE_READ_MARKER", marker)
	t.Setenv("SURLA_LIVE_REAL_GIT", realGit)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cp.liveMu.Lock()
	locked := true
	defer func() {
		if locked {
			cp.liveMu.Unlock()
		}
	}()
	type result struct {
		target *gensurla.LiveTargetResult
		err    error
	}
	started := make(chan result, 1)
	go func() {
		target, err := cp.StartLiveTarget(builder, &gensurla.StartLiveTargetPayload{
			ChangeID: "chg", CandidateCommit: repo.candidate,
		})
		started <- result{target: target, err: err}
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond, "the queued caller has selected the earlier accepted tests")
	acceptTests(t, cp, "chg", repo.candidate)
	secondGateway := "127.0.0.1:9191"
	_, err = cp.ConfigureModel(user, &gensurla.ConfigureModelPayload{GatewayAddr: &secondGateway})
	require.NoError(t, err)
	cp.liveMu.Unlock()
	locked = false
	var launched result
	select {
	case launched = <-started:
		require.NoError(t, launched.err)
	case <-time.After(30 * time.Second):
		t.Fatal("the queued start did not finish after releasing liveMu")
	}
	dirs, err := verifyrun.LeftoverLiveDirs(cp.liveRoot)
	require.NoError(t, err)
	require.Len(t, dirs, 1)
	var live verifyrun.LiveRequest
	readJSONFile(t, filepath.Join(dirs[0], "live.json"), &live)
	assert.Equal(t, repo.candidate, live.Plan.SuiteCommit, "the plan captured before waiting is not launched")
	status, body := getWithToken(t, launched.target.URL+"/model", "")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, secondGateway, body, "the same accepted launch captures the current model choice")

	owned := boundTargetForTest(cp)
	running := readRunning(t, cp)
	for _, transition := range []struct {
		name  string
		apply func(*testing.T)
		state string
	}{
		{name: "test writer reported again", state: "reported", apply: func(t *testing.T) {
			saveTestWriter(t, cp, "chg", testWriterAssignment{State: testsReported, Read: true, Findings: []string{}})
		}},
		{name: "user built the same revision again", state: "assigned", apply: func(t *testing.T) {
			acceptTests(t, cp, "chg", repo.candidate)
			_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			require.NoError(t, err)
		}},
	} {
		t.Run(transition.name, func(t *testing.T) {
			transition.apply(t)
			_, err := cp.StartLiveTarget(builder, &gensurla.StartLiveTargetPayload{
				ChangeID: "chg", CandidateCommit: repo.candidate,
			})
			assert.Equal(t, "tests_not_accepted", goaErrorName(err))
			assert.ErrorContains(t, err, "their state is "+transition.state)
			assert.Same(t, owned, boundTargetForTest(cp), "leaving Accepted does not stop an existing target")
			assert.Equal(t, running, readRunning(t, cp))
		})
	}
}

// TestGatewayAddressValidationPreservesThePreviousChoice validates syntax at
// Apply, without resolving names or sending traffic to the chosen gateway.
func TestGatewayAddressValidationPreservesThePreviousChoice(t *testing.T) {
	cp := newControlPlane(t, Options{})
	user := asCaller("alice@example.com", CallerUser)
	for _, address := range []string{"model.local:1", "127.0.0.1:65535", "[::1]:9090"} {
		_, err := cp.ConfigureModel(user, &gensurla.ConfigureModelPayload{GatewayAddr: &address})
		require.NoError(t, err, address)
		read, err := cp.GetModelConnection(user)
		require.NoError(t, err)
		gateway, ok := read.Connection.AsGateway()
		require.True(t, ok)
		assert.Equal(t, address, gateway.GatewayAddr, "accepted addresses retain their exact text")
	}
	before, err := cp.GetModelConnection(user)
	require.NoError(t, err)
	for _, address := range []string{
		":9090", "model.local", "model.local:", "https://model.local:9090", "model.local/path:9090",
		"model.local:0", "model.local:65536", "model.local:-1", "model.local:+1", "model.local:9e3",
		" model.local:9090", "model.local:9090 ", "model .local:9090", "model.local:\t9090", "model\u2003.local:9090",
	} {
		t.Run(address, func(t *testing.T) {
			_, err := cp.ConfigureModel(user, &gensurla.ConfigureModelPayload{GatewayAddr: &address})
			assert.Equal(t, "unprocessable", goaErrorName(err))
			assert.NotContains(t, err.Error(), address, "refusals do not echo a submitted address")
			after, err := cp.GetModelConnection(user)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
	handler := NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{"tok": {ID: "alice@example.com"}}))
	body := `{"gateway_addr":"https://secret-address.invalid:9090"}`
	req := httptest.NewRequest(http.MethodPost, "/cp1/model", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	assert.Equal(t, http.StatusUnprocessableEntity, response.Code)
	assert.NotContains(t, response.Body.String(), "secret-address")
	after, err := cp.GetModelConnection(user)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// TestEndedLiveTargetRetainsOnlyItsPublicObservation covers read/reload,
// refusal preservation, successful Stop and server restart lifetimes.
func TestEndedLiveTargetRetainsOnlyItsPublicObservation(t *testing.T) {
	cp, root := newLiveControlPlane(t, "serve")
	builder := asCaller("alice@example.com", CallerBuilder)
	commit := strings.Repeat("a", 40)
	started, err := cp.StartLiveTarget(builder, &gensurla.StartLiveTargetPayload{
		ChangeID: liveTestChangeID, CandidateCommit: commit,
	})
	require.NoError(t, err)
	_, err = draft(builder, cp, fakePromptID, "A private prompt draft.")
	require.NoError(t, err)
	running := readRunning(t, cp)
	target := boundTargetForTest(cp)
	key := target.key
	if response, err := http.Post(started.URL+"/exit", "text/plain", nil); err == nil {
		require.NoError(t, response.Body.Close())
	}
	require.Eventually(t, func() bool { return cp.liveTargetState().Kind() == gensurla.LiveTargetKindEnded },
		30*time.Second, 20*time.Millisecond)
	read, err := cp.GetLiveTarget(builder)
	require.NoError(t, err)
	ended, ok := read.LiveTarget.AsEnded()
	require.True(t, ok)
	assert.Equal(t, commit, ended.CandidateCommit)
	assert.Equal(t, running.StartedAt, ended.StartedAt)
	assert.False(t, parseLiveTime(t, ended.EndedAt).Before(parseLiveTime(t, ended.StartedAt)))
	assert.Contains(t, ended.Message, "exact cause is unavailable")
	assert.Contains(t, ended.Message, "start")
	assert.Nil(t, boundTargetForTest(cp))
	cp.liveMu.Lock()
	assert.Nil(t, cp.owned, "confirmed cleanup releases the process owner")
	cp.liveMu.Unlock()
	cp.bindMu.RLock()
	assert.Nil(t, cp.shown, "the retained notice must not keep a process/key/settings/draft-bearing target")
	cp.bindMu.RUnlock()
	dirs, err := verifyrun.LeftoverLiveDirs(root)
	require.NoError(t, err)
	assert.Empty(t, dirs)
	_, err = draft(builder, cp, fakePromptID, "A later draft.")
	assert.Equal(t, "live_target_ended", goaErrorName(err))
	_, err = cp.StartLiveTarget(asCaller("bob@example.com", CallerBuilder), &gensurla.StartLiveTargetPayload{
		ChangeID: liveTestChangeID, CandidateCommit: strings.Repeat("b", 40),
	})
	assert.Equal(t, "forbidden", goaErrorName(err))
	again, err := cp.GetLiveTarget(builder)
	require.NoError(t, err)
	assert.Equal(t, read, again, "reads and a refused start keep the original observation")
	server := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{"tok": {ID: "alice@example.com"}})))
	t.Cleanup(server.Close)
	for range 2 {
		status, body := getWithToken(t, server.URL+"/live-target", "tok-builder")
		require.Equal(t, http.StatusOK, status)
		var result struct {
			LiveTarget map[string]any `json:"live_target"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &result))
		assert.Len(t, result.LiveTarget, 5, "state plus exactly the four public observation fields")
		assert.Equal(t, "ended", result.LiveTarget["state"])
		for _, private := range []string{started.URL, key, "A private prompt draft.", fakePromptID} {
			assert.NotContains(t, body, private)
		}
	}
	stopped, err := cp.StopLiveTarget(builder)
	require.NoError(t, err)
	assert.False(t, stopped.Stopped)
	assert.Contains(t, stopped.Message, "cleared")
	assert.Equal(t, gensurla.LiveTargetKindNone, cp.liveTargetState().Kind())
	_, err = draft(builder, cp, fakePromptID, "After clearing.")
	assert.Equal(t, "no_live_target", goaErrorName(err))
	// End another launch and restart while its notice is still present, so
	// the reopened none state proves the notice was not saved to the card store.
	started, err = cp.StartLiveTarget(builder, &gensurla.StartLiveTargetPayload{
		ChangeID: liveTestChangeID, CandidateCommit: strings.Repeat("b", 40),
	})
	require.NoError(t, err)
	if response, err := http.Post(started.URL+"/exit", "text/plain", nil); err == nil {
		require.NoError(t, response.Body.Close())
	}
	require.Eventually(t, func() bool { return cp.liveTargetState().Kind() == gensurla.LiveTargetKindEnded },
		30*time.Second, 20*time.Millisecond)
	require.NoError(t, cp.Close())
	reopened, err := New(Options{CardsDir: filepath.Dir(root)})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
	assert.Equal(t, gensurla.LiveTargetKindNone, reopened.liveTargetState().Kind(), "an ended notice is not durable history")
}

// TestEndedPublicationNeverLooksLikeNoTarget coordinates output EOF with
// cleanup and exercises both read consumers throughout the transition.
func TestEndedPublicationNeverLooksLikeNoTarget(t *testing.T) {
	cp, _ := newLiveControlPlane(t, "serve")
	builder := asCaller("alice@example.com", CallerBuilder)
	started, err := cp.StartLiveTarget(builder, &gensurla.StartLiveTargetPayload{
		ChangeID: liveTestChangeID, CandidateCommit: strings.Repeat("a", 40),
	})
	require.NoError(t, err)
	target := boundTargetForTest(cp)
	cp.liveMu.Lock()
	locked := true
	defer func() {
		if locked {
			cp.liveMu.Unlock()
		}
	}()
	if response, err := http.Post(started.URL+"/exit", "text/plain", nil); err == nil {
		require.NoError(t, response.Body.Close())
	}
	select {
	case <-target.proc.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the fake target did not close its output")
	}
	read, err := cp.GetLiveTarget(builder)
	require.NoError(t, err)
	assert.Equal(t, gensurla.LiveTargetKindRunning, read.LiveTarget.Kind(), "cleanup is still waiting for liveMu")
	_, err = draft(builder, cp, fakePromptID, "After output ended.")
	assert.Equal(t, "live_target_ended", goaErrorName(err))
	cp.liveMu.Unlock()
	locked = false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		read, err = cp.GetLiveTarget(builder)
		require.NoError(t, err)
		assert.Contains(t, []gensurla.LiveTargetKind{gensurla.LiveTargetKindRunning, gensurla.LiveTargetKindEnded}, read.LiveTarget.Kind(),
			"confirmed cleanup publishes ended atomically, without an intermediate none")
		_, err = draft(builder, cp, fakePromptID, "During cleanup.")
		assert.Equal(t, "live_target_ended", goaErrorName(err), "the end is never mistaken for a target that never ran")
		if read.LiveTarget.Kind() == gensurla.LiveTargetKindEnded {
			return
		}
	}
	t.Fatal("confirmed cleanup did not publish the ended observation")
}

// TestAcceptedReplacementDoesNotReviveTheOlderEnd verifies an accepted new
// attempt owns its failure, even when the executor cannot be launched.
func TestAcceptedReplacementDoesNotReviveTheOlderEnd(t *testing.T) {
	cp, _ := newLiveControlPlane(t, "serve")
	builder := asCaller("alice@example.com", CallerBuilder)
	started, err := cp.StartLiveTarget(builder, &gensurla.StartLiveTargetPayload{
		ChangeID: liveTestChangeID, CandidateCommit: strings.Repeat("a", 40),
	})
	require.NoError(t, err)
	if response, err := http.Post(started.URL+"/exit", "text/plain", nil); err == nil {
		require.NoError(t, response.Body.Close())
	}
	require.Eventually(t, func() bool { return cp.liveTargetState().Kind() == gensurla.LiveTargetKindEnded },
		30*time.Second, 20*time.Millisecond)
	cp.liveMu.Lock()
	cp.liveCommand = []string{filepath.Join(t.TempDir(), "missing-executor")}
	cp.liveMu.Unlock()
	_, err = cp.StartLiveTarget(builder, &gensurla.StartLiveTargetPayload{
		ChangeID: liveTestChangeID, CandidateCommit: strings.Repeat("b", 40),
	})
	assert.Equal(t, "live_target_failed", goaErrorName(err))
	assert.Equal(t, gensurla.LiveTargetKindNone, cp.liveTargetState().Kind(), "an older end cannot describe the new failed launch")
	_, err = draft(builder, cp, fakePromptID, "After a refused launch.")
	assert.Equal(t, "no_live_target", goaErrorName(err))
}
