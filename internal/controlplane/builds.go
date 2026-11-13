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
	// working folder. BuildTeam does not check reported tests:
	// report_tests_finished only records a report as reported, and the check
	// that accepts the tests or returns findings is added to this interface
	// when reports are judged.
	BuildTeam interface {
		// Prepare readies the roles for build and returns the absolute path of
		// the test writer's working copy. An error refuses the build:
		// start_build saves nothing and returns the error to the user. Prepare
		// runs without the card lock, and start_build checks the change again
		// before saving; when the change was revised or confirmed again
		// meanwhile, the start is refused, no build is saved, and what Prepare
		// made stays. Together with the user building the same change again,
		// at the same or a later revision, this is why Prepare must be safe to
		// repeat.
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

	// heldBuild is a saved build together with whether it is for its change's
	// revision in force, as every read of a build or an assignment reports.
	heldBuild struct {
		// file is the saved build.
		file buildFile
		// inForce is buildInForce for file and its change's card.
		inForce bool
	}

	// buildFile is the saved form of one change's build, in its own file beside
	// the change's card in the cards directory. Starting a build again
	// replaces the whole file. A file that does not match this schema, or
	// whose test writer state contradicts its fields, stops the call that
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

	// builderAssignment is the builder's saved assignment for one build. It
	// has no state: it exists from the moment the user starts the build, and
	// verification of the build's revision, not the assignment, judges the
	// builder's work.
	builderAssignment struct {
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
		Builder:         builderAssignment{},
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
	inForce := buildInForce(saved, record.card)
	return &gensurla.ChangeBuild{ChangeID: p.ChangeID, Build: gensurla.NewBuildStarted(startedBuild(saved, inForce))}, nil
}

// ListBuilderAssignments returns the builder's assignment of every build of
// the caller's changes, most recently started first. It records nothing.
func (c *ControlPlane) ListBuilderAssignments(ctx context.Context) (*gensurla.BuilderAssignmentList, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	builds, err := c.ownedBuilds(principal)
	if err != nil {
		return nil, err
	}
	items := make([]*gensurla.BuilderAssignment, len(builds))
	for i, held := range builds {
		items[i] = builderAssignmentResult(held)
	}
	return &gensurla.BuilderAssignmentList{Assignments: items}, nil
}

// GetBuilderAssignment returns the builder's assignment for the caller's
// change and records that the builder read it; a later build of the change
// replaces it unread. It fails with not_found when the change or its build
// does not exist and forbidden for another owner's change.
func (c *ControlPlane) GetBuilderAssignment(
	ctx context.Context,
	p *gensurla.GetBuilderAssignmentPayload,
) (*gensurla.BuilderAssignment, error) {
	held, err := c.readAssignment(ctx, "surla.controlplane.get_builder_assignment", p.ChangeID, func(saved *buildFile) bool {
		changed := !saved.Builder.Read
		saved.Builder.Read = true
		return changed
	})
	if err != nil {
		return nil, err
	}
	return builderAssignmentResult(held), nil
}

// ListTestWriterAssignments returns the test writer's assignment of every
// build of the caller's changes, most recently started first. It records
// nothing.
func (c *ControlPlane) ListTestWriterAssignments(ctx context.Context) (*gensurla.TestWriterAssignmentList, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	builds, err := c.ownedBuilds(principal)
	if err != nil {
		return nil, err
	}
	items := make([]*gensurla.TestWriterAssignmentItem, len(builds))
	for i, held := range builds {
		items[i] = testWriterAssignmentItem(held)
	}
	return &gensurla.TestWriterAssignmentList{Assignments: items}, nil
}

// GetTestWriterAssignment returns the test writer's assignment for the
// caller's change in full, with the Story as agreed at the built revision and one
// required test per finished flow, and records that the test writer read it;
// a later build of the change replaces it unread. It fails with not_found when
// the change or its build does not exist and forbidden for another owner's
// change.
func (c *ControlPlane) GetTestWriterAssignment(
	ctx context.Context,
	p *gensurla.GetTestWriterAssignmentPayload,
) (*gensurla.TestWriterAssignment, error) {
	held, err := c.readAssignment(ctx, "surla.controlplane.get_test_writer_assignment", p.ChangeID, func(saved *buildFile) bool {
		changed := !saved.TestWriter.Read
		saved.TestWriter.Read = true
		return changed
	})
	if err != nil {
		return nil, err
	}
	// start_build refused a Story whose flows cannot be named, so an error
	// here means the saved Story changed outside this package.
	scenarios, _, err := designeval.StoryScenarios(held.file.Stories)
	if err != nil {
		return nil, fmt.Errorf("name the required tests of change %q: %w", held.file.ChangeID, err)
	}
	required := make([]*gensurla.RequiredTest, len(scenarios))
	for i, s := range scenarios {
		required[i] = &gensurla.RequiredTest{Story: s.Story, FlowKey: s.Key, Scenario: s.Scenario, Hook: s.Hook}
	}
	item := testWriterAssignmentItem(held)
	return &gensurla.TestWriterAssignment{
		ChangeID:        item.ChangeID,
		RequestRevision: item.RequestRevision,
		InForce:         item.InForce,
		Progress:        item.Progress,
		WorkingFolder:   held.file.TestWriter.WorkingFolder,
		Stories:         storySummaryItems(held.file.Stories),
		RequiredTests:   required,
	}, nil
}

// ReportTestsFinished records that the test writer finished its tests for the
// caller's change at the build's revision: the test writer's state becomes
// reported and any earlier findings or accepted commit are cleared. It fails
// with not_found when the change or its build does not exist, forbidden for
// another owner's change, and conflict for any other revision; a refused
// report changes nothing.
func (c *ControlPlane) ReportTestsFinished(
	ctx context.Context,
	p *gensurla.ReportTestsFinishedPayload,
) (*gensurla.TestWriterAssignmentItem, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	_, span := c.tracer("surla/controlplane").Start(ctx, "surla.controlplane.report_tests_finished", trace.WithAttributes(
		attribute.String("surla.change_id", p.ChangeID),
		attribute.String("surla.request_revision", p.RequestRevision),
	))
	defer span.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	held, err := c.ownedBuild(principal, p.ChangeID)
	if err != nil {
		return nil, recordBuildError(span, err, "load build")
	}
	saved := &held.file
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
		if err := c.cards.saveBuild(*saved); err != nil {
			return nil, recordBuildError(span, err, "save build")
		}
	}
	span.SetAttributes(attribute.String("surla.tests_state", saved.TestWriter.State))
	return testWriterAssignmentItem(held), nil
}

// revisionInForce returns the revision of card the user agreed, which
// start_build requires and against which every read of a build or an
// assignment reports in_force, or "" when the card awaits the user's
// confirmation and no revision is in force. preview_architecture,
// verify_change and approve_merge check the card's status and revision
// themselves.
func revisionInForce(card *gensurla.ChangeCardResult) string {
	if card.Status != StatusConfirmed {
		return ""
	}
	return card.RequestRevision
}

// buildInForce reports whether saved is a build of card's revision in force;
// get_build and every assignment read report it as in_force.
func buildInForce(saved buildFile, card *gensurla.ChangeCardResult) bool {
	return saved.RequestRevision == revisionInForce(card)
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

// builderAssignmentResult converts a held build into the builder's
// assignment.
func builderAssignmentResult(held heldBuild) *gensurla.BuilderAssignment {
	return &gensurla.BuilderAssignment{
		ChangeID:        held.file.ChangeID,
		RequestRevision: held.file.RequestRevision,
		InForce:         held.inForce,
		Read:            held.file.Builder.Read,
	}
}

// testWriterAssignmentItem converts a held build into the test writer's
// assignment summary.
func testWriterAssignmentItem(held heldBuild) *gensurla.TestWriterAssignmentItem {
	return &gensurla.TestWriterAssignmentItem{
		ChangeID:        held.file.ChangeID,
		RequestRevision: held.file.RequestRevision,
		InForce:         held.inForce,
		Progress:        testWriterProgress(held.file.TestWriter),
	}
}

// builderProgress converts the builder's saved assignment.
func builderProgress(saved builderAssignment) *gensurla.BuilderProgress {
	return &gensurla.BuilderProgress{Read: saved.Read}
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
// whose hash names it, and the test writer's state must match its fields.
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
func (c *ControlPlane) ownedBuild(principal Principal, changeID string) (heldBuild, error) {
	record, err := c.ownedCard(principal, changeID)
	if err != nil {
		return heldBuild{}, err
	}
	saved, found, err := c.cards.loadBuild(changeID)
	if err != nil {
		return heldBuild{}, err
	}
	if !found {
		return heldBuild{}, gensurla.MakeNotFound(fmt.Errorf("change_id %q has no build; the user starts one on the views page", changeID))
	}
	return heldBuild{file: saved, inForce: buildInForce(saved, record.card)}, nil
}

// ownedBuilds returns the build of every change principal owns, most recently
// started first (tie-broken by change_id). One unreadable card or build fails
// the call rather than being left out.
func (c *ControlPlane) ownedBuilds(principal Principal) ([]heldBuild, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	records, err := c.cards.all()
	if err != nil {
		return nil, err
	}
	var builds []heldBuild
	for _, record := range records {
		if record.owner != principal.ID {
			continue
		}
		saved, found, err := c.cards.loadBuild(record.card.ChangeID)
		if err != nil {
			return nil, err
		}
		if found {
			builds = append(builds, heldBuild{file: saved, inForce: buildInForce(saved, record.card)})
		}
	}
	slices.SortFunc(builds, func(a, b heldBuild) int {
		if byTime := b.file.StartedAt.Compare(a.file.StartedAt); byTime != 0 {
			return byTime
		}
		return strings.Compare(a.file.ChangeID, b.file.ChangeID)
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
) (heldBuild, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return heldBuild{}, errors.New("authenticated principal is required")
	}
	_, span := c.tracer("surla/controlplane").Start(ctx, spanName, trace.WithAttributes(attribute.String("surla.change_id", changeID)))
	defer span.End()
	c.mu.Lock()
	defer c.mu.Unlock()
	held, err := c.ownedBuild(principal, changeID)
	if err != nil {
		return heldBuild{}, recordBuildError(span, err, "load build")
	}
	span.SetAttributes(attribute.String("surla.request_revision", held.file.RequestRevision))
	if markRead(&held.file) {
		if err := c.cards.saveBuild(held.file); err != nil {
			return heldBuild{}, recordBuildError(span, err, "save build")
		}
		span.AddEvent("assignment read for the first time")
	}
	return held, nil
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
