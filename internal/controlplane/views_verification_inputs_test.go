// This file follows the list's owner-derived input match through the served
// page. Saved checker/run results are fixtures, not executed verification.
package controlplane

import (
	"strings"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/verify"
	"github.com/stretchr/testify/require"
)

// TestViewsPageSeparatesRecordedRunsFromCurrentInputs preserves a passing
// historical run while accepted tests change and the user refines agreement.
// Only matching inputs receive current-input presentation; old proof remains
// accessible with its own revision and the written-test notice.
func TestViewsPageSeparatesRecordedRunsFromCurrentInputs(t *testing.T) {
	runsDir := t.TempDir()
	cp := newBuildControlPlane(t, Options{
		RunsDir: runsDir, WrittenTestRuns: &WrittenTestRuns{ReportDir: t.TempDir(), Timeout: time.Minute},
		Team: &TeamBinding{Owner: pageOwner, Service: &teamServiceFixture{}},
	})
	cp.changesWait, cp.changesPoll = 400*time.Millisecond, 20*time.Millisecond
	user := asCaller(pageOwner, CallerUser)
	card := confirmedChange(t, cp, "chg-summary")
	_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{
		ChangeID: card.ChangeID, RequestRevision: card.RequestRevision,
	})
	require.NoError(t, err)
	const runID = "11111111111111111111111111111111"
	writeListedRun(t, runsDir, pageOwner, card, runID,
		time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC), true)
	setListedRunSource(t, runsDir, runID, verify.TestsWritten)
	saveListedTestState(t, cp, card.ChangeID, testsAccepted, strings.Repeat("2", 40))
	initial, err := cp.ListChanges(user)
	require.NoError(t, err)
	require.Len(t, initial.Changes, 1)
	require.NotNil(t, initial.Changes[0].LatestVerification)
	require.True(t, initial.Changes[0].LatestVerification.InputsCurrent, "the saved fixture must match before opening the page")
	ps := startPageServer(t, cp)
	ps.act("replace-tests", func() {
		saveListedTestState(t, cp, card.ChangeID, testsAccepted, strings.Repeat("3", 40))
	})
	ps.act("reuse-tests", func() {
		saveListedTestState(t, cp, card.ChangeID, testsAccepted, strings.Repeat("2", 40))
	})
	ps.act("refine", func() {
		_, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
			ChangeID: card.ChangeID, RequestRevision: card.RequestRevision, Answers: map[string]string{},
			Examples: append(card.Examples, &gensurla.ExampleItem{
				ID: "check_02", Scenario: "Refund over the limit", InputSummary: "Order total $50.01",
				ExpectedOutcome: "Waits for a manager", VerificationKind: "deterministic",
			}),
		})
		require.NoError(t, err)
	})
	runPageScript(t, ps, map[string]any{"run": runID, "revision": card.RequestRevision}, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor")?.dataset.changeId === "chg-summary", "the page did not finish opening its single change");
page.$("#surla-nav-changes").click();
const row = () => page.$('#surla-change-list [data-change-id="chg-summary"]');
const basis = () => row()?.querySelector("[data-inputs-current]");
const badge = () => Array.from(row().querySelectorAll(".shadcn-badge")).find((node) => node.textContent.startsWith("Latest saved run:"));
await until(() => basis()?.dataset.inputsCurrent === "true", "the matching owner inputs did not appear");
expect(row().textContent.includes("Latest saved run: passed") && basis().textContent.includes("Matches the agreed revision and selected tests."), "the recorded run or its narrow input match is missing");
expect(badge().classList.contains("shadcn-badge-success"), "the passing matching inputs lost their presentation");
expect(page.text("#surla-change-run-history").includes("recorded candidate"), "the page claims the working tree is verified");
await control(origin, "act", { key: "replace-tests" });
await until(() => basis()?.dataset.inputsCurrent === "false", "new accepted tests did not update the list through its existing wait");
expect(row().textContent.includes("Latest saved run: passed") && basis().textContent.includes("Earlier inputs") && !badge().classList.contains("shadcn-badge-success"), "the old test commit is presented as current proof or its run disappeared");
expect(row().textContent.includes(input.run), "the historical run ID was hidden");
await control(origin, "act", { key: "reuse-tests" });
await until(() => basis()?.dataset.inputsCurrent === "true", "reusing the recorded test commit did not restore the owner match");
await control(origin, "act", { key: "refine" });
await until(() => basis()?.dataset.inputsCurrent === "false", "a new agreed revision retained current-proof presentation");
expect(row().textContent.includes("Latest saved run: passed") && !badge().classList.contains("shadcn-badge-success"), "a historical verdict became this revision's result");
await until(() => page.$("#surla-open-change-chg-summary") && !page.$("#surla-open-change-chg-summary").disabled, "the changed agreement could not be opened");
page.$("#surla-open-change-chg-summary").click();
await until(() => page.$("#surla-cp1-editor")?.dataset.requestRevision !== input.revision, "the new agreement did not open");
page.$("#surla-nav-cp4").click();
await until(() => page.$("#surla-result-view").dataset.runId === input.run && page.$("#surla-cp4-test-source"), "the old saved proof could not be inspected");
expect(page.text("#surla-cp4-details").includes(input.revision), "the proof lost its recorded revision");
expect(page.text("#surla-cp4-test-source").includes("not isolated from the builder"), "inspecting history lost the written-test notice");
expect(page.sent("POST", "/changes/chg-summary/build").length === 0 && page.sent("POST", "/changes/verify").length === 0, "reading history started work");
page.close();
`)
}
