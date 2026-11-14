// This file checks the tab title through the served page and its real API calls.
package controlplane

import "testing"

// TestViewsPageTitleFollowsChangeDecisions checks the session's folder name,
// loading, list and decision steps without inventing build or approval readiness.
func TestViewsPageTitleFollowsChangeDecisions(t *testing.T) {
	f := &teamServiceFixture{snapshot: pageTeamSnapshot("working")}
	ps := startPageServer(t, pageControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: f}}))
	runPageScript(t, ps, nil, viewsPageScriptHelpers+`
const title = (page, step) => page.doc.title === step + ": Refunds café";
const bare = await openPage(origin);
await until(() => bare.$("#surla-signed-out"), "bare page did not finish opening");
expect(bare.doc.title === "Surla", "unauthenticated page invented a project name: " + bare.doc.title);
bare.close();
await control(origin, "hold", { key: "GET /changes" });
const page = await openPage(origin, { link: input.link });
await until(() => title(page, "Opening project"), "session did not identify its folder while opening");
await control(origin, "release", { key: "GET /changes" });
await until(() => title(page, "Ask guide to propose a change"), "empty project did not name its next step");
const proposed = await api(origin, "POST", "/changes/propose", {
  change_id: "chg-title", prompt: "Route refunds to a manager.",
  analysis: { summary: "Route refunds to a manager.", ambiguities: [], examples: [{ scenario: "Refund", input_summary: "$40", expected_outcome: "Approved", verification_kind: "deterministic" }], preserved_behaviors: [] },
}, input.guide);
expect(proposed.status === 200, "proposal failed: " + JSON.stringify(proposed));
await until(() => title(page, "Open a change"), "list did not name the open step");
page.$("#surla-open-change-chg-title").click();
await until(() => title(page, "Confirm change"), "unconfirmed change did not name confirmation");
page.$("#surla-confirm-submit").click();
await until(() => title(page, "Review change"), "confirmed change invented build or approval eligibility");
page.type(page.$('textarea[data-field="expected_outcome"]'), "Unsaved refinement");
await until(() => title(page, "Refine change"), "unsaved refinement did not reach the title");
const card = await readCard("chg-title");
await revise(card, "Route larger refunds to a manager.", "Manager review");
await until(() => title(page, "Review proposed change"), "waiting proposal did not outrank the draft");
page.close();
const reloaded = await openPage(origin);
await until(() => reloaded.$("#surla-signed-out"), "reload did not end the session");
expect(reloaded.doc.title === "Surla", "reload retained the session's project name");
reloaded.close();
`)
}

// TestViewsPageTitleRecoversTeamAndSession checks ordered role attention,
// stale and recovered reads, working transitions, and session-end precedence.
func TestViewsPageTitleRecoversTeamAndSession(t *testing.T) {
	f := &teamServiceFixture{snapshot: pageTeamSnapshot("needs_you")}
	f.snapshot.Roles[0].State = "needs_you"
	f.snapshot.Roles[0].Attention = "permission"
	// Deliberately reorder the wire result: the title follows role order.
	f.snapshot.Roles[0], f.snapshot.Roles[1] = f.snapshot.Roles[1], f.snapshot.Roles[0]
	cp := pageControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: f}})
	ps := startPageServer(t, cp)
	ps.act("working", func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.snapshot = pageTeamSnapshot("working")
	})
	ps.act("needs-you", func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.snapshot = pageTeamSnapshot("needs_you")
	})
	runPageScript(t, ps, nil, `
const title = (page, step) => page.doc.title === step + ": Refunds café";
const page = await openPage(origin, { link: input.link });
await until(() => title(page, "Guide needs you: permission"), "role order or project name is wrong: " + page.doc.title);
expect(!page.doc.title.includes("page-team"), "title used the sanitized team identity");
page.$("#surla-nav-team").click();
await until(() => page.$("#surla-team-refresh") && !page.$("#surla-team-refresh").disabled, "Team did not open");
await control(origin, "reply", { key: "GET /team", status: 503, body: { name: "unavailable", message: "Team unavailable." } });
page.$("#surla-team-refresh").click();
await until(() => title(page, "Refresh Team"), "failed read presented retained attention as current");
page.$("#surla-team-refresh").click();
await until(() => title(page, "Guide needs you: permission"), "recovered read did not restore current attention");
await control(origin, "act", { key: "working" });
await until(() => title(page, "Ask guide to propose a change"), "working role kept stale needs-you attention");
await control(origin, "act", { key: "needs-you" });
await until(() => title(page, "Builder needs you: question"), "second attention did not arrive");
await control(origin, "reply", { key: "GET /team", status: 401, body: { name: "unauthorized", message: "Session ended." } });
page.$("#surla-team-refresh").click();
await until(() => title(page, "Reopen project"), "ended session did not override retained facts");
page.close();
`)
}

// TestViewsPageTitleRecoversWaiting checks the notice-only synchronization path
// when the existing version wait fails and recovers without a new list version.
func TestViewsPageTitleRecoversWaiting(t *testing.T) {
	f := &teamServiceFixture{snapshot: pageTeamSnapshot("needs_you")}
	ps := startPageServer(t, pageControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: f}}))
	runPageScript(t, ps, nil, `
const page = await openPage(origin, { link: input.link });
await until(() => page.doc.title === "Builder needs you: question: Refunds café", "initial attention did not arrive");
await control(origin, "reply", { key: "GET /change-updates", status: 503, body: { name: "unavailable", message: "Wait unavailable." } });
await until(() => page.doc.title === "Reconnecting: Refunds café", "wait trouble did not update the tab title");
await until(() => page.doc.title === "Builder needs you: question: Refunds café", "wait recovery left stale recovery title", 10000);
page.close();
`)
}
