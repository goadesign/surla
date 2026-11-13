// This file decides what a verification run builds and runs: one function
// chooses the plan for a change at a revision, and one reader turns a
// commit's test layout into a plan that live copies use too.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"path"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/verify"
)

type (
	// WrittenTestRuns says where and how the project server runs verification
	// of the tests accepted for each change, in a project without a stored
	// evaluator. Runs are kept in Options.RunsDir. The composition root binds
	// it; the plan of each run comes from the change's build and the
	// repository, never from this value.
	WrittenTestRuns struct {
		// ReportDir receives each run's full report, named by its digest. It
		// must lie outside the workspace.
		ReportDir string
		// Command is the argument list that runs this surla executable;
		// runs start `<Command> verify-run supervise <dir>`.
		Command []string
		// Timeout is how long a run may take before it is stopped and
		// recorded as interrupted.
		Timeout time.Duration
	}

	// verifier is how a control plane verifies changes, fixed by New:
	// noVerification, storedVerification or writtenTestVerification.
	verifier interface {
		// isVerifier marks the three cases.
		isVerifier()
	}

	// noVerification is a control plane that runs no verification.
	noVerification struct{}

	// storedVerification verifies every change with the operator's stored
	// evaluator: cfg is complete, its Plan naming the operator's suite.
	storedVerification struct {
		// cfg is Options.Verification with its folders resolved.
		cfg *VerificationConfig
	}

	// writtenTestVerification verifies each change with the tests accepted
	// for its build. runs holds the run settings: RunsDir, Command, Timeout
	// and Plan.ReportDir; the rest of each plan comes from verificationPlan.
	writtenTestVerification struct {
		// runs is Options.WrittenTestRuns and Options.RunsDir with their
		// folders resolved.
		runs *VerificationConfig
	}

	// layoutActor names who changes a commit that lacks the test layout:
	// the builder for the candidate, the test writer for accepted tests.
	layoutActor string

	// refusalReason is why starterPlanAt refuses: refusedReplacedFolder,
	// refusedNoRepository, refusedUnknownCommit or refusedNoLayout.
	refusalReason int

	// layoutRefusal is starterPlanAt's refusal: the project folder was
	// replaced or is not in a git repository, the repository does not hold
	// the candidate commit, or a commit lacks the test layout. Each caller
	// maps it to its own declared error.
	layoutRefusal struct {
		// reason is why starterPlanAt refused.
		reason refusalReason
		// folder is the project folder, for refusedReplacedFolder and
		// refusedNoRepository.
		folder string
		// commit is the commit refused, for refusedUnknownCommit and
		// refusedNoLayout.
		commit string
		// actor is who changes that commit, for refusedNoLayout.
		actor layoutActor
		// missing says what the commit lacks, for refusedNoLayout.
		missing string
	}
)

const (
	// builderActs refuses a candidate commit without the layout.
	builderActs layoutActor = "builder"
	// testWriterActs refuses an accepted tests' commit without the layout.
	testWriterActs layoutActor = "test writer"
)

const (
	// refusedReplacedFolder is a project folder that is no longer the
	// directory the control plane bound: replaced, removed or made a link.
	// Reasons start at 1, so a zero refusal is none of them.
	refusedReplacedFolder refusalReason = iota + 1
	// refusedNoRepository is a project folder outside any git repository.
	refusedNoRepository
	// refusedUnknownCommit is a candidate commit the repository does not
	// hold.
	refusedUnknownCommit
	// refusedNoLayout is a commit without the test layout `surla init`
	// makes.
	refusedNoLayout
)

// Error says what is refused, why and who acts.
func (r *layoutRefusal) Error() string {
	switch r.reason {
	case refusedReplacedFolder:
		return fmt.Sprintf("the project folder %s was replaced or removed after Surla started:"+
			" the user stops and starts Surla in the project folder, then the call is made again", r.folder)
	case refusedNoRepository:
		return fmt.Sprintf("the project folder %s is not inside a git repository, so it has no commits to verify:"+
			" the builder runs git init, commits the project and verifies again", r.folder)
	case refusedUnknownCommit:
		return fmt.Sprintf("the candidate commit %s is not in the project's repository; commit it and try again", r.commit)
	}
	step := "the builder commits the layout surla init makes and verifies again"
	if r.actor == testWriterActs {
		step = "the test writer reports again, so Surla checks and records its tests again"
	}
	return fmt.Sprintf("commit %s does not have the test layout surla init makes (%s): %s", r.commit, r.missing, step)
}

// verificationPlan returns the configuration of a verification run of
// candidateCommit for the change changeID at revision, which its caller has
// already authorized: it owns the change, the change is confirmed and
// revision is its revision in force. It is the only function that chooses
// which tests judge a change.
//
//   - With the stored evaluator, the operator's plan, as stored; it needs no
//     build.
//   - Otherwise the change's build must be at revision and its tests
//     accepted. The candidate's programs come from candidateCommit's layout,
//     the tests from the accepted commit's, and the test command is that
//     commit's generated entry point (scaffold.Layout.GeneratedSuiteCommand).
//
// The returned Plan carries the candidate commit, the revision and its test
// source; the caller adds the run's work folder. It refuses with not
// implemented when this control plane runs no verification, no_build,
// tests_not_accepted and no_test_layout as the error constructors below
// describe, and conflict when the repository does not hold candidateCommit.
func (c *ControlPlane) verificationPlan(ctx context.Context, changeID, revision, candidateCommit string) (VerificationConfig, error) {
	switch v := c.verifier.(type) {
	case storedVerification:
		cfg := *v.cfg
		cfg.Plan.Tests = verify.TestsEvaluator
		cfg.Plan.CandidateCommit, cfg.Plan.RequestRevision = candidateCommit, revision
		return cfg, nil
	case writtenTestVerification:
		c.mu.RLock()
		build, found, err := c.cards.loadBuild(changeID)
		c.mu.RUnlock()
		if err != nil {
			return VerificationConfig{}, err
		}
		if !found || build.RequestRevision != revision {
			return VerificationConfig{}, refuseNoBuild(changeID, revision, build, found)
		}
		if build.TestWriter.State != testsAccepted {
			return VerificationConfig{}, refuseTestsNotAccepted(build.TestWriter)
		}
		plan, err := c.starterPlanAt(ctx, candidateCommit, build.TestWriter.Commit, testWriterActs)
		var refusal *layoutRefusal
		switch {
		case errors.As(err, &refusal) && refusal.reason == refusedNoLayout:
			return VerificationConfig{}, refuseNoTestLayout(refusal)
		case errors.As(err, &refusal):
			return VerificationConfig{}, gensurla.MakeConflict(refusal)
		case err != nil:
			return VerificationConfig{}, err
		}
		cfg := *v.runs
		plan.ReportDir, plan.RequestRevision = cfg.Plan.ReportDir, revision
		cfg.Plan = plan
		return cfg, nil
	default:
		return VerificationConfig{}, gensurla.MakeNotImplemented(errVerificationUnavailable)
	}
}

// starterPlanAt reads, with git and never from the working tree, the test
// layout `surla init` makes at two commits of the repository holding the
// bound workspace, and returns the plan that builds candidateCommit's
// programs and runs testsCommit's generated entry point, written by agents
// (verify.TestsWritten). Verification passes the accepted tests' commit; a
// live copy passes the candidate commit twice. A project folder that is no
// longer the directory this control plane bound or is outside any git
// repository, a candidateCommit the repository does not hold, or a commit
// lacking the layout is refused with a *layoutRefusal saying what and who
// acts: the builder for the folder and candidateCommit, testsActor for
// testsCommit. New guarantees a bound workspace whenever this runs.
func (c *ControlPlane) starterPlanAt(ctx context.Context, candidateCommit, testsCommit string, testsActor layoutActor) (verify.Plan, error) {
	wsDir, wsDev, wsIno := c.currentWorkspaceTarget()
	_, root, err := openModuleRootWithIdentity(wsDir, wsDev, wsIno)
	if err != nil {
		return verify.Plan{}, &layoutRefusal{reason: refusedReplacedFolder, folder: wsDir}
	}
	if err := root.Close(); err != nil {
		return verify.Plan{}, fmt.Errorf("close project folder %s: %w", wsDir, err)
	}
	ctx, cancel := context.WithTimeout(ctx, layoutReadTimeout)
	defer cancel()
	repoRoot, prefix, err := resolveStarterRepo(ctx, wsDir)
	if errors.Is(err, errNoRepository) {
		return verify.Plan{}, &layoutRefusal{reason: refusedNoRepository, folder: wsDir}
	}
	if err != nil {
		return verify.Plan{}, err
	}
	exists, err := commitExists(ctx, repoRoot, candidateCommit)
	if err != nil {
		return verify.Plan{}, err
	}
	if !exists {
		return verify.Plan{}, &layoutRefusal{reason: refusedUnknownCommit, commit: candidateCommit}
	}
	// Module folders and committed paths are slash-separated and relative to
	// the repository root: "." and "eval" for a project at the root,
	// "services/shop" and "services/shop/eval" for one nested in it.
	candidateModule := path.Clean("./" + prefix)
	testModule := path.Join(candidateModule, scaffold.SuiteDir)
	candidate, err := starterLayoutAt(ctx, repoRoot, candidateCommit, candidateModule, testModule, builderActs)
	if err != nil {
		return verify.Plan{}, err
	}
	if err := requireLayoutTree(ctx, repoRoot, candidateCommit, path.Join(candidateModule, candidate.CandidatePackage), builderActs); err != nil {
		return verify.Plan{}, err
	}
	tests, err := starterLayoutAt(ctx, repoRoot, testsCommit, candidateModule, testModule, testsActor)
	if err != nil {
		return verify.Plan{}, err
	}
	entry := tests.GeneratedSuiteCommand()
	if err := requireLayoutTree(ctx, repoRoot, testsCommit, path.Join(testModule, entry), testsActor); err != nil {
		return verify.Plan{}, err
	}
	return verify.Plan{
		CandidateRepo:   repoRoot,
		CandidateCommit: candidateCommit,
		CandidateModule: candidateModule,
		Binaries:        map[string]string{candidate.Service: candidate.CandidatePackage},
		SuiteRepo:       repoRoot,
		SuiteCommit:     testsCommit,
		SuiteModule:     testModule,
		SuiteCommand:    entry,
		Tests:           verify.TestsWritten,
	}, nil
}

// starterLayoutAt reads the service module's and the test module's go.mod at
// commit and returns the layout scaffold.StarterLayout derives from them,
// refusing with a *layoutRefusal, for actor, a commit without either file or
// whose files StarterLayout refuses.
func starterLayoutAt(ctx context.Context, repoRoot, commit, serviceModule, testModule string, actor layoutActor) (scaffold.Layout, error) {
	var goMods [2][]byte
	for i, file := range []string{path.Join(serviceModule, "go.mod"), path.Join(testModule, "go.mod")} {
		content, found, err := readGitBlob(ctx, repoRoot, commit, file)
		if err != nil {
			return scaffold.Layout{}, err
		}
		if !found {
			return scaffold.Layout{}, &layoutRefusal{reason: refusedNoLayout, commit: commit, actor: actor, missing: "it has no regular file " + file}
		}
		goMods[i] = content
	}
	layout, err := scaffold.StarterLayout(goMods[0], goMods[1])
	if err != nil {
		return scaffold.Layout{}, &layoutRefusal{reason: refusedNoLayout, commit: commit, actor: actor, missing: err.Error()}
	}
	return layout, nil
}

// requireLayoutTree refuses with a *layoutRefusal, for actor, a commit
// without the committed folder treePath.
func requireLayoutTree(ctx context.Context, repoRoot, commit, treePath string, actor layoutActor) error {
	kind, err := gitEntry(ctx, repoRoot, commit, treePath)
	if err != nil {
		return err
	}
	if kind != gitTree {
		return &layoutRefusal{reason: refusedNoLayout, commit: commit, actor: actor, missing: "it has no folder " + treePath}
	}
	return nil
}

// isVerifier marks noVerification as a verifier.
func (noVerification) isVerifier() {}

// isVerifier marks storedVerification as a verifier.
func (storedVerification) isVerifier() {}

// isVerifier marks writtenTestVerification as a verifier.
func (writtenTestVerification) isVerifier() {}

// newVerifier builds New's verifier from opts, resolving its folders: the
// stored evaluator when opts.Verification is set, the written-test runs (in
// runsDir) when opts.WrittenTestRuns is, and no verification otherwise. New
// has already refused both together.
func newVerifier(opts Options, runsDir string) (verifier, error) {
	switch {
	case opts.Verification != nil:
		cfg, err := normalizeVerificationConfig(opts.Verification)
		if err != nil {
			return nil, err
		}
		return storedVerification{cfg: cfg}, nil
	case opts.WrittenTestRuns != nil:
		runs, err := normalizeVerificationConfig(&VerificationConfig{
			RunsDir: runsDir,
			Plan:    verify.Plan{ReportDir: opts.WrittenTestRuns.ReportDir},
			Command: opts.WrittenTestRuns.Command,
			Timeout: opts.WrittenTestRuns.Timeout,
		})
		if err != nil {
			return nil, err
		}
		return writtenTestVerification{runs: runs}, nil
	default:
		return noVerification{}, nil
	}
}

// verifierRuns returns the run settings of v whose folders New checks lie
// outside the workspace, or nil for noVerification.
func verifierRuns(v verifier) *VerificationConfig {
	switch v := v.(type) {
	case storedVerification:
		return v.cfg
	case writtenTestVerification:
		return v.runs
	default:
		return nil
	}
}

// refuseNoBuild is verify_change's refusal for a change with no build at
// revision: it names the built revision when there is one, the revision in
// force, and the user's step.
func refuseNoBuild(changeID, revision string, build buildFile, found bool) error {
	if !found {
		return gensurla.MakeConflict(fmt.Errorf(
			"the user has not started a build of change %q, so no tests are accepted for revision %s: the user starts a build on the views page",
			changeID, revision))
	}
	return gensurla.MakeConflict(fmt.Errorf(
		"change %q is built at revision %s, but the revision in force is %s: the user starts a build of revision %s on the views page",
		changeID, build.RequestRevision, revision, revision))
}

// refuseTestsNotAccepted is verify_change's refusal for a build whose tests
// are not accepted: it names the test writer's state and who acts.
func refuseTestsNotAccepted(saved testWriterAssignment) error {
	var who string
	switch saved.State {
	case testsAssigned:
		who = "the test writer writes the required tests and reports them finished"
	case testsReported:
		who = "Surla is checking the test writer's report; verify again after its verdict"
	case testsNeedChanges:
		who = "the test writer fixes Surla's findings and reports again"
	case testsCouldNotRun:
		who = "Surla's check could not run (" + saved.Reason + "); the user's step: " + saved.Step
	}
	return gensurla.MakeConflict(fmt.Errorf("the tests for this build are not accepted (state %s): %s", saved.State, who))
}

// refuseNoTestLayout is verify_change's refusal for a commit without the test
// layout `surla init` makes: it names the commit, what is missing and who
// acts.
func refuseNoTestLayout(refusal *layoutRefusal) error {
	return gensurla.MakeConflict(refusal)
}
