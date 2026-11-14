// These served-page tests keep saved verification evidence separate from the
// owner's changing observation of accepted tests. Runs and checker results are
// saved fixtures; neither candidate code nor a model executes.
package controlplane

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selectedInputsListRead captures a real owner answer before the HTTP harness
// delays its delivery. Other operations continue to reach the real owner.
type selectedInputsListRead struct {
	*ControlPlane
	// armed selects one list answer after its delivery gate is installed.
	armed atomic.Bool
	// captured proves the old positive observation preceded new acceptance.
	captured chan *gensurla.VerificationListResult
}

// TestViewsPageAcceptedTestsRefreshOpenedBuildAndSelectedProof follows a second
// accepted test commit without changing views or reloading the page. Returning
// to the recorded commit restores the owner match in both opened surfaces.
func TestViewsPageAcceptedTestsRefreshOpenedBuildAndSelectedProof(t *testing.T) {
	cp, card, runsDir := selectedInputsFixture(t)
	runID := strings.Repeat("1", 32)
	writeListedRun(t, runsDir, pageOwner, card, runID,
		time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC), true)
	setListedRunSource(t, runsDir, runID, verify.TestsWritten)
	ps := startPageServer(t, cp)
	selectedInputsActions(t, ps, cp, card.ChangeID)
	runPageScript(t, ps, map[string]any{"run": runID}, `
const page = await openPage(origin, { link: input.link });
const review = () => page.$("#surla-build-review-check");
const currentBuild = () => review()?.classList.contains("shadcn-btn-primary") && !review().disabled;
await until(currentBuild, "the opened Build did not offer its current passing check");
expect(review().textContent === "Review and approve", "the current check has no approval decision");
expect(page.$$("#surla-build-section .shadcn-btn-primary").length === 1, "Build has competing primary actions");
const beforeReplacement = page.requests.length;
await control(origin, "act", { key: "accept-t2" });
await until(() => page.requests.slice(beforeReplacement).some((r) => r.path === "/verifications/" + input.run && r.status === 200 && JSON.parse(r.answer).inputs_current === false), "the opened Build did not read the second acceptance's negative match");
await until(() => review() && !review().disabled && !currentBuild() && review().textContent === "Review saved check", "a second accepted commit left the opened Build claiming current proof");
expect(page.$("#surla-cp1-editor") && !page.$("#surla-cp4-verification"), "a background acceptance navigated away from Build");
await control(origin, "act", { key: "accept-t1" });
await until(currentBuild, "reaccepting the recorded tests did not restore Build's decision");
review().click();
await until(() => page.$("#surla-result-view").dataset.runId === input.run && page.$("#surla-cp5-approve") && !page.$("#surla-cp5-approve").disabled, "Review and approve did not open the exact owner-supplied saved run");
const verdict = page.text("#surla-cp4-title");
const details = page.text("#surla-cp4-details");
expect(verdict === "The check passed: 2 of 2 scenarios.", "the saved passing verdict is missing");
await control(origin, "act", { key: "accept-t2" });
await until(() => page.text("#surla-cp4-inputs").includes("does not match") && page.$("#surla-cp5-approve").disabled, "the selected terminal run did not follow second acceptance");
expect(page.$("#surla-result-view").dataset.runId === input.run && page.text("#surla-cp4-title") === verdict && page.text("#surla-cp4-details") === details, "freshness changed the saved report or selection");
page.$("#surla-cp5-approve").click();
expect(page.sent("POST", "/changes/approve").length === 0, "a stale selected report submitted approval");
await control(origin, "act", { key: "accept-t1" });
await until(() => page.text("#surla-cp4-inputs").includes("matches the agreed revision") && !page.$("#surla-cp5-approve").disabled, "the selected report did not regain its current owner match");
expect(page.text("#surla-cp4-title") === verdict && page.text("#surla-cp4-details") === details, "restoring freshness replaced historical evidence");
expect(page.sent("POST", "/changes/verify").length === 0, "reading saved proof started verification");
page.close();
`)
}

// TestViewsPageRefreshesAnOlderSelectionWhenLatestStaysStale prevents the
// newest run's unchanged false match from masking changes to a selected older
// run. The list remains owner-ordered; selection stays on the older identity.
func TestViewsPageRefreshesAnOlderSelectionWhenLatestStaysStale(t *testing.T) {
	cp, card, runsDir := selectedInputsFixture(t)
	olderID, latestID := strings.Repeat("1", 32), strings.Repeat("2", 32)
	writeListedRun(t, runsDir, pageOwner, card, olderID,
		time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC), true)
	setListedRunSource(t, runsDir, olderID, verify.TestsWritten)
	writeSelectedInputsRun(t, runsDir, card, latestID, strings.Repeat("3", 40))
	ps := startPageServer(t, cp)
	selectedInputsActions(t, ps, cp, card.ChangeID)
	ps.act("accept-t3", func() {
		saveListedTestState(t, cp, card.ChangeID, testsAccepted, strings.Repeat("4", 40))
	})
	runPageScript(t, ps, map[string]any{"older": olderID, "latest": latestID}, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-review-check") && !page.$("#surla-build-review-check").disabled, "the latest saved check did not reach Build");
expect(!page.$("#surla-build-review-check").classList.contains("shadcn-btn-primary"), "the latest stale run was presented as an approval decision");
page.$("#surla-build-review-check").click();
await until(() => page.$("#surla-result-view").dataset.runId === input.latest && page.$("#surla-cp4-run-" + input.older) && page.text("#surla-cp4-inputs").includes("does not match"), "Build did not open the exact latest identity with its stale observation");
const listPath = "/changes/chg-selected-inputs/verifications";
await control(origin, "answer-later", { key: "GET " + listPath });
const listsBeforeSelection = page.sent("GET", listPath).length;
page.$("#surla-load-verifications").click();
await until(() => page.sent("GET", listPath).length > listsBeforeSelection && page.$("#surla-load-verifications").disabled, "the selection-preservation list did not remain pending");
page.$("#surla-cp4-run-" + input.older).click();
await until(() => page.$("#surla-result-view").dataset.runId === input.older, "the older run could not be selected during the pending list");
await control(origin, "release", { key: "GET " + listPath });
await until(() => page.$("#surla-result-view").dataset.runId === input.older && page.$("#surla-cp5-approve") && !page.$("#surla-cp5-approve").disabled, "the older matching run inherited the latest run's false observation");
const oldDetails = page.text("#surla-cp4-details");
const latestPath = "/verifications/" + input.latest;
const olderPath = "/verifications/" + input.older;
const before = page.requests.length;
await control(origin, "act", { key: "accept-t3" });
await until(() => page.$("#surla-result-view").dataset.runId === input.older && page.text("#surla-cp4-inputs").includes("does not match") && page.$("#surla-cp5-approve").disabled, "changing T1 to T3 did not invalidate the independently selected older run");
await until(() => page.requests.slice(before).some((r) => r.path === latestPath && r.status === 200 && JSON.parse(r.answer).inputs_current === false) && page.requests.slice(before).some((r) => r.path === olderPath && r.status === 200 && JSON.parse(r.answer).inputs_current === false), "the owner did not refresh both exact run identities");
expect(page.text("#surla-cp4-details") === oldDetails && page.text("#surla-cp4-title") === "The check passed: 2 of 2 scenarios.", "invalidating the older selection replaced its report");
await control(origin, "act", { key: "accept-t1" });
await until(() => page.$("#surla-result-view").dataset.runId === input.older && !page.$("#surla-cp5-approve").disabled, "the older run could not recover independently of the still-stale latest run");
page.$("#surla-cp4-run-" + input.latest).click();
await until(() => page.$("#surla-result-view").dataset.runId === input.latest && page.text("#surla-cp4-inputs").includes("does not match") && page.$("#surla-cp5-approve").disabled, "the latest run acquired the older run's positive match");
expect(page.text("#surla-cp4-details").includes("3333333333333333333333333333333333333333"), "the latest run lost its distinct saved tests commit");
expect(page.sent("POST", "/changes/approve").length === 0, "selecting historical evidence submitted approval");
page.close();
`)
}

// TestViewsPageAnOlderPositiveListCannotRestoreApproval captures a positive
// owner list, accepts different tests, and delivers that answer only after a
// negative current Get. The completed report remains visible throughout.
func TestViewsPageAnOlderPositiveListCannotRestoreApproval(t *testing.T) {
	cp, card, runsDir := selectedInputsFixture(t)
	runID := strings.Repeat("1", 32)
	writeListedRun(t, runsDir, pageOwner, card, runID,
		time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC), true)
	setListedRunSource(t, runsDir, runID, verify.TestsWritten)
	service := &selectedInputsListRead{ControlPlane: cp, captured: make(chan *gensurla.VerificationListResult, 1)}
	ps := startPageServer(t, service)
	selectedInputsActions(t, ps, cp, card.ChangeID)
	ps.act("hold-old-list", func() {
		ps.gates["GET /changes/"+card.ChangeID+"/verifications"] = &pageGate{
			open: make(chan struct{}), answered: true,
		}
		service.armed.Store(true)
	})
	ps.act("await-old-list", func() {
		select {
		case captured := <-service.captured:
			if assert.Len(t, captured.Runs, 1) {
				assert.Equal(t, runID, captured.Runs[0].RunID)
				assert.True(t, captured.Runs[0].InputsCurrent, "capture must precede the second accepted commit")
			}
		case <-time.After(10 * time.Second):
			t.Error("the owner did not capture the held list answer")
		}
	})
	runPageScript(t, ps, map[string]any{"run": runID}, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-review-check")?.classList.contains("shadcn-btn-primary") && !page.$("#surla-build-review-check").disabled, "the current saved run did not load");
page.$("#surla-build-review-check").click();
await until(() => page.$("#surla-cp5-approve") && !page.$("#surla-cp5-approve").disabled && !page.$("#surla-load-verifications").disabled, "initial proof reads did not settle");
const verdict = page.text("#surla-cp4-title");
const listPath = "/changes/chg-selected-inputs/verifications";
const runPath = "/verifications/" + input.run;
await control(origin, "act", { key: "hold-old-list" });
const previousLists = page.sent("GET", listPath).length;
page.$("#surla-load-verifications").click();
await until(() => page.sent("GET", listPath).length > previousLists && page.$("#surla-load-verifications").disabled && page.$("#surla-cp5-approve").disabled, "the held list did not make freshness unknown");
const old = page.sent("GET", listPath)[previousLists];
await control(origin, "act", { key: "await-old-list" });
expect(old.status === undefined, "the positive owner answer escaped its delivery gate");
const before = page.requests.length;
await control(origin, "act", { key: "accept-t2" });
await until(() => page.requests.slice(before).some((r) => r.path === runPath && r.status === 200 && JSON.parse(r.answer).inputs_current === false), "the second acceptance did not produce a newer negative owner answer");
expect(old.status === undefined && page.$("#surla-cp5-approve").disabled, "approval became available before the old answer was released");
const regressions = [];
const observe = () => {
  if (page.$("#surla-cp5-approve") && !page.$("#surla-cp5-approve").disabled) regressions.push("approval enabled");
  if (page.text("#surla-cp4-title") !== verdict) regressions.push("saved verdict changed");
};
const observer = new page.win.MutationObserver(observe);
observer.observe(page.$("#surla-result-view"), { childList: true, attributes: true, subtree: true });
await control(origin, "release", { key: "GET " + listPath });
await until(() => old.status === 200 && !page.$("#surla-load-verifications").disabled && page.text("#surla-cp4-inputs").includes("does not match"), "the delayed list did not settle behind the newer observation");
expect(JSON.parse(old.answer).runs[0].inputs_current === true, "the held response did not actually carry the older positive match");
await sleep(0);
observe();
observer.disconnect();
expect(regressions.length === 0, "a delayed positive list restored stale approval: " + JSON.stringify(regressions));
page.$("#surla-cp5-approve").click();
expect(page.sent("POST", "/changes/approve").length === 0, "the delayed positive answer enabled submission");
expect(page.$("#surla-result-view").dataset.runId === input.run, "the delayed list changed the selected identity");
page.close();
`)
}

// TestViewsPagePendingAndFailedFreshnessPreservesReportAndDraft exercises
// pending, failed and retried list reads on an already passed result. Only
// freshness changes; the saved verdict and unsaved agreement input survive.
func TestViewsPagePendingAndFailedFreshnessPreservesReportAndDraft(t *testing.T) {
	cp, card, runsDir := selectedInputsFixture(t)
	runID := strings.Repeat("1", 32)
	writeListedRun(t, runsDir, pageOwner, card, runID,
		time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC), true)
	setListedRunSource(t, runsDir, runID, verify.TestsWritten)
	ps := startPageServer(t, cp)
	runPageScript(t, ps, map[string]any{"run": runID}, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-review-check")?.classList.contains("shadcn-btn-primary") && !page.$("#surla-build-review-check").disabled, "the saved passing check did not load");
page.$("#surla-refine-agreement").open = true;
const draft = page.$('textarea[data-field="expected_outcome"]');
page.type(draft, "Keep this unsaved agreement outcome");
page.$("#surla-build-review-check").click();
await until(() => page.$("#surla-result-view").dataset.runId === input.run && page.$("#surla-cp5-approve") && !page.$("#surla-cp5-approve").disabled && !page.$("#surla-load-verifications").disabled, "the selected passing proof did not settle");
const verdict = page.text("#surla-cp4-title");
const details = page.text("#surla-cp4-details");
const preserved = () => page.text("#surla-cp4-title") === verdict && page.text("#surla-cp4-details") === details && page.$('textarea[data-field="expected_outcome"]') === draft && draft.value === "Keep this unsaved agreement outcome";
const path = "/changes/chg-selected-inputs/verifications";
await control(origin, "reply", { key: "GET " + path, status: 503, body: { name: "internal", message: "The saved run could not be read." } });
await control(origin, "hold", { key: "GET " + path });
const before = page.sent("GET", path).length;
page.$("#surla-load-verifications").click();
await until(() => page.sent("GET", path).length > before && page.$("#surla-load-verifications").disabled && page.$("#surla-cp5-approve").disabled, "pending freshness retained enabled approval");
expect(page.text("#surla-cp4-inputs").includes("not known") && preserved(), "a pending freshness read lost saved evidence or the unsaved draft");
page.$("#surla-cp5-approve").click();
expect(page.sent("POST", "/changes/approve").length === 0, "pending freshness submitted approval");
await control(origin, "release", { key: "GET " + path });
await until(() => page.$("#surla-cp4-list-error") && !page.$("#surla-load-verifications").disabled, "the failed freshness read offered no recovery");
expect(page.$("#surla-cp5-approve").disabled && page.text("#surla-cp4-inputs").includes("not known") && preserved(), "a failed read restored approval or discarded evidence/draft");
page.$("#surla-load-verifications").click();
await until(() => !page.$("#surla-cp4-list-error") && !page.$("#surla-cp5-approve").disabled, "a successful retry did not restore current-input approval");
expect(preserved(), "retry changed the saved verdict or unsaved draft");

// A delayed nonterminal snapshot may carry a newer freshness observation,
// but it cannot replace a terminal report already received for this run.
const snapshot = JSON.parse(page.sent("GET", path).findLast((r) => r.status === 200).answer);
snapshot.runs[0].state = { type: "running", value: { started_at: snapshot.runs[0].accepted_at } };
await control(origin, "reply", { key: "GET " + path, status: 200, body: snapshot });
const previous = page.sent("GET", path).length;
page.$("#surla-load-verifications").click();
await until(() => page.sent("GET", path).length > previous && page.sent("GET", path).at(-1).status === 200 && !page.$("#surla-load-verifications").disabled, "the delayed running snapshot did not settle");
expect(preserved() && !page.$("#surla-cp5-approve").disabled, "a running snapshot regressed a terminal saved report");
expect(page.sent("POST", "/changes/approve").length === 0 && page.sent("POST", "/changes/confirm").length === 0, "freshness recovery submitted unsaved work");
page.close();
`)
}

// TestViewsPageWaitOutageDisablesRenderedApprovalUntilSameVersionRecovers
// exercises recovery without a changed owner version or another navigation.
// Retained evidence cannot keep an already rendered approval control usable
// while the update channel cannot establish that the page is current.
func TestViewsPageWaitOutageDisablesRenderedApprovalUntilSameVersionRecovers(t *testing.T) {
	cp, card, runsDir := selectedInputsFixture(t)
	runID := strings.Repeat("1", 32)
	writeListedRun(t, runsDir, pageOwner, card, runID,
		time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC), true)
	setListedRunSource(t, runsDir, runID, verify.TestsWritten)
	ps := startPageServer(t, cp)
	runPageScript(t, ps, map[string]any{"run": runID}, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-review-check")?.classList.contains("shadcn-btn-primary") && !page.$("#surla-build-review-check").disabled, "the saved passing check did not load");
page.$("#surla-build-review-check").click();
await until(() => page.$("#surla-result-view").dataset.runId === input.run && page.$("#surla-cp5-approve") && !page.$("#surla-cp5-approve").disabled, "the initial approval did not settle");
const approve = page.$("#surla-cp5-approve");
const verdict = page.text("#surla-cp4-title");
const details = page.text("#surla-cp4-details");
const lists = page.sent("GET", "/changes").length;
const version = JSON.parse(page.sent("GET", "/changes").findLast((r) => r.status === 200).answer).version;
const waits = page.sent("GET", "/change-updates").length;
await control(origin, "reply", { key: "GET /change-updates", status: 503, body: { name: "internal", message: "The update channel is temporarily unavailable." } });
await until(() => page.sent("GET", "/change-updates").some((r) => r.status === 503) && page.status().includes("Lost contact with the Surla server"), "the wait outage was not announced");
expect(page.$("#surla-cp5-approve") === approve && approve.disabled && page.text("#surla-cp4-inputs").includes("not known"), "the outage left the already rendered approval usable");
expect(page.text("#surla-cp4-title") === verdict && page.text("#surla-cp4-details") === details, "the outage discarded saved evidence");
approve.click();
expect(page.sent("POST", "/changes/approve").length === 0, "the unavailable update channel allowed approval submission");
await until(() => page.status() === "This page is up to date with the Surla server again." && !approve.disabled && page.text("#surla-cp4-inputs").includes("matches the agreed revision"), "same-version wait recovery did not restore rendered approval", 15000);
expect(page.sent("GET", "/changes").length === lists, "the recovery depended on a changed version or another list read");
expect(page.sent("GET", "/change-updates").slice(waits).some((r) => r.status === 200 && JSON.parse(r.answer).version === version), "recovery did not observe the unchanged owner version");
expect(page.$("#surla-result-view").dataset.runId === input.run && page.text("#surla-cp4-title") === verdict, "wait recovery changed the selected evidence");
page.close();
`)
}

// TestViewsPageBackgroundCardReadRequiresRecoveryBeforeApproval keeps a
// terminal report and unsaved draft through a pending and failed background
// card refresh. Retrying the card must perform a successful owner read before
// the retained result becomes approvable again.
func TestViewsPageBackgroundCardReadRequiresRecoveryBeforeApproval(t *testing.T) {
	cp, card, runsDir := selectedInputsFixture(t)
	runID := strings.Repeat("1", 32)
	writeListedRun(t, runsDir, pageOwner, card, runID,
		time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC), true)
	setListedRunSource(t, runsDir, runID, verify.TestsWritten)
	ps := startPageServer(t, cp)
	ps.act("builder-reads-assignment", func() {
		_, err := cp.GetBuilderAssignment(asCaller(pageOwner, CallerBuilder),
			&gensurla.GetBuilderAssignmentPayload{ChangeID: card.ChangeID})
		require.NoError(t, err)
	})
	runPageScript(t, ps, map[string]any{"run": runID}, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-review-check")?.classList.contains("shadcn-btn-primary") && !page.$("#surla-build-review-check").disabled, "the saved passing check did not load");
page.$("#surla-refine-agreement").open = true;
const draft = page.$('textarea[data-field="expected_outcome"]');
page.type(draft, "Preserve this draft through the card read");
page.$("#surla-build-review-check").click();
await until(() => page.$("#surla-result-view").dataset.runId === input.run && page.$("#surla-cp5-approve") && !page.$("#surla-cp5-approve").disabled, "the selected passing proof did not settle");
const approve = page.$("#surla-cp5-approve");
const verdict = page.text("#surla-cp4-title");
const details = page.text("#surla-cp4-details");
const preserved = () => page.$("#surla-result-view").dataset.runId === input.run && page.text("#surla-cp4-title") === verdict && page.text("#surla-cp4-details") === details && page.$('textarea[data-field="expected_outcome"]') === draft && draft.value === "Preserve this draft through the card read";
const cardPath = "/changes/chg-selected-inputs";
const buildPath = cardPath + "/build";
const runPath = "/verifications/" + input.run;
const before = page.sent("GET", cardPath).length;
await control(origin, "reply", { key: "GET " + cardPath, status: 503, body: { name: "internal", message: "The saved card could not be refreshed." } });
await control(origin, "hold", { key: "GET " + cardPath });
await control(origin, "act", { key: "builder-reads-assignment" });
await until(() => page.sent("GET", cardPath).length > before && page.sent("GET", buildPath).every((r) => r.status === 200) && page.sent("GET", runPath).every((r) => r.status === 200) && page.$("#surla-build-builder").textContent.includes("Assignment read"), "the changed build did not reach the page with its card refresh held");
const held = page.sent("GET", cardPath)[before];
expect(held.status === undefined && approve.disabled && page.text("#surla-cp4-inputs").includes("not known"), "an outstanding background card read left the rendered approval usable");
expect(preserved(), "the pending card read discarded the report or draft");
approve.click();
expect(page.sent("POST", "/changes/approve").length === 0, "a pending card refresh submitted approval");
await control(origin, "release", { key: "GET " + cardPath });
await until(() => held.status === 503 && page.$("#surla-cp4-card-error") && page.$("#surla-cp4-retry-card") && !page.$("#surla-cp4-retry-card").disabled, "the failed background card read exposed no user recovery");
expect(page.text("#surla-cp4-card-error").includes("The saved card could not be refreshed.") && approve.disabled && preserved(), "the failed card read hid its failure, enabled approval or discarded work: " + JSON.stringify({message: page.text("#surla-cp4-card-error"), disabled: approve.disabled, preserved: preserved(), run: page.$("#surla-result-view").dataset.runId === input.run, verdict: page.text("#surla-cp4-title") === verdict, details: page.text("#surla-cp4-details") === details, field: page.$('textarea[data-field="expected_outcome"]') === draft, draft: draft.value === "Preserve this draft through the card read"}));
approve.click();
expect(page.sent("POST", "/changes/approve").length === 0, "a failed background card refresh submitted approval");
await control(origin, "hold", { key: "GET " + cardPath });
const retries = page.sent("GET", cardPath).length;
page.$("#surla-cp4-retry-card").click();
await until(() => page.sent("GET", cardPath).length > retries && page.$("#surla-cp4-retry-card").disabled, "retry did not issue a new card read");
expect(approve.disabled && preserved(), "requesting recovery cleared the card-read failure before its owner answered");
await control(origin, "release", { key: "GET " + cardPath });
await until(() => page.sent("GET", cardPath).slice(retries).some((r) => r.status === 200) && !page.$("#surla-cp4-card-error") && !approve.disabled, "a successful card retry did not restore current-input approval");
expect(preserved(), "card recovery replaced the saved report or unsaved draft");
expect(page.sent("POST", "/changes/approve").length === 0 && page.sent("POST", "/changes/confirm").length === 0, "card recovery submitted approval or draft edits");
page.close();
`)
}

// TestViewsPageBuildRetryInvalidatesProofBeforeTheVersionNotification makes
// an explicit build read discover changed tests before the held wait channel
// can announce them. A cached positive run observation cannot survive that
// independently obtained build fact.
func TestViewsPageBuildRetryInvalidatesProofBeforeTheVersionNotification(t *testing.T) {
	cp, card, runsDir := selectedInputsFixture(t)
	runID := strings.Repeat("1", 32)
	writeListedRun(t, runsDir, pageOwner, card, runID,
		time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC), true)
	setListedRunSource(t, runsDir, runID, verify.TestsWritten)
	ps := startPageServer(t, cp)
	selectedInputsActions(t, ps, cp, card.ChangeID)
	runPageScript(t, ps, map[string]any{"run": runID}, `
const cardPath = "/changes/chg-selected-inputs";
const buildPath = cardPath + "/build";
const runPath = "/verifications/" + input.run;
// Install both controls before opening the page: there is no earlier wait
// request that can deliver the changed version around the test's gate.
await control(origin, "hold", { key: "GET /change-updates" });
await control(origin, "reply", { key: "GET " + buildPath, status: 503, body: { name: "internal", message: "Retry the saved build read." } });
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-refresh") && !page.$("#surla-build-refresh").disabled && page.sent("GET", runPath).some((r) => r.status === 200 && JSON.parse(r.answer).inputs_current === true), "the initial failed build read did not retain the positive T1 owner observation");
page.$("#surla-refine-agreement").open = true;
page.type(page.$('textarea[data-field="expected_outcome"]'), "Keep the editor beside saved proof");
page.$("#surla-nav-cp4").click();
await until(() => page.$("#surla-result-view").dataset.runId === input.run && page.$("#surla-cp5-approve") && page.$("#surla-cp5-approve").disabled && !page.$("#surla-load-verifications").disabled, "the failed build's saved proof did not load");
const verdict = page.text("#surla-cp4-title");
const lists = page.sent("GET", "/changes").length;
const before = page.requests.length;
await control(origin, "act", { key: "accept-t2" });
page.$("#surla-build-refresh").click();
await until(() => !page.$("#surla-build-error") && page.sent("GET", buildPath).some((r) => r.status === 200 && JSON.parse(r.answer).build.test_writer.tests.commit === "3333333333333333333333333333333333333333"), "the explicit retry did not discover accepted T2");
await until(() => page.requests.slice(before).some((r) => r.path === runPath && r.status === 200 && JSON.parse(r.answer).inputs_current === false) && page.text("#surla-cp4-inputs").includes("does not match"), "discovering T2 through Build did not replace the positive T1 run observation");
expect(page.sent("GET", "/changes").length === lists && page.sent("GET", "/change-updates").every((r) => r.status === undefined), "the test received a version notification before the explicit retry established freshness");
expect(page.$("#surla-cp5-approve").disabled && page.$("#surla-result-view").dataset.runId === input.run && page.text("#surla-cp4-title") === verdict, "Build recovery enabled stale proof or replaced the selected report");
page.$("#surla-cp5-approve").click();
expect(page.sent("POST", "/changes/approve").length === 0 && page.sent("POST", buildPath).length === 0, "the independent build read submitted approval or started a build");
page.close();
`)
}

// ListVerifications preserves the actual owner result and captures only the
// armed answer. The HTTP answer-later gate controls its later delivery.
func (s *selectedInputsListRead) ListVerifications(
	ctx context.Context,
	p *gensurla.ListVerificationsPayload,
) (*gensurla.VerificationListResult, error) {
	result, err := s.ControlPlane.ListVerifications(ctx, p)
	if err == nil && s.armed.CompareAndSwap(true, false) {
		s.captured <- result
	}
	return result, err
}

// selectedInputsFixture starts a real saved build with accepted T1 tests. Its
// simulated working-copy boundary and checker avoid executing candidate code.
func selectedInputsFixture(t *testing.T) (*ControlPlane, *gensurla.ChangeCardResult, string) {
	t.Helper()
	runsDir := t.TempDir()
	cp := newBuildControlPlane(t, Options{
		RunsDir: runsDir, WrittenTestRuns: &WrittenTestRuns{ReportDir: t.TempDir(), Timeout: time.Minute},
	})
	cp.changesWait, cp.changesPoll = 400*time.Millisecond, 20*time.Millisecond
	card := confirmedChange(t, cp, "chg-selected-inputs")
	_, err := cp.StartBuild(asCaller(pageOwner, CallerUser), &gensurla.StartBuildPayload{
		ChangeID: card.ChangeID, RequestRevision: card.RequestRevision,
	})
	require.NoError(t, err)
	saveListedTestState(t, cp, card.ChangeID, testsAccepted, strings.Repeat("2", 40))
	return cp, card, runsDir
}

// selectedInputsActions changes only the saved checker verdict through the
// shared fixture boundary; normal wait/list reads deliver these changes.
func selectedInputsActions(t *testing.T, ps *pageServer, cp *ControlPlane, changeID string) {
	t.Helper()
	for _, selected := range []struct{ name, commit string }{
		{name: "accept-t1", commit: strings.Repeat("2", 40)},
		{name: "accept-t2", commit: strings.Repeat("3", 40)},
	} {
		ps.act(selected.name, func() {
			saveListedTestState(t, cp, changeID, testsAccepted, selected.commit)
		})
	}
}

// writeSelectedInputsRun gives the newer saved run a distinct test commit in
// both its request and valid report binding. It reuses the ordinary supervisor
// fixture layout rather than mismatching a request with another report's bytes.
func writeSelectedInputsRun(
	t *testing.T,
	runsDir string,
	card *gensurla.ChangeCardResult,
	runID, suiteCommit string,
) {
	t.Helper()
	writeListedRun(t, runsDir, pageOwner, card, runID,
		time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC), true)
	setListedRunSource(t, runsDir, runID, verify.TestsWritten)
	requestPath := filepath.Join(runsDir, runID, "request.json")
	raw, err := os.ReadFile(requestPath)
	require.NoError(t, err)
	var request verifyrun.Request
	require.NoError(t, json.Unmarshal(raw, &request))
	request.Plan.SuiteCommit = suiteCommit
	report := passingReport(t, card.RequestRevision, request.Plan.CandidateCommit, reportSpec{
		SuiteID: "orders_acceptance", SuiteCommit: suiteCommit, Environment: "sim",
		Artifacts: map[string]string{"orders": strings.Repeat("a", 64)},
	})
	writeReportFile(t, request.Plan.ReportDir, report)
	raw, err = json.Marshal(request)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(requestPath, raw, 0o600))
	raw, err = json.Marshal(verifyrun.Result{Kind: verifyrun.ResultReport, ReportDigest: report.Digest})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(runsDir, runID, "result.json"), raw, 0o600))
}
