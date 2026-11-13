// This file tests the views page across revisions the guide saves while the
// page is open: the previous-revision comparison (kept, edited, removed),
// presentation-only updates of the same revision, preserved unsaved lines
// across successive revisions, and every Checkpoint 5 approval state.
package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// TestCheckpointUIReviseLifecyclePriorComparison follows one change through
// revisions the guide saves while the page is open, checking what each
// arrival does to the editor, its previous-revision panel, and the user's
// unsaved lines.
func TestCheckpointUIReviseLifecyclePriorComparison(t *testing.T) {
	cp := pageControlPlane(t, Options{})
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	mustPropose(t, cp, guide, "chg-rev-ui", "Refunds up to $50 are instant.", &gensurla.CP1Analysis{
		Summary:     "Refunds up to $50 are instant.",
		Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
		Examples: []*gensurla.CP1AnalysisExample{
			{Scenario: "Refund under $50", InputSummary: "$40 subtotal", ExpectedOutcome: "Instant refund", VerificationKind: "deterministic"},
		},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
	})
	ps := startPageServer(t, cp)
	runPageScript(t, ps, nil, `
const editor = () => page.$("#surla-cp1-editor");
const prior = () => page.$("#surla-prior-revision");
const preserved = () => page.$("#surla-preserved-draft");
const outcome = (id) => editor().querySelector('textarea[data-example-id="' + id + '"][data-field="expected_outcome"]');
const read = async () => (await api(origin, "GET", "/changes/chg-rev-ui", undefined, input.guide)).body;
async function revise(card, summary, analysis) {
  const out = await api(origin, "POST", "/changes/revise", { change_id: "chg-rev-ui", request_revision: card.request_revision, prompt: summary, analysis: { summary, ...analysis } }, input.guide);
  expect(out.status === 200, "could not revise: " + JSON.stringify(out));
  return out.body;
}
const taxQuestion = [{ question: "Does the $50 limit include tax?", options: ["subtotal_only", "total_with_tax"] }];
const twoExamples = (secondOutcome, extra) => ({
  ambiguities: taxQuestion,
  examples: [
    { prior_id: "check_01", scenario: "Refund under $50", input_summary: "$40 subtotal", expected_outcome: "Instant refund", verification_kind: "deterministic" },
    { ...(extra || {}), scenario: "Refund over $50", input_summary: "$75 subtotal", expected_outcome: secondOutcome, verification_kind: "deterministic" },
  ],
  removed_example_ids: [], preserved_behaviors: ["Cross-tenant lookups return 404."],
});

const page = await openPage(origin, { link: input.link });
await until(() => editor().dataset.changeId === "chg-rev-ui", "the change did not open");
const r1 = await read();
expect(editor().dataset.requestRevision === r1.request_revision && !prior(), "expected clean R1 mounted without #surla-prior-revision");

// 1. A clean editor shows a new revision with the previous one beside it.
const r2 = await revise(r1, "Refunds up to $50 are instant; refunds over $50 require approval.", twoExamples("Manager approval"));
await until(() => editor().dataset.requestRevision === r2.request_revision, "R2 did not replace the clean R1");
const onR2 = prior();
expect(onR2 && onR2.textContent.includes("Previous Saved Revision") && onR2.textContent.includes(r1.request_revision) && onR2.textContent.includes(r1.summary) && onR2.textContent.includes("check_01") && onR2.textContent.includes("Kept") && !preserved() && !page.$("#surla-openui-workspace"), "expected R2 to show R1 with check_01 Kept and no preserved draft or OpenUI workspace");

// 1b. A same-revision update (a new decision view) applies in place, keeping drafts and the previous revision.
page.type(outcome("check_02"), "Draft edit kept across same-hash OpenUI update");
const withA = await revise(r2, "Refunds up to $50 are instant; refunds over $50 require approval.", { ...twoExamples("Manager approval", { prior_id: "check_02" }), ui_composition: 'root = DecisionWorkspace("Refund Studio V1", [callout])\ncallout = ImpactCallout("info", "Illustration V1", "Initial OpenUI callout.")' });
expect(withA.request_revision === r2.request_revision, "a presentation-only revision changed request_revision");
await until(() => page.$("#surla-openui-workspace") && page.text("#surla-openui-workspace").includes("Refund Studio V1"), "the same-revision update did not render in place");
expect(page.text("#surla-openui-workspace").includes("Illustration V1") && outcome("check_02").value === "Draft edit kept across same-hash OpenUI update" && prior() && prior().textContent.includes(r1.request_revision) && !page.$("#surla-incoming-change-card"), "the same-revision update lost the draft or the previous revision");
await revise(r2, "Refunds up to $50 are instant; refunds over $50 require approval.", { ...twoExamples("Manager approval", { prior_id: "check_02" }), ui_composition: 'root = DecisionWorkspace("Refund Studio V2", [callout])\ncallout = ImpactCallout("warning", "Illustration V2", "Updated OpenUI callout.")' });
await until(() => page.$("#surla-openui-workspace") && page.text("#surla-openui-workspace").includes("Refund Studio V2"), "the second same-revision update did not render");
expect(!page.text("#surla-openui-workspace").includes("Refund Studio V1") && outcome("check_02").value === "Draft edit kept across same-hash OpenUI update" && prior() && prior().textContent.includes(r1.request_revision), "the second update lost the draft or the previous revision");
page.$("#surla-discard-draft").click();

// 2. Edited and removed examples are labeled against the previous revision.
const r3 = await revise(await read(), "Refunds up to $50 (subtotal only) are instant and emit a webhook.", {
  ambiguities: [{ question: "Should webhook retries use exponential backoff?", options: ["exponential_backoff", "fixed_delay"] }],
  examples: [{ prior_id: "check_01", scenario: "Refund under $50", input_summary: "$40 subtotal", expected_outcome: "Instant refund + webhook", verification_kind: "deterministic" }],
  removed_example_ids: ["check_02"], preserved_behaviors: ["Cross-tenant lookups return 404.", "The $50 limit applies to subtotal_only."],
});
await until(() => editor().dataset.requestRevision === r3.request_revision, "R3 did not replace the clean R2");
const onR3 = prior();
expect(onR3 && onR3.textContent.includes(r2.request_revision) && onR3.textContent.includes(r2.summary) && onR3.textContent.includes("check_01") && onR3.textContent.includes("Edited") && onR3.textContent.includes("check_02") && onR3.textContent.includes("Removed") && !preserved(), "expected R3 to show R2 with check_01 Edited and check_02 Removed");

// 3. An unsaved answer waits beside an incoming revision and is preserved with its question.
editor().querySelector('[data-ambiguity-id="chg-rev-ui-ambiguity-01"][data-option="fixed_delay"]').click();
const r4 = await revise(r3, "Refunds up to $50 are instant; refunds over $50 require approval and emit a webhook.", twoExamples("Manager approval"));
await until(() => page.$("#surla-replace-incoming-card"), "R4 did not wait beside the unsaved answer");
expect(editor().dataset.requestRevision === r3.request_revision, "the dirty R3 did not stay mounted");
page.$("#surla-replace-incoming-card").click();
const answerLine = preserved() ? preserved().textContent : "";
expect(editor().dataset.requestRevision === r4.request_revision && prior() && prior().textContent.includes(r3.request_revision) && answerLine.includes("[" + r3.request_revision + "] chg-rev-ui-ambiguity-01 (Should webhook retries use exponential backoff?)=fixed_delay") && !answerLine.includes("check_01") && !answerLine.includes("check_02"), "expected only the changed answer, with its question, in #surla-preserved-draft: " + answerLine);
page.$("#surla-discard-draft").click();

// 3b. Only a changed example is preserved, and successive revisions keep earlier lines.
editor().querySelector('[data-ambiguity-id="chg-rev-ui-ambiguity-01"][data-option="subtotal_only"]').click();
page.$("#surla-confirm-submit").click();
await until(() => editor().textContent.includes("Confirmed Change Decision"), "R4 did not confirm");
const r4c = await read();
page.type(outcome("check_02"), "VP finance approval over $50");
const r5 = await revise(r4c, "Refunds over $50 require approval.", twoExamples("Manager approval", { prior_id: "check_02" }));
await until(() => page.$("#surla-replace-incoming-card"), "R5 did not wait beside the edited example");
page.$("#surla-replace-incoming-card").click();
const oneExample = preserved() ? preserved().textContent : "";
expect(oneExample.includes("[" + r4c.request_revision + "] check_02: Refund over $50 | $75 subtotal -> VP finance approval over $50 [deterministic]") && !oneExample.includes("chg-rev-ui-ambiguity-01") && !oneExample.includes("subtotal_only") && !oneExample.includes("check_01"), "expected #surla-preserved-draft to include only the changed check_02: " + oneExample);
page.type(outcome("check_01"), "");
const r6 = await revise(r5, "Refunds over $50 require a manager.", twoExamples("Manager approval", { prior_id: "check_02" }));
await until(() => page.$("#surla-replace-incoming-card"), "R6 did not wait beside the cleared example");
page.$("#surla-replace-incoming-card").click();
const successive = preserved() ? preserved().textContent : "";
expect(editor().dataset.requestRevision === r6.request_revision && successive.includes("[" + r4c.request_revision + "] check_02: Refund over $50 | $75 subtotal -> VP finance approval over $50 [deterministic]") && successive.includes("[" + r5.request_revision + "] check_01: Refund under $50 | $40 subtotal ->  [deterministic]") && !successive.includes("chg-rev-ui-ambiguity-01"), "successive revisions did not keep both changed lines: " + successive);
page.$("#surla-discard-draft").click();

// 4. A revision saved while the page's own confirmation awaits its answer appears after the answer, with the confirmed card as its previous revision.
await control(origin, "answer-later", { key: "POST /changes/confirm" });
editor().querySelector('[data-ambiguity-id="chg-rev-ui-ambiguity-01"][data-option="subtotal_only"]').click();
page.$("#surla-confirm-submit").click();
await until(() => page.sent("POST", "/changes/confirm").length === 2, "the confirmation was not sent");
let r6c = await read();
for (let tries = 0; r6c.status !== "confirmed" && tries < 200; tries++) {
  await sleep(20);
  r6c = await read();
}
expect(r6c.status === "confirmed", "the held confirmation was not saved");
const r7 = await revise(r6c, "Refunds over $50 require a manager and a receipt.", twoExamples("Manager approval", { prior_id: "check_02" }));
await sleep(300);
expect(!page.$("#surla-incoming-change-card"), "a save during the page's own confirmation was held as incoming");
await control(origin, "release", { key: "POST /changes/confirm" });
await until(() => editor().dataset.requestRevision === r7.request_revision, "R7 did not appear after the confirmation's answer");
expect(prior() && prior().textContent.includes(r6c.request_revision), "expected R7's previous revision to be the confirmed card");

// Opening a different change shows no previous revision.
await api(origin, "POST", "/changes/propose", { change_id: "chg-other", prompt: "Show the order's shipping carrier on its receipt.", analysis: { summary: "Receipts name the carrier that ships the order.", ambiguities: [], examples: [{ scenario: "Shipped order receipt", input_summary: "Order shipped with a ground carrier", expected_outcome: "Receipt names the ground carrier", verification_kind: "deterministic" }], preserved_behaviors: [] } }, input.guide);
page.$("#surla-nav-changes").click();
await until(() => page.$("#surla-open-change-chg-other"), "the other change did not appear");
page.$("#surla-open-change-chg-other").click();
await until(() => editor().dataset.changeId === "chg-other", "the other change did not open");
expect(!prior(), "expected switching to a different change_id to clear #surla-prior-revision");
page.close();
`)
}

// TestCheckpointUIApprovalStates opens approved and unapproved cards and
// reads the approved one again as the promotion target's inputs change,
// fail, disappear and return, checking each of the five approval states.
func TestCheckpointUIApprovalStates(t *testing.T) {
	inputs := PromotionInputs{
		SuiteID: "orders_acceptance", SuiteCommit: strings.Repeat("3", 40),
		Environment: "sim", Artifacts: map[string]string{"orders": strings.Repeat("b", 64)},
	}
	target := &pageTarget{inputs: inputs}
	cp := pageControlPlane(t, Options{ResolvePromotionTarget: target.resolve})
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	user := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: pageUserID, Kind: CallerUser})
	confirm := func(changeID string) *gensurla.ChangeCardResult {
		proposed := mustPropose(t, cp, guide, changeID, "Refunds over $50 require approval.", refundAnalysis("Refunds over $50 require approval."))
		confirmed, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
			ChangeID: changeID, RequestRevision: proposed.RequestRevision, Answers: map[string]string{}, Examples: proposed.Examples,
		})
		require.NoError(t, err)
		return confirmed
	}
	confirm("chg-unapproved")
	approved := confirm("chg-approved")
	const commit = "2222222222222222222222222222222222222222"
	report := passingReport(t, approved.RequestRevision, commit, inputs)
	target.update(func(p *pageTarget) { p.report = report })
	_, err := cp.ApproveMerge(user, &gensurla.ApproveMergePayload{
		ChangeID: approved.ChangeID, RequestRevision: approved.RequestRevision, CandidateCommit: commit, ReportDigest: report.Digest,
	})
	require.NoError(t, err)
	ps := startPageServer(t, cp)
	ps.act("inputs-changed", func() {
		target.update(func(p *pageTarget) {
			p.inputs = PromotionInputs{SuiteID: inputs.SuiteID, SuiteCommit: inputs.SuiteCommit, Environment: inputs.Environment, Artifacts: map[string]string{"orders": strings.Repeat("c", 64)}}
		})
	})
	ps.act("inputs-unreadable", func() {
		target.update(func(p *pageTarget) {
			p.inputs = inputs
			p.inputsErr = errors.New("open /var/lib/surla/releases/orders: no such file or directory")
		})
	})
	ps.act("absent", func() { target.update(func(p *pageTarget) { p.absent = true }) })
	ps.act("restored", func() { target.update(func(p *pageTarget) { p.absent, p.inputsErr = false, nil }) })
	runPageScript(t, ps, nil, `
const page = await openPage(origin, { link: input.link });
const editor = () => page.$("#surla-cp1-editor");
const open = async (changeId) => {
  page.$("#surla-nav-changes").click();
  await until(() => page.$("#surla-open-change-" + changeId) && !page.$("#surla-open-change-" + changeId).disabled, "the list does not offer " + changeId);
  const reads = page.sent("GET", "/changes/" + changeId).length;
  page.$("#surla-open-change-" + changeId).click();
  await until(() => editor().dataset.changeId === changeId && page.sent("GET", "/changes/" + changeId).length > reads && !page.$("#surla-confirm-submit").disabled, changeId + " did not open");
};
const refresh = async () => {
  const reads = page.sent("GET", "/changes/chg-approved").length;
  page.$("#surla-cp5-refresh-approval").click();
  await until(() => page.sent("GET", "/changes/chg-approved").length > reads && !page.$("#surla-confirm-submit").disabled, "checking the approval did not finish");
};
await until(() => page.$("#surla-change-list"), "the page did not list the changes");

// State 1: no record.
await open("chg-unapproved");
expect(!page.$("#surla-cp5-approval") && !page.$("#surla-cp5-unverified-approval"), "State 1 (no record) must not render an approval");

// State 2: a current approval.
await open("chg-approved");
expect(page.$("#surla-cp5-approval") && !page.$("#surla-cp5-unverified-approval"), "State 2 (current) must render #surla-cp5-approval only");

// State 3: changed inputs hide the approval.
await control(origin, "act", { key: "inputs-changed" });
await refresh();
expect(!page.$("#surla-cp5-approval") && !page.$("#surla-cp5-unverified-approval"), "State 3 (changed inputs) must render neither approval");

// State 4: inputs the target cannot report give an honest, path-free unverified approval.
await control(origin, "act", { key: "inputs-unreadable" });
await open("chg-unapproved");
await open("chg-approved");
const reason = page.text("#surla-cp5-unverified-approval-reason");
expect(!page.$("#surla-cp5-approval") && page.$("#surla-cp5-unverified-approval") && reason.includes("could not report its current inputs") && !reason.includes("/var/lib/surla") && page.$("#surla-cp5-unverified-approval-details") && page.$("#surla-cp5-refresh-approval"), "State 4 (unavailable target) did not render an honest path-free #surla-cp5-unverified-approval: " + reason);

// State 5: no promotion target, then recovery through the check control.
await control(origin, "act", { key: "absent" });
await refresh();
expect(page.text("#surla-cp5-unverified-approval-reason").includes("no promotion target is configured") && page.$("#surla-cp5-refresh-approval"), "State 5 (absent target) did not render its reason and the check control");
await control(origin, "act", { key: "restored" });
await refresh();
expect(page.$("#surla-cp5-approval") && !page.$("#surla-cp5-unverified-approval"), "checking after the target recovered did not show #surla-cp5-approval");
page.close();
`)
}
