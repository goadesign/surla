// This file exercises Build actions through the served page's HTTP contract.
package controlplane

import (
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/require"
)

// TestViewsPageBuildRequiresASeparateRebuildConfirmation checks that saved
// assignments follow actual role reads and that replacing a current build
// requires a distinct confirming action without discarding an edited card.
// Git, agent windows and test generation are explicit fixture boundaries;
// this test claims page/HTTP proof, not candidate execution proof.
func TestViewsPageBuildRequiresASeparateRebuildConfirmation(t *testing.T) {
	f := &teamServiceFixture{snapshot: pageTeamSnapshot("working")}
	cp := newBuildControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: f}})
	cp.changesWait = 400 * time.Millisecond
	cp.changesPoll = 20 * time.Millisecond
	card := confirmedChange(t, cp, "chg")
	ps := startPageServer(t, cp)
	ps.act("builder-read", func() {
		_, err := cp.GetBuilderAssignment(asCaller(pageOwner, CallerBuilder), &gensurla.GetBuilderAssignmentPayload{ChangeID: "chg"})
		require.NoError(t, err)
	})
	runPageScript(t, ps, map[string]any{"revision": card.RequestRevision}, `
const path = "/changes/chg/build";
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "first Build action did not become available");
expect(page.sent("POST", path).length === 0, "opening the card started a build");
expect(page.text("#surla-build-start-explanation").includes("stored verification settings") && page.text("#surla-build-start-explanation").includes("outside the sandbox"), "pre-send explanation is missing");
await control(origin, "hold", { key: "POST " + path });
page.$("#surla-build-start").click();
await until(() => page.sent("POST", path).length === 1, "Build did not send a start");
expect(page.sent("POST", path)[0].body.request_revision === input.revision, "Build did not send the displayed revision");
expect(page.text("#surla-build-starting") === "Starting build… This can take a few minutes and continues if you close the page.", "pending work is not explained");
page.$("#surla-build-start").click();
expect(page.sent("POST", path).length === 1, "pending Build submitted twice");
await control(origin, "release", { key: "POST " + path });
await until(() => page.$("#surla-build-again") && !page.$("#surla-build-again").disabled, "saved Build did not appear");
expect(page.text("#surla-build-builder").includes("Assignment unread"), "page read marked the builder assignment read");
await control(origin, "act", { key: "builder-read" });
await until(() => page.text("#surla-build-builder").includes("Assignment read."), "real assignment read did not update the page");
const typed = page.$('textarea[data-example-id="check_01"][data-field="scenario"]');
expect(typed, "editable example is missing");
page.type(typed, "unsaved page text");
page.$("#surla-build-again").click();
await until(() => page.$("#surla-build-confirm"), "Build again did not ask first");
expect(page.text("#surla-build-question").includes("accepted again before verification"), "the rebuild question does not explain the verification consequence");
expect(page.sent("POST", path).length === 1, "opening rebuild confirmation sent a start");
page.$("#surla-build-confirm").dispatchEvent(new page.win.KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
await until(() => !page.$("#surla-build-confirm") && page.doc.activeElement === page.$("#surla-build-again"), "Escape did not cancel and restore focus");
page.$("#surla-build-again").click();
await until(() => page.$("#surla-build-cancel"), "rebuild question did not reopen");
page.$("#surla-build-cancel").click();
await until(() => !page.$("#surla-build-confirm"), "Cancel did not close the question");
expect(page.sent("POST", path).length === 1, "Cancel sent a start");
page.$("#surla-build-again").click();
await until(() => page.$("#surla-build-confirm"), "final rebuild question is missing");
page.$("#surla-build-confirm").click();
await until(() => page.sent("POST", path).length === 2 && page.$("#surla-build-again") && !page.$("#surla-build-again").disabled, "confirmed rebuild did not finish exactly once");
await until(() => page.text("#surla-build-builder").includes("Assignment unread"), "rebuild did not clear the assignment read");
expect(typed === page.$('textarea[data-example-id="check_01"][data-field="scenario"]') && typed.value === "unsaved page text", "build updates discarded the unsaved editor");
expect(page.sent("POST", path).length === 2, "one confirmed rebuild submitted twice");
page.close();
`)
}

// TestViewsPageBuildRetainsARefusalBesideItsSavedFacts keeps the owner's step
// visible after a successful recovery read and another Team status update.
func TestViewsPageBuildRetainsARefusalBesideItsSavedFacts(t *testing.T) {
	f := &teamServiceFixture{snapshot: pageTeamSnapshot("working")}
	cp := newBuildControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: f}})
	cp.changesWait = 400 * time.Millisecond
	cp.changesPoll = 20 * time.Millisecond
	confirmedChange(t, cp, "chg")
	ps := startPageServer(t, cp)
	ps.act("new-attention", func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.snapshot = pageTeamSnapshot("needs_you")
	})
	runPageScript(t, ps, nil, `
const path = "/changes/chg/build";
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "Build was not ready");
await control(origin, "reply", { key: "POST " + path, status: 409, body: { name: "working_copy_busy", id: "busy-start", message: "The current test check holds the working copy. Start the build after it ends.", fault: false, temporary: true, timeout: false } });
const reads = page.sent("GET", path).length;
page.$("#surla-build-start").click();
await until(() => page.$("#surla-build-action-error") && page.sent("GET", path).length > reads && !page.$("#surla-build-start").disabled, "the refusal and recovery read did not complete");
expect(page.text("#surla-build-action-error").includes("Build did not start.") && page.text("#surla-build-action-error").includes("Start the build after it ends."), "the refused action lost the owner's step");
expect(page.$("#surla-build-section").dataset.state === "not_started", "refusal invented a saved build");
await control(origin, "act", { key: "new-attention" });
await until(() => page.$("#surla-team-attention-builder"), "the independent Team update did not arrive");
expect(page.text("#surla-build-action-error").includes("Start the build after it ends."), "a later status update hid the refusal");
expect(page.sent("POST", path).length === 1, "a recovery read or Team update retried the effect");
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "the background build read did not settle before retry");
page.$("#surla-build-start").click();
await until(() => page.$("#surla-build-again") && !page.$("#surla-build-again").disabled, "an explicit retry did not build");
expect(!page.$("#surla-build-action-error"), "an explicit successful retry retained the refusal");
expect(page.sent("POST", path).length === 2, "explicit retry sent more than one start");
page.close();
`)
}
