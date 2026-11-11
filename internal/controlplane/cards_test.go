package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCardsSurviveANewControlPlane confirms a card, closes the control plane,
// and checks that a new one on the same directory serves the same card,
// owner and revision and can verify it.
func TestCardsSurviveANewControlPlane(t *testing.T) {
	dir := t.TempDir()
	cfg := verificationConfig(t)
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
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

	_, err = restarted.GetChange(WithPrincipal(context.Background(), Principal{ID: "bob@example.com"}), &gensurla.GetChangePayload{ChangeID: "chg"})
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

// TestUnreadableCardsAreErrors checks that a saved card file that is not a
// complete, consistent card fails the call instead of being skipped.
func TestUnreadableCardsAreErrors(t *testing.T) {
	saved := confirmedCardFile(t)
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
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
	inputs := PromotionInputs{
		SuiteID: "orders_acceptance", SuiteCommit: "suite-1", Environment: "sim",
		Artifacts: map[string]string{"orders": "d1"},
	}
	target := &promotionTargetStub{inputs: inputs, loading: make(chan struct{}), release: make(chan struct{})}
	cp, err := New(Options{CardsDir: dir, PromotionTarget: target})
	require.NoError(t, err)
	card := saveCard(t, cp, nil)
	const commit = "0123456789abcdef0123456789abcdef01234567"
	target.report = passingReport(t, card.RequestRevision, commit, inputs)
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
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

// TestAFailedSaveKeepsThePreviousCard makes the directory read-only and
// checks that a confirm_intent that cannot be saved leaves the saved card as
// it was.
func TestAFailedSaveKeepsThePreviousCard(t *testing.T) {
	dir := t.TempDir()
	cp := newControlPlane(t, Options{CardsDir: dir})
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
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

// TestASavedApprovalNeedsAPromotionTarget saves an approved card and reads it
// from a control plane without a promotion target, which cannot check the
// approval against current inputs and so reports it in UnverifiedApproval
// rather than Approval.
func TestASavedApprovalNeedsAPromotionTarget(t *testing.T) {
	cp := newControlPlane(t, Options{})
	recorded := &gensurla.ApprovalItem{
		ApprovedBy: "alice@example.com", ReportDigest: strings.Repeat("d", 64), CandidateCommit: strings.Repeat("c", 40),
		SuiteID: "orders", SuiteCommit: strings.Repeat("b", 40), Environment: "sim",
		Artifacts: map[string]string{"orders": strings.Repeat("e", 64)}, ScenarioCount: 5,
	}
	approved := saveCard(t, cp, recorded)
	got, err := cp.GetChange(WithPrincipal(context.Background(), Principal{ID: "alice@example.com"}), &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Nil(t, got.Approval)
	require.NotNil(t, got.UnverifiedApproval)
	assert.Equal(t, recorded, got.UnverifiedApproval.Approval)
	assert.Contains(t, got.UnverifiedApproval.Reason, "-promotion-config")
	saved, ok, err := cp.cards.load("chg")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Nil(t, saved.card.UnverifiedApproval)
	assert.Equal(t, approved, saved.card, "the approval itself stays saved, unchanged")
	approved.Approval = nil
	approved.UnverifiedApproval = got.UnverifiedApproval
	assert.Equal(t, approved, got, "the rest of the card is served with unverified_approval")
}

// TestSavedCardCompatibilityWithPrior12FieldReader verifies that cardStore.save
// writes only the 12 persisted Change Card fields (never UnverifiedApproval)
// and that a strict 12-field reader with DisallowUnknownFields decodes cards
// across nil/empty collections and optional Approval/UIComposition fields.
func TestSavedCardCompatibilityWithPrior12FieldReader(t *testing.T) {
	type prior12FieldCard struct {
		ChangeID           string
		Status             string
		Prompt             string
		Summary            string
		Ambiguities        []*gensurla.AmbiguityQuestion
		Answers            map[string]string
		Examples           []*gensurla.ExampleItem
		PreservedBehaviors []string
		ConfirmedBy        *string
		RequestRevision    string
		UIComposition      *gensurla.UICompositionItem
		Approval           *gensurla.ApprovalItem
	}
	type priorCardFile struct {
		Card         *prior12FieldCard `json:"card"`
		Owner        string            `json:"owner"`
		DesignSource string            `json:"design_source"`
	}

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

			var oldFormat priorCardFile
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			require.NoError(t, dec.Decode(&oldFormat))
			require.NotNil(t, oldFormat.Card)
			assert.Equal(t, tc.card.ChangeID, oldFormat.Card.ChangeID)
			assert.Equal(t, tc.card.RequestRevision, oldFormat.Card.RequestRevision)
			assert.Equal(t, tc.card.Approval, oldFormat.Card.Approval)
			assert.Equal(t, tc.card.UIComposition, oldFormat.Card.UIComposition)

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
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
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
	_, err := cp.ProposeChange(WithPrincipal(context.Background(), Principal{ID: "alice@example.com"}),
		&gensurla.ProposeChangePayload{ChangeID: "chg\xff", Prompt: "Refunds up to $50 are instant.", Analysis: refundAnalysis("Refunds up to $50 are instant.")})
	assertServiceError(t, err, "unprocessable")
	assert.ErrorContains(t, err, `["change_id"]: invalid UTF-8`)
}

// TestPrincipalIDsMustBeValidUTF8 checks that a principal whose ID holds
// invalid UTF-8, which only a Go authenticator can supply, is treated as
// unauthenticated, so no card is saved under an owner it could not match.
func TestPrincipalIDsMustBeValidUTF8(t *testing.T) {
	cp := newControlPlane(t, Options{})
	bad := WithPrincipal(context.Background(), Principal{ID: "alice\xff"})
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
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
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
