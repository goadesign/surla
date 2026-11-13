// This file tests Build recovery through the served page and generated HTTP API.
// Working copies, generation and agent windows are explicitly simulated by the
// existing build fixture; these tests do not prove candidate execution.
package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/require"
)

// TestViewsPageBuildIgnoresThePreviousChangesDelayedRead holds a real build
// answer while the user opens a different change with a saved build. Releasing
// the older answer must not replace the selected change's facts or controls.
func TestViewsPageBuildIgnoresThePreviousChangesDelayedRead(t *testing.T) {
	f := &teamServiceFixture{snapshot: pageTeamSnapshot("working")}
	cp := newBuildControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: f}})
	cp.changesWait = 400 * time.Millisecond
	cp.changesPoll = 20 * time.Millisecond
	confirmedChange(t, cp, "chg-first")
	second := confirmedChange(t, cp, "chg-second")
	started, err := cp.StartBuild(asCaller(pageOwner, CallerUser), &gensurla.StartBuildPayload{
		ChangeID: second.ChangeID, RequestRevision: second.RequestRevision,
	})
	require.NoError(t, err)
	ps := startPageServer(t, cp)

	runPageScript(t, ps, map[string]any{"revision": second.RequestRevision, "started": started.Build.StartedAt}, `
const firstPath = "/changes/chg-first/build";
await control(origin, "answer-later", { key: "GET " + firstPath });
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-open-change-chg-first"), "the change list did not open");
page.$("#surla-open-change-chg-first").click();
await until(() => page.sent("GET", firstPath).length === 1 && page.$("#surla-build-loading"), "the first build read was not held");
page.$("#surla-nav-changes").click();
page.$("#surla-open-change-chg-second").click();
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-second" && page.$("#surla-build-again") && !page.$("#surla-build-again").disabled, "the second change's current build did not open");
expect(page.text("#surla-build-details").includes(input.revision) && page.text("#surla-build-details").includes(input.started), "the selected build's revision and start are missing");
const replaced = [];
const observer = new page.win.MutationObserver(() => {
  if (page.$("#surla-build-start") || page.$("#surla-build-error") || page.$("#surla-build-section").dataset.state !== "started") replaced.push(page.text("#surla-build-section"));
});
observer.observe(page.$("#surla-build-section"), { childList: true, attributes: true, subtree: true });
await control(origin, "release", { key: "GET " + firstPath });
await until(() => page.sent("GET", firstPath)[0].status === 200, "the delayed answer never arrived");
// Let the delivered answer's promise and React update finish in the next task.
await sleep(0);
observer.disconnect();
expect(replaced.length === 0, "the previous change's delayed answer replaced the current build: " + JSON.stringify(replaced));
expect(page.text("#surla-build-details").includes(input.started), "the selected build lost its saved start");
expect(page.sent("POST", firstPath).length === 0 && page.sent("POST", "/changes/chg-second/build").length === 0, "navigation started a build");
page.close();
`)
}

// TestViewsPageBuildWaitsForConfirmationAndARecoveredRead distinguishes an
// unconfirmed card, an unread build and a failed read from a saved absent build.
// Only a successful retry can enable the confirmed card's first Build action.
func TestViewsPageBuildWaitsForConfirmationAndARecoveredRead(t *testing.T) {
	f := &teamServiceFixture{snapshot: pageTeamSnapshot("working")}
	cp := newBuildControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: f}})
	cp.changesWait = 400 * time.Millisecond
	cp.changesPoll = 20 * time.Millisecond
	confirmedChange(t, cp, "chg-confirmed")
	mustPropose(t, cp, asCaller(pageOwner, CallerGuide), "chg-unconfirmed", "Route refunds to a manager.", refundAnalysis("Route refunds to a manager."))
	ps := startPageServer(t, cp)

	runPageScript(t, ps, nil, `
const path = "/changes/chg-confirmed/build";
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-open-change-chg-unconfirmed"), "the change list did not open");
page.$("#surla-open-change-chg-unconfirmed").click();
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-unconfirmed", "the unconfirmed card did not open");
expect(!page.$("#surla-build-start") && !page.$("#surla-build-again") && !page.$("#surla-build-confirm"), "an unconfirmed card offers Build");
await control(origin, "hold", { key: "GET " + path });
await control(origin, "reply", { key: "GET " + path, status: 503, body: { name: "unavailable", id: "build-read", message: "Build owner unavailable.", fault: false, temporary: true, timeout: false } });
page.$("#surla-nav-changes").click();
page.$("#surla-open-change-chg-confirmed").click();
await until(() => page.$("#surla-build-loading") && page.sent("GET", path).length === 1, "the unread build was not held");
expect(!page.$("#surla-build-start") && !page.$("#surla-build-again"), "an unread build offers an unsafe start");
expect(page.$("#surla-build-section").dataset.state === "unread", "an unread build is presented as absent");
await control(origin, "release", { key: "GET " + path });
await until(() => page.$("#surla-build-error") && page.$("#surla-build-refresh") && !page.$("#surla-build-refresh").disabled, "the failed build read did not offer retry");
expect(page.text("#surla-build-error").includes("Build owner unavailable."), "the owner failure is missing");
expect(!page.$("#surla-build-start") && !page.$("#surla-build-again"), "a failed initial read offers an unsafe start");
page.$("#surla-build-refresh").click();
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "retry did not recover the first Build action");
expect(!page.$("#surla-build-error") && page.$("#surla-build-section").dataset.state === "not_started", "retry did not establish the saved absent-build state");
expect(page.sent("POST", path).length === 0, "confirmation checks, reads or retry started a build");
page.$("#surla-build-start").click();
await until(() => page.$("#surla-build-again") && !page.$("#surla-build-again").disabled, "the recovered Build action did not start successfully");
expect(page.sent("POST", path).length === 1, "the recovered action did not submit exactly once");
page.close();
`)
}

// TestViewsPageBuildWaitsForTheListedAgreementBeforeCardReadCompletes holds
// the detailed card read after the list announces a new agreement. The older
// displayed card must stop offering Build before the detailed answer arrives.
func TestViewsPageBuildWaitsForTheListedAgreementBeforeCardReadCompletes(t *testing.T) {
	f := &teamServiceFixture{snapshot: pageTeamSnapshot("working")}
	cp := newBuildControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: f}})
	cp.changesWait = 400 * time.Millisecond
	cp.changesPoll = 20 * time.Millisecond
	first := confirmedChange(t, cp, "chg")
	ps := startPageServer(t, cp)
	ps.act("refine-agreement", func() {
		_, err := cp.ConfirmIntent(asCaller(pageOwner, CallerUser), &gensurla.ConfirmIntentPayload{
			ChangeID: first.ChangeID, RequestRevision: first.RequestRevision, Answers: map[string]string{},
			Examples: []*gensurla.ExampleItem{{
				ID: first.Examples[0].ID, Scenario: first.Examples[0].Scenario,
				InputSummary:    first.Examples[0].InputSummary,
				ExpectedOutcome: "A manager reviews the refund", VerificationKind: "deterministic",
			}},
		})
		require.NoError(t, err)
	})

	runPageScript(t, ps, map[string]any{"revision": first.RequestRevision}, `
const path = "/changes/chg/build";
const cardPath = "/changes/chg";
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "the initial agreement's Build did not become available");
const cardReads = page.sent("GET", cardPath).length;
await control(origin, "hold", { key: "GET " + cardPath });
await control(origin, "act", { key: "refine-agreement" });
await until(() => page.sent("GET", "/changes").some((r) => r.status === 200 && JSON.parse(r.answer).changes.some((c) => c.change_id === "chg" && c.request_revision !== input.revision)) && page.sent("GET", cardPath).length > cardReads, "the new list agreement did not reach the page before its held card read");
await until(() => !page.$("#surla-build-loading"), "the independent build read did not settle");
expect(page.$("#surla-cp1-editor").dataset.requestRevision === input.revision, "the detailed card read was not held");
// Clicking the old DOM control must also be harmless if it remains rendered.
const oldBuild = page.$("#surla-build-start");
if (oldBuild) oldBuild.click();
expect(page.sent("POST", path).length === 0, "Build sent the old displayed revision after the list announced a new agreement");
expect(!oldBuild || oldBuild.disabled, "the old displayed agreement still offers Build while its replacement card is unread");
await control(origin, "release", { key: "GET " + cardPath });
await until(() => page.$("#surla-cp1-editor").dataset.requestRevision !== input.revision && page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "reading the new agreement did not recover Build");
expect(page.sent("POST", path).length === 0, "reading the new agreement started a build");
page.close();
`)
}

// TestViewsPageBuildUsesTheNewAgreementAndKeepsDraftAndProposal covers the
// earlier-build recovery and then rebuilding the current agreement. Draft text
// and a waiting proposal retain their own actions and survive the rebuild.
// Navigating away also dismisses the separate rebuild confirmation question.
func TestViewsPageBuildUsesTheNewAgreementAndKeepsDraftAndProposal(t *testing.T) {
	f := &teamServiceFixture{snapshot: pageTeamSnapshot("working")}
	cp := newBuildControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: f}})
	cp.changesWait = 400 * time.Millisecond
	cp.changesPoll = 20 * time.Millisecond
	first := confirmedChange(t, cp, "chg")
	_, err := cp.StartBuild(asCaller(pageOwner, CallerUser), &gensurla.StartBuildPayload{
		ChangeID: first.ChangeID, RequestRevision: first.RequestRevision,
	})
	require.NoError(t, err)
	second, err := cp.ConfirmIntent(asCaller(pageOwner, CallerUser), &gensurla.ConfirmIntentPayload{
		ChangeID: first.ChangeID, RequestRevision: first.RequestRevision, Answers: map[string]string{},
		Examples: []*gensurla.ExampleItem{{
			ID: first.Examples[0].ID, Scenario: first.Examples[0].Scenario,
			InputSummary:    first.Examples[0].InputSummary,
			ExpectedOutcome: "A manager reviews the refund", VerificationKind: "deterministic",
		}},
	})
	require.NoError(t, err)
	require.NotEqual(t, first.RequestRevision, second.RequestRevision)
	ps := startPageServer(t, cp)

	runPageScript(t, ps, map[string]any{"revision": second.RequestRevision}, viewsPageScriptHelpers+`
const path = "/changes/chg/build";
const primary = (element) => element.classList.contains("surla-btn-primary");
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "the earlier build did not offer a direct current build");
expect(page.text("#surla-build-start") === "Build current agreed revision", "the earlier build's recovery action is unclear");
expect(page.text("#surla-build-section").includes("Built for an earlier revision."), "the saved build is presented as current");
expect(primary(page.$("#surla-build-start")) && !primary(page.$("#surla-build-view-team")), "the direct current Build is not the primary action");
page.$("#surla-build-start").click();
expect(!page.$("#surla-build-confirm"), "building a new agreement asked to restart the earlier build");
await until(() => page.$("#surla-build-again") && !page.$("#surla-build-again").disabled, "the current agreement did not finish building");
expect(page.sent("POST", path).length === 1 && page.sent("POST", path)[0].body.request_revision === input.revision, "the direct action did not build the current agreed revision");
expect(primary(page.$("#surla-build-view-team")) && !primary(page.$("#surla-build-again")), "a current build does not prioritize View team over rebuilding");
const field = page.$('textarea[data-example-id="check_01"][data-field="scenario"]');
page.type(field, "Unsaved scenario on the agreed revision");
expect(primary(page.$("#surla-confirm-submit")) && !primary(page.$("#surla-build-view-team")), "an unsaved edit does not retain refinement priority");
const agreed = await readCard("chg");
await revise(agreed, "Refunds up to $60 are instant.", "A manager reviews the refund");
await until(() => page.$("#surla-proposal"), "the waiting proposal did not appear");
const answer = page.$('#surla-proposal [data-option="total_with_tax"]');
expect(answer, "the proposal's question is missing");
answer.click();
expect(primary(page.$("#surla-proposal-adopt")) && !primary(page.$("#surla-build-view-team")) && !primary(page.$("#surla-build-again")), "the waiting proposal lost action priority to Build");
await until(() => page.$("#surla-build-again") && !page.$("#surla-build-again").disabled, "the proposal's background build read did not settle before rebuilding");
page.$("#surla-build-again").click();
await until(() => page.$("#surla-build-confirm"), "rebuilding the current agreement did not ask for confirmation");
expect(page.sent("POST", path).length === 1, "opening the current rebuild question sent a start");
page.$("#surla-build-confirm").click();
await until(() => page.sent("POST", path).length === 2 && page.$("#surla-build-again") && !page.$("#surla-build-again").disabled, "the confirmed current rebuild did not finish");
expect(page.sent("POST", path)[1].body.request_revision === input.revision, "the rebuild used the unadopted proposal");
expect(field === page.$('textarea[data-example-id="check_01"][data-field="scenario"]') && field.value === "Unsaved scenario on the agreed revision", "the rebuild discarded or replaced the unsaved editor");
expect(page.$('#surla-proposal [data-option="total_with_tax"]').getAttribute("aria-pressed") === "true", "the rebuild discarded the proposal answer");
expect(page.$("#surla-proposal") && !page.$("#surla-proposal-adopt").disabled && page.$("#surla-confirm-submit").disabled, "the rebuild changed the waiting proposal's available actions");
const after = await readCard("chg");
expect(after.request_revision === input.revision && after.proposed_revision && after.examples[0].scenario !== field.value, "the rebuild saved draft text or adopted the proposal");
page.$("#surla-build-again").click();
await until(() => page.$("#surla-build-confirm"), "the next rebuild question did not open");
page.$("#surla-build-view-team").click();
await until(() => page.state() === "team" && !page.$("#surla-build-confirm"), "opening Team left the rebuild question active beside the retained draft");
expect(page.sent("POST", path).length === 2, "navigating away confirmed a rebuild");
page.$("#surla-nav-cp1").click();
await until(() => page.state() === "cp1-change-card" && page.$("#surla-build-again") && !page.$("#surla-build-again").disabled, "returning to the decisions did not restore Build again");
expect(!page.$("#surla-build-confirm"), "returning to the decisions restored an old rebuild question");
expect(field === page.$('textarea[data-example-id="check_01"][data-field="scenario"]') && field.value === "Unsaved scenario on the agreed revision" && page.$('#surla-proposal [data-option="total_with_tax"]').getAttribute("aria-pressed") === "true", "dismissing the rebuild question discarded draft text or the proposal answer");
page.$("#surla-build-again").click();
await until(() => page.$("#surla-build-confirm"), "a fresh Build again did not ask for confirmation");
expect(page.sent("POST", path).length === 2, "reopening the rebuild question sent a start");
page.$("#surla-build-cancel").click();
await until(() => !page.$("#surla-build-confirm"), "the fresh rebuild question did not cancel");
page.close();
`)
}

// TestViewsPageBuildTeamRecoveryClearsTheOldInstructionEverywhere proves a
// later successful Start agents supersedes a busy-window Build receipt in both
// the card and global status, while retaining that Build's working-copy history.
func TestViewsPageBuildTeamRecoveryClearsTheOldInstructionEverywhere(t *testing.T) {
	f := &teamServiceFixture{snapshot: pageTeamSnapshot("working")}
	f.start = func(context.Context) (*gensurla.ProjectTeam, error) {
		return nil, gensurla.MakeWindowBusy(errors.New("simulated open agent dialog"))
	}
	cp := newBuildControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: f}})
	cp.changesWait = 400 * time.Millisecond
	cp.changesPoll = 20 * time.Millisecond
	confirmedChange(t, cp, "chg")
	ps := startPageServer(t, cp)
	ps.act("allow-team-start", func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.start = func(context.Context) (*gensurla.ProjectTeam, error) {
			return &gensurla.ProjectTeam{
				Team: "page-team", Space: "page-space", Dialog: "opened",
				Step: "Choose the builder in the newly opened team dialog.", Roles: []*gensurla.ProjectTeamRole{},
			}, nil
		}
	})

	runPageScript(t, ps, nil, `
const path = "/changes/chg/build";
const oldStep = "Close the open Herdr dialog, then start the team agents.";
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "the first Build did not become available");
page.$("#surla-build-start").click();
await until(() => page.$("#surla-build-start-team-receipt") && page.$("#surla-build-again") && !page.$("#surla-build-again").disabled, "the busy-window build did not finish");
expect(page.text("#surla-build-start-team-receipt").includes("the build was saved and the team window was busy") && page.text("#surla-build-start-team-receipt").includes(oldStep), "the busy-window result was not shown as a historical receipt");
const copyReceipt = page.text("#surla-build-start-copy-receipt");
expect(copyReceipt.includes("the working copy was at the project commit"), "the Build's working-copy history is missing");
await control(origin, "act", { key: "allow-team-start" });
page.$("#surla-build-view-team").click();
await until(() => page.state() === "team" && page.$("#surla-team-start") && !page.$("#surla-team-start").disabled, "View team did not open the recovery action");
expect(page.sent("POST", "/team/agents").length === 0, "opening Team started agents implicitly");
page.$("#surla-team-start").click();
await until(() => page.$("#surla-team-start-receipt") && page.text("#surla-team-start-receipt").includes("Choose the builder in the newly opened team dialog."), "the successful Team start receipt did not appear");
expect(!page.doc.body.textContent.includes(oldStep), "the superseded Build instruction remained visible in Team");
page.$("#surla-nav-cp1").click();
await until(() => page.$("#surla-build-start-copy-receipt"), "returning to the card lost the Build history");
expect(!page.$("#surla-build-start-team-receipt") && !page.doc.body.textContent.includes(oldStep), "the old busy-window instruction returned on the card or global status");
expect(page.text("#surla-build-start-copy-receipt") === copyReceipt, "Team recovery rewrote the Build's working-copy history");
page.$("#surla-nav-team").click();
await until(() => page.$("#surla-team-start-receipt"), "the latest Team receipt was lost on navigation");
expect(!page.doc.body.textContent.includes(oldStep), "the old Build instruction returned after reopening Team");
expect(page.sent("POST", path).length === 1 && page.sent("POST", "/team/agents").length === 1, "Team recovery repeated Build or Start agents");
page.close();
`)
}
