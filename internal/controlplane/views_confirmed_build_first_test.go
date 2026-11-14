// This file exercises the confirmed card's Build-first reading order and its
// native refinement disclosure through the served page and generated HTTP API.
package controlplane

import (
	"encoding/json"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/require"
)

// TestViewsPageConfirmedBuildLeadsTheFoldedAgreement keeps the next work ahead
// of the saved decision while retaining editable answers, exact attribution,
// the explicit refinement action and the existing build consequence warning.
// Build and agent processes are fixture boundaries, not execution proof.
func TestViewsPageConfirmedBuildLeadsTheFoldedAgreement(t *testing.T) {
	cp := newBuildControlPlane(t, Options{
		Team: &TeamBinding{Owner: pageOwner, Service: &teamServiceFixture{}},
	})
	cp.changesWait, cp.changesPoll = 400*time.Millisecond, 20*time.Millisecond
	proposed := mustPropose(t, cp, asCaller(pageOwner, CallerGuide), "chg-fold",
		"Route refunds to a manager.", refundAnalysis("A manager reviews refunds.",
			&gensurla.CP1AnalysisAmbiguity{
				Question: "Does the limit include tax?", Options: []string{"Subtotal only", "With tax"},
			}))
	questionID := proposed.Ambiguities[0].ID
	card, err := cp.ConfirmIntent(asCaller(pageOwner, CallerUser), &gensurla.ConfirmIntentPayload{
		ChangeID: proposed.ChangeID, RequestRevision: proposed.RequestRevision,
		Answers: map[string]string{questionID: "Subtotal only"}, Examples: proposed.Examples,
	})
	require.NoError(t, err)
	ps := startPageServer(t, cp)
	ps.act("resave", func() {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		record, ok, err := cp.cards.load(card.ChangeID)
		require.NoError(t, err)
		require.True(t, ok)
		require.NoError(t, cp.cards.save(record))
	})
	ps.act("start-build", func() {
		_, err := cp.StartBuild(asCaller(pageOwner, CallerUser), &gensurla.StartBuildPayload{
			ChangeID: card.ChangeID, RequestRevision: card.RequestRevision,
		})
		require.NoError(t, err)
	})

	wireCard, err := gensurla.EncodeChangeCardResult(card)
	require.NoError(t, err)
	runPageScript(t, ps, map[string]any{"card": json.RawMessage(wireCard)}, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "Build was not ready");
const editor = page.$("#surla-cp1-editor");
const build = page.$("#surla-build-section");
const agreement = page.$("#surla-agreement-details");
const refinement = page.$("#surla-refine-agreement");
expect(build.compareDocumentPosition(agreement) & page.win.Node.DOCUMENT_POSITION_FOLLOWING, "the long agreement precedes Build");
expect(agreement.tagName === "DETAILS" && !agreement.open, "saved agreement is not folded in a native disclosure");
expect(agreement.querySelector("summary").textContent === "Agreement details", "agreement disclosure does not name its contents");
expect(agreement.parentElement.querySelector("h2").textContent === "Agreed: " + input.card.prompt, "agreement does not use the user's request");
expect(agreement.textContent.includes(input.card.summary) && agreement.textContent.includes("Subtotal only") && agreement.textContent.includes(input.card.confirmed_by) && agreement.textContent.includes("recorded user identity") && agreement.textContent.includes(input.card.request_revision), "folded agreement lost saved facts or attribution");
expect(refinement.tagName === "DETAILS" && !refinement.open && refinement.querySelector("summary").textContent === "Refine agreed answers and examples", "refinement is not folded behind a named native disclosure");
expect(page.$("#surla-cp1-form-card").closest("details") === refinement, "the full form still fills the confirmed view");
expect(page.text("#surla-cp1-form-card").includes("Your answers (1)") && !page.text("#surla-cp1-form-card").includes("[Confirmed answer:"), "confirmed answers retain open-question or duplicate-answer copy");
expect(page.sent("POST", "/changes/confirm").length === 0, "opening the agreement submitted a refinement");

refinement.open = true;
const field = page.$('textarea[data-field="expected_outcome"]');
field.focus();
field.setSelectionRange(2, 7);
const reads = page.sent("GET", "/changes/chg-fold").length;
await control(origin, "act", { key: "resave" });
await until(() => page.sent("GET", "/changes/chg-fold").some((r, i) => i >= reads && r.status === 200), "the saved card was not refreshed");
await sleep(100);
expect(refinement.open && page.$('textarea[data-field="expected_outcome"]') === field && page.doc.activeElement === field && field.selectionStart === 2 && field.selectionEnd === 7, "refresh collapsed or remounted the focused refinement");

page.type(field, "A manager reviews this refund");
field.setSelectionRange(3, 9);
await control(origin, "act", { key: "start-build" });
await until(() => page.$("#surla-build-section")?.dataset.state === "started" && !page.$("#surla-build-loading"), "the background build update did not arrive");
expect(refinement.open && page.$('textarea[data-field="expected_outcome"]') === field && field.value === "A manager reviews this refund" && page.doc.activeElement === field && field.selectionStart === 3 && field.selectionEnd === 9, "Build refresh hid or disturbed the dirty refinement");
expect(page.beforeUnload(), "the folded draft lost its unload protection");
const submit = page.$("#surla-confirm-submit");
const warning = page.$("#surla-confirm-build-consequence");
expect(warning && warning.textContent.includes("earlier revision") && submit.getAttribute("aria-describedby") === warning.id, "refinement lost its build consequence warning");
expect(!submit.disabled && submit.textContent.includes("Refine Confirmed Answers & Examples"), "the explicit refinement action is missing");
expect(page.sent("POST", "/changes/confirm").length === 0, "editing or refreshing submitted the draft");
submit.click();
await until(() => editor.dataset.requestRevision !== input.card.request_revision, "the explicit refinement did not save");
expect(page.sent("POST", "/changes/confirm").length === 1 && page.sent("POST", "/changes/chg-fold/build").length === 0, "refinement submitted twice or automatically rebuilt");
page.close();
`)
}

// TestViewsPageConfirmedProposalOpensRefinement makes the proposal's blocked
// refinement path visible even when the user had not opened the saved form.
func TestViewsPageConfirmedProposalOpensRefinement(t *testing.T) {
	ps, agreed := proposalPage(t)
	runPageScript(t, ps, map[string]any{"agreed": agreed}, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-refine-agreement"), "the agreed change did not open");
expect(!page.$("#surla-refine-agreement").open, "an unchanged form was expanded");
await control(origin, "act", { key: "propose" });
await until(() => page.$("#surla-proposal") && page.$("#surla-refine-agreement").open, "the proposal's refinement path stayed hidden");
expect(page.$("#surla-confirm-submit").disabled && page.$("#surla-confirm-blocked") && page.$("#surla-cp1-editor").dataset.requestRevision === input.agreed, "the proposal replaced the agreement or lost its explanation");
page.$("#surla-proposal-decline").click();
await until(() => !page.$("#surla-proposal") && !page.$("#surla-confirm-submit").disabled, "declining did not restore refinement");
expect(page.$("#surla-refine-agreement").open, "resolving the proposal collapsed the refinement path");
page.close();
`)
}

// TestViewsPageUnconfirmedIntentKeepsItsFullEditableForm preserves the first
// confirmation journey: answers, examples and submission stay directly visible.
func TestViewsPageUnconfirmedIntentKeepsItsFullEditableForm(t *testing.T) {
	ps := startPageServer(t, pageControlPlane(t, Options{}))
	runPageScript(t, ps, nil, viewsPageScriptHelpers+`
const card = await propose("chg-unconfirmed-fold", "Route refunds to a manager.", "Auto-approved");
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-form-card"), "the proposed card did not open");
expect(!page.$("#surla-refine-agreement") && !page.$("#surla-agreement-details") && !page.$("#surla-cp1-form-card").closest("details"), "the first confirmation form was folded");
expect(page.text("#surla-cp1-form-card").includes("Open Questions (1)"), "the unconfirmed questions changed meaning");
page.$('[data-ambiguity-id="chg-unconfirmed-fold-ambiguity-01"][data-option="subtotal_only"]').click();
const field = page.$('textarea[data-field="expected_outcome"]');
page.type(field, "Manager approval");
checkControl(page, page.$("#surla-confirm-submit"), "confirmation");
expect(page.text("#surla-confirm-submit").includes("Confirm Intent & Lock Examples"), "initial confirmation is missing");
page.$("#surla-confirm-submit").click();
await until(() => page.$("#surla-agreement-details"), "confirmation did not show the agreed card");
const sent = page.sent("POST", "/changes/confirm");
expect(sent.length === 1 && sent[0].body.answers[card.ambiguities[0].id] === "subtotal_only" && sent[0].body.examples[0].expected_outcome === "Manager approval", "confirmation lost the chosen answer or edited example");
page.close();
`)
}
