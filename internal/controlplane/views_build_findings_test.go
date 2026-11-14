// This file checks findings-read correlation and recovery through the real page.
// Generation is simulated here; the separate real-report test proves checking.
package controlplane

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/testwriter"
)

// pageGenerationFindings creates actual saved generation verdicts with distinct
// text for each start, using only the existing simulated-copy boundary.
func pageGenerationFindings(t *testing.T) (*pageServer, *gensurla.ChangeCardResult) {
	t.Helper()
	team := &teamServiceFixture{snapshot: pageTeamSnapshot("working")}
	cp := newBuildControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: team}})
	cp.changesWait = 400 * time.Millisecond
	cp.changesPoll = 20 * time.Millisecond
	var starts atomic.Int32
	copy := &buildCopyFixture{folder: t.TempDir(), prepare: func(context.Context, []designeval.StorySummary) error {
		return &testwriter.AuthorError{Finding: testwriter.Finding{
			Kind: testwriter.FindingGeneration, Message: fmt.Sprintf("Finding from build %d.", starts.Add(1)),
		}}
	}}
	cp.buildCopyFactory = func(context.Context, string, scaffold.Layout) (buildCopy, error) { return copy, nil }
	card := confirmedChange(t, cp, "chg")
	return startPageServer(t, cp), card
}

// TestViewsPageIgnoresFindingTextFromThePreviousBuild delivers the first build's
// findings after a second start. It also proves assignment-read changes do not
// refetch the same verdict's text or introduce another polling loop.
func TestViewsPageIgnoresFindingTextFromThePreviousBuild(t *testing.T) {
	ps, _ := pageGenerationFindings(t)
	runPageScript(t, ps, nil, `
const buildPath = "/changes/chg/build";
const findingsPath = buildPath + "/findings";
await control(origin, "answer-later", { key: "GET " + findingsPath });
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "first Build was not ready");
page.$("#surla-build-start").click();
await until(() => page.$("#surla-build-again") && !page.$("#surla-build-again").disabled && page.sent("GET", findingsPath).length === 1, "the first finding read was not held");
expect(!page.text("#surla-build-findings").includes("Finding from build 1."), "held finding text appeared before its answer");
const leaked = [];
const observer = new page.win.MutationObserver(() => {
  if (page.text("#surla-build-section").includes("Finding from build 1.")) leaked.push("old finding shown");
});
observer.observe(page.$("#surla-build-section"), { subtree: true, childList: true, characterData: true });
page.$("#surla-build-again").click();
await until(() => page.$("#surla-build-confirm") && !page.$("#surla-build-confirm").disabled, "rebuild did not offer an enabled confirmation");
page.$("#surla-build-confirm").click();
await until(() => page.sent("POST", buildPath).length === 2 && page.sent("POST", buildPath)[1].status === 200 && page.$("#surla-build-again") && !page.$("#surla-build-again").disabled, "second Build did not finish");
await control(origin, "release", { key: "GET " + findingsPath });
await until(() => page.text("#surla-build-findings").includes("Finding from build 2."), "the second verdict's user text did not appear");
observer.disconnect();
expect(leaked.length === 0, "the previous build's late finding flashed in the current view");
const answers = page.sent("GET", findingsPath);
expect(answers.length === 2 && JSON.parse(answers[0].answer).findings[0].includes("build 1") && JSON.parse(answers[1].answer).findings[0].includes("build 2"), "the test did not deliver two different owner answers");
const current = JSON.parse(page.sent("POST", buildPath)[1].answer).build;
expect(JSON.parse(answers[1].answer).started_at === current.started_at, "the shown text belongs to another start");
const read = await api(origin, "GET", "/assignments/builder/chg", undefined, input.builder);
expect(read.status === 200, "the builder could not read its assignment");
await until(() => page.text("#surla-build-builder").includes("Assignment read."), "the assignment read did not refresh Build");
expect(page.sent("GET", findingsPath).length === 2, "an unchanged verdict was read again after an assignment change");
page.close();
`)
}

// TestViewsPageFindingsRequireTheirExactOwnerBasis rejects each correlation
// mismatch and a failed user read without discarding the saved build. A user
// retry reads the actual owner result, rather than using the rejected text.
func TestViewsPageFindingsRequireTheirExactOwnerBasis(t *testing.T) {
	for _, field := range []string{"change_id", "request_revision", "started_at", "report", "checked_at", "count", "read_failure"} {
		t.Run(field, func(t *testing.T) {
			ps, _ := pageGenerationFindings(t)
			runPageScript(t, ps, map[string]any{"field": field}, `
const buildPath = "/changes/chg/build";
const findingsPath = buildPath + "/findings";
await control(origin, "answer-later", { key: "POST " + buildPath });
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "Build was not ready");
page.$("#surla-build-start").click();
await until(() => page.sent("POST", buildPath).length === 1, "Build was not sent");
// Read the saved verdict while the user's POST answer is held, so every
// non-mutated correlation field comes from the actual owner, not a guess.
let saved = null;
const deadline = Date.now() + 10000;
while (saved === null) {
  const read = await api(origin, "GET", buildPath, undefined, input.builder);
  expect(read.status === 200, "the build owner read failed");
  if (read.body.build.state === "started" && read.body.build.test_writer.tests.state === "needs_changes") saved = read.body.build;
  else {
    expect(Date.now() < deadline, "Build never saved its generation verdict");
    await sleep(10);
  }
}
const result = { change_id: "chg", request_revision: saved.request_revision, started_at: saved.started_at,
  report: saved.test_writer.report, checked_at: saved.test_writer.tests.checked_at, findings: ["Wrong correlated text must not render."] };
if (input.field === "count") result.findings.push("Another wrong finding.");
else if (input.field === "report") result.report++;
else if (input.field === "started_at" || input.field === "checked_at") result[input.field] = "2026-01-01T00:00:00Z";
else if (input.field !== "read_failure") result[input.field] += "-different";
await control(origin, "reply", input.field === "read_failure"
  ? { key: "GET " + findingsPath, status: 503, body: { name: "unavailable", id: "finding-read", message: "Finding owner unavailable.", fault: false, temporary: true, timeout: false } }
  : { key: "GET " + findingsPath, status: 200, body: result });
await control(origin, "release", { key: "POST " + buildPath });
await until(() => page.$("#surla-build-findings-error") && page.$("#surla-build-findings-retry") && !page.$("#surla-build-findings-retry").disabled, "a mismatched or failed findings read did not offer recovery");
expect(page.$("#surla-build-section").dataset.state === "started" && !page.text("#surla-build-section").includes("Wrong correlated text"), "a failed findings read replaced the build or rendered unbound text");
expect(page.text("#surla-build-findings-error").includes(input.field === "read_failure" ? "Finding owner unavailable." : "no longer match this build"), "the current error does not explain the findings failure");
expect(page.sent("GET", findingsPath).length === 1, "a failed findings read was retried automatically");
page.$("#surla-build-findings-retry").click();
await until(() => page.text("#surla-build-findings").includes("Finding from build 1.") && !page.$("#surla-build-findings-error"), "an explicit retry did not read the owner text");
expect(page.sent("GET", findingsPath).length === 2 && page.sent("POST", buildPath).length === 1, "findings recovery repeated an effect or read");
page.close();
`)
		})
	}
}
