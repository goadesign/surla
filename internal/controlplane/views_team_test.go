// This file drives the Team controls through the served page and generated HTTP API.
package controlplane

import (
	"context"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/require"
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

// TestViewsPageTeamAndBuildUpdatesPreserveTheModelDraft keeps all four model
// inputs through the new update reasons and navigation without applying them.
// A rejected session then leaves the team facts labeled and its actions inert.
func TestViewsPageTeamAndBuildUpdatesPreserveTheModelDraft(t *testing.T) {
	f := &teamServiceFixture{snapshot: pageTeamSnapshot("working")}
	cp := newBuildControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: f}})
	cp.changesWait = 400 * time.Millisecond
	cp.changesPoll = 20 * time.Millisecond
	card := confirmedChange(t, cp, "chg")
	ps := startPageServer(t, cp)
	ps.act("start-build-and-attention", func() {
		_, err := cp.StartBuild(asCaller(pageOwner, CallerUser), &gensurla.StartBuildPayload{
			ChangeID: "chg", RequestRevision: card.RequestRevision,
		})
		require.NoError(t, err)
		f.mu.Lock()
		f.snapshot = pageTeamSnapshot("needs_you")
		f.mu.Unlock()
	})
	runPageScript(t, ps, nil, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-section")?.dataset.state === "not_started" && !page.$("#surla-build-loading"), "the initial selected build was not read");
page.$("#surla-nav-live").click();
await until(() => page.$("#surla-model-name"), "the model form did not open");
const provider = page.$("#surla-model-provider");
provider.value = "openai";
provider.dispatchEvent(new page.win.Event("change", { bubbles: true }));
const name = page.$("#surla-model-name");
page.type(name, "unsent-model-choice");
page.$("input[name='surla-model-kind'][value='gateway']").click();
const gateway = page.$("#surla-model-gateway");
page.type(gateway, "127.0.0.1:9455");
gateway.focus();
gateway.setSelectionRange(2, 7);
const buildReads = page.sent("GET", "/changes/chg/build").length;
await control(origin, "act", { key: "start-build-and-attention" });
await until(() => page.sent("GET", "/changes/chg/build").slice(buildReads).some((r) => r.status === 200 && JSON.parse(r.answer).build.state === "started") && page.$("#surla-team-attention-builder"), "background build and team changes did not reach Live");
// Let the completed owner answer render before checking the retained fields.
await sleep(0);
expect(page.$("#surla-model-name") === name && name.value === "unsent-model-choice", "a new update reason replaced the model input");
expect(page.$("#surla-model-provider") === provider && provider.value === "openai", "a new update reason lost the provider");
expect(page.$("#surla-model-gateway") === gateway && gateway.value === "127.0.0.1:9455" && page.doc.activeElement === gateway && gateway.selectionStart === 2 && gateway.selectionEnd === 7, "a new update reason changed gateway text, focus or selection");
expect(page.$("input[name='surla-model-kind'][value='gateway']").checked, "a new update reason changed the model kind");
page.$("#surla-nav-team").click();
await until(() => page.$("#surla-team-title"), "Team did not open");
page.$("#surla-nav-live").click();
await until(() => page.$("#surla-model-gateway"), "Live did not reopen");
expect(page.$("input[name='surla-model-kind'][value='gateway']").checked && page.$("#surla-model-provider").value === "openai" && page.$("#surla-model-name").value === "unsent-model-choice" && page.$("#surla-model-gateway").value === "127.0.0.1:9455", "the Team round-trip lost model draft fields");
expect(page.sent("POST", "/cp1/model").length === 0, "an update or navigation applied the unsent model draft");
page.$("#surla-nav-team").click();
await until(() => page.$("#surla-team-refresh") && !page.$("#surla-team-refresh").disabled, "Team did not become ready");
await control(origin, "reply", { key: "GET /team", status: 401, body: { name: "unauthorized", message: "Session ended." } });
page.$("#surla-team-refresh").click();
await until(() => page.$("#surla-checkpoint-root").dataset.session === "ended", "the refused session remained active");
expect(page.text("#surla-team-attention-builder").includes("Last read:"), "ended session shows retained attention as current");
for (const button of page.$$("#surla-team-stage button, #surla-team-attention button")) {
  expect(button.disabled, "ended session left an active Team control: " + button.id);
}
expect(page.$("#surla-team-role-builder").dataset.state === "needs_you", "ended session discarded the observed team");
expect(page.sent("POST", "/cp1/model").length === 0, "session end applied the model draft");
page.close();
`)
}
