// This file checks the saved Proof presentation through the served page and
// generated HTTP API. Fixture reports prove presentation, not candidate runs.
package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// TestViewsPageProofPresentsOneSavedVerdict keeps report verdicts independent
// of current-input observations and counts, preserving every supplied outcome.
func TestViewsPageProofPresentsOneSavedVerdict(t *testing.T) {
	diagnostic := "The result differed."
	cases := []struct {
		name          string
		passed        bool
		suiteFailed   bool
		inputsCurrent bool
		counts        gensurla.VerificationCounts
		scenarios     []*gensurla.VerificationScenario
		heading       string
		showCounts    bool
	}{
		{
			name: "current pass", passed: true, inputsCurrent: true,
			counts: gensurla.VerificationCounts{Required: 1, Passed: 1},
			scenarios: []*gensurla.VerificationScenario{
				{ID: "holdout-1", Outcome: "passed", Checks: []*gensurla.VerificationCheck{
					{Name: "A visible scenario may use this name", Passed: true},
				}},
			},
			heading: "The check passed: 1 of 1 scenarios.",
		},
		{
			name: "historical pass", passed: true,
			counts:    gensurla.VerificationCounts{Required: 1, Passed: 1},
			scenarios: []*gensurla.VerificationScenario{{ID: "visible-check", Outcome: "passed"}},
			heading:   "The check passed: 1 of 1 scenarios.",
		},
		{
			name: "suite failure despite passed scenarios", suiteFailed: true, inputsCurrent: true,
			counts:    gensurla.VerificationCounts{Required: 1, Passed: 1},
			scenarios: []*gensurla.VerificationScenario{{ID: "visible-check", Outcome: "passed"}},
			heading:   "The check did not pass: 1 of 1 scenarios passed.", showCounts: true,
		},
		{
			name: "all outcome kinds", inputsCurrent: true,
			counts: gensurla.VerificationCounts{
				Required: 4, Passed: 1, Failed: 1, NotRun: 1, HarnessError: 1,
			},
			scenarios: []*gensurla.VerificationScenario{
				{ID: "passed-check", Outcome: "passed"},
				{ID: "failed-check", Outcome: "failed", Checks: []*gensurla.VerificationCheck{
					{Name: "Expected result", Passed: false, Diagnostic: &diagnostic},
				}},
				{ID: "missing-check", Outcome: "not_run"},
				{ID: "holdout-1", Outcome: "harness_error"},
			},
			heading: "The check did not pass: 1 of 4 scenarios passed.", showCounts: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := pageControlPlane(t, Options{})
			guide := WithPrincipal(context.Background(), Principal{
				ID: pageOwner, CallerID: "guide", Kind: CallerGuide,
			})
			user := WithPrincipal(context.Background(), Principal{
				ID: pageOwner, CallerID: pageUserID, Kind: CallerUser,
			})
			proposed := mustPropose(t, cp, guide, "chg-proof-presentation",
				"Route refunds to a manager.", refundAnalysis("Route refunds to a manager."))
			card, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
				ChangeID: proposed.ChangeID, RequestRevision: proposed.RequestRevision,
				Answers: map[string]string{}, Examples: proposed.Examples,
			})
			require.NoError(t, err)
			run := passedRun(card, "11111111-1111-4111-8111-111111111111",
				strings.Repeat("1", 40), strings.Repeat("3", 64))
			run.InputsCurrent = tc.inputsCurrent
			run.TestSource = gensurla.NewTestSourceWrittenTests(&gensurla.AgentWrittenTests{
				Commit: run.SuiteCommit, Notice: writtenTestsNotice,
			})
			run.State = gensurla.NewStateCompleted(&gensurla.VerificationCompleted{
				SuiteID: "accepted_checks", Environment: "sim",
				Report: &gensurla.BuilderVerificationReport{
					ReportDigest: strings.Repeat("3", 64), Passed: tc.passed,
					SuiteFailed: tc.suiteFailed, Counts: &tc.counts, Scenarios: tc.scenarios,
				},
			})
			svc := newPageVerifications(cp)
			svc.set(run)
			svc.list(card.ChangeID, run.RunID)
			ps := startPageServer(t, svc)
			runPageScript(t, ps, map[string]any{
				"change": card.ChangeID, "run": run, "heading": tc.heading,
				"showCounts": tc.showCounts, "notice": writtenTestsNotice,
			}, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === input.change, "the change did not open");
page.$("#surla-nav-cp4").click();
await until(() => page.$("#surla-result-view").dataset.runId === input.run.run_id, "the saved run did not open");
expect(page.text("#surla-cp4-title") === input.heading, "the saved verdict or denominator changed");
const notice = page.$("#surla-cp4-test-source");
expect(notice && notice.tagName === "P" && notice.textContent === input.notice, "the source notice is not neutral and intact");
expect(notice.textContent.includes("not isolated from the builder"), "the source notice lost its non-isolation meaning");
expect(!page.$("#surla-cp4-run-list"), "a singleton run acquired a selector");
expect(Boolean(page.$('[aria-label="Scenario outcome counts"]')) === input.showCounts, "detailed counts are shown in the wrong state");
expect(!page.text("#surla-cp4-verification").includes("Unseen audit scenarios"), "a scenario ID was treated as a hidden-scenario flag");
const started = page.$("#surla-cp4-started time");
expect(started && started.dateTime === input.run.accepted_at, "the visible start time lost the accepted timestamp");
expect(page.text("#surla-cp4-started").startsWith("Check started "), "request acceptance is not labelled as a check start");
expect(!started.textContent.includes("T12:34:56"), "the start time is still raw ISO in the reading path");
expect(page.$$("#surla-cp4-details time").some((time) => time.textContent === input.run.accepted_at), "the exact ISO start disappeared from Details");
await until(() => page.text("#surla-cp4-inputs").includes(input.run.inputs_current ? "This result matches" : "does not match"), "the selected run's current-input fact is missing");
const report = input.run.state.value.report;
const outcomes = page.$$(".surla-verification-scenarios > li");
expect(outcomes.length === report.scenarios.length, "the declared scenario denominator was truncated");
for (let index = 0; index < outcomes.length; index++) {
  expect(outcomes[index].textContent.includes(report.scenarios[index].id), "a received scenario disappeared or changed order");
}
if (report.suite_failed) {
  expect(page.text("#surla-cp4-verification").includes("The evaluation failed or ended unexpectedly."), "suite failure context disappeared");
}
if (report.counts.failed) {
  expect(outcomes[1].textContent.includes("The result differed."), "failure diagnostics disappeared");
  expect(outcomes[2].textContent.includes("Not run") && outcomes[3].textContent.includes("Could not evaluate"), "non-pass outcomes were collapsed into failure");
}
expect(page.sent("POST", "/changes/" + input.change + "/verifications").length === 0, "reading saved proof started another check");
page.close();
`)
		})
	}
}
