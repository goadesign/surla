// This file tests the served Proof page's test-source disclosure. Runs are
// explicit generated-service fixtures, so these checks prove presentation of
// saved results without claiming that candidate verification ran.
package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// TestViewsPageVerificationTestSource keeps the accepted-test notice visible
// in every run state and preserves the operator evaluator's accurate labels.
func TestViewsPageVerificationTestSource(t *testing.T) {
	const suiteCommit = "2222222222222222222222222222222222222222"
	sources := []struct {
		name        string
		source      gensurla.TestSource
		commitLabel string
		suiteLabel  string
		notice      string
	}{
		{
			name: "written_tests",
			source: gensurla.NewTestSourceWrittenTests(&gensurla.AgentWrittenTests{
				Commit: suiteCommit, Notice: writtenTestsNotice,
			}),
			commitLabel: "Accepted tests commit", suiteLabel: "Written tests suite",
			notice: writtenTestsNotice,
		},
		{
			name:        "evaluator",
			source:      gensurla.NewTestSourceEvaluator(&gensurla.OperatorEvaluatorTests{}),
			commitLabel: "Evaluator commit", suiteLabel: "Evaluator suite",
		},
	}
	states := []struct {
		name    string
		state   gensurla.State
		heading string
	}{
		{
			name: "running",
			state: gensurla.NewStateRunning(&gensurla.VerificationRunning{
				StartedAt: "2026-10-31T12:34:56Z",
			}),
			heading: "Verification in progress",
		},
		{
			name: "passed",
			state: gensurla.NewStateCompleted(&gensurla.VerificationCompleted{
				SuiteID: "accepted_checks", Environment: "sim",
				Report: &gensurla.BuilderVerificationReport{
					ReportDigest: strings.Repeat("3", 64), Passed: true,
					Counts: &gensurla.VerificationCounts{Required: 1, Passed: 1},
					Scenarios: []*gensurla.VerificationScenario{
						{ID: "holdout-1", Outcome: "passed"},
					},
				},
			}),
			heading: "Verification passed",
		},
		{
			name: "not_completed",
			state: gensurla.NewStateNotCompleted(&gensurla.VerificationNotCompleted{
				Kind: "execution_failed", Stage: "suite", Summary: "The checks could not finish.",
			}),
			heading: "Verification could not finish",
		},
	}
	for _, source := range sources {
		for _, state := range states {
			t.Run(source.name+"/"+state.name, func(t *testing.T) {
				cp := pageControlPlane(t, Options{})
				guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
				user := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: pageUserID, Kind: CallerUser})
				proposed := mustPropose(t, cp, guide, "chg-proof-source", "Route refunds to a manager.", refundAnalysis("Route refunds to a manager."))
				card, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
					ChangeID: proposed.ChangeID, RequestRevision: proposed.RequestRevision,
					Answers: map[string]string{}, Examples: proposed.Examples,
				})
				require.NoError(t, err)
				run := &gensurla.VerificationStatusResult{
					RunID: "11111111-1111-4111-8111-111111111111", ChangeID: card.ChangeID,
					RequestRevision: card.RequestRevision, CandidateCommit: strings.Repeat("1", 40),
					SuiteCommit: suiteCommit, AcceptedAt: "2026-10-31T12:34:56Z",
					TestSource: source.source, State: state.state,
				}
				svc := newPageVerifications(cp)
				svc.set(run)
				svc.list(card.ChangeID, run.RunID)
				ps := startPageServer(t, svc)
				runPageScript(t, ps, map[string]any{
					"change": card.ChangeID, "run": run.RunID, "state": state.name,
					"heading": state.heading, "notice": source.notice,
					"commitLabel": source.commitLabel, "suiteLabel": source.suiteLabel,
					"suiteCommit": suiteCommit,
				}, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === input.change, "the confirmed change did not open");
page.$("#surla-nav-cp4").click();
await until(() => page.$("#surla-result-view").dataset.runId === input.run, "the saved run did not open");
expect(page.text("#surla-cp4-title") === input.heading, "the run's status changed");
const notice = page.$("#surla-cp4-test-source");
if (input.notice) {
  expect(notice && notice.textContent === input.notice, "the server's written-test notice is missing or changed");
  expect(!notice.closest("details") && !notice.hidden, "the source notice is hidden behind a disclosure");
  expect(!page.text("#surla-cp4-details").includes("Evaluator"), "written tests are labeled as the evaluator");
} else {
  expect(!notice && !page.text("#surla-cp4-verification").includes("not isolated from the builder"), "the evaluator acquired a written-test notice");
  expect(!page.text("#surla-cp4-details").includes("Accepted tests"), "the evaluator acquired an accepted-test label");
}
const fields = [...page.$$("#surla-cp4-details dt")];
const commit = fields.find((field) => field.textContent === input.commitLabel);
expect(commit && commit.nextElementSibling.textContent === input.suiteCommit, "the source commit label or value is wrong");
if (input.state === "passed") {
  const suite = fields.find((field) => field.textContent === input.suiteLabel);
  expect(suite && suite.nextElementSibling.textContent === "accepted_checks", "the source suite label or value is wrong");
  expect(page.text("#surla-cp4-verification").includes("holdout-1"), "the anonymous scenario outcome disappeared");
} else if (input.state === "not_completed") {
  expect(page.text("#surla-cp4-verification").includes("Stopped during: Running the checks."), "the stopped stage misrepresents the checks");
}
expect(page.sent("POST", "/changes/" + input.change + "/verifications").length === 0, "reading proof started verification");
page.close();
`)
			})
		}
	}
}

// TestViewsPageEmptyVerificationNamesBuilder explains who starts checks instead
// of directing a browser user to an operation reserved for the builder.
func TestViewsPageEmptyVerificationNamesBuilder(t *testing.T) {
	cp := pageControlPlane(t, Options{})
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	mustPropose(t, cp, guide, "chg-empty-proof", "Route refunds to a manager.", refundAnalysis("Route refunds to a manager."))
	ps := startPageServer(t, newPageVerifications(cp))
	runPageScript(t, ps, nil, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-empty-proof", "the change did not open");
page.$("#surla-nav-cp4").click();
await until(() => page.$("#surla-cp4-empty"), "the empty proof state did not appear");
expect(page.text("#surla-cp4-empty").includes("The builder starts checks for a confirmed change"), "the empty state does not explain who starts checks");
expect(!page.text("#surla-cp4-empty").includes("verify_change"), "the browser user is directed to the builder-only operation");
page.close();
`)
}
