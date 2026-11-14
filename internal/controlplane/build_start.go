// This file owns build admission through the copy, saved assignments and team.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	genproject "github.com/goadesign/surla/gen/project"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/shquote"
	"github.com/goadesign/surla/internal/testwriter"
	"github.com/goadesign/surla/verify/candidate"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	goa "goa.design/goa/v3/pkg"
)

// defaultBuildStartTimeout bounds opening and refreshing one copy and one
// sandboxed generation while the start holds the copy turn. The team owner's
// window phase has its own 45-second bound after that turn is released.
const defaultBuildStartTimeout = 5 * time.Minute

// StartBuild saves the agreed revision and its snapshots before registering
// roles or generating tests. The copy turn is nonblocking and covers both test
// sources; team window work starts only after it is released. Accepted work
// survives a client disconnect and drains before Close releases its owners.
func (c *ControlPlane) StartBuild(
	ctx context.Context, p *gensurla.StartBuildPayload,
) (*gensurla.StartBuildResult, error) {
	team, err := c.ownedTeam(ctx)
	if err != nil {
		return nil, err
	}
	principal, _ := PrincipalFromContext(ctx) // ownedTeam established the principal and owner.
	accepted, finish, err := c.admitBuildStart(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	accepted, span := c.tracer("surla/controlplane").Start(accepted, "surla.controlplane.start_build", trace.WithAttributes(
		attribute.String("surla.change_id", p.ChangeID),
		attribute.String("surla.request_revision", p.RequestRevision),
	))
	defer span.End()
	select {
	case c.workingCopyTurn <- struct{}{}:
	default:
		message := "Another build is starting or a test check is using the working copy. "
		if c.storedEvaluator() {
			message = "Another build is starting. "
		}
		return nil, gensurla.MakeWorkingCopyBusy(errors.New(message + "Start the build again when that has ended."))
	}
	heldTurn := true
	defer func() {
		if heldTurn {
			<-c.workingCopyTurn
		}
	}()
	work, cancel := c.buildStartDeadline(accepted)
	defer cancel()

	// Check the agreed revision before any working-copy or team effect.
	c.mu.RLock()
	record, err := c.buildableCard(principal, p.ChangeID, p.RequestRevision)
	c.mu.RUnlock()
	if err != nil {
		return nil, recordBuildError(span, err, "check change for build")
	}
	if _, _, err := designeval.StoryScenarios(record.stories); err != nil {
		return nil, gensurla.MakeUnprocessable(fmt.Errorf("the agreed Story cannot name its required tests: %w", err))
	}
	source, layout, workspace, err := c.projectTestLayout(work)
	if err != nil {
		return nil, recordBuildError(span, buildStartError(work, "", err), "read test source")
	}
	if workspace == "" {
		return nil, gensurla.MakeNotImplemented(errors.New("No project working folder is bound. Start this project with surla to build it."))
	}
	var copy buildCopy
	refresh := gensurla.NewWorkingCopyNotUsed(&gensurla.BuildCopyNotUsed{})
	if source != testSourceStoredEvaluator {
		if source != testSourceStarterLayout {
			return nil, gensurla.MakeNoTestLayout(errors.New("This project has neither stored verification settings nor Surla's test layout. Store verification settings, then build again."))
		}
		identity := c.buildIdentityCheck
		if identity == nil {
			identity = testwriter.CheckIdentity
		}
		if err := identity(work, workspace); err != nil {
			return nil, recordBuildError(span, buildStartError(work, workspace, err), "check Git identity")
		}
		sandbox := c.buildSandboxCheck
		if sandbox == nil {
			sandbox = candidate.CheckSandbox
		}
		if err := sandbox(); err != nil {
			return nil, recordBuildError(span, buildStartError(work, workspace, err), "check sandbox")
		}
		copy, err = c.openBuildCopy(work, workspace, layout)
		if err != nil {
			return nil, recordBuildError(span, buildStartError(work, workspace, err), "open test-writer copy")
		}
		state, err := copy.Refresh(work)
		if err != nil {
			return nil, recordBuildError(span, buildStartError(work, workspace, err), "refresh test-writer copy")
		}
		refresh = buildCopyOutcome(state)
	}

	// Save only after the second agreed-revision check. Clone this snapshot,
	// never the proposal or the earlier read used by the preflight.
	c.mu.Lock()
	record, err = c.buildableCard(principal, p.ChangeID, p.RequestRevision)
	if err == nil {
		err = work.Err()
	}
	if err != nil {
		c.mu.Unlock()
		return nil, recordBuildError(span, buildStartError(work, workspace, err), "check change before save")
	}
	saved := buildFile{
		ChangeID: p.ChangeID, RequestRevision: p.RequestRevision, StartedAt: time.Now().UTC(),
		Roles: buildRolesBuilderOnly, Stories: cloneStorySummaries(record.stories),
		DesignSource: record.designSource, TestWriter: testWriterAssignment{State: testsAssigned},
		Answers: confirmedAnswers(record.card), Examples: record.card.Examples,
		PreservedBehaviors: append([]string{}, record.card.PreservedBehaviors...),
	}
	var folders BuildFolders = BuilderOnly{BuilderFolder: workspace}
	if copy != nil {
		folder := copy.Project()
		saved.Roles = buildRolesBuilderAndTestWriter
		saved.TestWriter.WorkingFolder = &folder
		folders = BuilderAndTestWriter{BuilderFolder: workspace, TestWriterFolder: folder}
	}
	err = c.cards.saveBuild(saved)
	c.mu.Unlock()
	if err != nil {
		return nil, recordBuildError(span, err, "save build")
	}
	result := &gensurla.StartBuildResult{
		Build:       startedBuild(heldBuild{file: saved, inForce: true, storedEvaluator: c.storedEvaluator()}),
		WorkingCopy: refresh,
	}
	// Registration is durable local work and is never starved by a generation
	// or window deadline. The project owner completes it even after cancellation.
	if err := team.RegisterBuildRoles(accepted, folders); err != nil {
		span.RecordError(errors.New("saved build roles could not be registered"))
		span.SetStatus(codes.Error, "register saved build roles")
		result.Team = buildAgainTeamOutcome()
		return result, nil
	}
	if copy != nil {
		err = work.Err()
		if err == nil {
			err = copy.Prepare(work, saved.Stories)
		}
		// An author's failure becomes a finding. Any other is a failure of
		// this start: the saved reason is worded for the page, and the trace
		// keeps the cause as it was.
		if author := (*testwriter.AuthorError)(nil); err != nil && !errors.As(err, &author) {
			span.RecordError(err)
			span.SetStatus(codes.Error, "prepare required tests")
		}
		current, saveErr := c.saveBuildPreparation(work, saved, err)
		if saveErr != nil {
			span.RecordError(errors.New("saved build preparation could not be recorded"))
			span.SetStatus(codes.Error, "record build preparation")
			result.Team = buildAgainTeamOutcome()
			return result, nil
		}
		result.Build = startedBuild(current)
	}
	<-c.workingCopyTurn
	heldTurn = false
	cancel()
	if accepted.Err() != nil {
		result.Team = buildAgainTeamOutcome()
		return result, nil
	}
	prepared, err := team.StartAgents(accepted)
	result.Team = buildTeamOutcome(prepared, err)
	return result, nil
}

// saveBuildPreparation preserves a report made while generation ran. A verdict
// changes only this start's still-assigned report generation, under the lock;
// the saved file, including any assignment read, is always the update's base.
func (c *ControlPlane) saveBuildPreparation(
	ctx context.Context, saved buildFile, prepareErr error,
) (heldBuild, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, found, err := c.cards.loadBuild(saved.ChangeID)
	if err != nil {
		return heldBuild{}, err
	}
	if !found {
		return heldBuild{}, errors.New("the saved build disappeared during preparation")
	}
	if prepareErr != nil && current.RequestRevision == saved.RequestRevision &&
		current.StartedAt.Equal(saved.StartedAt) && current.ReportNumber == saved.ReportNumber &&
		current.TestWriter.State == testsAssigned {
		tw := &current.TestWriter
		tw.Read = false
		tw.CheckedAt = time.Now().UTC()
		var author *testwriter.AuthorError
		if errors.As(prepareErr, &author) {
			tw.State = testsNeedChanges
			tw.Findings = []string{author.Finding.Message + "\nCorrect the problem in the test module, then report_tests_finished."}
		} else {
			tw.State = testsCouldNotRun
			reason, step := preparationFailure(ctx, prepareErr)
			tw.Reason, tw.Step = boundedBuildReason(reason), step
		}
		if err := c.cards.saveBuild(current); err != nil {
			return heldBuild{}, err
		}
	}
	record, err := c.ownedCard(Principal{ID: c.team.Owner}, saved.ChangeID)
	if err != nil {
		return heldBuild{}, err
	}
	return heldBuild{file: current, inForce: buildInForce(current, record), storedEvaluator: c.storedEvaluator()}, nil
}

// confirmedAnswers pairs each question of the confirmed card with the answer
// the user confirmed for it, in question order. confirm_intent confirmed the
// card only with one declared option for every question and no other answer.
func confirmedAnswers(card *gensurla.ChangeCardResult) []*gensurla.ConfirmedAnswer {
	answers := make([]*gensurla.ConfirmedAnswer, len(card.Ambiguities))
	for i, q := range card.Ambiguities {
		answers[i] = &gensurla.ConfirmedAnswer{ID: q.ID, Question: q.Question, Answer: card.Answers[q.ID]}
	}
	return answers
}

// preparationFailure returns the reason and the user's step saved when a
// build's required tests could not be prepared for a cause the test writer
// cannot fix. ctx is the start's limit and err what the preparation returned.
// The reason says which step failed: the limit or a server stop, a download
// of the test module's dependencies that fails for a cause outside the test
// writer's changes (testwriter.ErrDependencies, whose step has the user run
// the download where its output can be read), the generator (worded by
// designeval.GenerationDiagnostic, which names its phase), or writing the
// generated folder, whose cause is quoted without absolute paths and whose
// step names what to repair before building again.
// Any other failure has no text safe to show: its reason points at the server
// trace, where StartBuild records the cause.
func preparationFailure(ctx context.Context, err error) (reason, step string) {
	const buildAgain = "Build this change again on the views page."
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "The build-start time limit ended before the required tests could be prepared.", buildAgain
	case errors.Is(ctx.Err(), context.Canceled):
		return "The server stopped before the required tests could be prepared.", buildAgain
	case errors.Is(err, testwriter.ErrDependencies):
		return "Surla could not prepare the required tests: " + testwriter.WithoutAbsolutePaths(err.Error()),
			stepDependencies + ", then build this change again on the views page."
	case errors.Is(err, testwriter.ErrGeneration):
		return designeval.GenerationDiagnostic(err), buildAgain
	case errors.Is(err, testwriter.ErrWriteGenerated):
		return "Surla generated the required tests but could not write them into the test writer's working copy: " +
			testwriter.WithoutAbsolutePaths(err.Error()), stepWriteCopy + ", then build this change again on the views page."
	default:
		return "Surla could not prepare the required tests, for a cause outside the test module. " +
			"The server trace of this build start has the details.", buildAgain
	}
}

// buildStartError preserves named library steps and maps only pre-save refusals.
func buildStartError(ctx context.Context, workspace string, err error) error {
	if ctx.Err() != nil {
		return gensurla.MakeUnavailable(errors.New("The server stopped or the build-start limit ended before saving. Call again on a running server."))
	}
	switch {
	case errors.Is(err, testwriter.ErrNotRepository):
		return gensurla.MakeNotRepository(fmt.Errorf("Project folder %s: %w", shquote.Quote(workspace), err))
	case errors.Is(err, testwriter.ErrNoCommit):
		return gensurla.MakeNoCommit(fmt.Errorf("Project folder %s: %w", shquote.Quote(workspace), err))
	case errors.Is(err, testwriter.ErrIdentityMissing):
		return gensurla.MakeGitIdentityMissing(err)
	case errors.Is(err, testwriter.ErrFolderInUse):
		return gensurla.MakeCopyFolderInUse(fmt.Errorf("Working-copy folder %s is not Surla's working copy. Move it out of the project, then try again.", shquote.Quote(filepath.Join(workspace, testwriter.Folder))))
	case errors.Is(err, testwriter.ErrBranchInUse):
		return gensurla.MakeTestsBranchInUse(err)
	case errors.Is(err, candidate.ErrIsolationUnavailable):
		return gensurla.MakeSandboxUnavailable(errors.New("This system has no candidate sandbox. Start this project on a system with Surla's candidate sandbox, then build again."))
	default:
		return err
	}
}

// buildCopyOutcome renders the copy owner's postcondition without another Git read.
func buildCopyOutcome(result testwriter.RefreshResult) gensurla.WorkingCopy {
	switch result.State {
	case testwriter.RefreshAtProjectCommit:
		return gensurla.NewWorkingCopyAtProjectCommit(&gensurla.BuildCopyAtProjectCommit{Commit: result.Commit})
	case testwriter.RefreshNotContained:
		return gensurla.NewWorkingCopyNotContained(&gensurla.BuildCopyNotContained{
			Commit: result.Commit, Step: "Merge the tests branch into the project's branch, then build again.",
		})
	case testwriter.RefreshDirty:
		return gensurla.NewWorkingCopyDirty(&gensurla.BuildCopyDirty{Commit: result.Commit})
	}
	panic("unknown working-copy refresh result")
}

// buildAgainTeamOutcome is the recovery for a saved start interrupted before its team phase.
func buildAgainTeamOutcome() gensurla.Team {
	return gensurla.NewTeamUnavailable(&gensurla.BuildTeamUnavailable{Step: "Build this change again on the views page."})
}

// boundedBuildReason fits a safe owner diagnostic to the declared reason bound.
func boundedBuildReason(reason string) string {
	runes := []rune(reason)
	return string(runes[:min(len(runes), maxCheckReason)])
}

// admitBuildStart uses the workspace admission barrier so Close cannot miss
// accepted work. The caller's values and tracing survive, while cancellation
// belongs to the server and this start's limit. The returned finish must be
// called once after every effect, including post-save team admission, ends.
func (c *ControlPlane) admitBuildStart(ctx context.Context) (context.Context, func(), error) {
	c.workspaceMu.RLock()
	defer c.workspaceMu.RUnlock()
	if c.serverCtx.Err() != nil {
		return nil, nil, gensurla.MakeUnavailable(errors.New("the server is shutting down; call again on a running server"))
	}
	c.buildStartOps.Add(1)
	accepted, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(c.serverCtx, cancel)
	if c.serverCtx.Err() != nil {
		cancel()
	}
	return accepted, func() {
		cancel()
		stop()
		c.buildStartOps.Done()
	}, nil
}

// buildStartDeadline bounds only the work under the copy turn. Team start uses
// the server-owned admission context after this phase, with its own limit.
func (c *ControlPlane) buildStartDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	withTimeout := c.buildStartWithTimeout
	if withTimeout == nil {
		withTimeout = context.WithTimeout
	}
	return withTimeout(ctx, defaultBuildStartTimeout)
}

// buildTeamOutcome preserves a saved build when its team cannot become ready.
// Outside error messages never become user steps: the team returns typed
// conditions, and this boundary owns the action shown by start_build.
func buildTeamOutcome(team *gensurla.ProjectTeam, err error) gensurla.Team {
	if err == nil {
		return gensurla.NewTeamReady(team)
	}
	var spaces *genproject.TeamSpaceConflict
	if errors.As(err, &spaces) {
		return gensurla.NewTeamAmbiguousSpace(&gensurla.BuildTeamAmbiguousSpace{
			Count: spaces.Count,
			Step:  gensurla.BuildStartStep(duplicateTeamSpacesStep()),
		})
	}
	var unavailable *goa.ServiceError
	if errors.As(err, &unavailable) && unavailable.Name == "window_busy" {
		return gensurla.NewTeamWindowBusy(&gensurla.BuildTeamWindowBusy{
			Step: "Close the open Herdr dialog, then start the team agents.",
		})
	}
	return gensurla.NewTeamUnavailable(&gensurla.BuildTeamUnavailable{
		Step: "Check that Herdr is available, then start the team agents for this saved build.",
	})
}
