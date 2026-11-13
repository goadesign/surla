// This file drives the views page's Story stage through the real generated
// HTTP API: the Story saved with the open change, the flow and draft counts
// the confirm view states, the owner's current Story evaluation in each of
// its states, and how a draft that differs from the change is named.
package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
)

// TestViewsPageStoryStageDrawsTheOpenChangesStory opens a change saved with
// a Story and checks the confirm view's counts and link, then the Story
// stage: every flow in order as a row of step cards with role, summary and
// kind, complete and draft marks with the reasons the data gives, read-only
// open questions, keyboard-reachable controls with names, rows that scroll
// inside the stage, and that the identical current draft is called current.
func TestViewsPageStoryStageDrawsTheOpenChangesStory(t *testing.T) {
	cp, ps := storyPageServer(t)
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	op := seedStoryRefreshForTest(t, cp, pageOwner, storyRefreshStateSucceeded, pageStories(), "")
	proposeWithStory(t, cp, guide, "chg-story", op)
	runPageScript(t, ps, nil, viewsPageScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-story", "the change did not open");
const confirmStory = page.text("#surla-confirm-story");
expect(confirmStory.includes("This change's Story has 3 flows, 2 of them drafts that will not be tested."), "the confirm view does not state the flow and draft counts: " + confirmStory);
expect(!page.$("#surla-confirm-submit").disabled, "drafts disabled confirming");
checkControl(page, page.$("#surla-confirm-open-story"), "the confirm view's link to the Story");
page.$("#surla-confirm-open-story").click();
await until(() => page.$("#surla-story-stage") && page.$("#surla-story-draft").dataset.state === "succeeded", "the Story stage did not open with the current draft");
expect(page.$("#surla-nav-story").getAttribute("aria-current") === "page", "the Story tab is not marked current");
checkControl(page, page.$("#surla-nav-story"), "Story tab");
const context = page.text("#surla-workspace-context");
expect(context.includes("Story") && context.includes("chg-story") && context.includes("current draft succeeded"), "the context strip does not name the Story shown: " + context);

const flows = page.$$("#surla-story-change .surla-story-flow");
expect(JSON.stringify(flows.map((f) => f.dataset.flowKey)) === JSON.stringify(["instant-refund", "high-value-refund", "partial-refund"]), "flows are not drawn in order: " + flows.map((f) => f.dataset.flowKey));
const mark = (flow) => flow.querySelector(".surla-story-mark").textContent;
const reasons = (flow) => Array.from(flow.querySelectorAll(".surla-story-reasons > li")).map((li) => li.textContent);
const cards = (flow) => Array.from(flow.querySelectorAll(".surla-story-step")).map((s) => [s.querySelector(".surla-story-actor").textContent, s.querySelector(".surla-story-summary").textContent, s.querySelector(".surla-story-kind").textContent]);

expect(flows[0].dataset.complete === "true" && mark(flows[0]) === "will be tested end to end" && reasons(flows[0]).length === 0, "the complete flow is not marked as tested end to end");
expect(JSON.stringify(cards(flows[0])) === JSON.stringify([["Customer", "Customer requests a $25 refund", "Starts the flow"], ["Customer", "Store credit is issued immediately", "Outcome"]]), "the complete flow's steps are wrong: " + JSON.stringify(cards(flows[0])));

expect(flows[1].dataset.complete === "false" && mark(flows[1]) === "draft: not tested", "the draft flow is not marked as untested");
expect(JSON.stringify(reasons(flows[1])) === JSON.stringify(["No step is an outcome.", "Open questions: Is $50.00 itself instant or manager-reviewed?"]), "the draft's reasons are wrong: " + JSON.stringify(reasons(flows[1])));
expect(JSON.stringify(cards(flows[1])) === JSON.stringify([["Customer", "Customer requests a $120 refund", "Starts the flow"], ["Support Manager", "Manager reviews the refund request", "Step"]]), "the draft's steps are wrong: " + JSON.stringify(cards(flows[1])));
const stepQuestion = flows[1].querySelector('.surla-story-step [data-question-key="limit-boundary"]');
expect(stepQuestion && stepQuestion.textContent.includes("Is $50.00 itself instant or manager-reviewed?") && JSON.stringify(Array.from(stepQuestion.querySelectorAll(".surla-story-options li")).map((li) => li.textContent)) === JSON.stringify(["Instant at $50.00", "Manager review at $50.00"]), "a step's open question lacks its prompt or options");

expect(JSON.stringify(reasons(flows[2])) === JSON.stringify(["No step starts the flow.", "No step is an outcome.", "Open questions: Who approves a partial refund?"]), "the third draft's reasons are wrong: " + JSON.stringify(reasons(flows[2])));
const flowQuestion = flows[2].querySelector(':scope > .surla-story-questions [data-question-key="approver"]');
expect(flowQuestion && JSON.stringify(Array.from(flowQuestion.querySelectorAll(".surla-story-options li")).map((li) => li.textContent)) === JSON.stringify(["Support Manager", "Finance"]), "a flow's open question lacks its options");
expect(page.$$("#surla-story-change input, #surla-story-change textarea, #surla-story-change select, #surla-story-change button").length === 0, "the change's Story offers editing controls");

const row = flows[2].querySelector(".surla-story-steps");
expect(row.getAttribute("aria-label") === "Steps of Partial refund", "a row of steps has no name");
expect(!row.hasAttribute("tabindex"), "a row of steps that fits is a tab stop");
Object.defineProperty(row, "scrollWidth", { configurable: true, get: () => 900 });
Object.defineProperty(row, "clientWidth", { configurable: true, get: () => 300 });
page.win.dispatchEvent(new page.win.Event("resize"));
await until(() => row.tabIndex === 0, "a row of steps that overflows is not a tab stop");
row.focus();
expect(page.doc.activeElement === row, "a row of steps that overflows does not take keyboard focus");
expect(!flows[0].querySelector(".surla-story-steps").hasAttribute("tabindex"), "a row that still fits became a tab stop");
expect(page.win.getComputedStyle(row).overflowX === "auto", "a long flow's steps do not scroll inside the stage");
expect(page.win.getComputedStyle(row.querySelector(".surla-story-step")).overflowWrap === "anywhere", "a step's long text does not wrap");
expect(["0", "0px"].includes(page.win.getComputedStyle(page.$("#surla-story-stage")).minWidth), "the stage can grow wider than its column");

const comparison = page.$("#surla-story-comparison");
expect(comparison.dataset.same === "true" && comparison.textContent.includes("This change's Story is current"), "an identical draft is not called current: " + comparison.textContent);
const summary = page.$("#surla-story-draft-details > summary");
summary.focus();
expect(page.doc.activeElement === summary && summary.textContent.includes("3 flows"), "the current draft's disclosure is not reachable by keyboard with a name");

page.$("#surla-nav-cp1").click();
await until(() => page.$("#surla-confirm-submit"), "returning to the change did not show the confirm view");
page.$("#surla-confirm-submit").click();
await until(() => page.$("#surla-cp1-editor").dataset.requestRevision !== undefined && page.text("#surla-cp1-editor").includes("Confirmed Change Decision"), "a change with draft flows could not be confirmed");
page.close();
`)
}

// TestViewsPageStoryStageFollowsTheEvaluationWithoutAChange opens the Story
// stage with no change and checks each evaluation state as the server
// reaches it, without a reload: none with the step to take, running,
// succeeded with the Story drawn, and failed with the server's diagnostic.
// The page reads the evaluation once per change of state and keeps a
// single waiting read open.
func TestViewsPageStoryStageFollowsTheEvaluationWithoutAChange(t *testing.T) {
	cp, ps := storyPageServer(t)
	const diagnostic = "Story evaluation failed while compiling the design: design/design.go:12: undefined: Flowz"
	ps.act("running", func() { seedStoryRefreshForTest(t, cp, pageOwner, storyRefreshStateRunning, nil, "") })
	ps.act("succeeded", func() { seedStoryRefreshForTest(t, cp, pageOwner, storyRefreshStateSucceeded, pageStories(), "") })
	ps.act("failed", func() { seedStoryRefreshForTest(t, cp, pageOwner, storyRefreshStateFailed, nil, diagnostic) })
	runPageScript(t, ps, map[string]any{"diagnostic": diagnostic}, viewsPageScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-change-list-empty"), "the empty project did not show");
checkControl(page, page.$("#surla-nav-story"), "Story tab without a change");
page.$("#surla-nav-story").click();
await until(() => page.$("#surla-story-draft").dataset.state === "none", "the Story stage did not show the none state");
const announced = () => page.text("#surla-story-draft-announcement");
const liveRegions = () => page.$$("#surla-story-draft [role=status]").length;
expect(announced() === "" && liveRegions() === 1, "the none state was announced or the draft has several live regions");
expect(!page.$("#surla-story-change"), "a Story of a change is shown with no change open");
expect(page.text("#surla-story-draft-none").includes("Ask the guide to refresh the Story"), "the none state does not name the step to take");
const context = page.text("#surla-workspace-context");
expect(context.includes("Story") && context.includes("current draft none"), "the context strip does not name what is shown: " + context);

await control(origin, "act", { key: "running" });
await until(() => page.$("#surla-story-draft").dataset.state === "running", "a running evaluation did not appear without a reload");
expect(/[0-9a-f]{64}/.test(page.text("#surla-story-draft-running")), "the running state does not name its snapshot");
expect(announced() === "The Story evaluation is running." && liveRegions() === 1, "the running state was not announced once: " + announced());

await control(origin, "act", { key: "succeeded" });
await until(() => page.$("#surla-story-draft").dataset.state === "succeeded", "a succeeded evaluation did not appear without a reload");
const flows = page.$$("#surla-story-draft .surla-story-flow");
expect(flows.length === 3 && !page.$("#surla-story-draft-details") && !page.$("#surla-story-comparison"), "with no change the draft is not drawn directly");
expect(page.text("#surla-story-draft-succeeded").includes("The draft has 3 flows, 2 of them drafts that will not be tested."), "the draft's counts are missing");
expect(announced() === "The Story evaluation succeeded." && liveRegions() === 1, "the succeeded state was not announced once: " + announced());

await control(origin, "act", { key: "failed" });
await until(() => page.$("#surla-story-draft").dataset.state === "failed", "a failed evaluation did not appear without a reload");
expect(page.text("#surla-story-draft-diagnostic") === input.diagnostic, "the failure does not show the server's diagnostic: " + page.text("#surla-story-draft-diagnostic"));
expect(page.$("#surla-story-draft-diagnostic").children.length === 0, "the diagnostic was not shown as plain text");
expect(announced() === "The Story evaluation failed." && liveRegions() === 1, "the failed state was not announced once: " + announced());

expect(page.sent("GET", "/workspace/story/refresh").length === 4, "the page did not read the evaluation exactly once per change: " + page.sent("GET", "/workspace/story/refresh").length);
page.close();
`)
}

// TestViewsPageStoryStageNamesHowTheDraftDiffers opens a change whose Story
// the current draft changes: one flow edited, one removed and one added. The
// stage names each flow and states that the draft is not part of the change
// until the guide revises it and the user confirms.
func TestViewsPageStoryStageNamesHowTheDraftDiffers(t *testing.T) {
	cp, ps := storyPageServer(t)
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	op := seedStoryRefreshForTest(t, cp, pageOwner, storyRefreshStateSucceeded, pageStories(), "")
	proposeWithStory(t, cp, guide, "chg-differs", op)
	draft := pageStories()
	draft[0].Flows[0].Steps[1].Summary = "Store credit is issued within a minute"
	draft[0].Flows = append(draft[0].Flows[:2], designeval.StoryFlow{
		Key: "exchange", Title: "Exchange instead of refund", Complete: true,
		Steps: []designeval.StoryStep{
			{Key: "ask", ActorKey: "customer", Summary: "Customer asks for an exchange", Kind: "start"},
			{Key: "shipped", ActorKey: "customer", Summary: "The replacement ships", Kind: "outcome"},
		},
	})
	seedStoryRefreshForTest(t, cp, pageOwner, storyRefreshStateSucceeded, draft, "")
	runPageScript(t, ps, nil, viewsPageScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-differs", "the change did not open");
page.$("#surla-nav-story").click();
await until(() => page.$("#surla-story-comparison"), "the comparison did not appear");
const comparison = page.$("#surla-story-comparison");
expect(comparison.dataset.same === "false", "a differing draft is called current");
const named = (kind) => Array.from(comparison.querySelectorAll('[data-difference="' + kind + '"] strong')).map((s) => s.textContent);
expect(JSON.stringify(named("added")) === JSON.stringify(["Exchange instead of refund"]), "added flows: " + JSON.stringify(named("added")));
expect(JSON.stringify(named("removed")) === JSON.stringify(["Partial refund"]), "removed flows: " + JSON.stringify(named("removed")));
expect(JSON.stringify(named("changed")) === JSON.stringify(["Instant refund under the limit"]), "changed flows: " + JSON.stringify(named("changed")));
expect(page.text("#surla-story-rule") === "The draft is not part of this change until the guide revises the change with it and you confirm the new revision.", "the rule is not stated: " + page.text("#surla-story-rule"));
expect(page.$$("#surla-story-change .surla-story-flow").length === 3, "the change's own Story was replaced by the draft");
page.close();
`)
}

// TestViewsPageStoryStageNamesEachKindOfDifference keeps one change open and
// replaces the current draft with variants of the change's Story, each
// differing in one way, and checks that the stage names exactly that
// difference: a renamed role (and the flows that use it), an edited
// question, a step of another kind, steps in another order, flows in
// another order, and a new Story description.
func TestViewsPageStoryStageNamesEachKindOfDifference(t *testing.T) {
	cp, ps := storyPageServer(t)
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	op := seedStoryRefreshForTest(t, cp, pageOwner, storyRefreshStateSucceeded, pageStories(), "")
	proposeWithStory(t, cp, guide, "chg-kinds", op)
	variants := map[string]func([]designeval.StorySummary){
		"role": func(s []designeval.StorySummary) { s[0].Actors[1].Title = "Refund Supervisor" },
		"question": func(s []designeval.StorySummary) {
			s[0].Flows[1].Steps[0].Questions[0].Prompt = "Is exactly $50.00 instant?"
		},
		"kind":        func(s []designeval.StorySummary) { s[0].Flows[0].Steps[1].Kind = "step" },
		"step-order":  func(s []designeval.StorySummary) { st := s[0].Flows[0].Steps; st[0], st[1] = st[1], st[0] },
		"flow-order":  func(s []designeval.StorySummary) { s[0].Flows[0], s[0].Flows[1] = s[0].Flows[1], s[0].Flows[0] },
		"description": func(s []designeval.StorySummary) { s[0].Description = "Refunds and who approves them." },
	}
	for name, change := range variants {
		ps.act(name, func() {
			draft := pageStories()
			change(draft)
			seedStoryRefreshForTest(t, cp, pageOwner, storyRefreshStateSucceeded, draft, "")
		})
	}
	runPageScript(t, ps, nil, viewsPageScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-kinds", "the change did not open");
page.$("#surla-nav-story").click();
await until(() => page.$("#surla-story-comparison") && page.$("#surla-story-comparison").dataset.same === "true", "the change's own draft is not called current");
const named = () => {
  const out = {};
  for (const item of page.$$("#surla-story-comparison [data-difference]")) {
    out[item.dataset.difference] = Array.from(item.querySelectorAll("strong")).map((s) => s.textContent);
  }
  return JSON.stringify(out);
};
const cases = [
  ["role", { changed: ["Refund over the limit", "Partial refund"], details: ["Order Refunds"] }],
  ["question", { changed: ["Refund over the limit"] }],
  ["kind", { changed: ["Instant refund under the limit"] }],
  ["step-order", { "step-order": ["Instant refund under the limit"] }],
  ["flow-order", { "flow-order": ["Order Refunds"] }],
  ["description", { details: ["Order Refunds"] }],
];
for (const [name, want] of cases) {
  await control(origin, "act", { key: name });
  await until(() => named() === JSON.stringify(want), name + ": the stage did not name exactly " + JSON.stringify(want));
  expect(page.$("#surla-story-comparison").dataset.same === "false" && page.$("#surla-story-rule"), name + ": the difference does not state the rule");
}
page.close();
`)
}

// TestViewsPageStoryStageNamesStoriesInAnotherOrder opens a change saved
// with two Stories whose draft swaps them: the stage says the Stories are in
// another order and names no flow.
func TestViewsPageStoryStageNamesStoriesInAnotherOrder(t *testing.T) {
	cp, ps := storyPageServer(t)
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	second := designeval.StorySummary{Name: "Store Credit", Actors: []designeval.StoryActor{{Key: "customer", Title: "Customer"}}}
	op := seedStoryRefreshForTest(t, cp, pageOwner, storyRefreshStateSucceeded, append(pageStories(), second), "")
	proposeWithStory(t, cp, guide, "chg-stories", op)
	seedStoryRefreshForTest(t, cp, pageOwner, storyRefreshStateSucceeded, append([]designeval.StorySummary{second}, pageStories()...), "")
	runPageScript(t, ps, nil, viewsPageScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-stories", "the change did not open");
page.$("#surla-nav-story").click();
await until(() => page.$("#surla-story-comparison"), "the comparison did not appear");
const kinds = page.$$("#surla-story-comparison [data-difference]").map((item) => item.dataset.difference);
expect(page.$("#surla-story-comparison").dataset.same === "false" && JSON.stringify(kinds) === JSON.stringify(["story-order"]), "Stories in another order were not named alone: " + JSON.stringify(kinds));
page.close();
`)
}

// TestViewsPageStoryStageRetriesAFailedRead fails the stage's first read of
// the current draft: the stage shows the server's message with a keyboard-
// operable retry, and retrying shows the draft and clears the error.
func TestViewsPageStoryStageRetriesAFailedRead(t *testing.T) {
	_, ps := storyPageServer(t)
	runPageScript(t, ps, nil, viewsPageScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-change-list-empty"), "the empty project did not show");
await control(origin, "reply", { key: "GET /workspace/story/refresh", status: 500, body: { name: "fault", id: "x", message: "The Story evaluation could not be read", temporary: false, timeout: false, fault: true } });
page.$("#surla-nav-story").click();
await until(() => page.$("#surla-story-draft-error"), "a failed read of the draft was not shown");
expect(page.text("#surla-story-draft-error").includes("The Story evaluation could not be read"), "the error does not carry the server's message");
expect(page.$("#surla-story-draft").dataset.state === "unread", "a failed first read shows a draft state");
checkControl(page, page.$("#surla-story-draft-retry"), "retry of the draft read");
page.$("#surla-story-draft-retry").click();
await until(() => page.$("#surla-story-draft").dataset.state === "none", "retrying did not read the draft");
expect(!page.$("#surla-story-draft-error"), "the error stayed after a successful retry");
expect(page.sent("GET", "/workspace/story/refresh").length === 2, "the page did not read the draft exactly twice");
page.close();
`)
}

// TestViewsPageStoryStageSaysAChangeHasNoStory opens a change saved without
// a Story: the confirm view and the Story stage both say so.
func TestViewsPageStoryStageSaysAChangeHasNoStory(t *testing.T) {
	cp, ps := storyPageServer(t)
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	mustPropose(t, cp, guide, "chg-plain", "Route refunds to a manager.", refundAnalysis("Route refunds to a manager."))
	runPageScript(t, ps, nil, viewsPageScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-plain", "the change did not open");
expect(page.text("#surla-confirm-story").includes("This change has no Story."), "the confirm view does not say the change has no Story");
page.$("#surla-confirm-open-story").click();
await until(() => page.$("#surla-story-change-empty"), "the Story stage does not say the change has no Story");
expect(page.$$("#surla-story-change .surla-story-flow").length === 0, "a change without a Story drew flows");
page.close();
`)
}

// storyPageServer serves a control plane bound to a workspace, so the owner's
// Story evaluation can be read, behind the page session.
func storyPageServer(t *testing.T) (*ControlPlane, *pageServer) {
	t.Helper()
	ws := t.TempDir()
	writeWorkspaceFiles(t, ws, validWorkspaceStub("package design\n"))
	cp := pageControlPlane(t, Options{WorkspaceDir: ws})
	return cp, startPageServer(t, cp)
}

// proposeWithStory saves a change whose Story is the succeeded evaluation op.
func proposeWithStory(t *testing.T, cp *ControlPlane, guide context.Context, changeID, op string) {
	t.Helper()
	_, err := cp.ProposeChange(guide, &gensurla.ProposeChangePayload{
		ChangeID:         changeID,
		Prompt:           "Route refunds to a manager.",
		Analysis:         refundAnalysis("Route refunds to a manager."),
		StoryRefreshOpID: &op,
	})
	require.NoError(t, err)
}

// pageStories is sampleAdoptedStories with a third draft flow that has no
// start, no outcome, a flow-level question, and enough long steps to scroll.
func pageStories() []designeval.StorySummary {
	stories := sampleAdoptedStories()
	steps := make([]designeval.StoryStep, 0, 9)
	for i := range 9 {
		steps = append(steps, designeval.StoryStep{
			Key:      "partial-" + string(rune('a'+i)),
			ActorKey: "manager",
			Summary:  "Manager checks part " + string(rune('A'+i)) + " " + strings.Repeat("of-a-very-long-unbroken-receipt-identifier-", 4),
			Kind:     "step",
		})
	}
	stories[0].Flows = append(stories[0].Flows, designeval.StoryFlow{
		Key:   "partial-refund",
		Title: "Partial refund",
		Steps: steps,
		Questions: []designeval.StoryQuestion{{
			Key: "approver", Prompt: "Who approves a partial refund?", Options: []string{"Support Manager", "Finance"},
		}},
	})
	return stories
}
