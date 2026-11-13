// This file judges the test writer's reports: one worker per project checks
// each reported build's working copy against the Story saved with the build,
// records accepted tests on the tests branch, and saves the verdict, so a
// verdict follows report_tests_finished without another call.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/testwriter"
	"github.com/goadesign/surla/verify/candidate"
)

type (
	// testsLibrary is what the report checker uses of internal/testwriter: a
	// Checker's Prepare and Check, and Record. testwriterLibrary binds the
	// real one; tests bind stand-ins.
	testsLibrary interface {
		// Prepare writes the required tests for stories into the copy.
		Prepare(
			ctx context.Context, wc testwriter.WorkingCopy, s testwriter.Suite, stories []designeval.StorySummary,
		) ([]designeval.StoryScenario, error)
		// Check judges the copy against stories.
		Check(
			ctx context.Context, wc testwriter.WorkingCopy, s testwriter.Suite, stories []designeval.StorySummary,
		) (testwriter.Verdict, error)
		// Record commits an accepted verdict on the tests branch and returns
		// the commit.
		Record(ctx context.Context, v testwriter.Verdict, message string) (string, error)
	}

	// testwriterLibrary is the testsLibrary of internal/testwriter.
	testwriterLibrary struct {
		*testwriter.Checker
	}

	// reportChecker is the one worker per project that judges the test
	// writer's reports. run judges every build found reported when it
	// starts, then those each report wakes it for. Each judgment runs in the
	// project's working-copy turn and is bounded by limit. Its last step
	// (confirm that the build still answers to the report checked, record
	// accepted tests, save the verdict) holds reportMu, so a report cannot
	// fall between the confirmation and the commit. When ctx ends, the
	// sandboxed steps end with it and nothing is saved: the build stays
	// reported, and the next run judges it.
	reportChecker struct {
		// cp owns the builds, the turn, reportMu and reportWake.
		cp *ControlPlane
		// tests prepares, checks and records the working copy.
		tests testsLibrary
		// copyFor opens the project's test working copy and locates its
		// test module and program.
		copyFor func(ctx context.Context) (testwriter.WorkingCopy, testwriter.Suite, error)
		// limit bounds one report's judgment; checkLimit outside tests.
		limit time.Duration
		// retry is how long after a failed last step the checker wakes to
		// judge again; retryAfterFailure outside tests.
		retry time.Duration
		// now gives each verdict its time.
		now func() time.Time
	}

	// reportIdentity names the report a judgment answers: the build (its
	// change, revision and start) and the report's number. A new start
	// changes the start time and a new report the number, so a verdict is
	// saved only while the build still answers to the identity it was
	// checked for. Only the report checker saves a verdict, so a build that
	// answers to the identity is still reported.
	reportIdentity struct {
		// changeID, revision and startedAt identify the build.
		changeID, revision string
		startedAt          time.Time
		// report is the build's report number.
		report int
	}

	// openCopyError is a failure to open the test writer's working copy.
	openCopyError struct {
		// err is copyFor's error.
		err error
	}
)

const (
	// checkLimit bounds one report's whole judgment: writing the required
	// tests, the check with its build (ten minutes) and listing (sixty
	// seconds), and recording. The five minutes over the two inner limits
	// cover generation, dependency downloads and git, so it is reached only
	// when something other than the author's build or listing hangs: a cause
	// the test writer cannot fix.
	checkLimit = 15 * time.Minute

	// retryAfterFailure is how long the checker waits before judging again
	// when its last step failed: the build could not be read to confirm it,
	// or the verdict could not be saved. The build then stays reported, and
	// without a later report nothing else would wake the checker. A minute
	// lets a passing cause, such as a full disk freed by the user, clear
	// without judging in a tight loop.
	retryAfterFailure = time.Minute

	// stepOpenCopy, stepSandbox, stepNoCommonBase, stepLimit and stepFailure
	// are the user's steps of a check that could not run, one per cause; each
	// fits the design's 512 characters.
	stepOpenCopy = "Build the change again: starting a build opens the test writer's working copy " +
		"and names the step when it cannot."
	stepSandbox = "Run surla from a terminal on macOS that is not itself sandboxed, " +
		"then ask the test writer to report again."
	stepNoCommonBase = "Delete the test writer's working copy and branch, then build the change again: " +
		"in the project folder run git worktree remove .surla-tests and git branch -D surla/tests."
	stepLimit = "Check that git and Go module downloads work in the project folder, " +
		"then ask the test writer to report again."
	stepFailure = "Make sure the project's git repository works (git status in the project folder) " +
		"and the disk has free space, then ask the test writer to report again."

	// replacedEvent is the span event of a judgment whose report a newer
	// report or build replaced before its verdict was saved.
	replacedEvent = "a newer report or build replaced the one checked; nothing saved"
)

// testwriterLibrary is the production testsLibrary.
var _ testsLibrary = testwriterLibrary{}

// Record commits v with testwriter.Record.
func (testwriterLibrary) Record(ctx context.Context, v testwriter.Verdict, message string) (string, error) {
	return testwriter.Record(ctx, v, message)
}

// Error returns the failure's text.
func (e *openCopyError) Error() string {
	return e.err.Error()
}

// Unwrap returns copyFor's error.
func (e *openCopyError) Unwrap() error {
	return e.err
}

// newReportChecker returns the report checker of cp, judging with tests in
// the working copy copyFor opens, within checkLimit.
func newReportChecker(
	cp *ControlPlane,
	tests testsLibrary,
	copyFor func(context.Context) (testwriter.WorkingCopy, testwriter.Suite, error),
) *reportChecker {
	return &reportChecker{cp: cp, tests: tests, copyFor: copyFor, limit: checkLimit, retry: retryAfterFailure, now: time.Now}
}

// run judges reported builds until ctx ends: first every build found reported
// when it starts, as after a restart, then every reported build again each
// time report_tests_finished wakes it. A report made during a judgment leaves
// its wake waiting, so it is judged after the judgment in progress. Once ctx
// ends no judgment starts, even when a wake is waiting too.
func (r *reportChecker) run(ctx context.Context) {
	for ctx.Err() == nil {
		for _, changeID := range r.reported(ctx) {
			if ctx.Err() != nil {
				return
			}
			r.judge(ctx, changeID)
		}
		select {
		case <-ctx.Done():
		case <-r.cp.reportWake:
		}
	}
}

// reported returns the changes whose usable builds (usableBuild) are
// reported, in the order the builds started. A project that verifies with its
// stored evaluator uses no test writer, so it has none. A card or build that cannot be read is left
// out and recorded on a span: it fails every read of it, and the next wake
// reads it again.
func (r *reportChecker) reported(ctx context.Context) []string {
	if r.cp.storedEvaluator() {
		return nil
	}
	r.cp.mu.RLock()
	defer r.cp.mu.RUnlock()
	records, err := r.cp.cards.all()
	if err != nil {
		r.recordScanFailure(ctx, err)
		return nil
	}
	var builds []buildFile
	for _, record := range records {
		saved, found, err := r.cp.cards.loadBuild(record.card.ChangeID)
		if err != nil {
			r.recordScanFailure(ctx, err)
			continue
		}
		if found && r.cp.usableBuild(saved) && saved.TestWriter.State == testsReported {
			builds = append(builds, saved)
		}
	}
	slices.SortFunc(builds, func(a, b buildFile) int { return a.StartedAt.Compare(b.StartedAt) })
	changes := make([]string, len(builds))
	for i, saved := range builds {
		changes[i] = saved.ChangeID
	}
	return changes
}

// judge judges the report of changeID's build in the working-copy turn.
//
// Phase 1 takes the turn, waiting for it, or returns when ctx ends. Phase 2
// reads the build and keeps the identity of its report; a build that is no
// longer reported is left alone. Phase 3 writes the required tests into the
// copy from the build's Story and checks the copy, within limit. Phase 4,
// under reportMu, confirms the build still answers to the identity, records
// accepted tests and saves the verdict, marking the test writer's assignment
// unread, and the builder's too for an acceptance. When ctx ends first,
// or a newer report or build replaced the one checked, nothing is recorded
// or saved. When the last step fails, the checker judges again after
// r.retry (wakeLater).
func (r *reportChecker) judge(ctx context.Context, changeID string) {
	// Phase 1: take the turn. A free turn and an ended ctx may both be ready.
	select {
	case r.cp.workingCopyTurn <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-r.cp.workingCopyTurn }()
	if ctx.Err() != nil {
		return
	}

	// Phase 2: read the build.
	r.cp.mu.RLock()
	saved, found, err := r.cp.cards.loadBuild(changeID)
	r.cp.mu.RUnlock()
	if err != nil {
		r.recordScanFailure(ctx, err)
		return
	}
	if !found || !r.cp.usableBuild(saved) || saved.TestWriter.State != testsReported {
		return
	}
	id := reportIdentityOf(saved)
	ctx, span := r.cp.tracer("surla/controlplane").Start(ctx, "surla.controlplane.check_reported_tests", trace.WithAttributes(
		attribute.String("surla.change_id", id.changeID),
		attribute.String("surla.request_revision", id.revision),
		attribute.Int("surla.report_number", id.report),
	))
	defer span.End()

	// Phase 3: prepare and check within the limit.
	deadline := time.Now().Add(r.limit)
	checkCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	v, err := r.check(checkCtx, saved.Stories)
	if ctx.Err() != nil {
		span.AddEvent("stopped before a verdict; the build stays reported")
		return
	}
	verdict := r.verdictOf(v, err, deadline)

	// Phase 4: confirm, record and save.
	r.cp.reportMu.Lock()
	defer r.cp.reportMu.Unlock()
	if verdict.State == testsAccepted {
		current, err := r.answersTo(id)
		if err != nil {
			recordJudgeFailure(span, err, "confirm build")
			r.wakeLater()
			return
		}
		if !current {
			span.AddEvent(replacedEvent)
			return
		}
		message := fmt.Sprintf("Add the tests Surla accepted for change %s, report %d", id.changeID, id.report)
		commit, err := r.tests.Record(checkCtx, v, message)
		switch {
		case ctx.Err() != nil:
			span.AddEvent("stopped before the tests were recorded; the build stays reported")
			return
		case err != nil:
			verdict = r.couldNotRun(err, deadline, "Surla could not record the accepted tests")
		default:
			verdict.Commit = commit
		}
	}
	saved, current, err := r.saveVerdict(id, verdict)
	if err != nil {
		recordJudgeFailure(span, err, "save verdict")
		r.wakeLater()
		return
	}
	if !current {
		span.AddEvent(replacedEvent)
		return
	}
	span.SetAttributes(attribute.String("surla.tests_state", saved.TestWriter.State))
	if saved.TestWriter.State == testsCouldNotRun {
		span.RecordError(errors.New(saved.TestWriter.Reason))
		span.SetStatus(codes.Error, "the check could not run")
	}
}

// check opens the working copy, writes the required tests for stories into
// it, and checks it. An error from Prepare that the author caused is an
// *testwriter.AuthorError; any other error says nothing about the author's
// work.
func (r *reportChecker) check(ctx context.Context, stories []designeval.StorySummary) (testwriter.Verdict, error) {
	wc, s, err := r.copyFor(ctx)
	if err != nil {
		return testwriter.Verdict{}, &openCopyError{err: err}
	}
	if _, err := r.tests.Prepare(ctx, wc, s, stories); err != nil {
		return testwriter.Verdict{}, err
	}
	return r.tests.Check(ctx, wc, s, stories)
}

// answersTo reports whether id's build still answers to id, before accepted
// tests are recorded for it. Callers hold reportMu, so no report can change
// the answer until they save.
func (r *reportChecker) answersTo(id reportIdentity) (bool, error) {
	r.cp.mu.RLock()
	defer r.cp.mu.RUnlock()
	saved, found, err := r.cp.cards.loadBuild(id.changeID)
	if err != nil {
		return false, err
	}
	return found && reportIdentityOf(saved) == id, nil
}

// saveVerdict saves verdict on id's build, with its time, when the build
// still answers to id, and marks the test writer's assignment unread, and the
// builder's too for an acceptance. It returns the saved build, or false and
// saves nothing when a newer report or build replaced id. Callers hold
// reportMu.
func (r *reportChecker) saveVerdict(id reportIdentity, verdict testWriterAssignment) (buildFile, bool, error) {
	r.cp.mu.Lock()
	defer r.cp.mu.Unlock()
	saved, found, err := r.cp.cards.loadBuild(id.changeID)
	if err != nil || !found || reportIdentityOf(saved) != id {
		return buildFile{}, false, err
	}
	verdict.WorkingFolder = saved.TestWriter.WorkingFolder
	verdict.CheckedAt = r.now().UTC()
	saved.TestWriter = verdict
	if verdict.State == testsAccepted {
		saved.Builder.Read = false
	}
	if err := r.cp.cards.saveBuild(saved); err != nil {
		return buildFile{}, false, err
	}
	return saved, true, nil
}

// wakeLater wakes the checker after r.retry, as a report would, so a build
// whose judgment was lost is judged again. A wake already waiting stands for
// it, and one sent after the checker stopped is never read.
func (r *reportChecker) wakeLater() {
	time.AfterFunc(r.retry, func() {
		select {
		case r.cp.reportWake <- struct{}{}:
		default:
		}
	})
}

// recordScanFailure records on its own span that the report checker could not
// read a card or a build.
func (r *reportChecker) recordScanFailure(ctx context.Context, err error) {
	_, span := r.cp.tracer("surla/controlplane").Start(ctx, "surla.controlplane.read_reported_tests")
	defer span.End()
	recordJudgeFailure(span, err, "read builds")
}

// reportIdentityOf returns the identity of saved's latest report.
func reportIdentityOf(saved buildFile) reportIdentity {
	return reportIdentity{
		changeID:  saved.ChangeID,
		revision:  saved.RequestRevision,
		startedAt: saved.StartedAt,
		report:    saved.ReportNumber,
	}
}

// verdictOf returns the verdict to save for a judgment that returned v and err
// with the given deadline, without its time. Findings stand whenever the
// author can act on them, even at the deadline. Otherwise a judgment that
// returned at or after its deadline could not run, whatever it returned, so
// the limit is reached exactly at the deadline. Any other error could not
// run either; an accepted copy is accepted, with its commit set when it is
// recorded.
func (r *reportChecker) verdictOf(v testwriter.Verdict, err error, deadline time.Time) testWriterAssignment {
	if author := (*testwriter.AuthorError)(nil); errors.As(err, &author) {
		return testWriterAssignment{State: testsNeedChanges, Findings: []string{author.Finding.Message}}
	}
	if err == nil && !v.Accepted() {
		findings := make([]string, len(v.Findings))
		for i, f := range v.Findings {
			findings[i] = f.Message
		}
		return testWriterAssignment{State: testsNeedChanges, Findings: findings}
	}
	if err != nil || !time.Now().Before(deadline) {
		return r.couldNotRun(err, deadline, "Surla's check could not run")
	}
	return testWriterAssignment{State: testsAccepted}
}

// couldNotRun returns the verdict of a check that could not run because of
// err, or because it reached its limit when the deadline has passed: a
// reason, without absolute paths and at most maxCheckReason characters, and
// the user's step for the cause. failed says what failed, for a cause no
// other case names.
func (r *reportChecker) couldNotRun(err error, deadline time.Time, failed string) testWriterAssignment {
	var reason, step string
	var open *openCopyError
	switch {
	case !time.Now().Before(deadline):
		reason, step = fmt.Sprintf("Checking the tests did not finish within %s, "+
			"though building the test program and listing its tests each have their own shorter limit.", limitWords(r.limit)), stepLimit
	case errors.As(err, &open):
		reason, step = "Surla could not open the test writer's working copy: "+err.Error(), stepOpenCopy
	case errors.Is(err, candidate.ErrIsolationUnavailable):
		reason, step = "Surla's sandbox could not start: "+err.Error(), stepSandbox
	case errors.Is(err, testwriter.ErrNoCommonBase):
		reason, step = "Surla cannot tell which files the test writer changed: "+err.Error(), stepNoCommonBase
	default:
		reason, step = failed+": "+err.Error(), stepFailure
	}
	return testWriterAssignment{State: testsCouldNotRun, Reason: boundReason(testwriter.WithoutAbsolutePaths(reason)), Step: step}
}

// limitWords words a limit for a reason: whole minutes as minutes, as for
// checkLimit, anything else as Go prints a duration.
func limitWords(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	}
	return d.String()
}

// boundReason shortens reason to at most maxCheckReason characters, ending a
// shortened one with an ellipsis.
func boundReason(reason string) string {
	if utf8.RuneCountInString(reason) <= maxCheckReason {
		return reason
	}
	runes := []rune(reason)
	return string(runes[:maxCheckReason-1]) + "…"
}

// recordJudgeFailure records err on span as a failure of the report checker.
func recordJudgeFailure(span trace.Span, err error, status string) {
	span.RecordError(err)
	span.SetStatus(codes.Error, status)
}
