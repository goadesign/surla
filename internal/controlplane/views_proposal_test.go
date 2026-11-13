// This file drives the views page when the guide revises an agreed change:
// the revision waits beside the agreed one as a proposal, which the page
// shows against the agreed revision and lets the user adopt or decline.
package controlplane

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
)

// TestViewsPageAdoptsAndDeclinesAProposedRevision opens an agreed change with
// a Story and a typed edit. The guide's revision appears as a proposal
// without a reload, keeping the edit; the page names what would change (the
// summary, a new question, an added flow), blocks refining the agreed
// revision, keeps a chosen answer across a re-read, lists the proposal, and
// declines it. A second proposal is adopted: it becomes the agreed revision,
// confirmed by the user, with the earlier revision shown as prior and the
// edit typed on it preserved.
func TestViewsPageAdoptsAndDeclinesAProposedRevision(t *testing.T) {
	cp, ps := storyPageServer(t)
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	user := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: pageUserID, Kind: CallerUser})
	op := seedStoryRefreshForTest(t, cp, pageOwner, storyRefreshStateSucceeded, pageStories(), "")
	proposeWithStory(t, cp, guide, "chg-proposal", op)
	proposed, err := cp.GetChange(guide, &gensurla.GetChangePayload{ChangeID: "chg-proposal"})
	require.NoError(t, err)
	agreed, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
		ChangeID: proposed.ChangeID, RequestRevision: proposed.RequestRevision,
		Answers: map[string]string{}, Examples: proposed.Examples,
	})
	require.NoError(t, err)
	ps.act("propose", func() {
		draft := pageStories()
		draft[0].Flows = append(draft[0].Flows, designeval.StoryFlow{
			Key: "exchange", Title: "Exchange instead of refund", Complete: true,
			Steps: []designeval.StoryStep{
				{Key: "ask", ActorKey: "customer", Summary: "Customer asks for an exchange", Kind: "start"},
				{Key: "shipped", ActorKey: "customer", Summary: "The replacement ships", Kind: "outcome"},
			},
		})
		storyOp := seedStoryRefreshForTest(t, cp, pageOwner, storyRefreshStateSucceeded, draft, "")
		change, err := cp.GetChange(guide, &gensurla.GetChangePayload{ChangeID: "chg-proposal"})
		require.NoError(t, err)
		latest := change.RequestRevision
		if change.ProposedRevision != nil {
			latest = change.ProposedRevision.RequestRevision
		}
		kept := "check_01"
		_, err = cp.ReviseChange(guide, &gensurla.ReviseChangePayload{
			ChangeID: "chg-proposal", RequestRevision: latest, Prompt: "Refunds up to $60 are instant.", StoryRefreshOpID: &storyOp,
			Analysis: &gensurla.CP1RevisionAnalysis{
				Summary:     "Refunds up to $60 are instant; exchanges ship a replacement.",
				Ambiguities: []*gensurla.CP1AnalysisAmbiguity{refundTaxQuestion()},
				Examples: []*gensurla.CP1RevisionAnalysisExample{{
					PriorID: &kept, Scenario: "Refund at the limit", InputSummary: "Order total $60.00",
					ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
				}},
				RemovedExampleIds:  []string{},
				PreservedBehaviors: []string{},
			},
		})
		require.NoError(t, err)
	})
	ps.act("resave", func() {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		record, ok, err := cp.cards.load("chg-proposal")
		require.NoError(t, err)
		require.True(t, ok)
		require.NoError(t, cp.cards.save(record))
	})
	runPageScript(t, ps, map[string]any{"agreed": agreed.RequestRevision}, viewsPageScriptHelpers+`
const page = await openPage(origin, { link: input.link });
const editor = () => page.$("#surla-cp1-editor");
const outcome = () => page.$('textarea[data-example-id="check_01"][data-field="expected_outcome"]');
const answer = () => page.$('#surla-proposal [data-question-id="chg-proposal-ambiguity-01"] [data-option="with_tax"]');
await until(() => editor().dataset.changeId === "chg-proposal" && page.text("#surla-cp1-editor").includes("Confirmed Change Decision"), "the agreed change did not open");
page.type(outcome(), "Typed on the agreed revision");

await control(origin, "act", { key: "propose" });
await until(() => page.$("#surla-proposal"), "the guide's revision did not appear without a reload");
expect(page.status().includes("proposed a new revision"), "the proposal was not announced: " + page.status());
expect(editor().dataset.requestRevision === input.agreed && outcome().value === "Typed on the agreed revision", "the proposal replaced the agreed revision or lost a typed edit");
expect(page.$("#surla-proposal-summary").dataset.changed === "true" && page.text("#surla-proposal-summary").includes("exchanges ship a replacement"), "the changed summary is not shown");
const question = page.$('#surla-proposal [data-question-id="chg-proposal-ambiguity-01"]');
expect(question && question.dataset.new === "true" && question.textContent.includes("Does the limit include tax?"), "the new question is not shown");
const added = page.$$('#surla-proposal-story [data-difference="added"] strong').map((s) => s.textContent);
expect(JSON.stringify(added) === JSON.stringify(["Exchange instead of refund"]), "the added flow is not named: " + JSON.stringify(added));
expect(page.$("#surla-confirm-submit").disabled && page.$("#surla-confirm-blocked"), "refining the agreed revision is still offered");
expect(page.$("#surla-proposal-adopt").disabled && page.$("#surla-proposal-adopt-hint"), "Adopt is offered before every question is answered");
checkControl(page, page.$("#surla-proposal-decline"), "Decline");

answer().click();
expect(!page.$("#surla-proposal-adopt").disabled, "answering every question did not enable Adopt");
const reads = page.sent("GET", "/changes/chg-proposal").length;
await control(origin, "act", { key: "resave" });
await until(() => page.sent("GET", "/changes/chg-proposal").length > reads, "the change was not read again");
await sleep(100);
expect(answer().getAttribute("aria-pressed") === "true" && !page.$("#surla-proposal-adopt").disabled, "a re-read lost the chosen answer");

page.$("#surla-nav-changes").click();
await until(() => page.$('#surla-change-list [data-change-id="chg-proposal"] .surla-proposal-badge'), "the change list does not name the waiting proposal");
page.$("#surla-nav-cp1").click();
await until(() => page.$("#surla-proposal-decline"), "returning to the change did not show the proposal");

page.$("#surla-proposal-decline").click();
await until(() => !page.$("#surla-proposal"), "declining did not remove the proposal");
expect(editor().dataset.requestRevision === input.agreed && outcome().value === "Typed on the agreed revision" && !page.$("#surla-confirm-submit").disabled, "declining changed the agreed revision or lost the typed edit");
const declined = await readCard("chg-proposal");
expect(declined.request_revision === input.agreed && !declined.proposed_revision, "the server still holds the declined proposal");

await control(origin, "act", { key: "propose" });
await until(() => page.$("#surla-proposal"), "the second proposal did not appear");
answer().click();
checkControl(page, page.$("#surla-proposal-adopt"), "Adopt");
const proposal = (await readCard("chg-proposal")).proposed_revision;
page.$("#surla-proposal-adopt").click();
await until(() => !page.$("#surla-proposal") && editor().dataset.requestRevision !== input.agreed, "adopting did not make the proposal the agreed revision");
const adoption = page.sent("POST", "/changes/confirm").pop().body;
expect(adoption.request_revision === proposal.request_revision && adoption.answers["chg-proposal-ambiguity-01"] === "with_tax", "Adopt did not confirm the proposal with the chosen answer: " + JSON.stringify(adoption));
const adopted = await readCard("chg-proposal");
expect(adopted.status === "confirmed" && adopted.confirmed_by === input.owner && !adopted.proposed_revision && adopted.summary.includes("exchanges"), "the adopted revision is not in force: " + JSON.stringify(adopted));
expect(editor().dataset.requestRevision === adopted.request_revision && page.$("#surla-prior-revision") && page.text("#surla-prior-revision").includes(input.agreed), "the earlier agreed revision is not shown as prior");
expect(page.$("#surla-preserved-draft") && page.text("#surla-preserved-draft").includes("Typed on the agreed revision"), "adopting lost the edit typed on the earlier revision");
page.close();
`)
}
