// This file owns builds: the user's decision to build a confirmed change at
// one revision, saved in its own file beside the change's card, and the
// builder's and the test writer's assignments for that build.
package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	goa "goa.design/goa/v3/pkg"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
)

type (
	// BuildTeam makes the builder and the test writer ready for a build the
	// user started. The composition root binds it with Options.BuildTeam;
	// start_build calls it once per start, before the build is saved. With
	// none bound, a build is saved and the test writer's assignment names no
	// working folder.
	BuildTeam interface {
		// Prepare readies the roles for build and returns the absolute path of
		// the test writer's working copy. The user may build the same change
		// again, at the same or a later revision, so Prepare must be safe to
		// repeat. An error refuses the build: start_build saves nothing and
		// returns the error to the user.
		Prepare(ctx context.Context, build Build) (workingFolder string, err error)
	}

	// Build is a build the user started, as BuildTeam.Prepare sees it.
	Build struct {
		// ChangeID names the confirmed Change Card being built.
		ChangeID string
		// RequestRevision is the Change Card's revision in force that the user
		// built.
		RequestRevision string
		// Stories is the Story as agreed at RequestRevision, in declaration
		// order; nil when the change has none.
		Stories []designeval.StorySummary
	}

	// buildFile is the saved form of one change's build, in its own file beside
	// the change's card in the cards directory. Starting a build again
	// replaces the whole file. A file that does not match this schema, or
	// whose assignment states contradict their fields, stops the call that
	// reads it.
	buildFile struct {
		// ChangeID is the Change Card the build is for; its hash names the file.
		ChangeID string `json:"change_id"`
		// RequestRevision is the confirmed revision the user built.
		RequestRevision string `json:"request_revision"`
		// StartedAt is when the user started this build.
		StartedAt time.Time `json:"started_at"`
		// Stories is the Story as agreed at RequestRevision, copied from the
		// card when the build started, because the test writer's work is
		// judged against it and not against a later revision's Story.
		Stories []designeval.StorySummary `json:"stories"`
		// Builder is the builder's assignment.
		Builder builderAssignment `json:"builder"`
		// TestWriter is the test writer's assignment.
		TestWriter testWriterAssignment `json:"test_writer"`
	}

	// builderAssignment is the builder's saved assignment for one build.
	builderAssignment struct {
		// State is always builderAssigned: verification of the build's
		// revision, not the assignment, judges the builder's work.
		State string `json:"state"`
		// Read records that the builder read the assignment in full.
		Read bool `json:"read"`
	}

	// testWriterAssignment is the test writer's saved assignment for one
	// build. Findings are set exactly in testsNeedChanges and Commit exactly
	// in testsAccepted.
	testWriterAssignment struct {
		// State is one of testsAssigned, testsReported, testsNeedChanges or
		// testsAccepted.
		State string `json:"state"`
		// Read records that the test writer read the assignment in full.
		Read bool `json:"read"`
		// WorkingFolder is the test writer's working copy that
		// BuildTeam.Prepare returned; empty when no BuildTeam is bound.
		WorkingFolder string `json:"working_folder"`
		// Findings lists what the last check found, in testsNeedChanges.
		Findings []string `json:"findings"`
		// Commit is the commit the accepted tests were recorded at, in
		// testsAccepted.
		Commit string `json:"commit"`
	}
)

const (
	// builderAssigned is the builder's one state.
	builderAssigned = "assigned"
	// testsAssigned is the test writer's state until it reports.
	testsAssigned = "assigned"
	// testsReported is the test writer's state after it reported its tests
	// finished and before they are judged.
	testsReported = "reported"
	// testsNeedChanges is the test writer's state after a check found problems.
	testsNeedChanges = "needs_changes"
	// testsAccepted is the test writer's state after a check accepted the tests.
	testsAccepted = "accepted"
	// buildFileSuffix ends every build file name. Card listing reads only
	// <hex>.json names, so it never reads a build file.
	buildFileSuffix = ".build.json"
	// buildStagingFile is the name, inside the cards directory, of the file
	// each build save writes before renaming it over the build's file. Saves
	// hold ControlPlane.mu, so one staging file suffices.
	buildStagingFile = "pending-build"
)

// StartBuild saves the user's build of a confirmed change at its revision in
// force, with the Story as agreed at that revision and unread assignments for
// the builder and the test writer, replacing any earlier build of the change.
// When a BuildTeam is bound it prepares the roles first, without holding the
// card lock, and the change is checked again before the build is saved. It
// refuses an unknown change (not_found), another owner's change (forbidden),
// a change that is not confirmed or a revision that is not the one in force
// (conflict), and a Story whose finished flows the generator cannot name
// (unprocessable); a refused start saves nothing.
func (c *ControlPlane) StartBuild(ctx context.Context, p *gensurla.StartBuildPayload) (*gensurla.StartedBuild, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	ctx, span := c.tracer("surla/controlplane").Start(ctx, "surla.controlplane.start_build", trace.WithAttributes(
		attribute.String("surla.change_id", p.ChangeID),
		attribute.String("surla.request_revision", p.RequestRevision),
	))
	defer span.End()

	// Phase 1: check the change under the read lock before preparing anything.
	c.mu.RLock()
	record, err := c.buildableCard(principal, p.ChangeID, p.RequestRevision)
	c.mu.RUnlock()
	if err != nil {
		return nil, recordBuildError(span, err, "check change for build")
	}
	if _, _, err := designeval.StoryScenarios(record.stories); err != nil {
		return nil, gensurla.MakeUnprocessable(fmt.Errorf("the agreed Story cannot name its required tests: %w", err))
	}
	build := Build{ChangeID: p.ChangeID, RequestRevision: p.RequestRevision, Stories: cloneStorySummaries(record.stories)}

	// Phase 2: prepare the roles without the lock; preparing may be slow.
	var folder string
	if c.buildTeam != nil {
		folder, err = c.buildTeam.Prepare(ctx, build)
		if err == nil && !filepath.IsAbs(folder) {
			err = fmt.Errorf("working folder %q is not an absolute path", folder)
		}
		if err != nil {
			return nil, recordBuildError(span, fmt.Errorf("prepare build: %w", err), "prepare build")
		}
	}

	// Phase 3: check the change again and save the build. The revision covers
	// the Story, so the Story saved is the one Prepare saw.
	c.mu.Lock()
	defer c.mu.Unlock()
	record, err = c.buildableCard(principal, p.ChangeID, p.RequestRevision)
	if err != nil {
		return nil, recordBuildError(span, err, "check change for build")
	}
	saved := buildFile{
		ChangeID:        p.ChangeID,
		RequestRevision: p.RequestRevision,
		StartedAt:       time.Now().UTC(),
		Stories:         cloneStorySummaries(record.stories),
		Builder:         builderAssignment{State: builderAssigned},
		TestWriter:      testWriterAssignment{State: testsAssigned, WorkingFolder: folder},
	}
	if err := c.cards.saveBuild(saved); err != nil {
		return nil, recordBuildError(span, err, "save build")
	}
	return startedBuild(saved, true), nil
}

// GetBuild returns the owner's change's build, or not_started when the user
// never built it, and whether the build is for the change's revision in force.
// It fails for an unknown change (not_found) or another owner's change
// (forbidden) and changes nothing.
func (c *ControlPlane) GetBuild(ctx context.Context, p *gensurla.GetBuildPayload) (*gensurla.ChangeBuild, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	record, err := c.ownedCard(principal, p.ChangeID)
	if err != nil {
		return nil, err
	}
	saved, found, err := c.cards.loadBuild(p.ChangeID)
	if err != nil {
		return nil, err
	}
	if !found {
		return &gensurla.ChangeBuild{ChangeID: p.ChangeID, Build: gensurla.NewBuildNotStarted(&gensurla.BuildNotStarted{})}, nil
	}
	inForce := saved.RequestRevision == revisionInForce(record.card)
	return &gensurla.ChangeBuild{ChangeID: p.ChangeID, Build: gensurla.NewBuildStarted(startedBuild(saved, inForce))}, nil
}

// ListBuildAssignments returns the builder's assignment of every build of the
// caller's changes, most recently started first. It records nothing.
func (c *ControlPlane) ListBuildAssignments(ctx context.Context) (*gensurla.BuildAssignmentList, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	builds, err := c.ownedBuilds(principal)
	if err != nil {
		return nil, err
	}
	items := make([]*gensurla.BuildAssignment, len(builds))
	for i, saved := range builds {
		items[i] = buildAssignment(saved)
	}
	return &gensurla.BuildAssignmentList{Assignments: items}, nil
}

// GetBuildAssignment returns the builder's assignment for the caller's change
// and records that the builder read it; a later build of the change replaces
// it unread. It fails with not_found when the change or its build does not
// exist and forbidden for another owner's change.
func (c *ControlPlane) GetBuildAssignment(
	ctx context.Context,
	p *gensurla.GetBuildAssignmentPayload,
) (*gensurla.BuildAssignment, error) {
	saved, err := c.readAssignment(ctx, "surla.controlplane.get_build_assignment", p.ChangeID, func(saved *buildFile) bool {
		changed := !saved.Builder.Read
		saved.Builder.Read = true
		return changed
	})
	if err != nil {
		return nil, err
	}
	return buildAssignment(saved), nil
}

// ListTestAssignments returns the test writer's assignment of every build of
// the caller's changes, most recently started first. It records nothing.
func (c *ControlPlane) ListTestAssignments(ctx context.Context) (*gensurla.TestAssignmentList, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	builds, err := c.ownedBuilds(principal)
	if err != nil {
		return nil, err
	}
	items := make([]*gensurla.TestAssignmentItem, len(builds))
	for i, saved := range builds {
		items[i] = testAssignmentItem(saved)
	}
	return &gensurla.TestAssignmentList{Assignments: items}, nil
}

// GetTestAssignment returns the test writer's assignment for the caller's
// change in full, with the Story as agreed at the built revision and one
// required test per finished flow, and records that the test writer read it;
// a later build of the change replaces it unread. It fails with not_found when
// the change or its build does not exist and forbidden for another owner's
// change.
func (c *ControlPlane) GetTestAssignment(
	ctx context.Context,
	p *gensurla.GetTestAssignmentPayload,
) (*gensurla.TestAssignment, error) {
	saved, err := c.readAssignment(ctx, "surla.controlplane.get_test_assignment", p.ChangeID, func(saved *buildFile) bool {
		changed := !saved.TestWriter.Read
		saved.TestWriter.Read = true
		return changed
	})
	if err != nil {
		return nil, err
	}
	// start_build refused a Story whose flows cannot be named, so an error
	// here means the saved Story changed outside this package.
	scenarios, _, err := designeval.StoryScenarios(saved.Stories)
	if err != nil {
		return nil, fmt.Errorf("name the required tests of change %q: %w", saved.ChangeID, err)
	}
	required := make([]*gensurla.RequiredTest, len(scenarios))
	for i, s := range scenarios {
		required[i] = &gensurla.RequiredTest{Story: s.Story, FlowKey: s.Key, Scenario: s.Scenario, Hook: s.Hook}
	}
	item := testAssignmentItem(saved)
	return &gensurla.TestAssignment{
		ChangeID:        item.ChangeID,
		RequestRevision: item.RequestRevision,
		Progress:        item.Progress,
		WorkingFolder:   saved.TestWriter.WorkingFolder,
		Stories:         storySummaryItems(saved.Stories),
		RequiredTests:   required,
	}, nil
}

// ReportTests records that the test writer finished its tests for the
// caller's change at the build's revision: the test writer's state becomes
// reported and any earlier findings or accepted commit are cleared. It fails
// with not_found when the change or its build does not exist, forbidden for
// another owner's change, and conflict for any other revision; a refused
// report changes nothing.
func (c *ControlPlane) ReportTests(ctx context.Context, p *gensurla.ReportTestsPayload) (*gensurla.TestAssignmentItem, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	_, span := c.tracer("surla/controlplane").Start(ctx, "surla.controlplane.report_tests", trace.WithAttributes(
		attribute.String("surla.change_id", p.ChangeID),
		attribute.String("surla.request_revision", p.RequestRevision),
	))
	defer span.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	saved, err := c.ownedBuild(principal, p.ChangeID)
	if err != nil {
		return nil, recordBuildError(span, err, "load build")
	}
	if saved.RequestRevision != p.RequestRevision {
		return nil, gensurla.MakeConflict(fmt.Errorf(
			"the test assignment for %q is for request_revision %q, not %q",
			p.ChangeID, saved.RequestRevision, p.RequestRevision,
		))
	}
	if saved.TestWriter.State != testsReported {
		saved.TestWriter.State = testsReported
		saved.TestWriter.Findings = nil
		saved.TestWriter.Commit = ""
		if err := c.cards.saveBuild(saved); err != nil {
			return nil, recordBuildError(span, err, "save build")
		}
	}
	span.SetAttributes(attribute.String("surla.tests_state", saved.TestWriter.State))
	return testAssignmentItem(saved), nil
}

// revisionInForce returns the revision of card the user agreed and that every
// operation naming an exact revision works on, or "" when the card awaits the
// user's confirmation and no revision is in force.
func revisionInForce(card *gensurla.ChangeCardResult) string {
	if card.Status != StatusConfirmed {
		return ""
	}
	return card.RequestRevision
}

// startedBuild converts a saved build into its transport form; inForce says
// whether its revision is the change's revision in force.
func startedBuild(saved buildFile, inForce bool) *gensurla.StartedBuild {
	return &gensurla.StartedBuild{
		RequestRevision: saved.RequestRevision,
		StartedAt:       saved.StartedAt.UTC().Format(time.RFC3339Nano),
		InForce:         inForce,
		Builder:         builderProgress(saved.Builder),
		TestWriter:      testWriterProgress(saved.TestWriter),
	}
}

// buildAssignment converts a saved build into the builder's assignment.
func buildAssignment(saved buildFile) *gensurla.BuildAssignment {
	return &gensurla.BuildAssignment{
		ChangeID:        saved.ChangeID,
		RequestRevision: saved.RequestRevision,
		Progress:        builderProgress(saved.Builder),
	}
}

// testAssignmentItem converts a saved build into the test writer's
// assignment summary.
func testAssignmentItem(saved buildFile) *gensurla.TestAssignmentItem {
	return &gensurla.TestAssignmentItem{
		ChangeID:        saved.ChangeID,
		RequestRevision: saved.RequestRevision,
		Progress:        testWriterProgress(saved.TestWriter),
	}
}

// builderProgress converts the builder's saved assignment.
func builderProgress(saved builderAssignment) *gensurla.BuilderProgress {
	return &gensurla.BuilderProgress{State: saved.State, Read: saved.Read}
}

// testWriterProgress converts the test writer's saved assignment. decodeBuild
// accepted only the four states below, each with exactly its fields.
func testWriterProgress(saved testWriterAssignment) *gensurla.TestWriterProgress {
	var tests gensurla.Tests
	switch saved.State {
	case testsAssigned:
		tests = gensurla.NewTestsAssigned(&gensurla.TestsAssigned{})
	case testsReported:
		tests = gensurla.NewTestsReported(&gensurla.TestsReported{})
	case testsNeedChanges:
		tests = gensurla.NewTestsNeedsChanges(&gensurla.TestsNeedChanges{Findings: slices.Clone(saved.Findings)})
	case testsAccepted:
		tests = gensurla.NewTestsAccepted(&gensurla.TestsAccepted{Commit: saved.Commit})
	}
	return &gensurla.TestWriterProgress{Tests: tests, Read: saved.Read}
}

// recordBuildError records err on span as a failure unless it is a refusal
// the design declares, and returns err.
func recordBuildError(span trace.Span, err error, status string) error {
	var refused *goa.ServiceError
	if !errors.As(err, &refused) || refused.Fault {
		span.RecordError(err)
		span.SetStatus(codes.Error, status)
	}
	return err
}

// buildFileName names changeID's build file inside the cards directory.
func buildFileName(changeID string) string {
	sum := sha256.Sum256([]byte(changeID))
	return hex.EncodeToString(sum[:]) + buildFileSuffix
}

// decodeBuild strictly decodes the build file named fileName: unknown fields
// and trailing data are refused, the file must hold the build of the change
// whose hash names it, and each assignment's state must match its fields.
func decodeBuild(fileName string, data []byte) (buildFile, error) {
	var saved buildFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&saved); err != nil {
		return buildFile{}, fmt.Errorf("decode build file %s: %w", fileName, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return buildFile{}, fmt.Errorf("decode build file %s: trailing data", fileName)
	}
	if saved.ChangeID == "" || buildFileName(saved.ChangeID) != fileName {
		return buildFile{}, fmt.Errorf("build file %s does not hold that change's build", fileName)
	}
	if saved.RequestRevision == "" || saved.StartedAt.IsZero() {
		return buildFile{}, fmt.Errorf("build of change %q has no revision or start time", saved.ChangeID)
	}
	if saved.Builder.State != builderAssigned {
		return buildFile{}, fmt.Errorf("build of change %q has unknown builder state %q", saved.ChangeID, saved.Builder.State)
	}
	tw := saved.TestWriter
	var valid bool
	switch tw.State {
	case testsAssigned, testsReported:
		valid = len(tw.Findings) == 0 && tw.Commit == ""
	case testsNeedChanges:
		valid = len(tw.Findings) > 0 && tw.Commit == ""
	case testsAccepted:
		valid = len(tw.Findings) == 0 && commitIDPattern.MatchString(tw.Commit)
	default:
		return buildFile{}, fmt.Errorf("build of change %q has unknown test writer state %q", saved.ChangeID, tw.State)
	}
	if !valid {
		return buildFile{}, fmt.Errorf("build of change %q has findings or a commit its test writer state %q does not allow", saved.ChangeID, tw.State)
	}
	return saved, nil
}

// buildableCard loads the caller's card for changeID and checks that it is
// confirmed at revision. Callers hold c.mu.
func (c *ControlPlane) buildableCard(principal Principal, changeID, revision string) (storedChangeCard, error) {
	record, err := c.ownedCard(principal, changeID)
	if err != nil {
		return storedChangeCard{}, err
	}
	inForce := revisionInForce(record.card)
	if inForce == "" {
		return storedChangeCard{}, gensurla.MakeConflict(fmt.Errorf("change_id %q must be confirmed before it is built (status %q)", changeID, record.card.Status))
	}
	if inForce != revision {
		return storedChangeCard{}, gensurla.MakeConflict(fmt.Errorf("request_revision %q is not the revision in force of %q, %q", revision, changeID, inForce))
	}
	return record, nil
}

// ownedCard loads the card for changeID and checks that principal owns it.
// Callers hold c.mu.
func (c *ControlPlane) ownedCard(principal Principal, changeID string) (storedChangeCard, error) {
	record, ok, err := c.cards.load(changeID)
	if err != nil {
		return storedChangeCard{}, err
	}
	if !ok {
		return storedChangeCard{}, gensurla.MakeNotFound(fmt.Errorf("unknown change_id %q", changeID))
	}
	if record.owner != principal.ID {
		return storedChangeCard{}, gensurla.MakeForbidden(fmt.Errorf("principal %q is not the owner of change_id %q", principal.ID, changeID))
	}
	return record, nil
}

// ownedBuild loads the build of the caller's change changeID, failing with
// not_found when the change or its build does not exist. Callers hold c.mu.
func (c *ControlPlane) ownedBuild(principal Principal, changeID string) (buildFile, error) {
	if _, err := c.ownedCard(principal, changeID); err != nil {
		return buildFile{}, err
	}
	saved, found, err := c.cards.loadBuild(changeID)
	if err != nil {
		return buildFile{}, err
	}
	if !found {
		return buildFile{}, gensurla.MakeNotFound(fmt.Errorf("change_id %q has no build; the user starts one on the views page", changeID))
	}
	return saved, nil
}

// ownedBuilds returns the build of every change principal owns, most recently
// started first (tie-broken by change_id). One unreadable card or build fails
// the call rather than being left out.
func (c *ControlPlane) ownedBuilds(principal Principal) ([]buildFile, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	records, err := c.cards.all()
	if err != nil {
		return nil, err
	}
	var builds []buildFile
	for _, record := range records {
		if record.owner != principal.ID {
			continue
		}
		saved, found, err := c.cards.loadBuild(record.card.ChangeID)
		if err != nil {
			return nil, err
		}
		if found {
			builds = append(builds, saved)
		}
	}
	slices.SortFunc(builds, func(a, b buildFile) int {
		if byTime := b.StartedAt.Compare(a.StartedAt); byTime != 0 {
			return byTime
		}
		return strings.Compare(a.ChangeID, b.ChangeID)
	})
	return builds, nil
}

// readAssignment loads the build of the caller's change under the write lock,
// applies markRead, and saves the build when markRead reports a change, all
// within spanName.
func (c *ControlPlane) readAssignment(
	ctx context.Context,
	spanName, changeID string,
	markRead func(*buildFile) bool,
) (buildFile, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return buildFile{}, errors.New("authenticated principal is required")
	}
	_, span := c.tracer("surla/controlplane").Start(ctx, spanName, trace.WithAttributes(attribute.String("surla.change_id", changeID)))
	defer span.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	saved, err := c.ownedBuild(principal, changeID)
	if err != nil {
		return buildFile{}, recordBuildError(span, err, "load build")
	}
	span.SetAttributes(attribute.String("surla.request_revision", saved.RequestRevision))
	if markRead(&saved) {
		if err := c.cards.saveBuild(saved); err != nil {
			return buildFile{}, recordBuildError(span, err, "save build")
		}
		span.AddEvent("assignment read for the first time")
	}
	return saved, nil
}

// loadBuild returns the saved build of changeID, or false when the user never
// built it. A build file that cannot be read or decoded is an error.
func (s *cardStore) loadBuild(changeID string) (buildFile, bool, error) {
	if s.lock == nil {
		return buildFile{}, false, errCardsClosed
	}
	name := buildFileName(changeID)
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return buildFile{}, false, nil
	}
	if err != nil {
		return buildFile{}, false, fmt.Errorf("read build of change %q: %w", changeID, err)
	}
	saved, err := decodeBuild(name, data)
	if err != nil {
		return buildFile{}, false, err
	}
	return saved, true, nil
}

// saveBuild replaces the build file of saved.ChangeID. It writes and syncs the
// staging file, renames it over the build's file and syncs the directory, as
// the card store saves cards; callers hold c.mu for writing. An error before
// the rename leaves the previous build in place.
func (s *cardStore) saveBuild(saved buildFile) error {
	if s.lock == nil {
		return errCardsClosed
	}
	data, err := json.Marshal(saved)
	if err != nil {
		return fmt.Errorf("encode build of change %q: %w", saved.ChangeID, err)
	}
	staging := filepath.Join(s.dir, buildStagingFile)
	// O_NOFOLLOW: a link planted at the staging name must not redirect the
	// write to another file.
	f, err := os.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err == nil {
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
	}
	if err == nil {
		err = os.Rename(staging, filepath.Join(s.dir, buildFileName(saved.ChangeID)))
	}
	if err != nil {
		return fmt.Errorf("save build of change %q: %w", saved.ChangeID, err)
	}
	if err := syncDir(s.dir); err != nil {
		return fmt.Errorf("build of change %q is saved but may not survive a crash: %w", saved.ChangeID, err)
	}
	return nil
}
