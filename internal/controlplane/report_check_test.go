// This file tests the report checker that a control plane with a bound team
// runs, with a stand-in for the test writer's working copy: a verdict follows
// each report without another call, a newer report or a stop saves nothing
// for the check in progress, a restarted server judges what is reported, a
// report waits for the last step of a check, each limit and cause of a check
// that cannot run, who reads the findings' text, and what supervision sees
// of verdicts.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	genclient "github.com/goadesign/surla/gen/http/surla/client"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/testmodule"
	"github.com/goadesign/surla/internal/testwriter"
	"github.com/goadesign/surla/verify/candidate"
)

// standInCopy is a stand-in for the test writer's working copy that the
// report checker opens. Prepare returns prepareErr; each Check and Record call
// is answered by check and record with its call number, starting at 1, and
// Record also keeps its message. Without check, a check waits until its
// context ends, so a report stays reported.
type standInCopy struct {
	mu sync.Mutex
	// prepareErr is what Prepare returns.
	prepareErr error
	// check answers Check calls.
	check func(ctx context.Context, call int) (testwriter.Verdict, error)
	// record answers Record calls; nil returns standInCommit.
	record func(ctx context.Context, call int) (string, error)
	// checks and records count the calls so far.
	checks, records int
	// messages lists Record's commit messages in call order.
	messages []string
}

// standInCommit is the commit the stand-in Record returns by default.
var standInCommit = strings.Repeat("a", 40)

// Project returns a folder no check reads.
func (s *standInCopy) Project() string { return "/nonexistent/stand-in" }

// Refresh reports the copy at the project's commit.
func (s *standInCopy) Refresh(context.Context) (testwriter.RefreshResult, error) {
	return testwriter.RefreshResult{State: testwriter.RefreshAtProjectCommit, Commit: strings.Repeat("c", 40)}, nil
}

// Prepare returns prepareErr.
func (s *standInCopy) Prepare(context.Context, []designeval.StorySummary) error {
	return s.prepareErr
}

// Check answers with check, or waits until ctx ends when check is nil.
func (s *standInCopy) Check(ctx context.Context, _ []designeval.StorySummary) (testwriter.Verdict, error) {
	s.mu.Lock()
	s.checks++
	call := s.checks
	s.mu.Unlock()
	if s.check == nil {
		<-ctx.Done()
		return testwriter.Verdict{}, ctx.Err()
	}
	return s.check(ctx, call)
}

// Record keeps message and answers with record, or standInCommit when record
// is nil.
func (s *standInCopy) Record(ctx context.Context, _ testwriter.Verdict, message string) (string, error) {
	s.mu.Lock()
	s.records++
	call := s.records
	s.messages = append(s.messages, message)
	s.mu.Unlock()
	if s.record == nil {
		return standInCommit, nil
	}
	return s.record(ctx, call)
}

// checked returns the number of Check calls so far.
func (s *standInCopy) checked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checks
}

// recorded returns Record's commit messages so far.
func (s *standInCopy) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.messages)
}

// accepting is a stand-in check that accepts the copy.
func accepting(context.Context, int) (testwriter.Verdict, error) { return testwriter.Verdict{}, nil }

// TestAReportIsJudgedWithoutAnotherCall runs the checker and reports twice. A
// report returns reported with its number at once, while the check it wakes
// is still running; its verdict follows with no other call. Findings make the
// test writer's assignment unread and leave the builder's read; an acceptance
// records the tests once, carries the commit and the fixed sentence, and
// makes both assignments unread.
func TestAReportIsJudgedWithoutAnotherCall(t *testing.T) {
	checking, release := make(chan int, 2), make(chan struct{})
	tests := &standInCopy{check: func(_ context.Context, call int) (testwriter.Verdict, error) {
		checking <- call
		<-release
		if call == 1 {
			return needsChanges("RefundOverLimit is not implemented"), nil
		}
		return testwriter.Verdict{}, nil
	}}
	cp, revision := newReportFixture(t, Options{}, tests)
	readBothAssignments(t, cp)

	first := reportTests(t, cp, revision)
	assert.Equal(t, gensurla.TestsKindReported, first.Progress.Tests.Kind())
	assert.Equal(t, 1, first.Progress.Report)
	assert.Equal(t, 1, receive(t, checking), "the report woke the check, which is still running")
	close(release)
	build := waitForTests(t, cp, gensurla.TestsKindNeedsChanges)
	needs, _ := build.TestWriter.Tests.AsNeedsChanges()
	assert.Equal(t, 1, needs.FindingCount)
	assert.False(t, build.TestWriter.Read, "a verdict marks the test writer's assignment unread")
	assert.True(t, build.Builder.Read, "findings leave the builder's assignment read")
	assignment, err := cp.GetTestWriterAssignment(asCaller("alice@example.com", CallerTestWriter), &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, []string{"RefundOverLimit is not implemented"}, assignment.Findings)

	second := reportTests(t, cp, revision)
	assert.Equal(t, 2, second.Progress.Report)
	assert.Equal(t, 2, receive(t, checking))
	build = waitForTests(t, cp, gensurla.TestsKindAccepted)
	accepted, _ := build.TestWriter.Tests.AsAccepted()
	assert.Equal(t, standInCommit, accepted.Commit)
	assert.Equal(t, testsNotJudged, accepted.NotJudged)
	assert.False(t, build.TestWriter.Read)
	assert.False(t, build.Builder.Read, "an acceptance marks the builder's assignment unread too")
	assert.Equal(t, []string{"Add the tests Surla accepted for change chg, report 2"}, tests.recorded())
}

// TestASecondReportDuringACheckIsJudgedAfterIt reports again while the first
// report's check runs, once with checks that find problems and once with
// checks that accept. Nothing is saved for the first report, and an
// accepting first check records nothing on the tests branch; the second
// report is checked after it, and the saved verdict is the second's.
func TestASecondReportDuringACheckIsJudgedAfterIt(t *testing.T) {
	for _, accept := range []bool{false, true} {
		t.Run(map[bool]string{false: "findings", true: "accepted"}[accept], func(t *testing.T) {
			checking, release := make(chan int, 2), make(chan struct{}, 2)
			tests := &standInCopy{check: func(_ context.Context, call int) (testwriter.Verdict, error) {
				checking <- call
				<-release
				if accept {
					return testwriter.Verdict{}, nil
				}
				return needsChanges(fmt.Sprintf("finding of check %d", call)), nil
			}, record: func(_ context.Context, call int) (string, error) {
				// Each record moves the tests branch to a commit of its own.
				return strings.Repeat(fmt.Sprint(call), 40), nil
			}}
			cp, revision := newReportFixture(t, Options{}, tests)

			reportTests(t, cp, revision)
			require.Equal(t, 1, receive(t, checking))
			reportTests(t, cp, revision)
			release <- struct{}{}
			require.Equal(t, 2, receive(t, checking), "the second report is checked after the first")
			build := mustStartedBuild(t, cp, asCaller("alice@example.com", CallerUser), "chg")
			assert.Equal(t, gensurla.TestsKindReported, build.TestWriter.Tests.Kind(), "nothing was saved for the first report")
			assert.Equal(t, 2, build.TestWriter.Report)
			assert.Empty(t, tests.recorded(), "nothing was recorded for the first report")
			release <- struct{}{}
			if accept {
				build = waitForTests(t, cp, gensurla.TestsKindAccepted)
				assert.Equal(t, 2, build.TestWriter.Report)
				assert.Equal(t, []string{"Add the tests Surla accepted for change chg, report 2"}, tests.recorded(),
					"only the second report's tests are recorded")
				accepted, _ := build.TestWriter.Tests.AsAccepted()
				assert.Equal(t, strings.Repeat("1", 40), accepted.Commit,
					"the tests branch is where the one record, for report 2, put it")
				return
			}
			waitForTests(t, cp, gensurla.TestsKindNeedsChanges)
			findings, err := cp.GetTestFindings(asCaller("alice@example.com", CallerUser), &gensurla.GetTestFindingsPayload{ChangeID: "chg"})
			require.NoError(t, err)
			assert.Equal(t, 2, findings.Report)
			assert.Equal(t, []string{"finding of check 2"}, findings.Findings)
		})
	}
}

// TestAReportDuringRecordWaitsAndKeepsItsNumber reports while an accepted
// check records its commit: the report waits until the verdict is saved,
// then counts as report 2, which is judged next.
func TestAReportDuringRecordWaitsAndKeepsItsNumber(t *testing.T) {
	recording, release := make(chan int, 2), make(chan struct{})
	tests := &standInCopy{check: accepting, record: func(_ context.Context, call int) (string, error) {
		recording <- call
		if call == 1 {
			<-release
		}
		return standInCommit, nil
	}}
	cp, revision := newReportFixture(t, Options{}, tests)

	reportTests(t, cp, revision)
	require.Equal(t, 1, receive(t, recording))
	reported := make(chan *gensurla.TestWriterAssignmentItem, 1)
	go func() {
		item, err := cp.ReportTestsFinished(asCaller("alice@example.com", CallerTestWriter), &gensurla.ReportTestsFinishedPayload{
			ChangeID: "chg", RequestRevision: revision,
		})
		assert.NoError(t, err)
		reported <- item
	}()
	select {
	case <-reported:
		t.Fatal("a report returned while the check recorded its commit")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	second := receive(t, reported)
	assert.Equal(t, 2, second.Progress.Report, "the waiting report keeps its number")
	assert.Equal(t, gensurla.TestsKindReported, second.Progress.Tests.Kind())
	require.Equal(t, 2, receive(t, recording), "report 2 is judged next")
	build := waitForTests(t, cp, gensurla.TestsKindAccepted)
	assert.Equal(t, 2, build.TestWriter.Report)
	assert.Equal(t, []string{
		"Add the tests Surla accepted for change chg, report 1",
		"Add the tests Surla accepted for change chg, report 2",
	}, tests.recorded())
}

// TestAVerdictThatCannotBeSavedIsJudgedAgain makes the save of a verdict
// fail once, by putting a folder where the card store writes its staging
// file: the build stays reported, and the checker judges it again by itself
// after its retry delay, with no other report, and saves that verdict. The
// report's own wake is spent on the first judgment, so only the retry can
// wake the checker again.
func TestAVerdictThatCannotBeSavedIsJudgedAgain(t *testing.T) {
	checking, release := make(chan int, 2), make(chan struct{}, 2)
	tests := &standInCopy{check: func(_ context.Context, call int) (testwriter.Verdict, error) {
		checking <- call
		<-release
		return testwriter.Verdict{}, nil
	}}
	cp, revision := newReportFixture(t, Options{}, tests)
	cp.reports.retry = 200 * time.Millisecond
	reportTests(t, cp, revision)
	require.Equal(t, 1, receive(t, checking))
	staging := filepath.Join(cp.cards.dir, buildStagingFile)
	require.NoError(t, os.Mkdir(staging, 0o700))
	release <- struct{}{}
	require.Equal(t, 2, receive(t, checking), "the checker judges the build again by itself")
	assert.Equal(t, gensurla.TestsKindReported, mustStartedBuild(t, cp, asCaller("alice@example.com", CallerUser), "chg").TestWriter.Tests.Kind(),
		"the first verdict was not saved")
	require.NoError(t, os.Remove(staging))
	release <- struct{}{}
	build := waitForTests(t, cp, gensurla.TestsKindAccepted)
	assert.Equal(t, 1, build.TestWriter.Report, "no other report was needed")
}

// TestRetriesAfterAFailedSaveAreBounded makes every save of a verdict fail.
// The checker judges the report and then again by itself, each failure an
// error on the judgment's span, but only maxRetriesAfterFailure times: after
// that it stays idle, the build still reported, however long the test waits.
// Once saving works again, the next report is judged and its verdict saved.
func TestRetriesAfterAFailedSaveAreBounded(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	// The first check waits until saving is broken, after the report saved.
	broken := make(chan struct{})
	tests := &standInCopy{check: func(_ context.Context, call int) (testwriter.Verdict, error) {
		if call == 1 {
			<-broken
		}
		return testwriter.Verdict{}, nil
	}}
	cp, revision := newReportFixture(t, Options{TracerProvider: provider}, tests)
	cp.reports.retry = 50 * time.Millisecond
	reportTests(t, cp, revision)
	staging := filepath.Join(cp.cards.dir, buildStagingFile)
	require.NoError(t, os.Mkdir(staging, 0o700))
	close(broken)

	judgments := 1 + maxRetriesAfterFailure
	require.Eventually(t, func() bool { return tests.checked() == judgments }, 10*time.Second, 10*time.Millisecond)
	time.Sleep(10 * cp.reports.retry)
	assert.Equal(t, judgments, tests.checked(), "no judgment after the last retry")
	assert.Equal(t, gensurla.TestsKindReported, mustStartedBuild(t, cp, asCaller("alice@example.com", CallerUser), "chg").TestWriter.Tests.Kind())
	var failed int
	for _, span := range exporter.GetSpans() {
		if span.Name == "surla.controlplane.check_reported_tests" && span.Status.Code == codes.Error {
			failed++
		}
	}
	assert.Equal(t, judgments, failed, "each failed save is an error on its judgment's span")

	require.NoError(t, os.Remove(staging))
	second := reportTests(t, cp, revision)
	build := waitForTests(t, cp, gensurla.TestsKindAccepted)
	assert.Equal(t, second.Progress.Report, build.TestWriter.Report)
}

// TestABuildStartIsRefusedDuringACheck holds a check open: starting the
// build again is refused as busy, because the check holds the working-copy
// turn, and succeeds once the check has ended.
func TestABuildStartIsRefusedDuringACheck(t *testing.T) {
	checking, release := make(chan int, 1), make(chan struct{})
	tests := &standInCopy{check: func(_ context.Context, call int) (testwriter.Verdict, error) {
		checking <- call
		<-release
		return testwriter.Verdict{}, nil
	}}
	cp, revision := newReportFixture(t, Options{}, tests)
	user := asCaller("alice@example.com", CallerUser)
	start := &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: revision}
	reportTests(t, cp, revision)
	receive(t, checking)
	_, err := cp.StartBuild(user, start)
	assertRefused(t, err, "working_copy_busy")
	close(release)
	waitForTests(t, cp, gensurla.TestsKindAccepted)
	_, err = cp.StartBuild(user, start)
	require.NoError(t, err, "the check released the turn")
}

// TestAStoppedServerSavesNothingAndARestartedOneJudges stops the server
// while a check runs: Close waits until the check, which takes a moment to
// stop, has returned, and nothing is saved, so the build stays reported. A server started again
// on the same folders judges that build without another report. Its working
// copy cannot be opened here, as the fixture's project is no git repository,
// so the verdict is that the check could not run, with the step to build
// again.
func TestAStoppedServerSavesNothingAndARestartedOneJudges(t *testing.T) {
	checking, ended := make(chan int, 1), make(chan struct{})
	tests := &standInCopy{check: func(ctx context.Context, call int) (testwriter.Verdict, error) {
		checking <- call
		<-ctx.Done()
		// A sandboxed step takes a moment to stop; Close waits for it.
		time.Sleep(300 * time.Millisecond)
		close(ended)
		return testwriter.Verdict{}, ctx.Err()
	}}
	opts := Options{CardsDir: t.TempDir(), WorkspaceDir: t.TempDir()}
	cp, revision := newReportFixture(t, opts, tests)
	reportTests(t, cp, revision)
	receive(t, checking)
	reported := readBuildFile(t, cp, "chg")
	require.NoError(t, cp.Close())
	select {
	case <-ended:
	default:
		t.Fatal("Close returned before the check saw its context end")
	}
	assert.Equal(t, reported, readBuildFile(t, cp, "chg"), "nothing is saved after the report")
	assert.Empty(t, tests.recorded())

	opts.Team = &TeamBinding{Owner: "alice@example.com", Service: &teamServiceFixture{}}
	restarted := newControlPlane(t, opts)
	build := waitForTests(t, restarted, gensurla.TestsKindCouldNotRun)
	assert.Equal(t, 1, build.TestWriter.Report)
	failed, _ := build.TestWriter.Tests.AsCouldNotRun()
	assert.Contains(t, failed.Reason, "could not open the test writer's working copy")
	assert.Equal(t, stepOpenCopy, failed.Step)
}

// TestAReportIsJudgedThroughTheProductionCopy starts a build of a real project
// whose test module lacks its go.sum, through the production working copy,
// then reports. The report checker opens the same copy through the production
// owner, writes the required tests again and saves its own verdict for the
// report, with the module's finding for the test writer to fix.
func TestAReportIsJudgedThroughTheProductionCopy(t *testing.T) {
	if err := candidate.CheckSandbox(); err != nil {
		t.Skipf("judging through the production copy requires the candidate sandbox: %v", err)
	}
	eval := map[string]string{
		"go.mod":           fmt.Sprintf("module example.com/shop/eval\n\ngo 1.27.3\n\nrequire goa.design/goa/v3 %s\n", testmodule.SurlaVersion(t, "goa.design/goa/v3")),
		"design/design.go": buildProbeDesign,
	}
	_, project := productionBuildProject(t, ".", eval)
	cp := newControlPlane(t, Options{
		WorkspaceDir: project, Team: &TeamBinding{Owner: "alice@example.com", Service: &teamServiceFixture{}},
	})
	card := confirmedChange(t, cp, "chg")
	started, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{
		ChangeID: "chg", RequestRevision: card.RequestRevision,
	})
	require.NoError(t, err)
	require.Equal(t, gensurla.TestsKindNeedsChanges, started.Build.TestWriter.Tests.Kind(), "the start found the module's problem")
	assert.Equal(t, 0, started.Build.TestWriter.Report)

	reported := reportTests(t, cp, card.RequestRevision)
	assert.Equal(t, 1, reported.Progress.Report)
	build := waitForTests(t, cp, gensurla.TestsKindNeedsChanges)
	assert.Equal(t, 1, build.TestWriter.Report, "the verdict answers the report")
	assignment, err := cp.GetTestWriterAssignment(asCaller("alice@example.com", CallerTestWriter), &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
	require.NoError(t, err)
	require.Len(t, assignment.Findings, 1)
	assert.Contains(t, assignment.Findings[0], "go.sum")
	assert.NotContains(t, assignment.Findings[0], "report_tests_finished", "the checker's own finding, not the start's")
}

// TestTheWholeCheckLimit judges with a shortened limit and stand-in checks
// that sleep without watching their context: just below the limit the copy
// is accepted; at and just above it the check could not run, naming the limit
// and the user's step. A check that ends with its context at the limit could
// not run either, while findings given at the limit stand.
func TestTheWholeCheckLimit(t *testing.T) {
	const limit = 500 * time.Millisecond
	sleepThen := func(d time.Duration, v testwriter.Verdict) func(context.Context, int) (testwriter.Verdict, error) {
		return func(context.Context, int) (testwriter.Verdict, error) {
			time.Sleep(d)
			return v, nil
		}
	}
	cases := []struct {
		name  string
		check func(context.Context, int) (testwriter.Verdict, error)
		want  gensurla.TestsKind
	}{
		{name: "just below", check: sleepThen(limit-200*time.Millisecond, testwriter.Verdict{}), want: gensurla.TestsKindAccepted},
		{name: "at", check: sleepThen(limit, testwriter.Verdict{}), want: gensurla.TestsKindCouldNotRun},
		{name: "just above", check: sleepThen(limit+200*time.Millisecond, testwriter.Verdict{}), want: gensurla.TestsKindCouldNotRun},
		{name: "ended with its context", check: func(ctx context.Context, int2 int) (testwriter.Verdict, error) {
			<-ctx.Done()
			return testwriter.Verdict{}, ctx.Err()
		}, want: gensurla.TestsKindCouldNotRun},
		{name: "findings at the limit", check: sleepThen(limit, needsChanges("The suite command did not finish building within 10 minutes.")),
			want: gensurla.TestsKindNeedsChanges},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp, revision := newReportFixture(t, Options{}, &standInCopy{check: tc.check})
			cp.reports.limit = limit
			reportTests(t, cp, revision)
			build := waitForTests(t, cp, tc.want)
			if failed, ok := build.TestWriter.Tests.AsCouldNotRun(); ok {
				assert.Contains(t, failed.Reason, "did not finish within 500ms")
				assert.Equal(t, stepLimit, failed.Step)
			}
		})
	}
	assert.Equal(t, "15 minutes", limitWords(checkLimit))
}

// TestACheckThatCannotRunNamesTheUsersStep makes each step of a judgment fail
// for a cause the test writer cannot fix. Each saves could_not_run with a
// reason holding no absolute path and at most the contract's length, the
// step for its cause, and an error on the judgment's span; the test writer's
// assignment becomes unread and the builder's stays read. An author's
// failure from Prepare is a finding instead, recorded as no error.
func TestACheckThatCannotRunNamesTheUsersStep(t *testing.T) {
	cases := []struct {
		name       string
		copyErr    error
		tests      *standInCopy
		wantKind   gensurla.TestsKind
		wantReason []string
		wantStep   string
	}{
		{name: "the working copy cannot be opened", copyErr: errors.New("open /Users/someone/shop/.surla-tests: permission denied"),
			tests: &standInCopy{check: accepting}, wantKind: gensurla.TestsKindCouldNotRun,
			wantReason: []string{"could not open the test writer's working copy", "open <absolute path>: permission denied"}, wantStep: stepOpenCopy},
		{name: "the sandbox does not start", tests: &standInCopy{prepareErr: fmt.Errorf("%w: sandbox-exec: sandbox_apply: Operation not permitted", candidate.ErrIsolationUnavailable)},
			wantKind: gensurla.TestsKindCouldNotRun, wantReason: []string{"sandbox could not start", "Operation not permitted"}, wantStep: stepSandbox},
		{name: "the history was replaced", tests: &standInCopy{check: failing(fmt.Errorf("measure the copy's changes: %w", testwriter.ErrNoCommonBase))},
			wantKind: gensurla.TestsKindCouldNotRun, wantReason: []string{"share no history"}, wantStep: stepNoCommonBase},
		{name: "git fails", tests: &standInCopy{check: failing(errors.New("git write-tree in /private/var/folders/x/check: exit status 128"))},
			wantKind: gensurla.TestsKindCouldNotRun, wantReason: []string{"check could not run", "git write-tree in <absolute path>: exit status 128"}, wantStep: stepFailure},
		{name: "recording fails", tests: &standInCopy{check: accepting, record: func(context.Context, int) (string, error) {
			return "", errors.New("git commit-tree: exit status 1")
		}}, wantKind: gensurla.TestsKindCouldNotRun, wantReason: []string{"could not record the accepted tests", "exit status 1"}, wantStep: stepFailure},
		{name: "a long failure", tests: &standInCopy{check: failing(errors.New(strings.Repeat("é", 2*maxCheckReason)))},
			wantKind: gensurla.TestsKindCouldNotRun, wantReason: []string{"…"}, wantStep: stepFailure},
		{name: "the author's design does not compile", tests: &standInCopy{prepareErr: &testwriter.AuthorError{Finding: testwriter.Finding{
			Kind: testwriter.FindingGeneration, Message: "Code generation failed while compiling the design.",
		}}}, wantKind: gensurla.TestsKindNeedsChanges},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
			cp, revision := newReportFixture(t, Options{TracerProvider: provider}, tc.tests)
			if tc.copyErr != nil {
				cp.buildCopyFactory = func(context.Context, string, scaffold.Layout) (buildCopy, error) { return nil, tc.copyErr }
			}
			readBothAssignments(t, cp)
			reportTests(t, cp, revision)

			build := waitForTests(t, cp, tc.wantKind)
			assert.False(t, build.TestWriter.Read)
			assert.True(t, build.Builder.Read)
			span := waitForSpan(t, exporter, "surla.controlplane.check_reported_tests")
			failed, ok := build.TestWriter.Tests.AsCouldNotRun()
			if !ok {
				assert.Equal(t, codes.Unset, span.Status.Code, "findings are not a failure")
				return
			}
			assert.Equal(t, codes.Error, span.Status.Code)
			for _, want := range tc.wantReason {
				assert.Contains(t, failed.Reason, want)
			}
			assert.NotRegexp(t, `(^|[\s"'(=:])/[A-Za-z]`, failed.Reason, "no absolute path")
			assert.LessOrEqual(t, utf8.RuneCountInString(failed.Reason), maxCheckReason)
			assert.Equal(t, tc.wantStep, failed.Step)
		})
	}
	for _, step := range []string{stepOpenCopy, stepSandbox, stepNoCommonBase, stepLimit, stepFailure} {
		assert.True(t, boundedText(step, maxCheckStep), "each step fits the contract: %s", step)
	}
}

// TestStoredEvaluatorProjectsNeverJudge uses a project that verifies with its
// stored evaluator. A report on its builder-only build is refused as for a
// change with no test assignment. A build saved with a test writer before
// the settings were stored reads as not used: a report on it is refused,
// saying why, the findings read is refused, and the build, saved as
// reported, is never judged.
func TestStoredEvaluatorProjectsNeverJudge(t *testing.T) {
	cp, revision := newReportFixture(t, Options{Verification: &VerificationConfig{RunsDir: t.TempDir()}}, &standInCopy{})
	tests := asCaller("alice@example.com", CallerTestWriter)
	report := &gensurla.ReportTestsFinishedPayload{ChangeID: "chg", RequestRevision: revision}
	before := readBuildFile(t, cp, "chg")
	_, err := cp.ReportTestsFinished(tests, report)
	assertRefused(t, err, "not_found")
	assert.Equal(t, before, readBuildFile(t, cp, "chg"), "a refused report changes nothing")

	editBuildFile(t, cp, "chg", func(f map[string]any) map[string]any {
		f["roles"] = buildRolesBuilderAndTestWriter
		f["test_writer"].(map[string]any)["working_folder"] = t.TempDir()
		return f
	})
	before = readBuildFile(t, cp, "chg")
	_, err = cp.ReportTestsFinished(tests, report)
	assertRefused(t, err, "conflict")
	assert.ErrorContains(t, err, "no test writer is used: this project verifies with its stored evaluator")
	assert.Equal(t, before, readBuildFile(t, cp, "chg"), "a refused report changes nothing")
	editBuildFile(t, cp, "chg", savedTestWriter(map[string]any{"state": testsReported}))
	assert.Empty(t, cp.reports.reported(t.Context()), "no build is judged")
	_, err = cp.GetTestFindings(asCaller("alice@example.com", CallerUser), &gensurla.GetTestFindingsPayload{ChangeID: "chg"})
	assertRefused(t, err, "conflict")
}

// TestOnlyTheTestWriterAndTheUserReadFindings saves findings and reads the
// build as every role over HTTP and over MCP: every read of the build carries
// their number and never their text; the test writer reads the text in its
// assignment and the user in get_test_findings, an HTTP read no other caller
// may make and no tool offers. Once the tests are reported again, the
// findings read is refused.
func TestOnlyTheTestWriterAndTheUserReadFindings(t *testing.T) {
	const finding = "RefundOverLimit is not implemented"
	cp, revision := newReportFixture(t, Options{}, &standInCopy{})
	srv := roleServer(t, cp)
	at := "2026-11-13T09:30:00Z"
	editBuildFile(t, cp, "chg", savedTestWriter(map[string]any{"state": testsNeedChanges, "findings": []string{finding}, "checked_at": at}))
	started := mustStartedBuild(t, cp, asCaller("alice@example.com", CallerUser), "chg").StartedAt

	for _, token := range []string{"user", "guide", "builder", "tests"} {
		status, body := httpCall(t, srv.URL, token, http.MethodGet, "/changes/chg/build", "")
		require.Equal(t, http.StatusOK, status, body)
		assert.JSONEq(t, "1", jsonAt(t, []byte(body), "build", "test_writer", "tests", "finding_count"), token)
		assert.NotContains(t, body, finding, "get_build over HTTP as %s", token)
		failure, result := mcpToolCall(t, srv.URL, token, "get_build", `{"change_id":"chg"}`)
		require.Empty(t, failure)
		assert.NotContains(t, string(result), finding, "get_build over MCP as %s", token)
		assert.False(t, listToolNames(t, srv.URL, token)["get_test_findings"], "no tool reads the findings")
	}
	status, body := httpCall(t, srv.URL, "builder", http.MethodGet, "/assignments/builder/chg", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.NotContains(t, body, finding)
	status, body = httpCall(t, srv.URL, "tests", http.MethodGet, "/assignments/test-writer", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.NotContains(t, body, finding, "the list of assignments counts findings")
	status, body = httpCall(t, srv.URL, "tests", http.MethodGet, "/assignments/test-writer/chg", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.JSONEq(t, `["`+finding+`"]`, jsonAt(t, []byte(body), "findings"), "the test writer reads the text over HTTP")
	failure, result := mcpToolCall(t, srv.URL, "tests", "get_test_writer_assignment", `{"change_id":"chg"}`)
	require.Empty(t, failure)
	assert.JSONEq(t, `["`+finding+`"]`, jsonAt(t, result, "findings"), "the test writer reads the text over MCP")

	status, body = httpCall(t, srv.URL, "user", http.MethodGet, "/changes/chg/build/findings", "")
	require.Equal(t, http.StatusOK, status, body)
	assert.JSONEq(t, fmt.Sprintf(`{"change_id":"chg","request_revision":%q,"started_at":%q,"report":1,"checked_at":%q,"findings":[%q]}`,
		revision, started, at, finding), body)
	for _, token := range []string{"guide", "builder", "tests"} {
		status, body := httpCall(t, srv.URL, token, http.MethodGet, "/changes/chg/build/findings", "")
		assert.Equal(t, http.StatusForbidden, status, token)
		assert.Contains(t, body, "may not call get_test_findings")
	}
	reportTests(t, cp, revision)
	status, body = httpCall(t, srv.URL, "user", http.MethodGet, "/changes/chg/build/findings", "")
	assert.Equal(t, http.StatusConflict, status, body)
	assert.Contains(t, body, "their state is reported")
}

// TestAcceptedTestsCarryTheContractsSentence passes the sentence the server
// writes through the generated client's validation of an accepted result,
// which allows only the design's sentence.
func TestAcceptedTestsCarryTheContractsSentence(t *testing.T) {
	commit, at := standInCommit, "2026-11-13T09:30:00Z"
	for _, tc := range []struct {
		sentence string
		valid    bool
	}{
		{sentence: testsNotJudged, valid: true},
		{sentence: "Surla judged everything.", valid: false},
	} {
		err := genclient.ValidateTestsAcceptedResponseBody(&genclient.TestsAcceptedResponseBody{Commit: &commit, CheckedAt: &at, NotJudged: &tc.sentence})
		assert.Equal(t, tc.valid, err == nil, "%q: %v", tc.sentence, err)
	}
}

// TestVerdictsGiveSupervisionNewGenerations reads both assignments, then
// saves verdicts: findings owe the test writer a read of a new generation and
// leave the builder's read; an acceptance owes both roles a read, each of a
// generation it never had.
func TestVerdictsGiveSupervisionNewGenerations(t *testing.T) {
	answers := []testwriter.Verdict{needsChanges("RefundOverLimit is not implemented"), {}}
	tests := &standInCopy{check: func(_ context.Context, call int) (testwriter.Verdict, error) { return answers[call-1], nil }}
	cp, revision := newReportFixture(t, Options{}, tests)
	unread, err := cp.assignmentGenerations(t.Context())
	require.NoError(t, err)
	require.Len(t, unread, 2)
	readBothAssignments(t, cp)
	owed, err := cp.assignmentGenerations(t.Context())
	require.NoError(t, err)
	assert.Empty(t, owed)

	reportTests(t, cp, revision)
	waitForTests(t, cp, gensurla.TestsKindNeedsChanges)
	afterFindings, err := cp.assignmentGenerations(t.Context())
	require.NoError(t, err)
	require.Len(t, afterFindings, 1)
	assert.Equal(t, "test_writer", afterFindings[0].Role)
	assert.NotEqual(t, unread[1].Generation, afterFindings[0].Generation)

	readBothAssignments(t, cp)
	reportTests(t, cp, revision)
	waitForTests(t, cp, gensurla.TestsKindAccepted)
	afterAcceptance, err := cp.assignmentGenerations(t.Context())
	require.NoError(t, err)
	require.Len(t, afterAcceptance, 2)
	for i, role := range []string{"builder", "test_writer"} {
		assert.Equal(t, role, afterAcceptance[i].Role)
		assert.NotEqual(t, unread[i].Generation, afterAcceptance[i].Generation, role)
	}
	assert.NotEqual(t, afterFindings[0].Generation, afterAcceptance[1].Generation)
}

// newReportFixture returns a control plane with options opts and a bound
// team, so it runs its report checker, in which alice's change "chg" is
// confirmed and built, and the built revision. After the start, the checker
// opens copy as the test writer's working copy: the start itself wrote the
// required tests into the fixture's own copy, and the checker reads the
// factory only once a report wakes it.
func newReportFixture(t *testing.T, opts Options, copy *standInCopy) (*ControlPlane, string) {
	t.Helper()
	cp := newBuildControlPlane(t, opts)
	card := confirmedChange(t, cp, "chg")
	_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	cp.buildCopyFactory = func(context.Context, string, scaffold.Layout) (buildCopy, error) { return copy, nil }
	return cp, card.RequestRevision
}

// reportTests reports alice's change "chg" finished at revision as its test
// writer.
func reportTests(t *testing.T, cp *ControlPlane, revision string) *gensurla.TestWriterAssignmentItem {
	t.Helper()
	item, err := cp.ReportTestsFinished(asCaller("alice@example.com", CallerTestWriter), &gensurla.ReportTestsFinishedPayload{
		ChangeID: "chg", RequestRevision: revision,
	})
	require.NoError(t, err)
	return item
}

// readBothAssignments reads the builder's and the test writer's assignments of
// alice's change "chg", recording both as read.
func readBothAssignments(t *testing.T, cp *ControlPlane) {
	t.Helper()
	_, err := cp.GetBuilderAssignment(asCaller("alice@example.com", CallerBuilder), &gensurla.GetBuilderAssignmentPayload{ChangeID: "chg"})
	require.NoError(t, err)
	_, err = cp.GetTestWriterAssignment(asCaller("alice@example.com", CallerTestWriter), &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
	require.NoError(t, err)
}

// waitForTests reads alice's build of "chg" until its tests are in state
// want, for at most ten seconds, and returns it.
func waitForTests(t *testing.T, cp *ControlPlane, want gensurla.TestsKind) *gensurla.StartedBuild {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		build := mustStartedBuild(t, cp, asCaller("alice@example.com", CallerUser), "chg")
		if build.TestWriter.Tests.Kind() == want {
			return build
		}
		if time.Now().After(deadline) {
			require.Failf(t, "no verdict", "the tests are %s after ten seconds, not %s", build.TestWriter.Tests.Kind(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// receive returns the next value from ch, failing the test after ten
// seconds.
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		require.FailNow(t, "nothing arrived within ten seconds")
		var zero T
		return zero
	}
}

// needsChanges returns a stand-in verdict with one build finding per message.
func needsChanges(messages ...string) testwriter.Verdict {
	var v testwriter.Verdict
	for _, m := range messages {
		v.Findings = append(v.Findings, testwriter.Finding{Kind: testwriter.FindingBuild, Message: m})
	}
	return v
}

// failing returns a stand-in check that fails with err.
func failing(err error) func(context.Context, int) (testwriter.Verdict, error) {
	return func(context.Context, int) (testwriter.Verdict, error) { return testwriter.Verdict{}, err }
}

// waitForSpan returns the last ended span named name, waiting for one for at
// most ten seconds: a judgment ends its span just after it saves its verdict.
func waitForSpan(t *testing.T, exporter *tracetest.InMemoryExporter, name string) tracetest.SpanStub {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		spans := exporter.GetSpans()
		for i := len(spans) - 1; i >= 0; i-- {
			if spans[i].Name == name {
				return spans[i]
			}
		}
		if time.Now().After(deadline) {
			require.FailNow(t, "no span named "+name+" after ten seconds")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
