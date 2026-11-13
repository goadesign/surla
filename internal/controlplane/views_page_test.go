// This file drives the views page through the requests it sends to the real
// generated HTTP API: finding work, opening and confirming a change, staying
// current while the user types, and the diagnostics an idle page leaves.
package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// viewsPageScriptHelpers defines JavaScript helpers the page tests share:
// proposal payloads and the guide's direct calls.
const viewsPageScriptHelpers = `
function analysis(summary, outcome) {
  return {
    summary,
    ambiguities: [{ question: "Does the $50 limit include tax?", options: ["subtotal_only", "total_with_tax"] }],
    examples: [{ scenario: "Refund under $50", input_summary: "$40 subtotal", expected_outcome: outcome, verification_kind: "deterministic" }],
    preserved_behaviors: ["Cross-tenant lookups return 404."],
  };
}
async function propose(changeId, summary, outcome) {
  const out = await api(origin, "POST", "/changes/propose", { change_id: changeId, prompt: summary, analysis: analysis(summary, outcome) }, input.guide);
  expect(out.status === 200, "guide could not propose " + changeId + ": " + JSON.stringify(out));
  return out.body;
}
async function revise(card, summary, outcome) {
  const out = await api(origin, "POST", "/changes/revise", {
    change_id: card.change_id, request_revision: card.request_revision, prompt: summary,
    analysis: { ...analysis(summary, outcome), examples: [{ prior_id: card.examples[0].id, scenario: "Refund under $50", input_summary: "$40 subtotal", expected_outcome: outcome, verification_kind: "deterministic" }], removed_example_ids: [] },
  }, input.guide);
  expect(out.status === 200, "guide could not revise " + card.change_id + ": " + JSON.stringify(out));
  return out.body;
}
// checkControl checks that a control is a native, focusable, enabled button
// with an accessible name, so it works from the keyboard and is announced.
function checkControl(page, element, label) {
  expect(element, label + " is missing");
  expect(element.tagName === "BUTTON" && element.type === "button", label + " is not a native button");
  expect(!element.disabled && element.tabIndex >= 0, label + " is not reachable by keyboard");
  const name = (element.getAttribute("aria-label") || element.textContent || "").trim();
  expect(name.length > 0, label + " has no accessible name");
  element.focus();
  expect(page.doc.activeElement === element, label + " does not take keyboard focus");
}
async function readCard(changeId) {
  const out = await api(origin, "GET", "/changes/" + encodeURIComponent(changeId), undefined, input.guide);
  expect(out.status === 200, "guide could not read " + changeId);
  return out.body;
}
`

// TestViewsPageFindsOpensAndConfirmsChanges walks the user's journey: the
// empty project explains how a change appears, a proposed change appears by
// itself, the list orders changes by save time and opens one, and confirming
// records the user's session as who confirmed.
func TestViewsPageFindsOpensAndConfirmsChanges(t *testing.T) {
	cp := pageControlPlane(t, Options{})
	ps := startPageServer(t, cp)
	runPageScript(t, ps, nil, viewsPageScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-change-list-empty"), "empty project did not show its empty state");
expect(page.text("#surla-change-list-empty").includes("Ask the guide to propose a change; it appears here by itself."), "empty state did not say how a change appears");
expect(page.win.location.href === origin + "/views", "the link stayed in the address: " + page.win.location.href);

await propose("chg-first", "Route refunds to a manager.", "Auto-approved");
await until(() => page.$$("#surla-change-list li").length === 1, "a proposed change did not appear without a reload");
await propose("chg-second", "Hold refunds for review.", "Held");
await until(() => page.$$("#surla-change-list li").length === 2, "a second change did not appear");
const ids = page.$$("#surla-change-list li").map((li) => li.dataset.changeId);
expect(JSON.stringify(ids) === JSON.stringify(["chg-second", "chg-first"]), "list is not most recently saved first: " + ids);
expect(page.text("#surla-change-list").includes("Needs confirmation") && page.text("#surla-change-list").includes("Not verified yet"), "list omitted state or verification state");

checkControl(page, page.$("#surla-open-change-chg-first"), "list control");
expect(page.$("#surla-open-change-chg-first").textContent.includes("Route refunds to a manager."), "the list control is not named by its change");
page.$("#surla-open-change-chg-first").click();
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-first", "opening a listed change did not show its card");
page.$('[data-ambiguity-id="chg-first-ambiguity-01"][data-option="subtotal_only"]').click();
checkControl(page, page.$("#surla-confirm-submit"), "confirm control");
await control(origin, "hold", { key: "POST /changes/confirm" });
page.$("#surla-confirm-submit").click();
expect(page.$("#surla-confirm-submit").disabled && page.$("#surla-cp1-form-card").getAttribute("aria-busy") === "true", "confirming showed no busy state");
await control(origin, "release", { key: "POST /changes/confirm" });
await until(() => page.$("#surla-cp1-editor").textContent.includes("Confirmed Change Decision"), "confirming did not show the confirmed card");
const confirmed = await readCard("chg-first");
expect(confirmed.status === "confirmed" && confirmed.confirmed_by === input.user, "the saved change does not record the user as who confirmed: " + confirmed.confirmed_by);
expect(page.doc.activeElement === page.$("#surla-cp1-editor"), "confirming did not move focus to the result");
page.close();
`)
}

// TestViewsPageOpensTheOnlyChangeDirectly opens a project with one change on
// that change rather than on the list.
func TestViewsPageOpensTheOnlyChangeDirectly(t *testing.T) {
	cp := pageControlPlane(t, Options{})
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	mustPropose(t, cp, guide, "chg-only", "Route refunds to a manager.", refundAnalysis("Route refunds to a manager."))
	ps := startPageServer(t, cp)
	runPageScript(t, ps, nil, viewsPageScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-only", "the only change did not open directly");
expect(page.state() === "cp1-change-card" && !page.$("#surla-change-list"), "the page showed the list instead of the only change");
expect(page.$("#surla-nav-changes") && page.$("#surla-nav-cp1").getAttribute("aria-current") === "page", "stage navigation does not offer the list and mark the open stage");
page.$("#surla-nav-changes").click();
expect(page.$$("#surla-change-list li").length === 1 && page.$("#surla-open-change-chg-only").getAttribute("aria-current") === "true", "the list does not mark the open change");
page.close();
`)
}

// TestViewsPageKeepsTypingWhenAnotherCallerSavesARevision checks that a
// revision another caller saves appears without a reload while typed edits,
// the cursor and focus survive, that the page announces it once, and that
// leaving with unsaved edits asks the browser to confirm.
func TestViewsPageKeepsTypingWhenAnotherCallerSavesARevision(t *testing.T) {
	cp := pageControlPlane(t, Options{})
	ps := startPageServer(t, cp)
	runPageScript(t, ps, nil, viewsPageScriptHelpers+`
const card = await propose("chg-live", "Route refunds to a manager.", "Auto-approved");
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-live", "the change did not open");
expect(!page.beforeUnload(), "the page asked to confirm leaving without unsaved edits");
const field = page.$('textarea[data-field="expected_outcome"]');
field.focus();
page.type(field, "Typed but not saved");
field.setSelectionRange(5, 9);
expect(page.beforeUnload(), "leaving with unsaved edits did not ask the browser to confirm");
const announced = [];
new page.win.MutationObserver(() => announced.push(page.status())).observe(page.$("#surla-checkpoint-status"), { childList: true, characterData: true, subtree: true });

await revise(card, "Route refunds over $50 to a manager.", "Manager approval");
await until(() => page.$("#surla-replace-incoming-card"), "a revision saved by another caller did not appear without a reload");
expect(page.$('textarea[data-field="expected_outcome"]') === field && field.value === "Typed but not saved", "the new revision discarded typed text");
expect(page.doc.activeElement === field, "the new revision moved focus");
expect(field.selectionStart === 5 && field.selectionEnd === 9, "the new revision moved the cursor");
expect(page.$("#surla-cp1-editor").dataset.requestRevision === card.request_revision, "the editor left the revision being edited");

const listReads = page.sent("GET", "/changes").length;
await propose("chg-unrelated", "Show carriers on receipts.", "Carrier shown");
await until(() => page.sent("GET", "/changes").length > listReads, "another saved change did not reach the page");
await sleep(100);
const told = announced.filter((text) => text.includes("newer revision"));
expect(told.length === 1, "the new revision was announced " + told.length + " times: " + JSON.stringify(announced));
expect(page.doc.activeElement === field && field.value === "Typed but not saved", "a later list change disturbed the editor");
expect(page.sent("GET", "/changes/chg-unrelated").length === 0, "the page read a change the list did not ask it to");
page.close();
`)
}

// TestViewsPageIdleAddsNothingToDiagnostics keeps a page open with nothing
// changing across several waiting reads and checks that the diagnostics
// history did not grow, while each waiting read was still traced.
func TestViewsPageIdleAddsNothingToDiagnostics(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	cp := pageControlPlane(t, Options{TracerProvider: provider})
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	mustPropose(t, cp, guide, "chg-idle", "Route refunds to a manager.", refundAnalysis("Route refunds to a manager."))
	ps := startPageServer(t, cp)
	runPageScript(t, ps, nil, viewsPageScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-idle", "the change did not open");
await until(() => page.sent("GET", "/change-updates").length >= 1, "the page did not keep a waiting read open");
const history = async () => {
  const out = await api(origin, "GET", "/traces?status=all&limit=100", undefined, input.guide);
  expect(out.status === 200, "diagnostics could not be read: " + JSON.stringify(out));
  return out.body.spans;
};
const before = await history();
const waits = page.sent("GET", "/change-updates").length;
await until(() => page.sent("GET", "/change-updates").length >= waits + 3, "the idle page stopped waiting", 15000);
const after = await history();
const added = after.slice(0, after.length - before.length).map((span) => span.name);
expect(JSON.stringify(added) === JSON.stringify(["surla.controlplane.list_traces"]), "an idle page added diagnostics: " + JSON.stringify(added));
expect(!after.some((span) => span.name.includes("wait_for_changes")), "a waiting read was saved in the diagnostics history");
expect(page.sent("GET", "/changes").length === 1 && page.sent("GET", "/changes/chg-idle").length === 1, "an idle page read the list or the change again");
page.setHidden(true);
const hiddenWaits = page.sent("GET", "/change-updates").length;
await sleep(1200);
expect(page.sent("GET", "/change-updates").length === hiddenWaits, "a hidden tab kept asking for changes");
page.setHidden(false);
await until(() => page.sent("GET", "/change-updates").length > hiddenWaits, "showing the tab again did not resume waiting");
page.close();
`)
	var waits int
	for _, span := range exporter.GetSpans() {
		if span.Name == waitForChangesSpan {
			waits++
		}
	}
	assert.GreaterOrEqual(t, waits, 3, "each waiting read is traced like any operation")
}

// TestViewsPageApprovesAPassedResult confirms a change, opens its passed
// verification run, and approves it with a keyboard-operable control that
// shows a busy state and moves focus to the recorded approval. It also
// shows why approval is unavailable without promotion settings, and sends a
// stale approval through the editor's revision recovery.
func TestViewsPageApprovesAPassedResult(t *testing.T) {
	inputs := PromotionInputs{
		SuiteID: "orders_acceptance", SuiteCommit: strings.Repeat("2", 40),
		Environment: "sim", Artifacts: map[string]string{"orders": strings.Repeat("a", 64)},
	}
	const commit = "1111111111111111111111111111111111111111"
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	user := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: pageUserID, Kind: CallerUser})

	t.Run("approves", func(t *testing.T) {
		target := &pageTarget{inputs: inputs}
		cp := pageControlPlane(t, Options{ResolvePromotionTarget: target.resolve})
		proposed := mustPropose(t, cp, guide, "chg-approve", "Route refunds to a manager.", refundAnalysis("Route refunds to a manager."))
		confirmed, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
			ChangeID: proposed.ChangeID, RequestRevision: proposed.RequestRevision,
			Answers: map[string]string{}, Examples: proposed.Examples,
		})
		require.NoError(t, err)
		report := passingReport(t, confirmed.RequestRevision, commit, inputs)
		target.update(func(p *pageTarget) { p.report = report })
		svc := newPageVerifications(cp)
		run := passedRun(confirmed, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", commit, report.Digest)
		svc.set(run)
		svc.list(confirmed.ChangeID, run.RunID)
		ps := startPageServer(t, svc)
		runPageScript(t, ps, map[string]any{"run": run.RunID, "digest": report.Digest, "revision": confirmed.RequestRevision}, viewsPageScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-approve", "the change did not open");
page.$("#surla-nav-cp4").click();
await until(() => page.$("#surla-cp5-approve"), "a passed result offered no approval control");
expect(page.text("#surla-result-view").includes("Verification passed") && page.text("#surla-result-view").includes("holdout-1"), "the run's result is not shown");
checkControl(page, page.$("#surla-cp5-approve"), "approve control");
await control(origin, "hold", { key: "POST /changes/approve" });
page.$("#surla-cp5-approve").click();
expect(page.$("#surla-cp5-approve").disabled && page.$("#surla-cp5-approve").textContent === "Approving..." && page.$("#surla-cp5-approve-region").getAttribute("aria-busy") === "true", "approving showed no busy state");
const sent = page.sent("POST", "/changes/approve");
expect(sent.length === 1 && JSON.stringify(sent[0].body) === JSON.stringify({ change_id: "chg-approve", request_revision: input.revision, candidate_commit: "1111111111111111111111111111111111111111", report_digest: input.digest }), "approval did not act on the exact revision and result shown: " + JSON.stringify(sent));
await control(origin, "release", { key: "POST /changes/approve" });
await until(() => page.$("#surla-cp5-approval"), "the recorded approval is not shown");
expect(page.doc.activeElement === page.$("#surla-cp5-approval"), "approving did not move focus to the result");
const approval = page.text("#surla-cp5-approval");
expect(approval.includes("Approval recorded") && approval.includes("No merge or deploy side effect") && approval.includes(input.user) && approval.includes(input.digest) && approval.includes("orders_acceptance"), "the approval panel lacks its record: " + approval);
expect(!page.$("#surla-cp5-approval-dirty-warning"), "a clean approved card warned about unsaved edits");
const outcome = page.$('textarea[data-field="expected_outcome"]');
const saved = outcome.value;
page.type(outcome, "edited after approval");
expect(page.$("#surla-cp5-approval-dirty-warning"), "unsaved edits on an approved card did not warn that saving clears the approval");
page.type(outcome, saved);
expect(!page.$("#surla-cp5-approval-dirty-warning"), "restoring the saved text did not clear the warning");
const stored = await readCard("chg-approve");
expect(stored.approval && stored.approval.approved_by === input.user && stored.approval.report_digest === input.digest, "the saved change does not record the user's approval");
page.close();
`)
	})

	t.Run("unavailable without promotion settings", func(t *testing.T) {
		target := &pageTarget{inputs: inputs, absent: true}
		cp := pageControlPlane(t, Options{ResolvePromotionTarget: target.resolve})
		proposed := mustPropose(t, cp, guide, "chg-unavailable", "Route refunds to a manager.", refundAnalysis("Route refunds to a manager."))
		confirmed, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
			ChangeID: proposed.ChangeID, RequestRevision: proposed.RequestRevision,
			Answers: map[string]string{}, Examples: proposed.Examples,
		})
		require.NoError(t, err)
		svc := newPageVerifications(cp)
		run := passedRun(confirmed, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", commit, strings.Repeat("3", 64))
		svc.set(run)
		svc.list(confirmed.ChangeID, run.RunID)
		ps := startPageServer(t, svc)
		runPageScript(t, ps, nil, viewsPageScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-unavailable", "the change did not open");
page.$("#surla-nav-cp4").click();
await until(() => page.$("#surla-cp5-approve"), "no approval control");
page.$("#surla-cp5-approve").click();
await until(() => page.$("#surla-cp5-unavailable"), "approval was not shown as unavailable");
const reason = page.text("#surla-cp5-unavailable");
expect(reason.includes("no promotion target is bound") && reason.includes("promotion settings") && reason.includes("surla stop"), "unavailable approval does not name the enabling step: " + reason);
expect(page.doc.activeElement === page.$("#surla-cp5-unavailable"), "the unavailable result did not take focus");
expect(!page.$("#surla-cp5-approve"), "an unavailable approval still offered its control");
page.close();
`)
	})

	t.Run("stale revision uses the editor's recovery", func(t *testing.T) {
		target := &pageTarget{inputs: inputs}
		cp := pageControlPlane(t, Options{ResolvePromotionTarget: target.resolve})
		proposed := mustPropose(t, cp, guide, "chg-stale", "Route refunds to a manager.", refundAnalysis("Route refunds to a manager."))
		confirmed, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
			ChangeID: proposed.ChangeID, RequestRevision: proposed.RequestRevision,
			Answers: map[string]string{}, Examples: proposed.Examples,
		})
		require.NoError(t, err)
		svc := newPageVerifications(cp)
		run := passedRun(confirmed, "cccccccccccccccccccccccccccccccc", commit, strings.Repeat("3", 64))
		svc.set(run)
		svc.list(confirmed.ChangeID, run.RunID)
		ps := startPageServer(t, svc)
		runPageScript(t, ps, nil, viewsPageScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-stale", "the change did not open");
page.$("#surla-nav-cp4").click();
await until(() => page.$("#surla-cp5-approve"), "no approval control");
page.setHidden(true);
await revise(await readCard("chg-stale"), "Route refunds over $50 to a manager.", "Manager approval");
page.$("#surla-cp5-approve").click();
await until(() => page.$("#surla-confirm-error"), "a stale approval did not show the editor's recovery");
expect(page.text("#surla-confirm-error").includes("must be confirmed before approval") && page.$("#surla-load-latest-card"), "stale approval did not show the server's refusal and offer Load Latest Card: " + page.text("#surla-confirm-error"));
expect(page.doc.activeElement === page.$("#surla-confirm-error"), "the refusal did not take focus");
page.$("#surla-load-latest-card").click();
await until(() => page.$("#surla-cp1-editor").dataset.requestRevision !== input.revision && page.$("#surla-prior-revision"), "Load Latest Card did not show the newer revision");
page.close();
`)
	})
}
