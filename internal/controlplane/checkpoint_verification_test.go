// This file tests the views page's verification stage through its real
// list_verifications and get_verification requests: every run state and its
// anonymized holdouts, status reads that never restart work, answers that
// belong to the run they were asked about, and the editor staying
// independent while runs are read.
package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// TestCheckpointUIVerificationStatesAndStatusReads drives the verification
// stage against fixture runs served by the real generated encoder.
func TestCheckpointUIVerificationStatesAndStatusReads(t *testing.T) {
	cp := pageControlPlane(t, Options{})
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	user := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: pageUserID, Kind: CallerUser})
	revisions := map[string]string{}
	confirm := func(changeID string) *gensurla.ChangeCardResult {
		proposed := mustPropose(t, cp, guide, changeID, "Refunds above $50 require a manager.", refundAnalysis("Refunds above $50 require a manager."))
		confirmed, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
			ChangeID: changeID, RequestRevision: proposed.RequestRevision, Answers: map[string]string{}, Examples: proposed.Examples,
		})
		require.NoError(t, err)
		revisions[changeID] = confirmed.RequestRevision
		return confirmed
	}
	card := confirm("chg-verify")
	confirm("chg-verify-b")
	confirm("__proto__")
	confirm("toString")
	incoming := mustPropose(t, cp, guide, "chg-incoming", "Refunds above $50 require a manager.", refundAnalysis("Refunds above $50 require a manager."))
	revisions[incoming.ChangeID] = incoming.RequestRevision

	run := func(runID, changeID string, state gensurla.State) *gensurla.VerificationStatusResult {
		return &gensurla.VerificationStatusResult{
			RunID: runID, ChangeID: changeID, RequestRevision: revisions[changeID],
			CandidateCommit: strings.Repeat("1", 40), SuiteCommit: strings.Repeat("2", 40),
			AcceptedAt: "2026-10-31T12:34:56.123Z", State: state,
			// These evaluator fixtures model an owner observation of the selected tests.
			InputsCurrent: true,
			TestSource:    gensurla.NewTestSourceEvaluator(&gensurla.OperatorEvaluatorTests{}),
		}
	}
	passed := &gensurla.BuilderVerificationReport{
		ReportDigest: strings.Repeat("3", 64), Passed: true,
		Counts: &gensurla.VerificationCounts{Required: 2, Passed: 2},
		Scenarios: []*gensurla.VerificationScenario{
			{ID: "visible_refund", Outcome: "passed", Checks: []*gensurla.VerificationCheck{{Name: "one_refund", Passed: true}}},
			{ID: "holdout-1", Outcome: "passed"},
		},
	}
	diagnostic := "Exact failing diagnostic <img src=x onerror=alert(1)>"
	environmentError := "Test dependency was unavailable"
	failed := &gensurla.BuilderVerificationReport{
		ReportDigest: strings.Repeat("4", 64), SuiteFailed: true,
		Counts: &gensurla.VerificationCounts{Required: 6, Passed: 3, Failed: 1, NotRun: 1, HarnessError: 1},
		Scenarios: []*gensurla.VerificationScenario{
			{ID: "visible_refund", Outcome: "failed", Checks: []*gensurla.VerificationCheck{{Name: "one_refund", Diagnostic: &diagnostic}}},
			{ID: "not_executed", Outcome: "not_run"},
			{ID: "dependency_failed", Outcome: "harness_error", Error: &environmentError},
			{ID: "visible_pass", Outcome: "passed"},
			{ID: "holdout-1", Outcome: "passed"},
			{ID: "holdout-2", Outcome: "passed"},
		},
	}
	suiteFailed := *passed
	suiteFailed.Passed, suiteFailed.SuiteFailed = false, true
	suiteFailed.ReportDigest = strings.Repeat("5", 64)
	runningState := gensurla.NewStateRunning(&gensurla.VerificationRunning{StartedAt: "2026-10-31T12:34:56.123Z"})
	completed := func(report *gensurla.BuilderVerificationReport) gensurla.State {
		return gensurla.NewStateCompleted(&gensurla.VerificationCompleted{SuiteID: "orders_acceptance", Environment: "sim", Report: report})
	}
	const runA, runB = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	early := run("10000000-0000-4000-8000-000000000001", "chg-verify", completed(failed))
	early.AcceptedAt = "2026-10-31T12:00:00.1Z"
	late := run("20000000-0000-4000-8000-000000000002", "chg-verify", completed(passed))
	late.AcceptedAt = "2026-10-31T12:00:00.9Z"
	otherRevision := run("88888888-8888-4888-8888-888888888888", "chg-verify", completed(passed))
	otherRevision.RequestRevision = strings.Repeat("e", 64)
	otherRevision.InputsCurrent = false
	incomingRun := run("99999999-9999-4999-8999-999999999999", incoming.ChangeID, completed(passed))
	incomingRun.InputsCurrent = false // The proposed card has no agreed revision.
	fixtures := map[string]*gensurla.VerificationStatusResult{
		"running":         run(runA, "chg-verify", runningState),
		"passed":          run(runA, "chg-verify", completed(passed)),
		"otherRun":        run(runB, "chg-verify", runningState),
		"otherPassed":     run(runB, "chg-verify", completed(passed)),
		"failed":          run("44444444-4444-4444-8444-444444444444", "chg-verify", completed(failed)),
		"suiteFailed":     run("55555555-5555-4555-8555-555555555555", "chg-verify", completed(&suiteFailed)),
		"executionFailed": run("66666666-6666-4666-8666-666666666666", "chg-verify", gensurla.NewStateNotCompleted(&gensurla.VerificationNotCompleted{Kind: "execution_failed", Stage: "build", Summary: "The candidate could not be built."})),
		"interrupted":     run("77777777-7777-4777-8777-777777777777", "chg-verify", gensurla.NewStateNotCompleted(&gensurla.VerificationNotCompleted{Kind: "interrupted", Stage: "supervise", Summary: "Verification stopped before a report was saved."})),
		"early":           early,
		"late":            late,
		"otherRevision":   otherRevision,
		"incoming":        incomingRun,
		"cardB":           run("33333333-3333-4333-8333-333333333333", "chg-verify-b", runningState),
		"proto":           run("55555555-0000-4555-8555-555555555555", "__proto__", completed(passed)),
		"toStringRun":     run("66666666-0000-4666-8666-666666666666", "toString", completed(passed)),
	}
	svc := newPageVerifications(cp)
	ps := startPageServer(t, svc)
	// Every evaluator fixture uses the configured suite. Observe the card
	// again when publishing it, because the save scenario revises that card
	// before later scenarios reuse its historical runs.
	setFixture := func(name string) {
		snapshot := *fixtures[name]
		current, err := cp.GetChange(guide, &gensurla.GetChangePayload{ChangeID: snapshot.ChangeID})
		require.NoError(t, err)
		snapshot.InputsCurrent = snapshot.InputsCurrent && current.Status == "confirmed" &&
			current.RequestRevision == snapshot.RequestRevision
		svc.set(&snapshot)
	}
	// "runs <change_id> <name,...|->" makes the change's runs exactly the
	// named fixtures; "run <name>" replaces one run's current snapshot.
	ps.fixtures = func(key string) bool {
		fields := strings.Fields(key)
		switch {
		case len(fields) == 3 && fields[0] == "runs":
			var ids []string
			if fields[2] != "-" {
				for _, name := range strings.Split(fields[2], ",") {
					setFixture(name)
					ids = append(ids, fixtures[name].RunID)
				}
			}
			svc.list(fields[1], ids...)
			return true
		case len(fields) == 2 && fields[0] == "run":
			setFixture(fields[1])
			return true
		}
		return false
	}
	runPageScript(t, ps, map[string]any{"revision": card.RequestRevision, "runA": runA, "runB": runB, "commit": strings.Repeat("1", 40)}, `
let pages = 0;
const fixture = (key) => control(origin, "act", { key });
const open = async (changeId) => {
  const page = await openPage(origin, { link: input.link + "-" + (++pages) });
  await until(() => page.$("#surla-change-list li"), "the page did not list the changes");
  await openChange(page, changeId);
  return page;
};
const openChange = async (page, changeId) => {
  page.$("#surla-nav-changes").click();
  await until(() => page.$("#surla-open-change-" + changeId) && !page.$("#surla-open-change-" + changeId).disabled, changeId + " is not offered");
  page.$("#surla-open-change-" + changeId).click();
  await until(() => page.$("#surla-cp1-editor").dataset.changeId === changeId, changeId + " did not open");
};
const proof = async (page, changeId) => {
  const lists = page.sent("GET", "/changes/" + changeId + "/verifications").length;
  page.$("#surla-nav-cp4").click();
  await until(() => page.sent("GET", "/changes/" + changeId + "/verifications").length > lists && !page.$("#surla-cp4-verification").getAttribute("aria-busy"), "the runs of " + changeId + " were not read");
};
const text = (page) => page.text("#surla-result-view");
const field = (page) => page.$('textarea[data-field="expected_outcome"]');
const button = (page) => page.$("#surla-cp4-check-status");
const statusReads = (page, runId) => page.sent("GET", "/verifications/" + runId);
const selectRun = async (page, runId) => {
  page.$("#surla-cp4-run-" + runId).click();
  await until(() => page.$("#surla-result-view").dataset.runId === runId, "run " + runId + " was not selected");
};
const runJSON = async (runId) => (await api(origin, "GET", "/verifications/" + runId, undefined, input.guide)).body;

// Refreshing a run preserves the exact editable draft and starts no poller.
{
  await fixture("runs chg-verify running");
  const page = await open("chg-verify");
  page.type(field(page), "Unsaved CP4 draft");
  await proof(page, "chg-verify");
  expect(page.text("#surla-cp4-title") === "The check is running", "running state is missing");
  expect(page.$("#surla-cp4-verification").getAttribute("aria-busy") === null, "idle run card marked aria-busy");
  await sleep(300);
  expect(statusReads(page, input.runA).length === 0, "showing a run started an automatic status read");
  await fixture("run passed");
  await control(origin, "hold", { key: "GET /verifications/" + input.runA });
  button(page).click();
  expect(statusReads(page, input.runA).length === 1, "status action did not read this exact run");
  expect(button(page).disabled && page.$("#surla-cp4-verification").getAttribute("aria-busy") === "true", "status request was not disabled or marked aria-busy");
  expect(!field(page).disabled, "status read disabled independent CP1 editing");
  page.type(field(page), "Edited while checking status");
  await control(origin, "release", { key: "GET /verifications/" + input.runA });
  await until(() => text(page).includes("The check passed: 2 of 2 scenarios."), "completed report is not shown");
  expect(!button(page) && page.$("#surla-cp4-verification").getAttribute("aria-busy") === null, "completed report is not final or remained aria-busy");
  expect(text(page).includes("The check passed: 2 of 2 scenarios.") && text(page).includes("holdout-1"), "report denominator or anonymous outcome is missing");
  expect(text(page).includes("Not tested") && text(page).includes(input.commit), "coverage or verified inputs are missing");
  expect(field(page).value === "Edited while checking status", "status reply erased a draft");
  await sleep(200);
  expect(statusReads(page, input.runA).length === 1, "completed report started another request");
  page.close();
}

// Every report outcome stays visible; failure is never inferred from missing detail.
{
  await fixture("runs chg-verify failed,suiteFailed,executionFailed,interrupted");
  const page = await open("chg-verify");
  await proof(page, "chg-verify");
  await selectRun(page, "44444444-4444-4444-8444-444444444444");
  expect(text(page).includes("The check did not pass:") && text(page).includes("Not run") && text(page).includes("Could not evaluate"), "failed report collapsed distinct outcomes");
  expect(text(page).includes("Exact failing diagnostic <img src=x onerror=alert(1)>"), "repair diagnostic was removed");
  expect(!page.$("#surla-cp4-verification img"), "diagnostic became executable HTML");
  expect(text(page).includes("The check did not pass: 3 of 6 scenarios passed.") && text(page).includes("holdout-2") && !text(page).includes("hidden"), "failed report denominator changed");
  await selectRun(page, "55555555-5555-4555-8555-555555555555");
  expect(text(page).includes("The check did not pass:") && text(page).includes("evaluation failed or ended unexpectedly"), "suite failure was converted to a passing count verdict");
  for (const [runId, title] of [["66666666-6666-4666-8666-666666666666", "The check could not finish"], ["77777777-7777-4777-8777-777777777777", "The check was interrupted"]]) {
    await selectRun(page, runId);
    expect(text(page).includes(title) && text(page).includes("Stopped during:"), "incomplete outcome is inaccurate");
    expect(!text(page).includes("Declared scenarios") && !button(page), "no-report outcome offered a completed report or restarted work");
  }
  page.close();
}

// A failed read keeps its run and draft, and retries the same run.
{
  await fixture("runs chg-verify running");
  const page = await open("chg-verify");
  page.type(field(page), "Draft after read error");
  await proof(page, "chg-verify");
  await control(origin, "reply", { key: "GET /verifications/" + input.runA, status: 503, body: { name: "fault", id: "x", message: "Verification service unavailable", temporary: true, timeout: false, fault: true } });
  button(page).click();
  await until(() => text(page).includes("Verification service unavailable"), "the read failure is not shown");
  expect(text(page).includes("The check is running") && field(page).value === "Draft after read error" && !button(page).disabled, "read failure lost the run, the draft or the retry");
  await fixture("run passed");
  button(page).click();
  await until(() => text(page).includes("The check passed: 2 of 2 scenarios."), "the retry did not read the run");
  expect(!text(page).includes("Verification service unavailable"), "successful retry retained old feedback");
  page.close();
}

// An answer belongs to the run it was asked about, not to the run displayed when it arrives.
{
  await fixture("runs chg-verify running,otherRun");
  await fixture("run running");
  const page = await open("chg-verify");
  await proof(page, "chg-verify");
  await selectRun(page, input.runA);
  await control(origin, "hold", { key: "GET /verifications/" + input.runA });
  await control(origin, "hold", { key: "GET /verifications/" + input.runB });
  button(page).click();
  await selectRun(page, input.runB);
  button(page).click();
  expect(statusReads(page, input.runB).length === 1, "run B's status was not read");
  await fixture("run passed");
  await control(origin, "release", { key: "GET /verifications/" + input.runA });
  await sleep(200);
  expect(page.$("#surla-result-view").dataset.runId === input.runB && button(page).disabled, "late run A answer overwrote run B or cleared B's request");
  await control(origin, "release", { key: "GET /verifications/" + input.runB });
  await until(() => !button(page).disabled, "run B remained disabled after its own answer");
  page.close();
}

// Status reads and card saves are independent and do not relabel earlier reports.
{
  await fixture("runs chg-verify running");
  await fixture("run running");
  const page = await open("chg-verify");
  page.type(field(page), "Saved edited outcome");
  await proof(page, "chg-verify");
  expect(field(page), "the editor with unsaved edits did not stay beside the runs");
  await control(origin, "hold", { key: "POST /changes/confirm" });
  page.$("#surla-confirm-submit").click();
  await fixture("run passed");
  button(page).click();
  expect(page.sent("POST", "/changes/confirm").length === 1 && statusReads(page, input.runA).length === 1, "independent status read was blocked behind the save");
  await until(() => text(page).includes("The check passed: 2 of 2 scenarios."), "the status read did not finish during the save");
  expect(field(page).disabled, "status reply unlocked an unfinished card save");
  await control(origin, "release", { key: "POST /changes/confirm" });
  await until(() => !field(page).disabled, "card answer did not finish independently");
  expect(field(page).value === "Saved edited outcome", "card answer lost saved text");
  expect(page.$("#surla-cp4-different-revision") && text(page).includes(input.revision), "report for another revision looked current after the card changed");
  page.type(field(page), "Discard this local draft");
  page.$("#surla-discard-draft").click();
  expect(!field(page) && page.$("#surla-cp4-different-revision"), "closed editor lost the retained card revision warning");
  page.close();
}

// The list keeps the server's order, preserves exact times in Details, and shows its empty state.
{
  await fixture("runs chg-verify late,early");
  const page = await open("chg-verify");
  await proof(page, "chg-verify");
  expect(page.state() === "cp4-verification" && page.$("#surla-result-view").dataset.runId === "20000000-0000-4000-8000-000000000002", "the list did not select the first server-ordered run");
  const buttons = page.$$("#surla-cp4-run-list button");
  expect(buttons.length === 2 && buttons[0].dataset.runId === "20000000-0000-4000-8000-000000000002" && buttons[1].dataset.runId === "10000000-0000-4000-8000-000000000001", "run buttons changed the server's sub-second ordering");
  expect(buttons[0].textContent.includes("started ") && page.$('#surla-cp4-details time[datetime="2026-10-31T12:00:00.9Z"]').textContent === "2026-10-31T12:00:00.9Z", "the latest run lost its local start label or exact accepted_at time");
  expect(buttons[0].textContent.includes("candidate " + input.commit.slice(0, 7)), "candidate commit prefix missing in run button");
  expect(!page.$("#surla-checkpoint-nav").hidden && page.$("#surla-nav-cp4").getAttribute("aria-current") === "page", "the stage navigation does not mark the runs");
  const subject = page.$("#surla-context-result");
  expect(subject && subject.dataset.changeId === "chg-verify" && subject.dataset.requestRevision === input.revision, "the subject strip is not derived from the selected run");
  buttons[1].click();
  expect(page.$("#surla-result-view").dataset.runId === "10000000-0000-4000-8000-000000000001" && buttons[1].getAttribute("aria-pressed") === "true", "clicking the second run did not switch the displayed run");
  expect(page.$('#surla-cp4-details time[datetime="2026-10-31T12:00:00.1Z"]').textContent === "2026-10-31T12:00:00.1Z", "the older run lost its exact accepted_at time");
  await fixture("runs chg-verify -");
  page.$("#surla-load-verifications").click();
  await until(() => page.$("#surla-cp4-empty") && !page.$("#surla-cp4-run-list"), "an empty list did not show #surla-cp4-empty");
  page.close();
}

// Reloading prunes runs, keeps finished runs finished, and respects a selection made during a load.
{
  await fixture("runs chg-verify passed,otherRun");
  const page = await open("chg-verify");
  await proof(page, "chg-verify");
  expect(page.$("#surla-result-view").dataset.runId === input.runA && !page.$("#surla-cp4-unlisted-run"), "the first run was not selected");
  const reload = async () => {
    const lists = page.sent("GET", "/changes/chg-verify/verifications").length;
    page.$("#surla-load-verifications").click();
    await until(() => page.sent("GET", "/changes/chg-verify/verifications").length > lists && !page.$("#surla-load-verifications").disabled, "the list did not reload");
  };
  await fixture("runs chg-verify running,otherRun");
  await reload();
  expect(text(page).includes("The check passed: 2 of 2 scenarios."), "a stale running snapshot reverted a completed run");
  await control(origin, "hold", { key: "GET /changes/chg-verify/verifications" });
  page.$("#surla-load-verifications").click();
  expect(page.$("#surla-load-verifications").disabled, "the load button was not disabled while loading");
  const lists = page.sent("GET", "/changes/chg-verify/verifications").length;
  page.$("#surla-load-verifications").click();
  expect(page.sent("GET", "/changes/chg-verify/verifications").length === lists, "a second click sent another list read");
  page.$("#surla-cp4-run-" + input.runB).click();
  await control(origin, "release", { key: "GET /changes/chg-verify/verifications" });
  await until(() => !page.$("#surla-load-verifications").disabled, "the held list did not finish");
  expect(page.$("#surla-result-view").dataset.runId === input.runB, "the list answer overwrote a run selected during the load");
  await control(origin, "hold", { key: "GET /changes/chg-verify/verifications" });
  page.$("#surla-load-verifications").click();
  page.$("#surla-cp4-run-" + input.runA).click();
  await fixture("runs chg-verify otherRun");
  await control(origin, "release", { key: "GET /changes/chg-verify/verifications" });
  await until(() => !page.$("#surla-load-verifications").disabled, "the pruning list did not finish");
  expect(page.$("#surla-result-view").dataset.runId === input.runA && page.$("#surla-cp4-unlisted-run") && page.text("#surla-cp4-unlisted-run").includes("Not included in the last loaded list; reload to check."), "a pruned run selected during the load did not stay with #surla-cp4-unlisted-run");
  await reload();
  expect(page.$("#surla-result-view").dataset.runId === input.runB && !page.$("#surla-cp4-run-list") && page.text("#surla-cp4-title") === "The check is running", "pruning [A, B] -> [B] did not advance to B");
  await fixture("runs chg-verify -");
  await reload();
  expect(page.$("#surla-cp4-empty") && !page.$("#surla-result-view").dataset.runId, "pruning to [] did not reach #surla-cp4-empty");
  page.close();
}

// Prototype-named change IDs keep their own runs without touching Object.prototype.
{
  await fixture("runs __proto__ proto");
  await fixture("runs toString toStringRun");
  const page = await open("__proto__");
  await proof(page, "__proto__");
  expect(page.$("#surla-result-view").dataset.changeId === "__proto__" && !page.$("#surla-cp4-run-list") && page.text("#surla-cp4-title") === "The check passed: 2 of 2 scenarios." && !page.$("#surla-cp4-list-error") && !page.$("#surla-cp4-card-error") && !page.$("#surla-cp4-unlisted-run"), "the __proto__ change's runs did not render cleanly");
  expect(Object.prototype.hasOwnProperty.call(Object.prototype, "0") === false && ({}).toString === Object.prototype.toString, "a __proto__ change mutated Object.prototype");
  await openChange(page, "toString");
  await proof(page, "toString");
  expect(page.$("#surla-result-view").dataset.changeId === "toString" && !page.$("#surla-cp4-list-error") && !page.$("#surla-cp4-card-error"), "the toString change's runs did not render cleanly");
  const ambiguous = (await api(origin, "GET", "/changes/toString", undefined, input.guide)).body;
  ambiguous.ambiguities = [{ id: "toString", question: "Prototype key question?", options: ["optA", "optB"] }];
  ambiguous.answers = {};
  await control(origin, "reply", { key: "GET /changes/toString", status: 200, body: ambiguous });
  await openChange(page, "chg-verify");
  await openChange(page, "toString");
  expect(page.text("#surla-cp1-editor").includes("Prototype key question?") && !page.text("#surla-cp1-editor").includes("function toString") && !page.text("#surla-cp1-editor").includes("[Confirmed answer:"), "ambiguity id toString read Object.prototype.toString");
  page.close();
}

// Overlapping run lists of two changes settle on the change shown last, in both answer orders.
for (const order of ["A_THEN_B", "B_THEN_A"]) {
  await fixture("runs chg-verify passed");
  await fixture("runs chg-verify-b cardB");
  const page = await open("chg-verify");
  await control(origin, "hold", { key: "GET /changes/chg-verify/verifications" });
  await control(origin, "hold", { key: "GET /changes/chg-verify-b/verifications" });
  page.$("#surla-nav-cp4").click();
  await openChange(page, "chg-verify-b");
  page.$("#surla-nav-cp4").click();
  const first = order === "A_THEN_B" ? "chg-verify" : "chg-verify-b";
  const second = order === "A_THEN_B" ? "chg-verify-b" : "chg-verify";
  await control(origin, "release", { key: "GET /changes/" + first + "/verifications" });
  await sleep(150);
  await control(origin, "release", { key: "GET /changes/" + second + "/verifications" });
  await until(() => page.$("#surla-result-view").dataset.runId === "33333333-3333-4333-8333-333333333333", "the chg-verify-b list did not settle in order " + order);
  expect(!page.$("#surla-cp4-unlisted-run"), "overlapping lists left an unlisted run in order " + order);
  page.close();
}

// A revision held beside unsaved edits survives a round trip through the runs.
{
  await fixture("runs chg-incoming incoming");
  const page = await open("chg-incoming");
  const r1 = page.$("#surla-cp1-editor").dataset.requestRevision;
  page.type(field(page), "Dirty R1 before incoming R2");
  const current = (await api(origin, "GET", "/changes/chg-incoming", undefined, input.guide)).body;
  const revised = await api(origin, "POST", "/changes/revise", {
    change_id: "chg-incoming", request_revision: current.request_revision, prompt: "Refunds above $50 require a manager and an audit log.",
    analysis: { summary: "Refunds above $50 require a manager and an audit log.", ambiguities: [], examples: [{ prior_id: "check_01", scenario: "Refund at the limit", input_summary: "Order total $50.00", expected_outcome: "Instant store credit", verification_kind: "deterministic" }], removed_example_ids: [], preserved_behaviors: [] },
  }, input.guide);
  expect(revised.status === 200, "could not revise: " + JSON.stringify(revised));
  await until(() => page.$("#surla-incoming-change-card") && page.$("#surla-replace-incoming-card"), "incoming R2 did not render #surla-incoming-change-card");
  page.$("#surla-nav-cp4").click();
  await until(() => page.$("#surla-result-view").dataset.state === "cp4-verification", "did not switch to the runs while incoming R2 was held");
  page.$("#surla-nav-cp1").click();
  expect(page.$("#surla-incoming-change-card") && page.$("#surla-replace-incoming-card"), "returning from the runs lost the held incoming R2");
  page.$("#surla-nav-cp1").click();
  expect(page.$("#surla-incoming-change-card") && page.$("#surla-replace-incoming-card"), "clicking the open stage again cleared the held incoming R2");
  page.$("#surla-replace-incoming-card").click();
  expect(page.$("#surla-cp1-editor").dataset.requestRevision === revised.body.request_revision && page.text("#surla-prior-revision").includes(r1) && page.text("#surla-preserved-draft").includes("Dirty R1 before incoming R2"), "switching to the held R2 after the runs did not keep R1 as previous and the preserved draft");
  page.close();
}

// Revision inequality establishes identity, not which snapshot is newer.
{
  await fixture("runs chg-verify otherRevision");
  const page = await open("chg-verify");
  page.type(field(page), "Retain older editor");
  await proof(page, "chg-verify");
  const warning = page.$("#surla-cp4-different-revision");
  expect(warning && warning.textContent.includes("different saved Change Card revision") && !warning.textContent.includes("earlier"), "revision hashes were used to invent chronology");
  expect(page.text("#surla-cp4-inputs").includes("does not match"), "the saved other-revision fixture claimed current inputs");
  expect(field(page).value === "Retain older editor", "a report for a different revision erased the draft");
  page.close();
}

// A finished run stays finished across a stale answer, and a running list does not discard a later completed answer.
{
  await fixture("runs chg-verify running");
  await fixture("run running");
  const page = await open("chg-verify");
  await proof(page, "chg-verify");
  await control(origin, "answer-later", { key: "GET /verifications/" + input.runA });
  button(page).click();
  await sleep(100);
  await fixture("runs chg-verify passed");
  page.$("#surla-load-verifications").click();
  await until(() => text(page).includes("The check passed: 2 of 2 scenarios."), "the reloaded list did not show the finished run");
  await control(origin, "release", { key: "GET /verifications/" + input.runA });
  await until(() => statusReads(page, input.runA).length === 1 && !page.$("#surla-cp4-verification").getAttribute("aria-busy"), "the stale answer did not arrive");
  expect(text(page).includes("The check passed: 2 of 2 scenarios.") && !button(page), "an older answer reverted a completed run to running");
  page.close();
}
{
  await fixture("runs chg-verify running");
  await fixture("run running");
  const page = await open("chg-verify");
  await proof(page, "chg-verify");
  await control(origin, "hold", { key: "GET /verifications/" + input.runA });
  button(page).click();
  page.$("#surla-load-verifications").click();
  await until(() => !page.$("#surla-load-verifications").disabled, "the running list did not load");
  await fixture("run passed");
  await control(origin, "release", { key: "GET /verifications/" + input.runA });
  await until(() => text(page).includes("The check passed: 2 of 2 scenarios.") && !button(page), "another running snapshot caused a valid completion to be discarded");
  page.close();
}

// Another stage stays displayed when an older status answer arrives.
{
  await fixture("runs chg-verify running");
  await fixture("run running");
  const page = await open("chg-verify");
  await proof(page, "chg-verify");
  await control(origin, "hold", { key: "GET /verifications/" + input.runA });
  button(page).click();
  page.$("#surla-nav-cp1").click();
  await fixture("run passed");
  await control(origin, "release", { key: "GET /verifications/" + input.runA });
  await until(() => statusReads(page, input.runA).length === 1, "the late status answer was not requested");
  await sleep(200);
  expect(page.state() === "cp1-change-card" && page.$("#surla-result-view").dataset.state === undefined, "a late status answer replaced a different stage");
  page.close();
}

// Valid offsets are accepted; malformed unions, timestamps and the wrong run fail at the response boundary.
{
  await fixture("runs chg-verify running");
  await fixture("run running");
  const base = await runJSON(input.runA);
  const otherRun = await (async () => { await fixture("runs chg-verify running,otherRun"); return runJSON(input.runB); })();
  await fixture("runs chg-verify running");
  const page = await open("chg-verify");
  await proof(page, "chg-verify");
  const answer = async (body) => {
    await control(origin, "reply", { key: "GET /verifications/" + input.runA, status: 200, body });
    const reads = statusReads(page, input.runA).length;
    button(page).click();
    await until(() => statusReads(page, input.runA).length > reads && !button(page).disabled, "the status read did not finish");
  };
  for (const started_at of ["2028-02-29T23:59:59+05:30", "2026-10-31T12:34:56.123456789-07:00"]) {
    await answer({ ...base, state: { type: "running", value: { started_at } } });
    expect(text(page).includes("The check is running") && text(page).includes(started_at) && !text(page).includes("Could not check verification status"), "valid RFC3339 timestamp was rejected: " + started_at);
  }
  const passedValue = { suite_id: "suite", environment: "sim", report: { report_digest: "3".repeat(64), counts: { required: 1, passed: 1, failed: 0, not_run: 0, harness_error: 0 }, passed: true, suite_failed: false, scenarios: [] } };
  const invalid = [
    { ...base, inputs_current: undefined },
    { ...base, inputs_current: "true" },
    { ...base, state: undefined },
    { ...base, state: { type: "completed", value: { suite_id: "suite", environment: "sim" } } },
    { ...base, state: { type: "running", value: passedValue } },
    { ...base, state: { type: "running", value: { started_at: "2026-02-30T12:00:00Z" } } },
    { ...base, state: { type: "running", value: { started_at: "2026-10-31T12:00Z" } } },
    { ...base, state: { ...base.state, private_evidence: "Hidden evaluator content" } },
    { ...base, state: { type: "not_completed", value: { kind: "execution_failed", stage: "unknown", summary: "Unknown failure" } } },
  ];
  for (const body of invalid) {
    await answer(body);
    expect(text(page).includes("Could not check verification status") && text(page).includes("The check is running"), "invalid union replaced the valid run: " + JSON.stringify(body.state));
    expect(!text(page).includes("Hidden evaluator content"), "unknown evidence escaped the schema boundary");
  }
  await answer(otherRun);
  expect(text(page).includes("different verification run") && page.$("#surla-result-view").dataset.runId === input.runA, "wrong run was accepted for a correlated read");
  page.close();
}
`)
}
