// This file proves ordered build admission and its pre-save/post-save boundary.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/shquote"
	"github.com/goadesign/surla/internal/testwriter"
	"github.com/goadesign/surla/verify/candidate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStartBuildOrdersItsOwners observes the saved record at role registration,
// the saved Story at generation, and the released turn at the window boundary.
func TestStartBuildOrdersItsOwners(t *testing.T) {
	f := &teamServiceFixture{}
	cp := newBuildControlPlane(t, Options{Team: &TeamBinding{Owner: "alice@example.com", Service: f}})
	card := saveStoryCard(t, cp, "chg", StatusConfirmed, refundStory("Agreed flow"))
	cp.mu.Lock()
	record, _, err := cp.cards.load("chg")
	require.NoError(t, err)
	record.designSource = `{"design/design.go":"package design // agreed\n"}`
	record.card.RequestRevision, err = computeCanonicalRevision(record.card, record.owner, record.designSource)
	require.NoError(t, err)
	err = cp.cards.save(record)
	cp.mu.Unlock()
	require.NoError(t, err)
	card.RequestRevision = record.card.RequestRevision
	var events []string
	copy := &buildCopyFixture{folder: t.TempDir()}
	cp.buildCopyFactory = func(context.Context, string, scaffold.Layout) (buildCopy, error) {
		assertNoBuildSaved(t, cp, "chg")
		events = append(events, "open")
		return copy, nil
	}
	copy.refresh = func(context.Context) (testwriter.RefreshResult, error) {
		events = append(events, "refresh")
		return testwriter.RefreshResult{State: testwriter.RefreshAtProjectCommit, Commit: strings.Repeat("c", 40)}, nil
	}
	f.register = func(_ context.Context, folders BuildFolders) error {
		events = append(events, "register")
		workspace, _, _ := cp.currentWorkspaceTarget()
		assert.Equal(t, BuilderAndTestWriter{BuilderFolder: workspace, TestWriterFolder: copy.folder}, folders)
		saved, found, err := cp.cards.loadBuild("chg")
		require.NoError(t, err)
		require.True(t, found, "registration follows the durable save")
		assert.Equal(t, record.designSource, saved.DesignSource)
		assert.Equal(t, refundStory("Agreed flow"), saved.Stories)
		assert.Equal(t, testsAssigned, saved.TestWriter.State)
		return nil
	}
	copy.prepare = func(_ context.Context, stories []designeval.StorySummary) error {
		events = append(events, "prepare")
		assert.Equal(t, refundStory("Agreed flow"), stories)
		return nil
	}
	f.start = func(ctx context.Context) (*gensurla.ProjectTeam, error) {
		events = append(events, "start")
		select {
		case cp.workingCopyTurn <- struct{}{}:
			<-cp.workingCopyTurn
		default:
			t.Error("window work retained the copy turn")
		}
		_, err := cp.GetBuild(ctx, &gensurla.GetBuildPayload{ChangeID: "chg"})
		assert.NoError(t, err, "window work retains neither the card lock nor the phase deadline")
		_, bounded := ctx.Deadline()
		assert.False(t, bounded, "StartAgents receives its own server-owned lifetime")
		return &gensurla.ProjectTeam{Team: "team", Space: "space", Roles: []*gensurla.ProjectTeamRole{}, Dialog: "opened", Step: "Read the fixture assignments."}, nil
	}
	result, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	assert.Equal(t, []string{"open", "refresh", "register", "prepare", "start"}, events)
	assert.Equal(t, 1, result.Build.RequiredTestCount)
	assert.Equal(t, gensurla.TeamKindReady, result.Team.Kind())
	assignment, err := cp.GetTestWriterAssignment(asCaller("alice@example.com", CallerTestWriter), &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, record.designSource, assignment.DesignSource)
	assert.Equal(t, copy.folder, assignment.WorkingFolder)
}

// TestStartBuildKeepsPostSaveOutcomes distinguishes author findings, operational
// failures and reports made while required tests are being generated.
func TestStartBuildKeepsPostSaveOutcomes(t *testing.T) {
	for _, scenario := range []string{"author", "operational", "reported during generation"} {
		t.Run(scenario, func(t *testing.T) {
			cp := newBuildControlPlane(t, Options{})
			card := confirmedChange(t, cp, "chg")
			copy := &buildCopyFixture{folder: t.TempDir()}
			cp.buildCopyFactory = func(context.Context, string, scaffold.Layout) (buildCopy, error) { return copy, nil }
			copy.prepare = func(context.Context, []designeval.StorySummary) error {
				if scenario == "operational" {
					return errors.New("private setup failure")
				}
				if scenario == "reported during generation" {
					_, err := cp.ReportTestsFinished(asCaller("alice@example.com", CallerTestWriter), &gensurla.ReportTestsFinishedPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
					require.NoError(t, err)
					// A stand-in for 6b-2's report counter, which this step does not own.
					cp.mu.Lock()
					saved, _, err := cp.cards.loadBuild("chg")
					require.NoError(t, err)
					saved.ReportNumber++
					err = cp.cards.saveBuild(saved)
					cp.mu.Unlock()
					require.NoError(t, err)
				}
				return &testwriter.AuthorError{Finding: testwriter.Finding{Kind: testwriter.FindingGeneration, Message: "The test design does not compile."}}
			}
			result, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			require.NoError(t, err, "a post-save generation failure never refuses the build")
			saved := mustStartedBuild(t, cp, asCaller("alice@example.com", CallerUser), "chg")
			assert.Equal(t, result.Build, saved)
			switch scenario {
			case "author":
				finding, ok := saved.TestWriter.Tests.AsNeedsChanges()
				require.True(t, ok)
				assert.True(t, strings.HasSuffix(finding.Findings[0], "Correct the problem in the test module, then report_tests_finished."))
				assert.NotEmpty(t, finding.CheckedAt)
			case "operational":
				failure, ok := saved.TestWriter.Tests.AsCouldNotRun()
				require.True(t, ok)
				assert.NotEmpty(t, failure.CheckedAt)
				assert.NotContains(t, failure.Reason, "private setup failure")
				assert.Equal(t, "Build this change again on the views page.", failure.Step)
			case "reported during generation":
				assert.Equal(t, gensurla.TestsKindReported, saved.TestWriter.Tests.Kind())
				assert.Equal(t, 1, saved.TestWriter.Report)
			}
		})
	}
}

// TestStartBuildRefusalsReachTheHTTPConsumer exercises all eight ordinary named
// errors, their status, actionable suffix, and the absence of saved/team effects.
func TestStartBuildRefusalsReachTheHTTPConsumer(t *testing.T) {
	for _, name := range []string{"not_repository", "no_commit", "no_test_layout", "git_identity_missing", "copy_folder_in_use", "tests_branch_in_use", "working_copy_busy", "sandbox_unavailable"} {
		t.Run(name, func(t *testing.T) {
			cp := newBuildControlPlane(t, Options{})
			card := confirmedChange(t, cp, "chg")
			workspace, _, _ := cp.currentWorkspaceTarget()
			step := ""
			var branchMessage string
			var openErr error
			switch name {
			case "not_repository":
				openErr = testwriter.ErrNotRepository
				step = "run git init in the project folder and commit its files, then try again"
			case "no_commit":
				openErr = testwriter.ErrNoCommit
				step = "commit the project's files, then try again"
			case "no_test_layout":
				require.NoError(t, os.Remove(filepath.Join(workspace, scaffold.SuiteDir, "go.mod")))
				step = "Store verification settings, then build again."
			case "git_identity_missing":
				cp.buildIdentityCheck = func(context.Context, string) error { return testwriter.ErrIdentityMissing }
				step = "configure Git's name and address, then try again"
			case "copy_folder_in_use":
				openErr = fmt.Errorf("%w (%s is not a folder)", testwriter.ErrFolderInUse, filepath.Join(workspace, testwriter.Folder))
				step = "Move it out of the project, then try again."
			case "tests_branch_in_use":
				openErr = fmt.Errorf("%w: %s has it checked out; switch that folder to another branch, then try again", testwriter.ErrBranchInUse, shquote.Quote(filepath.Join(workspace, "other working copy")))
				branchMessage = openErr.Error()
				step = "switch that folder to another branch, then try again"
			case "working_copy_busy":
				cp.workingCopyTurn <- struct{}{}
				defer func() { <-cp.workingCopyTurn }()
				step = "Start the build again when that has ended."
			case "sandbox_unavailable":
				cp.buildSandboxCheck = func() error { return candidate.ErrIsolationUnavailable }
				step = "Start this project on a system with Surla's candidate sandbox, then build again."
			}
			cp.buildCopyFactory = func(context.Context, string, scaffold.Layout) (buildCopy, error) {
				if openErr == nil {
					t.Fatal("a refused preflight opened the working copy")
				}
				return nil, openErr
			}
			server := httptest.NewServer(NewHTTPHandler(cp, func(*http.Request) (Principal, error) {
				return Principal{ID: "alice@example.com", Kind: CallerUser}, nil
			}))
			defer server.Close()
			status, body := httpCall(t, server.URL, "fixture", http.MethodPost, "/changes/chg/build", `{"request_revision":"`+card.RequestRevision+`"}`)
			want := http.StatusConflict
			if name == "sandbox_unavailable" {
				want = http.StatusNotImplemented
			}
			assert.Equal(t, want, status, body)
			var refusal struct{ Name, Message string }
			require.NoError(t, json.Unmarshal([]byte(body), &refusal))
			assert.Equal(t, name, refusal.Name)
			assert.True(t, strings.HasSuffix(refusal.Message, step), refusal.Message)
			if name == "copy_folder_in_use" {
				assert.Contains(t, refusal.Message, shquote.Quote(filepath.Join(workspace, testwriter.Folder)))
			}
			if name == "tests_branch_in_use" {
				assert.Equal(t, branchMessage, refusal.Message, "the library's recovery text is propagated unchanged")
			}
			assertNoBuildSaved(t, cp, "chg")
			assert.Empty(t, cp.team.Service.(*teamServiceFixture).calls)
		})
	}
}

// TestStartBuildOwnerPrecedesTheTurn denies another owner even while the owner
// is busy, and an unbound server names its enabling step without saving.
func TestStartBuildOwnerPrecedesTheTurn(t *testing.T) {
	cp := newBuildControlPlane(t, Options{})
	card := confirmedChange(t, cp, "chg")
	cp.workingCopyTurn <- struct{}{}
	_, err := cp.StartBuild(asCaller("bob@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	assertRefused(t, err, "forbidden")
	<-cp.workingCopyTurn
	unbound := newControlPlane(t, Options{})
	_, err = unbound.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	assertRefused(t, err, "not_implemented")
	assert.ErrorContains(t, err, "start this project with surla")
	assertNoBuildSaved(t, cp, "chg")
}

// TestStartBuildDeadlineBoundaries uses a short seam around the same phase:
// before the deadline succeeds, its signal and later work save could_not_run.
// No test waits for the production five-minute limit.
func TestStartBuildDeadlineBoundaries(t *testing.T) {
	for _, when := range []string{"below", "at", "above"} {
		t.Run(when, func(t *testing.T) {
			cp := newBuildControlPlane(t, Options{})
			cp.buildStartTimeout = 50 * time.Millisecond
			card := confirmedChange(t, cp, "chg")
			copy := &buildCopyFixture{folder: t.TempDir()}
			cp.buildCopyFactory = func(context.Context, string, scaffold.Layout) (buildCopy, error) { return copy, nil }
			copy.prepare = func(ctx context.Context, _ []designeval.StorySummary) error {
				if when == "below" {
					return nil
				}
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
					return errors.New("deadline was not enforced")
				}
				if when == "above" {
					// An additional phase after the deadline must not turn the
					// saved build into a pre-save refusal.
					_, err := cp.GetBuild(asCaller("alice@example.com", CallerUser), &gensurla.GetBuildPayload{ChangeID: "chg"})
					require.NoError(t, err)
				}
				return ctx.Err()
			}
			result, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			require.NoError(t, err)
			want := gensurla.TestsKindCouldNotRun
			if when == "below" {
				want = gensurla.TestsKindAssigned
			}
			assert.Equal(t, want, result.Build.TestWriter.Tests.Kind())
			assert.Equal(t, []string{"register", "start"}, cp.team.Service.(*teamServiceFixture).calls, "registration survives the phase limit and team start has its own lifetime")
		})
	}
	// The same limit before the save is a retryable refusal and leaves nothing.
	cp := newBuildControlPlane(t, Options{})
	cp.buildStartTimeout = time.Nanosecond
	card := confirmedChange(t, cp, "chg")
	_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	assertRefused(t, err, "unavailable")
	assertNoBuildSaved(t, cp, "chg")
}
