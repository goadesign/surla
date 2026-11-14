// This file checks that the Build page assigns the next step to the right role.
package controlplane

import "testing"

// TestViewsPageBuildSentBackBelongsToTheTestWriter follows a preparation
// finding through the served page. A finding is an instruction to the writer,
// never a primary user action or evidence the builder has begun working.
func TestViewsPageBuildSentBackBelongsToTheTestWriter(t *testing.T) {
	ps, _ := pageGenerationFindings(t)
	runPageScript(t, ps, nil, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "Build did not become ready");
expect(page.text("#surla-build-state") === "Not built yet.", "the initial state must describe a build that has not started");
expect(!page.$("#surla-build-view-team").classList.contains("surla-btn-primary"), "team navigation must not be the primary action");
page.$("#surla-build-start").click();
await until(() => page.text("#surla-build-findings").includes("Finding from build 1.") && !page.$("#surla-build-loading"), "the saved finding and build read did not settle");
expect(page.text("#surla-build-state") === "Sent back to the test writer. It fixes this and reports again; nothing is yours to do unless it asks you in its pane.", "the state must identify who fixes the tests before showing findings");
const build = page.$("#surla-build-section");
expect(build.querySelector("p") === page.$("#surla-build-state"), "the saved step must be the first build explanation");
expect(page.text("#surla-build-findings").startsWith("What Surla told the test writer"), "writer instructions must be clearly attributed");
expect(build.querySelectorAll("button.surla-btn-primary").length === 0, "a sent-back test report has no primary user action");
expect(page.text("#surla-build-builder").includes("Assignment unread") && page.text("#surla-build-test-writer").includes("new findings are ready for it to read"), "the page must not infer work from a saved assignment");
const saved = JSON.parse(page.sent("POST", "/changes/chg/build").find((r) => r.status === 200).answer).build;
expect(page.text("#surla-build-details").includes(saved.started_at), "the exact start timestamp must remain in Build details");
expect(page.sent("POST", "/changes/chg/build").length === 1, "reading a finding must not rebuild the change");
page.close();
`)
}
