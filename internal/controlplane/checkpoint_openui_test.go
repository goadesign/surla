// This file tests the views page's OpenUI Decision Studio and the checks on
// rendered content: the built page matches its sources, authored sources
// never inject HTML, a valid composition drives the one shared draft, and
// every invalid composition falls back to the canonical editor.
package controlplane

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCheckpointUIBuildIsCurrentAndInjectsNoHTML checks that the served page
// and catalog match ui/checkpoint/src, that no authored source writes raw
// HTML, and that the bundle carries no legacy Zod v3 code.
func TestCheckpointUIBuildIsCurrentAndInjectsNoHTML(t *testing.T) {
	node := requirePageTools(t)
	check := exec.Command(node, "../../ui/checkpoint/build.mjs", "--check")
	check.Env = append(os.Environ(), "CI=true")
	out, err := check.CombinedOutput()
	require.NoError(t, err, "views page bundle drift check failed:\n%s", out)
	err = filepath.Walk("../../ui/checkpoint/src", func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return walkErr
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, writer := range []string{"innerHTML", "dangerouslySetInnerHTML", "outerHTML", "insertAdjacentHTML", "createContextualFragment", "document.write"} {
			require.NotContains(t, string(raw), writer, "authored UI source %s must not write raw HTML", path)
		}
		return nil
	})
	require.NoError(t, err)
	for _, legacy := range []string{"ZodEffects", "ZodParsedType"} {
		require.NotContains(t, viewsPageHTML, legacy, "the page still bundles legacy Zod v3 code")
	}
	for _, embedded := range []string{"window.parent", "parent.postMessage", "mcp-app", "ui/initialize", "Content-Security-Policy"} {
		require.NotContains(t, viewsPageHTML, embedded, "the page still carries embedding code or an in-document policy")
	}
}

// TestCheckpointUIOpenUIDecisionStudio verifies through the page's real
// requests that:
//  1. A Change Card carrying a valid ui_composition renders the OpenUI
//     Renderer inside #surla-openui-workspace over the single CP1 draft.
//  2. Choosing an option in PolicySimulatorTable updates the canonical answer
//     and highlights its column, and confirm_intent saves that answer.
//  3. Editing an example surfaces #surla-openui-local-edit-notice.
//  4. Malformed, incomplete, out-of-range, mismatched, revision-skewed or
//     forged compositions render #surla-openui-error while the canonical
//     editor keeps working, and an answer with undeclared fields is refused
//     at the page's response boundary.
func TestCheckpointUIOpenUIDecisionStudio(t *testing.T) {
	cp := pageControlPlane(t, Options{})
	ps := startPageServer(t, cp)
	validProgram := strings.Join([]string{
		`root = DecisionWorkspace("Refund Threshold Decision Studio", [sim, choice, timeline, callout])`,
		`sim = PolicySimulatorTable("Tax Inclusion Matrix", 1, [row1, row2])`,
		`row1 = PolicySimulatorRow("Borderline $48 order + $4 tax", "$48 subtotal / $52 total", ["Auto-approved (< $50 subtotal)", "Manager approval required (>= $50 total)"], 1)`,
		`row2 = PolicySimulatorRow("Large $75 order", "$75 subtotal / $81 total", ["Manager approval required", "Manager approval required"], 2)`,
		`choice = AmbiguityChoiceCard(1, "Subtotal keeps tax fluctuations out of approval routing.")`,
		`timeline = WorkflowTimeline("Refund Execution Flow", [step1, step2])`,
		`step1 = WorkflowStep("Validate Request", "orders.service", "Checks tenant isolation and order status.")`,
		`step2 = WorkflowStep("Evaluate Threshold", "refund_agent", "Routes orders above limit to manager approval.", "Confirmation")`,
		`callout = ImpactCallout("warning", "Tenant Isolation Invariant", "Cross-tenant lookups always return 404.")`,
	}, "\n")
	runPageScript(t, ps, map[string]any{"program": validProgram}, `
function analysis(program, questions) {
  return {
    summary: "Route refunds >= $50 to manager approval.",
    ambiguities: questions ? [{ question: "Does the $50 limit include tax?", options: ["subtotal_only", "total_with_tax"] }] : [],
    examples: [
      { scenario: "Borderline $48 order", input_summary: "$48 subtotal + $4 tax", expected_outcome: "Auto-approved", verification_kind: "deterministic" },
      { scenario: "Large $75 order", input_summary: "$75 subtotal + $6 tax", expected_outcome: "Manager approval required", verification_kind: "deterministic" },
    ],
    preserved_behaviors: ["Cross-tenant lookups return 404."],
    ...(program === undefined ? {} : { ui_composition: program }),
  };
}
async function propose(changeId, program, questions = true) {
  const out = await api(origin, "POST", "/changes/propose", { change_id: changeId, prompt: "Route refunds >= $50 to manager approval.", analysis: analysis(program, questions) }, input.guide);
  expect(out.status === 200, "could not propose " + changeId + ": " + JSON.stringify(out));
  return out.body;
}
const badCases = [
  { id: "chg-bad-1", program: 'root = DecisionWorkspace("Bad", [AmbiguityChoiceCard(99, "oob")])', wantErr: "out of bounds" },
  { id: "chg-bad-2", program: 'root = DecisionWorkspace("Bad", [PolicySimulatorTable("Sim", 1, [PolicySimulatorRow("S", "I", ["Only one outcome"])])])', wantErr: "outcomes must have exactly 2 non-empty strings" },
  { id: "chg-bad-3", program: input.program, wantErr: "does not match card.request_revision", skew: true },
  { id: "chg-bad-4", program: 'root = DecisionWorkspace("Unclosed", [AmbiguityChoiceCard(1', wantErr: "incomplete" },
  { id: "chg-bad-5", program: 'root = DecisionWorkspace("D", [PolicySimulatorRow("S", "I", ["X", "Y"])])', wantErr: "Unsupported component type PolicySimulatorRow" },
  { id: "chg-bad-6", program: 'root = DecisionWorkspace("D", [WorkflowTimeline("Flow", [AmbiguityChoiceCard(99)])])', wantErr: "Unsupported component type AmbiguityChoiceCard" },
  { id: "chg-bad-7", program: " \n\t", wantErr: "OpenUI program is empty" },
  { id: "chg-bad-8", program: 'root = DecisionWorkspace("D", [WorkflowTimeline("Flow", [{type: "element", typeName: "WorkflowStep", props: {}, partial: false, hasDynamicProps: false}])])', wantErr: "WorkflowStep props failed schema validation" },
  { id: "chg-bad-9", program: 'root = DecisionWorkspace("D", [WorkflowTimeline("Flow", [WorkflowStep("S", "A", {bad: "x"})])])', wantErr: "validation failed" },
  { id: "chg-bad-10", program: 'root = DecisionWorkspace("D", [{type: "element", typeName: "ImpactCallout", props: {variant: "info", title: "Shown", summary: "Shown", unexpected: "Dropped model field"}, partial: false}])', wantErr: "ImpactCallout props failed schema validation" },
];
await propose("chg-openui", input.program);
await propose("chg-zero-amb", undefined, false);
await propose("chg-extra-field", undefined);
await propose("chg-timeline-only", 'root = DecisionWorkspace("Flow Only", [WorkflowTimeline("Steps", [WorkflowStep("S1", "api", "D1")]), ImpactCallout("info", "Invariant", "Preserved")])');
for (const tc of badCases) {
  await propose(tc.id, tc.program);
}
const page = await openPage(origin, { link: input.link });
const editorRegion = () => page.$("#surla-cp1-editor");
const open = async (changeId) => {
  if (!page.$("#surla-change-list")) {
    page.$("#surla-nav-changes").click();
  }
  await until(() => page.$("#surla-open-change-" + changeId) && !page.$("#surla-open-change-" + changeId).disabled, "the list does not offer " + changeId);
  page.$("#surla-open-change-" + changeId).click();
  await until(() => editorRegion().dataset.changeId === changeId, changeId + " did not open");
};
await until(() => page.$$("#surla-change-list li").length === 14, "the page did not list every change");

// 1. A valid composition renders inside the editor.
await open("chg-openui");
const card = await (async () => (await api(origin, "GET", "/changes/chg-openui", undefined, input.guide)).body)();
const workspace = page.$("#surla-openui-workspace");
expect(workspace && workspace.getAttribute("data-composed-for-revision") === card.request_revision && !page.$("#surla-openui-error"), "expected #surla-openui-workspace to render valid OpenUI composition");
for (const want of ["Refund Threshold Decision Studio", "Tax Inclusion Matrix", "Borderline $48 order + $4 tax", "Auto-approved (< $50 subtotal)", "Subtotal keeps tax fluctuations out of approval routing.", "Refund Execution Flow", "Tenant Isolation Invariant"]) {
  expect(workspace.textContent.includes(want), "OpenUI workspace missing expected content " + JSON.stringify(want));
}
const table = workspace.querySelector('[role="region"][tabindex="0"] table.shadcn-table');
expect(table, "expected responsive shadcn-table inside focusable region");
const cells = table.querySelectorAll("tbody tr:first-child td[data-label]");
expect(cells.length === 4 && cells[0].getAttribute("data-label") === "Scenario" && cells[1].getAttribute("data-label") === "Input" && cells[2].getAttribute("data-label") === "subtotal_only" && cells[3].getAttribute("data-label") === "total_with_tax", "expected PolicySimulatorTable cells to carry data-label attributes for narrow responsive layouts");

// 2. Choosing in the simulator updates the canonical draft.
workspace.querySelector('[data-openui-ambiguity-id="chg-openui-ambiguity-01"][data-openui-option="subtotal_only"]').click();
const canonicalPill = editorRegion().querySelector('[data-ambiguity-id="chg-openui-ambiguity-01"][data-option="subtotal_only"]');
const activeCells = workspace.querySelectorAll(".shadcn-cell-active");
expect(canonicalPill && canonicalPill.getAttribute("aria-pressed") === "true" && activeCells.length === 3 && !page.$("#surla-confirmed-decisions") && editorRegion().textContent.includes("Summary: ") && editorRegion().textContent.includes("[Selected draft answer: subtotal_only]") && !editorRegion().textContent.includes("[Confirmed answer: subtotal_only]"), "OpenUI option click did not synchronize canonical pill, draft label, or active column cells (activeCells=" + activeCells.length + ")");

// 3. A local example edit qualifies the illustration.
expect(!page.$("#surla-openui-local-edit-notice"), "unexpected local-edit notice before editing examples");
page.type(editorRegion().querySelector('textarea[data-example-id="check_01"][data-field="expected_outcome"]'), "Auto-approved with audit log");
expect(page.$("#surla-openui-local-edit-notice"), "expected #surla-openui-local-edit-notice after editing example locally");

// The page's confirmation saves the OpenUI answer and the edited example.
page.$("#surla-confirm-submit").click();
await until(() => !page.$("#surla-openui-workspace") && editorRegion().textContent.includes("[Confirmed answer: subtotal_only]"), "expected #surla-openui-workspace to unmount and confirmed answer label to appear after confirm_intent");
const sent = page.sent("POST", "/changes/confirm")[0].body;
expect(sent.answers["chg-openui-ambiguity-01"] === "subtotal_only" && sent.examples[0].expected_outcome === "Auto-approved with audit log", "confirm_intent did not send the OpenUI answer: " + JSON.stringify(sent));
const confirmed = (await api(origin, "GET", "/changes/chg-openui", undefined, input.guide)).body;
expect(confirmed.status === "confirmed" && confirmed.answers["chg-openui-ambiguity-01"] === "subtotal_only" && !confirmed.ui_composition, "unexpected saved card after OpenUI selection: " + JSON.stringify(confirmed));
const decisions = page.$("#surla-confirmed-decisions");
expect(decisions && decisions.textContent.includes("Confirmed Decisions:") && decisions.textContent.includes("Does the $50 limit include tax?") && decisions.textContent.includes("subtotal_only") && editorRegion().textContent.includes("Initial Proposal Summary: "), "expected #surla-confirmed-decisions and Initial Proposal Summary heading on confirmed card with ambiguities");

// A confirmed card without questions keeps the plain Summary heading.
await open("chg-zero-amb");
page.$("#surla-confirm-submit").click();
await until(() => editorRegion().textContent.includes("Confirmed Change Decision"), "the zero-question card did not confirm");
expect(!page.$("#surla-confirmed-decisions") && editorRegion().textContent.includes("Summary: ") && !editorRegion().textContent.includes("Initial Proposal Summary: "), "expected zero-ambiguity confirmed card to omit #surla-confirmed-decisions and keep Summary heading");

// 4. Every invalid composition falls back to the canonical editor.
const skewed = (await api(origin, "GET", "/changes/chg-bad-3", undefined, input.guide)).body;
skewed.ui_composition.composed_for_revision = "rev-stale-mismatch";
await control(origin, "reply", { key: "GET /changes/chg-bad-3", status: 200, body: skewed });
for (const tc of badCases) {
  await open(tc.id);
  const errEl = page.$("#surla-openui-error");
  const errDetails = errEl && errEl.querySelector("details.surla-details");
  expect(errEl && errEl.textContent.includes("Interactive decision preview unavailable:") && errDetails && errDetails.textContent.includes(tc.wantErr) && !page.$("#surla-openui-workspace"), "case " + tc.id + " did not render expected #surla-openui-error with Details disclosure (" + tc.wantErr + "), got: " + (errEl && errEl.textContent));
  expect(editorRegion().querySelector('[data-ambiguity-id="' + tc.id + '-ambiguity-01"][data-option="total_with_tax"]'), "case " + tc.id + " broke canonical CP1 inspector fallback");
}

// An answer carrying an undeclared field is refused at the response boundary.
const extra = (await api(origin, "GET", "/changes/chg-extra-field", undefined, input.guide)).body;
extra.unexpected_wire_field = "must be rejected by generated schema";
await control(origin, "reply", { key: "GET /changes/chg-extra-field", status: 200, body: extra });
page.$("#surla-nav-changes").click();
page.$("#surla-open-change-chg-extra-field").click();
await until(() => page.$("#surla-change-open-error"), "an answer with an undeclared field was accepted");
expect(page.text("#surla-change-open-error").includes("did not match its declared result") && editorRegion().dataset.changeId !== "chg-extra-field", "the refused answer was shown as a card");

// A timeline and callout composition still marks itself as an illustration.
await open("chg-timeline-only");
const timeline = page.$("#surla-openui-workspace");
expect(timeline && timeline.textContent.includes("Model-Proposed Illustration"), "expected workspace-level Model-Proposed Illustration badge on timeline/callout workspace");
page.type(editorRegion().querySelector('textarea[data-example-id="check_01"][data-field="expected_outcome"]'), "Locally edited outcome on timeline workspace");
expect(page.$("#surla-openui-local-edit-notice"), "expected workspace-level #surla-openui-local-edit-notice on timeline/callout workspace after example edit");
page.close();
`)
}
