package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestCardsSurviveANewControlPlane confirms a card, closes the control plane,
// and checks that a new one on the same directory serves the same card,
// owner and revision and can verify it.
func TestCardsSurviveANewControlPlane(t *testing.T) {
	dir := t.TempDir()
	cfg := verificationConfig(t)
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	analysis := refundAnalysis("Refunds up to $50 are instant.", &gensurla.CP1AnalysisAmbiguity{
		Question: "Does the limit include tax?", Options: []string{"with_tax", "without_tax"},
	})

	first, err := New(Options{CardsDir: dir, Verification: cfg})
	require.NoError(t, err)
	proposed, err := first.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", Analysis: analysis})
	require.NoError(t, err)
	require.Equal(t, StatusNeedsConfirmation, proposed.Status)
	confirmed, err := first.ConfirmIntent(alice, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: proposed.RequestRevision,
		Answers:  map[string]string{proposed.Ambiguities[0].ID: "with_tax"},
		Examples: proposed.Examples,
	})
	require.NoError(t, err)
	require.Equal(t, StatusConfirmed, confirmed.Status)
	require.NoError(t, first.Close())

	restarted := newControlPlane(t, Options{CardsDir: dir, Verification: cfg})
	got, err := restarted.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, confirmed, got, "the saved card comes back unchanged")

	_, err = restarted.GetChange(WithPrincipal(context.Background(), Principal{ID: "bob@example.com", CallerID: "bob@example.com", Kind: CallerUser}), &gensurla.GetChangePayload{ChangeID: "chg"})
	assert.ErrorContains(t, err, "is not the owner", "ownership is saved with the card")

	again, err := restarted.ConfirmIntent(alice, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: confirmed.RequestRevision,
		Answers: confirmed.Answers, Examples: confirmed.Examples,
	})
	require.NoError(t, err)
	assert.Equal(t, confirmed.RequestRevision, again.RequestRevision, "the design source is saved, so the revision is reproduced")

	run, err := restarted.VerifyChange(alice, &gensurla.VerifyChangePayload{
		ChangeID: "chg", RequestRevision: confirmed.RequestRevision, CandidateCommit: confirmed.RequestRevision[:40],
	})
	require.NoError(t, err)
	_, running := run.State.AsRunning()
	assert.True(t, running, "a restarted control plane verifies a card confirmed before the restart")

	_, err = restarted.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Again.", Analysis: analysis})
	assert.ErrorContains(t, err, "already exists")
}

// TestCardsDirectoryIsHeldByOneControlPlane checks that a second control
// plane cannot open a directory that one already holds.
func TestCardsDirectoryIsHeldByOneControlPlane(t *testing.T) {
	dir := t.TempDir()
	newControlPlane(t, Options{CardsDir: dir})
	_, err := New(Options{CardsDir: dir})
	assert.ErrorContains(t, err, "is in use by another process")

	_, err = New(Options{})
	assert.ErrorContains(t, err, "cards directory is required")
}

// TestCardStoreCloseReleasesDuplicatedLock checks the shutdown contract even
// while another descriptor still refers to the lock, as happens between fork
// and exec when an unrelated goroutine starts a child process.
func TestCardStoreCloseReleasesDuplicatedLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first, err := openCardStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, first.close()) })
	fd, err := syscall.Dup(int(first.lock.Fd()))
	require.NoError(t, err)
	duplicate := os.NewFile(uintptr(fd), "duplicated cards lock")
	t.Cleanup(func() { assert.NoError(t, duplicate.Close()) })

	_, err = openCardStore(dir)
	require.ErrorContains(t, err, "is in use by another process")
	require.NoError(t, first.close())

	second, err := openCardStore(dir)
	require.NoError(t, err, "Close releases the lock while the duplicate remains open")
	t.Cleanup(func() { assert.NoError(t, second.close()) })
	_, err = openCardStore(dir)
	assert.ErrorContains(t, err, "is in use by another process", "the new writer still holds an exclusive lock")
}

// TestUnreadableCardsAreErrors checks that a saved card file that is not a
// complete, consistent card fails the call instead of being skipped.
func TestUnreadableCardsAreErrors(t *testing.T) {
	saved := confirmedCardFile(t)
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"not JSON", "{", "decode change card"},
		{"unknown field", `{"card":{"ChangeID":"chg"},"owner":"alice@example.com","design_source":"","extra":1}`, "unknown field"},
		{"another card's content", `{"card":{"ChangeID":"other"},"owner":"alice@example.com","design_source":""}`, "does not hold that card"},
		{"trailing data", saved + "}", "trailing data"},
		{"content changed after saving", strings.Replace(saved, "Refunds up to $50 are instant.", "Refunds up to $500 are instant.", 1), "does not match its request_revision"},
		{"only an ID and owner", `{"card":{"ChangeID":"chg"},"owner":"alice@example.com","design_source":""}`, "unknown status"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cp := newControlPlane(t, Options{})
			require.NoError(t, os.WriteFile(cp.cards.path("chg"), []byte(c.content), 0o600))
			_, err := cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
			assert.ErrorContains(t, err, c.want)
		})
	}
}

// TestCardFilesStayInsideTheDirectory checks that card file names come from
// a hash of the change ID.
func TestCardFilesStayInsideTheDirectory(t *testing.T) {
	cp := newControlPlane(t, Options{})
	path := cp.cards.path("../../etc/passwd")
	assert.Equal(t, cp.cards.dir, filepath.Dir(path))
	assert.NotEqual(t, cp.cards.path("Req-1"), cp.cards.path("req-1"), "IDs differing only in case get different files")
}

// TestRequestsFinishingAfterCloseDoNotWrite closes the control plane while an
// approve_merge waits on the promotion target and checks that the approval
// writes nothing to the directory a new control plane now holds.
func TestRequestsFinishingAfterCloseDoNotWrite(t *testing.T) {
	dir := t.TempDir()
	spec := reportSpec{
		SuiteID: "orders_acceptance", SuiteCommit: strings.Repeat("b", 40), Environment: "sim",
		Artifacts: map[string]string{"orders": "d1"},
	}
	target := &promotionTargetStub{artifacts: spec.Artifacts, loading: make(chan struct{}), release: make(chan struct{})}
	cp, err := New(Options{CardsDir: dir, Verification: verificationConfig(t), PromotionTarget: target})
	require.NoError(t, err)
	card := saveCard(t, cp, nil)
	const commit = "0123456789abcdef0123456789abcdef01234567"
	target.report = passingReport(t, card.RequestRevision, commit, spec)
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	approved := make(chan error, 1)
	go func() {
		_, err := cp.ApproveMerge(alice, &gensurla.ApproveMergePayload{
			ChangeID: "chg", RequestRevision: card.RequestRevision, CandidateCommit: commit, ReportDigest: target.report.Digest,
		})
		approved <- err
	}()
	<-target.loading
	require.NoError(t, cp.Close())
	next := newControlPlane(t, Options{CardsDir: dir})
	close(target.release)

	assert.ErrorIs(t, <-approved, errCardsClosed, "the request that outlived Close fails")
	saved, ok, err := next.cards.load("chg")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Nil(t, saved.card.Approval, "and writes nothing to the directory the next control plane holds")
	assert.NoError(t, cp.Close(), "closing twice does nothing")
}

// TestProposalsAndRevisionsFinishingAfterCloseDoNotWrite holds a
// propose_change or revise_change at its span start, before it checks and
// saves the card, closes the control plane, lets a new control plane take
// the cards directory, and then releases the request. The request fails with
// errCardsClosed and writes nothing to the directory the new control plane
// holds: no card for a proposal, and the card unchanged for a revision.
func TestProposalsAndRevisionsFinishingAfterCloseDoNotWrite(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	// open returns a control plane on a new cards directory whose next span
	// named held stops at its start until hold is released.
	open := func(t *testing.T, held string) (*ControlPlane, *spanHold, string) {
		t.Helper()
		dir := t.TempDir()
		hold := &spanHold{name: held, started: make(chan struct{}), release: make(chan struct{})}
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(hold))
		t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
		return newControlPlane(t, Options{CardsDir: dir, TracerProvider: tp}), hold, dir
	}

	t.Run("propose_change", func(t *testing.T) {
		cp, hold, dir := open(t, "surla.controlplane.propose_change")
		hold.armed.Store(true)
		done := hold.run(t, func() error {
			_, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{
				ChangeID: "late", Prompt: "Refunds up to $50 are instant.", Analysis: refundAnalysis("Refunds up to $50 are instant."),
			})
			return err
		})
		require.NoError(t, cp.Close())
		next := newControlPlane(t, Options{CardsDir: dir})
		hold.releaseOnce()

		assert.ErrorIs(t, awaitHeld(t, done), errCardsClosed, "the proposal that outlived Close fails")
		_, saved, err := next.cards.load("late")
		require.NoError(t, err)
		assert.False(t, saved, "and saves no card in the directory the next control plane holds")
	})

	t.Run("revise_change", func(t *testing.T) {
		cp, hold, dir := open(t, "surla.controlplane.revise_change")
		card := mustPropose(t, cp, alice, "chg", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
		before, err := os.ReadFile(cp.cards.path("chg"))
		require.NoError(t, err)
		hold.armed.Store(true)
		check01 := "check_01"
		done := hold.run(t, func() error {
			_, err := cp.ReviseChange(alice, &gensurla.ReviseChangePayload{
				ChangeID: "chg", RequestRevision: card.RequestRevision, Prompt: "Refunds up to $60 are instant.",
				Analysis: &gensurla.CP1RevisionAnalysis{
					Summary:     "Refunds up to $60 are instant.",
					Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
					Examples: []*gensurla.CP1RevisionAnalysisExample{{
						PriorID: &check01, Scenario: "Refund at the limit", InputSummary: "Order total $60.00",
						ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
					}},
					RemovedExampleIds:  []string{},
					PreservedBehaviors: []string{},
				},
			})
			return err
		})
		require.NoError(t, cp.Close())
		next := newControlPlane(t, Options{CardsDir: dir})
		hold.releaseOnce()

		assert.ErrorIs(t, awaitHeld(t, done), errCardsClosed, "the revision that outlived Close fails")
		after, err := os.ReadFile(next.cards.path("chg"))
		require.NoError(t, err)
		assert.Equal(t, string(before), string(after), "and leaves the card in the directory the next control plane holds unchanged")
		got, err := next.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
		require.NoError(t, err)
		assert.Equal(t, card.RequestRevision, got.RequestRevision)
	})
}

// TestAFailedSaveKeepsThePreviousCard makes the directory read-only and
// checks that a confirm_intent that cannot be saved leaves the saved card as
// it was.
func TestAFailedSaveKeepsThePreviousCard(t *testing.T) {
	dir := t.TempDir()
	cp := newControlPlane(t, Options{CardsDir: dir})
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	card := saveCard(t, cp, nil)
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(dir, 0o700)) })

	_, err := cp.ConfirmIntent(alice, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: card.RequestRevision, Examples: []*gensurla.ExampleItem{{
			ID: "chg-check-02", Scenario: "Another refund", InputSummary: "Order total $10.00",
			ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
		}},
	})
	require.ErrorContains(t, err, "save change card")
	got, err := cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, card, got)
}

// TestASavedApprovalNeedsAPromotionTarget OnlyWhenConfigured saves an approved
// card and verifies that without promotion settings it is returned in Approval,
// while with promotion settings whose release binaries cannot be read it is
// returned in UnverifiedApproval without mutating the saved card on disk.
func TestASavedApprovalNeedsAPromotionTarget(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	recorded := &gensurla.ApprovalItem{
		ApprovedBy: "alice@example.com", ReportDigest: strings.Repeat("d", 64), CandidateCommit: strings.Repeat("c", 40),
		SuiteID: "orders", SuiteCommit: strings.Repeat("b", 40), Environment: "sim",
		Artifacts: map[string]string{"orders": strings.Repeat("e", 64)}, ScenarioCount: 5,
	}
	expectedApproval := *recorded
	expectedApproval.TestSource = gensurla.NewApprovalTestSourceEvaluator(&gensurla.OperatorEvaluatorTests{})

	// Without promotion settings, the saved approval is valid as long as the
	// chosen tests' commit matches.
	cpNoTarget := newControlPlane(t, Options{Verification: verificationConfig(t)})
	saveCard(t, cpNoTarget, recorded)
	gotNoTarget, err := cpNoTarget.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, &expectedApproval, gotNoTarget.Approval)
	assert.Nil(t, gotNoTarget.UnverifiedApproval)

	// With promotion settings whose release binaries cannot be read, the card
	// is served with unverified_approval while the saved record on disk is
	// untouched.
	target := &promotionTargetStub{artifactsErr: errors.New("missing release directory")}
	cp := newControlPlane(t, Options{Verification: verificationConfig(t), PromotionTarget: target})
	approved := saveCard(t, cp, recorded)
	got, err := cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Nil(t, got.Approval)
	require.NotNil(t, got.UnverifiedApproval)
	assert.Equal(t, &expectedApproval, got.UnverifiedApproval.Approval)
	assert.Contains(t, got.UnverifiedApproval.Reason, "Check current approval")
	assert.Contains(t, got.UnverifiedApproval.Reason, "surla promotion remove")
	saved, ok, err := cp.cards.load("chg")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Nil(t, saved.card.UnverifiedApproval)
	assert.Equal(t, approved, saved.card, "the approval itself stays saved, unchanged")
	approved.Approval = nil
	approved.UnverifiedApproval = got.UnverifiedApproval
	assert.Equal(t, approved, got, "the rest of the card is served with unverified_approval")
}

// TestSavedCardInTodaysUntaggedFormWithApprovalDecodes verifies that a card
// file written in today's untagged form with an Approval record (and no
// TestSource field) decodes and is returned by get_change only while the
// chooser gives its SuiteCommit.
func TestSavedCardInTodaysUntaggedFormWithApprovalDecodes(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	suiteCommit := strings.Repeat("b", 40)
	candidateCommit := strings.Repeat("c", 40)
	reportDigest := strings.Repeat("d", 64)
	artifactDigest := strings.Repeat("e", 64)
	confirmedBy := "alice@example.com"
	base := &gensurla.ChangeCardResult{
		ChangeID: "chg", Status: StatusConfirmed, Prompt: "Refunds up to $50 are instant.",
		Summary: "Refunds up to $50 are instant.", Ambiguities: []*gensurla.AmbiguityQuestion{},
		Answers: map[string]string{}, PreservedBehaviors: []string{}, ConfirmedBy: &confirmedBy,
		Examples: []*gensurla.ExampleItem{{
			ID: "chg-check-01", Scenario: "Refund at the limit", InputSummary: "Order total $50.00",
			ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
		}},
	}
	rev, err := computeCanonicalRevision(base, "alice@example.com", "")
	require.NoError(t, err)
	rawFile := `{"card":{"ChangeID":"chg","Status":"confirmed","Prompt":"Refunds up to $50 are instant.",` +
		`"Summary":"Refunds up to $50 are instant.","Ambiguities":[],"Answers":{},` +
		`"Examples":[{"ID":"chg-check-01","Scenario":"Refund at the limit","InputSummary":"Order total $50.00","ExpectedOutcome":"Instant store credit","VerificationKind":"deterministic"}],` +
		`"PreservedBehaviors":[],"ConfirmedBy":"alice@example.com","RequestRevision":"` + rev + `",` +
		`"Approval":{"ApprovedBy":"alice@example.com","ReportDigest":"` + reportDigest + `","CandidateCommit":"` + candidateCommit + `",` +
		`"SuiteID":"orders","SuiteCommit":"` + suiteCommit + `","Environment":"sim","Artifacts":{"orders":"` + artifactDigest + `"},"ScenarioCount":5}},` +
		`"owner":"alice@example.com","design_source":"","saved_at":"2026-11-01T10:00:00Z"}`

	cfg := verificationConfig(t)
	cfg.Plan.SuiteCommit = suiteCommit
	target := &promotionTargetStub{artifacts: map[string]string{"orders": artifactDigest}}
	cp := newControlPlane(t, Options{Verification: cfg, PromotionTarget: target})
	require.NoError(t, os.WriteFile(cp.cards.path("chg"), []byte(rawFile), 0o600))

	got, err := cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	require.NotNil(t, got.Approval, "returned while the chooser gives its SuiteCommit")
	assert.Equal(t, &gensurla.ApprovalItem{
		ApprovedBy: "alice@example.com", ReportDigest: reportDigest, CandidateCommit: candidateCommit,
		SuiteID: "orders", SuiteCommit: suiteCommit, Environment: "sim",
		Artifacts: map[string]string{"orders": artifactDigest}, ScenarioCount: 5,
		TestSource: gensurla.NewApprovalTestSourceEvaluator(&gensurla.OperatorEvaluatorTests{}),
	}, got.Approval)

	// When the chooser gives another commit, the saved approval is hidden.
	cp.verifier.(storedVerification).cfg.Plan.SuiteCommit = strings.Repeat("9", 40)
	hidden, err := cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Nil(t, hidden.Approval)
	assert.Nil(t, hidden.UnverifiedApproval)
}

// TestCurrentSavedCardRoundTripExcludesResponseOnlyApproval verifies that cardStore.save
// saves current card data without response-only UnverifiedApproval, across
// nil/empty collections and optional Approval/UIComposition fields.
func TestCurrentSavedCardRoundTripExcludesResponseOnlyApproval(t *testing.T) {
	confirmedBy := "alice@example.com"
	cases := []struct {
		name string
		card *gensurla.ChangeCardResult
	}{
		{
			name: "confirmed with approval, ui_composition, and populated collections",
			card: &gensurla.ChangeCardResult{
				ChangeID: "chg-full",
				Status:   StatusConfirmed,
				Prompt:   "Refunds up to $50 are instant.",
				Summary:  "Instant refunds up to $50.",
				Ambiguities: []*gensurla.AmbiguityQuestion{{
					ID: "chg-full-ambiguity-01", Question: "Include tax?", Options: []string{"yes", "no"},
				}},
				Answers: map[string]string{"chg-full-ambiguity-01": "yes"},
				Examples: []*gensurla.ExampleItem{{
					ID: "chg-full-check-01", Scenario: "Limit", InputSummary: "$50",
					ExpectedOutcome: "Instant", VerificationKind: "deterministic",
				}},
				PreservedBehaviors: []string{"Over $50 requires manager."},
				ConfirmedBy:        &confirmedBy,
				UIComposition: &gensurla.UICompositionItem{
					Program: "root = Stack([])", ComposedForRevision: "rev-placeholder",
				},
				Approval: &gensurla.ApprovalItem{
					ApprovedBy: "alice@example.com", ReportDigest: strings.Repeat("a", 64),
					CandidateCommit: strings.Repeat("b", 40), SuiteID: "orders",
					SuiteCommit: strings.Repeat("c", 40), Environment: "sim",
					Artifacts: map[string]string{"orders": strings.Repeat("d", 64)}, ScenarioCount: 3,
				},
				UnverifiedApproval: &gensurla.UnverifiedApprovalItem{
					Approval: &gensurla.ApprovalItem{ApprovedBy: "must-not-persist"},
					Reason:   "response-only field must never be written to disk",
				},
			},
		},
		{
			name: "needs_confirmation with nil optional fields and nil/empty collections",
			card: &gensurla.ChangeCardResult{
				ChangeID: "chg-minimal",
				Status:   StatusNeedsConfirmation,
				Prompt:   "Add audit log.",
				Summary:  "Write audit log entries.",
				Examples: []*gensurla.ExampleItem{{
					ID: "chg-minimal-check-01", Scenario: "Audit", InputSummary: "Action",
					ExpectedOutcome: "Logged", VerificationKind: "deterministic",
				}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := newControlPlane(t, Options{})
			rev, err := computeCanonicalRevision(tc.card, "alice@example.com", "package design")
			require.NoError(t, err)
			tc.card.RequestRevision = rev
			if tc.card.UIComposition != nil {
				tc.card.UIComposition.ComposedForRevision = rev
			}
			require.NoError(t, cp.cards.save(storedChangeCard{
				card: tc.card, owner: "alice@example.com", designSource: "package design",
			}))

			raw, err := os.ReadFile(cp.cards.path(tc.card.ChangeID))
			require.NoError(t, err)
			assert.NotContains(t, string(raw), "UnverifiedApproval")
			assert.NotContains(t, string(raw), "unverified_approval")

			loaded, ok, err := cp.cards.load(tc.card.ChangeID)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Nil(t, loaded.card.UnverifiedApproval)
			expected := *tc.card
			expected.UnverifiedApproval = nil
			assert.Equal(t, &expected, loaded.card)
		})
	}
}

// saveCard saves a confirmed card "chg" owned by alice, with approval, in
// cp's store and returns it.
func saveCard(t *testing.T, cp *ControlPlane, approval *gensurla.ApprovalItem) *gensurla.ChangeCardResult {
	t.Helper()
	confirmedBy := "alice@example.com"
	card := &gensurla.ChangeCardResult{
		ChangeID: "chg", Status: StatusConfirmed, Prompt: "Refunds up to $50 are instant.",
		Summary: "Refunds up to $50 are instant.", Ambiguities: []*gensurla.AmbiguityQuestion{},
		Answers: map[string]string{}, PreservedBehaviors: []string{}, ConfirmedBy: &confirmedBy,
		Examples: []*gensurla.ExampleItem{{
			ID: "chg-check-01", Scenario: "Refund at the limit", InputSummary: "Order total $50.00",
			ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
		}},
	}
	rev, err := computeCanonicalRevision(card, "alice@example.com", "")
	require.NoError(t, err)
	card.RequestRevision = rev
	card.Approval = approval
	require.NoError(t, cp.cards.save(storedChangeCard{card: card, owner: "alice@example.com"}))
	return card
}

// confirmedCardFile returns the saved file content of a confirmed card "chg".
func confirmedCardFile(t *testing.T) string {
	t.Helper()
	cp := newControlPlane(t, Options{})
	saveCard(t, cp, nil)
	data, err := os.ReadFile(cp.cards.path("chg"))
	require.NoError(t, err)
	return string(data)
}

// TestAnUnfinishedSaveIsHarmless leaves a partly written staging file, as a
// save that crashed before its rename would, and checks that a new control
// plane still serves the saved card and that the next save succeeds.
func TestAnUnfinishedSaveIsHarmless(t *testing.T) {
	dir := t.TempDir()
	first, err := New(Options{CardsDir: dir})
	require.NoError(t, err)
	card := saveCard(t, first, nil)
	require.NoError(t, first.Close())
	require.NoError(t, os.WriteFile(filepath.Join(dir, stagingFile), []byte(`{"card":`), 0o600))

	next := newControlPlane(t, Options{CardsDir: dir})
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	got, err := next.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, card, got, "the saved card is untouched")

	updated, err := next.ConfirmIntent(alice, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: card.RequestRevision, Examples: []*gensurla.ExampleItem{{
			ID: "chg-check-02", Scenario: "Another refund", InputSummary: "Order total $10.00",
			ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
		}},
	})
	require.NoError(t, err)
	got, err = next.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, updated, got, "the next save overwrites the staging file and publishes")
	assert.NoFileExists(t, filepath.Join(dir, stagingFile), "the rename consumed it")
}

// TestCardsWithInvalidUTF8RoundTrip saves a card whose text holds invalid
// UTF-8, which only a Go caller can supply, and checks it reads back under
// the same request_revision: json.Marshal writes such bytes as U+FFFD, and so
// does every later encoding of the decoded card. The change_id itself must be
// valid (TestChangeIDsMustBeValidUTF8), and answer keys are code-owned
// ambiguity IDs.
func TestCardsWithInvalidUTF8RoundTrip(t *testing.T) {
	cp := newControlPlane(t, Options{})
	confirmedBy := "alice\xff"
	card := &gensurla.ChangeCardResult{ChangeID: "chg", Status: StatusConfirmed, Prompt: "Refunds \xff", ConfirmedBy: &confirmedBy}
	rev, err := computeCanonicalRevision(card, "alice\xff", "design \xff")
	require.NoError(t, err)
	card.RequestRevision = rev
	require.NoError(t, cp.cards.save(storedChangeCard{card: card, owner: "alice\xff", designSource: "design \xff"}))
	saved, ok, err := cp.cards.load("chg")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rev, saved.card.RequestRevision)
}

// TestStagingDoesNotFollowLinks plants a link at the staging name and checks
// that a save fails instead of writing through it.
func TestStagingDoesNotFollowLinks(t *testing.T) {
	cp := newControlPlane(t, Options{})
	victim := filepath.Join(t.TempDir(), "victim")
	require.NoError(t, os.WriteFile(victim, []byte("keep"), 0o600))
	require.NoError(t, os.Symlink(victim, filepath.Join(cp.cards.dir, stagingFile)))

	confirmedBy := "alice@example.com"
	card := &gensurla.ChangeCardResult{ChangeID: "chg", Status: StatusConfirmed, ConfirmedBy: &confirmedBy}
	err := cp.cards.save(storedChangeCard{card: card, owner: "alice@example.com"})
	assert.ErrorContains(t, err, "save change card")
	data, err := os.ReadFile(victim)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(data))
}

// TestChangeIDsMustBeValidUTF8 checks that propose_change rejects an ID that
// could not be found again once saved.
func TestChangeIDsMustBeValidUTF8(t *testing.T) {
	cp := newControlPlane(t, Options{})
	_, err := cp.ProposeChange(WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser}),
		&gensurla.ProposeChangePayload{ChangeID: "chg\xff", Prompt: "Refunds up to $50 are instant.", Analysis: refundAnalysis("Refunds up to $50 are instant.")})
	assertServiceError(t, err, "unprocessable")
	assert.ErrorContains(t, err, `["change_id"]: invalid UTF-8`)
}

// TestPrincipalIDsMustBeValidUTF8 checks that a principal whose ID holds
// invalid UTF-8, which only a Go authenticator can supply, is treated as
// unauthenticated, so no card is saved under an owner it could not match.
func TestPrincipalIDsMustBeValidUTF8(t *testing.T) {
	cp := newControlPlane(t, Options{})
	bad := WithPrincipal(context.Background(), Principal{ID: "alice\xff", CallerID: "alice\xff", Kind: CallerUser})
	_, err := cp.ProposeChange(bad, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", Analysis: refundAnalysis("Refunds up to $50 are instant.")})
	assert.ErrorContains(t, err, "authenticated principal is required")
	_, err = cp.GetChange(bad, &gensurla.GetChangePayload{ChangeID: "chg"})
	assert.ErrorContains(t, err, "authenticated principal is required")
	_, saved, err := cp.cards.load("chg")
	require.NoError(t, err)
	assert.False(t, saved)
}

// TestCloseRemovesTheEvaluatorWorkspace checks that a control plane that
// never evaluates creates no design evaluator workspace, that one that did
// removes it on Close, and that it refuses evaluations afterwards.
func TestCloseRemovesTheEvaluatorWorkspace(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	incomplete := &gensurla.DesignSnapshot{Files: map[string]string{"design/design.go": "package design\n"}}

	idle, err := New(Options{CardsDir: t.TempDir()})
	require.NoError(t, err)
	require.NoError(t, idle.Close())
	assert.Empty(t, workspaces(t, tmp), "a control plane that never evaluates creates no workspace")

	cp, err := New(Options{CardsDir: t.TempDir()})
	require.NoError(t, err)
	_, err = cp.Diff(alice, &gensurla.DiffPayload{Old: incomplete, New: incomplete})
	require.Error(t, err)
	require.Len(t, workspaces(t, tmp), 1)
	require.NoError(t, cp.Close())
	assert.Empty(t, workspaces(t, tmp))
	_, err = cp.Diff(alice, &gensurla.DiffPayload{Old: incomplete, New: incomplete})
	require.ErrorContains(t, err, "design evaluator is closed")
	require.NoError(t, cp.Close(), "closing again repeats the same results")
}
