// This file drives the Team controls through the served page and generated HTTP API.
package controlplane

import (
	"context"
	"testing"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// pageTeamSnapshot supplies independent terminal, watcher and assignment facts.
// Each replacement is immutable after publication to the fixture's readers.
func pageTeamSnapshot(state string) gensurla.TeamSnapshot {
	return gensurla.TeamSnapshot{Team: "page-team", Roles: []*gensurla.TeamRoleState{
		{Role: "guide", Handle: "page-guide", State: "not_started", Attention: "none", Watcher: "unknown", Assignment: "unknown"},
		{Role: "builder", Handle: "page-builder", State: state, Attention: "question", Watcher: "gone", Assignment: "unread"},
		{Role: "test_writer", Handle: "page-writer", State: "unknown", Attention: "unknown", Watcher: "held", Assignment: "nothing_owed"},
	}}
}

// TestViewsPageTeamFollowsAttentionAndShowsPane proves that global attention
// follows the one version chain, pane actions accept real empty 204 answers,
// and a removed focused attention control returns focus to the current view.
func TestViewsPageTeamFollowsAttentionAndShowsPane(t *testing.T) {
	f := &teamServiceFixture{snapshot: pageTeamSnapshot("needs_you")}
	cp := pageControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: f}})
	ps := startPageServer(t, cp)
	ps.act("builder-working", func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.snapshot = pageTeamSnapshot("working")
	})
	runPageScript(t, ps, nil, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-team-attention-builder"), "builder attention is missing outside Team");
expect(page.text("#surla-team-attention-builder").includes("Builder needs you: question"), "attention reason is missing");
page.$("#surla-team-attention-show-pane-builder").click();
await until(() => page.sent("POST", "/team/pane").some((r) => r.status === 204), "Show pane did not complete with 204");
expect(page.sent("POST", "/team/pane").length === 1, "Show pane was repeated");
expect(page.sent("POST", "/team/pane")[0].body.role === "builder", "Show pane selected a different role");
await until(() => !page.$("#surla-team-attention-show-pane-builder").disabled, "Show pane remained pending after empty success");
expect(page.sent("POST", "/team/agents").length === 0, "a read or pane action started agents");
page.$("#surla-nav-team").click();
await until(() => page.state() === "team" && page.$("#surla-team-role-builder"), "Team did not open");
expect(page.text("#surla-team-role-builder").includes("Chorus watcher absent") && page.text("#surla-team-role-builder").includes("Assignment unread"), "independent facts were inferred from terminal state");
expect(page.text("#surla-team-role-test_writer").includes("Status unknown") && page.text("#surla-team-role-test_writer").includes("Chorus watcher held"), "unknown terminal hid a known watcher");
expect(page.doc.activeElement === page.$("#surla-team-title"), "opening Team did not focus its title");
page.$("#surla-team-attention-show-pane-builder").focus();
await control(origin, "act", { key: "builder-working" });
await until(() => !page.$("#surla-team-attention-builder"), "attention did not clear through updates");
await until(() => page.$("#surla-team-role-builder").dataset.state === "working", "Team did not follow the same update");
await until(() => page.doc.activeElement === page.$("#surla-team-title"), "removed attention control lost focus");
const teamReads = page.sent("GET", "/team").length;
expect(teamReads <= page.sent("GET", "/changes").length + 2, "Team introduced an independent polling loop");
page.close();
`)
}

// TestViewsPageTeamStartsExplicitlyAndRetainsFailedFacts checks an explicit
// start receipt and a failed read without replacing known facts with emptiness.
func TestViewsPageTeamStartsExplicitlyAndRetainsFailedFacts(t *testing.T) {
	f := &teamServiceFixture{snapshot: pageTeamSnapshot("needs_you")}
	f.start = func(context.Context) (*gensurla.ProjectTeam, error) {
		f.mu.Lock()
		f.snapshot = pageTeamSnapshot("working")
		f.mu.Unlock()
		return &gensurla.ProjectTeam{Team: "page-team", Space: "page-space", Dialog: "opened", Step: "Choose the builder in the open team dialog.", Roles: []*gensurla.ProjectTeamRole{}}, nil
	}
	cp := pageControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: f}})
	ps := startPageServer(t, cp)
	runPageScript(t, ps, nil, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-team-attention-builder"), "initial team was not read");
page.$("#surla-nav-team").click();
await until(() => page.$("#surla-team-start"), "Start agents is missing");
expect(page.sent("POST", "/team/agents").length === 0, "opening Team started agents");
await control(origin, "reply", { key: "GET /team", status: 503, body: { name: "unavailable", message: "Team owner unavailable." } });
page.$("#surla-team-refresh").click();
await until(() => page.$("#surla-team-read-error"), "read failure was not shown");
expect(page.text("#surla-team-stale").includes("Last-read"), "retained facts are presented as current");
expect(page.text("#surla-team-attention-builder").includes("Last read:"), "global attention is presented as current after read failure");
expect(page.$("#surla-team-role-builder").dataset.state === "needs_you", "failed read discarded known facts");
page.$("#surla-team-refresh").click();
await until(() => !page.$("#surla-team-read-error"), "retry did not recover");
expect(!page.text("#surla-team-attention-builder").includes("Last read:"), "recovered facts remain marked stale");
page.$("#surla-team-start").click();
await until(() => page.$("#surla-team-start-receipt"), "explicit start receipt is missing");
expect(page.text("#surla-team-start-receipt").includes("This start (begun ") && page.text("#surla-team-start-receipt").includes("Choose the builder in the open team dialog."), "receipt is not dated owner text");
await until(() => page.$("#surla-team-role-builder").dataset.state === "working" && !page.$("#surla-team-attention-builder"), "current facts were replaced by the historical start receipt");
expect(page.sent("POST", "/team/agents").length === 1, "one click did not send one start");
page.close();
`)
}
