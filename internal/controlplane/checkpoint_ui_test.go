// This file tests the views page's Change Card editor and architecture stage
// through the requests the page sends to the real generated HTTP API: exact
// text preservation, the busy state of a confirmation, stale-revision
// recovery, incoming revisions, and the architecture drawing.
package controlplane

import (
	"context"
	"fmt"
	"testing"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/testmodule"
)

// checkpointUIProposal is the guide's analysis the editor tests start from.
func checkpointUIProposal() *gensurla.CP1Analysis {
	return &gensurla.CP1Analysis{
		Summary: "Route refunds >= $50 to manager approval.",
		Ambiguities: []*gensurla.CP1AnalysisAmbiguity{{
			Question: "Does the $50 limit include tax?",
			Options:  []string{"subtotal_only", "total_with_tax"},
		}},
		Examples: []*gensurla.CP1AnalysisExample{{
			Scenario: "Refund under $50", InputSummary: "$40 subtotal",
			ExpectedOutcome: "Auto-approved", VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
	}
}

// TestCheckpointUIEditorConfirmAndRevisionRecovery drives the editor: exact
// whitespace survives confirmation and a no-edit refinement, a confirmation
// shows one busy request, a stale revision keeps the user's edits and
// recovers them beside the latest card, and incoming revisions wait while
// the user has edits or a request in flight.
func TestCheckpointUIEditorConfirmAndRevisionRecovery(t *testing.T) {
	cp := pageControlPlane(t, Options{})
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	proposed := mustPropose(t, cp, guide, "chg-ui-test", "Route refunds >= $50 to manager approval.", checkpointUIProposal())
	ps := startPageServer(t, cp)
	runPageScript(t, ps, map[string]any{"r1": proposed.RequestRevision}, viewsPageScriptHelpers+`
const page = await openPage(origin, { link: input.link });
const editor = () => page.$("#surla-cp1-editor");
const outcome = () => page.$('textarea[data-example-id="check_01"][data-field="expected_outcome"]');
const pill = (option) => page.$('[data-ambiguity-id="chg-ui-test-ambiguity-01"][data-option="' + option + '"]');
const confirms = () => page.sent("POST", "/changes/confirm");
await until(() => editor().dataset.changeId === "chg-ui-test", "the change did not open");
expect(page.state() === "cp1-change-card" && editor().dataset.requestRevision === input.r1, "unexpected CP1 dataset: " + JSON.stringify(editor().dataset));
const cp1Text = page.text("#surla-checkpoint-view");
expect(cp1Text.includes(input.r1) && cp1Text.includes("Does the $50 limit include tax?") && cp1Text.includes("Scenario:") && cp1Text.includes("Input:") && cp1Text.includes("Expected result:"), "CP1 view missing revision, ambiguity question, or visible field labels: " + cp1Text);

// A confirmation sends exactly one request with the exact text and holds the form busy.
pill("subtotal_only").click();
page.type(outcome(), "  Instant store credit\nup to $50.00  ");
await control(origin, "hold", { key: "POST /changes/confirm" });
page.$("#surla-confirm-submit").click();
page.$("#surla-confirm-submit").click();
page.type(outcome(), "typed while saving");
expect(confirms().length === 1, "a second click sent another confirmation");
expect(page.$("#surla-cp1-form-card").getAttribute("aria-busy") === "true" && page.$("#surla-confirm-submit").disabled && outcome().disabled && page.$$('[data-ambiguity-id="chg-ui-test-ambiguity-01"]').every((p) => p.disabled), "controls were not busy and disabled while saving");
expect(outcome().value === "  Instant store credit\nup to $50.00  ", "typing while saving changed the submitted text");
const sentConfirm = confirms()[0].body;
expect(sentConfirm.request_revision === input.r1 && sentConfirm.answers["chg-ui-test-ambiguity-01"] === "subtotal_only" && sentConfirm.examples[0].expected_outcome === "  Instant store credit\nup to $50.00  ", "unexpected confirm_intent payload: " + JSON.stringify(sentConfirm));
await control(origin, "release", { key: "POST /changes/confirm" });
await until(() => editor().dataset.requestRevision !== input.r1 && !page.$("#surla-cp1-form-card").getAttribute("aria-busy"), "the confirmed revision did not replace the busy form");
const r2 = editor().dataset.requestRevision;

// A later list change does not overwrite edits typed after the confirmation.
page.type(outcome(), "post-confirm draft edit");
const listReads = page.sent("GET", "/changes").length;
await propose("chg-elsewhere", "Show carriers on receipts.", "Carrier shown");
await until(() => page.sent("GET", "/changes").length > listReads, "the other change did not reach the page");
expect(outcome().value === "post-confirm draft edit", "a list change overwrote a post-confirm edit");

// Refining without edits keeps multiline, space-padded text and the revision.
page.type(outcome(), "  Instant store credit\nup to $50.00  ");
page.$("#surla-confirm-submit").click();
await until(() => confirms().length === 2 && !page.$("#surla-confirm-submit").disabled, "the no-edit refinement did not finish");
expect(confirms()[1].body.examples[0].expected_outcome === "  Instant store credit\nup to $50.00  ", "no-edit refine mutated multiline/whitespace field");
expect(editor().dataset.requestRevision === r2, "no-edit refine changed request_revision");
expect((await readCard("chg-ui-test")).examples[0].expected_outcome === "  Instant store credit\nup to $50.00  ", "the saved text lost its exact whitespace");

// A confirmation against a revision the guide replaced keeps the edits and offers recovery.
page.setHidden(true);
const r3card = await revise(await readCard("chg-ui-test"), "Route refunds >= $50 to manager approval, with an audit.", "Audited approval");
pill("total_with_tax").click();
page.type(outcome(), "Edited outcome preserved across stale error");
page.$("#surla-confirm-submit").click();
await until(() => page.$("#surla-confirm-error"), "a stale confirmation showed no error");
const staleText = page.text("#surla-cp1-editor");
expect(page.status().includes("Change Card request failed; unsaved edits preserved.") && staleText.includes("request_revision mismatch") && staleText.includes("Your edited answers and examples remain in this form"), "stale submission did not explain itself: " + page.status() + " / " + staleText);
expect(!outcome().disabled && outcome().value === "Edited outcome preserved across stale error", "stale submission lost edits or kept controls disabled");
expect(page.doc.activeElement === page.$("#surla-confirm-error"), "the refused confirmation did not take focus");

// Load Latest Card shows the latest saved card beside the preserved edits.
page.$("#surla-load-latest-card").click();
await until(() => editor().dataset.requestRevision === r3card.request_revision, "Load Latest Card did not show the latest card");
const recovered = page.text("#surla-checkpoint-view");
expect(page.$("#surla-preserved-draft") && recovered.includes("Preserved Unsaved Draft Edits") && recovered.includes("total_with_tax") && recovered.includes("Edited outcome preserved across stale error"), "recovery did not keep the preserved draft: " + recovered);

// Recovering again keeps the lines already preserved.
page.$("#surla-confirm-submit").click();
await until(() => page.$("#surla-load-latest-card"), "an unanswered question did not offer recovery");
page.$("#surla-load-latest-card").click();
await until(() => !page.$("#surla-load-latest-card") || !page.$("#surla-load-latest-card").disabled, "the second recovery did not finish");
expect(page.text("#surla-cp1-editor").includes("Edited outcome preserved across stale error"), "repeated recovery lost the preserved draft");

// An incoming revision waits beside unsaved edits; switching keeps every unsaved line.
page.type(outcome(), "Dirty edit on confirmed card before incoming switch");
page.setHidden(false);
const r4card = await revise(r3card, "Route refunds >= $50 to manager approval, with an audit and a notice.", "Audited approval with notice");
await until(() => page.$("#surla-replace-incoming-card"), "an incoming revision did not wait beside unsaved edits");
expect(page.$("#surla-replace-incoming-card").textContent.includes("Keep Unsaved Edits"), "expected the switch button to say it keeps unsaved edits");
page.$("#surla-replace-incoming-card").click();
const switched = page.$("#surla-preserved-draft");
const switchedText = switched ? switched.textContent : "";
expect(editor().dataset.requestRevision === r4card.request_revision && editor().dataset.changeId === "chg-ui-test" && page.$("#surla-result-view").dataset.state === undefined && !page.$("#surla-replace-incoming-card"), "switching did not show the incoming card alone");
expect(switchedText.includes("Edited outcome preserved across stale error") && switchedText.includes("Dirty edit on confirmed card before incoming switch") && switchedText.includes(r3card.request_revision), "switching did not preserve earlier and new unsaved lines: " + switchedText);
expect(outcome().value === r4card.examples[0].expected_outcome, "the switched form did not start from the incoming card's saved values");
page.$("#surla-discard-draft").click();
expect(editor().dataset.requestRevision === r4card.request_revision && !page.$("#surla-preserved-draft"), "Discard Unsaved Edits did not clear the preserved draft");

// A revision held beside unsaved edits stays held while a confirmation is in flight and after its answer.
page.type(outcome(), "Edit on R4 before the guide's revision");
const r5card = await revise(r4card, "Route refunds >= $50 to manager approval, audited twice.", "Audited twice");
await until(() => page.$("#surla-replace-incoming-card"), "R5 did not wait beside unsaved edits");
pill("subtotal_only").click();
await control(origin, "hold", { key: "POST /changes/confirm" });
page.$("#surla-confirm-submit").click();
page.$("#surla-replace-incoming-card").click();
expect(page.status().includes("Wait for the pending CP1 Change Card request"), "switching during a pending confirmation did not ask to wait");
await control(origin, "release", { key: "POST /changes/confirm" });
await until(() => page.$("#surla-confirm-error"), "confirming a replaced revision was not refused");
expect(page.$("#surla-replace-incoming-card") && page.$("#surla-result-view").dataset.requestRevision === r5card.request_revision, "the confirmation's answer dropped the held revision");
page.$("#surla-replace-incoming-card").click();
expect(editor().dataset.requestRevision === r5card.request_revision && page.$("#surla-prior-revision") && page.text("#surla-prior-revision").includes(r4card.request_revision) && page.text("#surla-preserved-draft").includes("Edit on R4 before the guide's revision"), "switching after the answer did not keep the previous revision and the unsaved edit");
page.$("#surla-discard-draft").click();

// A revision another caller saves while the page's own confirmation awaits its answer is not shown as the page's own; it appears once the answer arrives.
await control(origin, "answer-later", { key: "POST /changes/confirm" });
pill("subtotal_only").click();
page.$("#surla-confirm-submit").click();
let confirmedR5 = await readCard("chg-ui-test");
for (let tries = 0; confirmedR5.status !== "confirmed" && tries < 200; tries++) {
  await sleep(20);
  confirmedR5 = await readCard("chg-ui-test");
}
expect(confirmedR5.status === "confirmed", "the held confirmation was not saved");
const r6card = await revise(confirmedR5, "Route refunds >= $50 to manager approval, audited.", "Audited");
await sleep(300);
expect(!page.$("#surla-replace-incoming-card") && !page.status().includes("newer revision"), "a save during the page's own confirmation was announced before its answer");
await control(origin, "release", { key: "POST /changes/confirm" });
await until(() => editor().dataset.requestRevision === r6card.request_revision, "the revision saved during the confirmation did not appear after its answer");
expect(page.$("#surla-prior-revision") && page.text("#surla-prior-revision").includes(confirmedR5.request_revision) && page.status().includes("newer revision"), "the newer revision does not show the confirmed card it replaced");
page.close();
`)
}

// TestCheckpointUIArchitecturePreviewOfTheBoundDesign opens the architecture
// stage, which asks the server to evaluate the bound workspace design for
// the exact confirmed revision shown, and checks the drawing it renders. An
// unconfirmed card shows the server's own refusal.
func TestCheckpointUIArchitecturePreviewOfTheBoundDesign(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	workspace := t.TempDir()
	writeWorkspaceFiles(t, workspace, testmodule.Snapshot(t, "", architectureTestDesign))
	cp := pageControlPlane(t, Options{WorkspaceDir: workspace})
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	mustPropose(t, cp, guide, "chg-arch", "Route refunds >= $50 to manager approval.", checkpointUIProposal())
	ps := startPageServer(t, cp)
	runPageScript(t, ps, nil, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-arch", "the change did not open");
page.$("#surla-nav-cp2").click();
await until(() => page.$("#surla-cp2-error"), "an unconfirmed card's architecture showed no refusal", 60000);
expect(page.text("#surla-cp2-error").includes("must be confirmed before architecture preview"), "the page did not show the server's own error: " + page.text("#surla-cp2-error"));
page.$("#surla-nav-cp1").click();
page.$('[data-ambiguity-id="chg-arch-ambiguity-01"][data-option="subtotal_only"]').click();
page.$("#surla-confirm-submit").click();
await until(() => page.$("#surla-cp1-editor").textContent.includes("Confirmed Change Decision"), "confirmation did not finish");
const revision = page.$("#surla-cp1-editor").dataset.requestRevision;
page.$("#surla-nav-cp2").click();
expect(page.$("#surla-cp2-loading") && page.$("#surla-cp2-loading").getAttribute("aria-busy") === "true", "evaluating showed no loading state");
const previews = page.sent("POST", "/changes/preview");
expect(JSON.stringify(previews[previews.length - 1].body) === JSON.stringify({ change_id: "chg-arch", request_revision: revision }), "the preview did not ask for the exact revision shown");
const result = page.$("#surla-result-view");
await until(() => result.dataset.designDigest, "the architecture was not drawn", 120000);
const cp2Text = page.text("#surla-checkpoint-view");
expect(page.state() === "cp2-architecture-preview" && result.dataset.requestRevision === revision, "unexpected CP2 dataset: " + JSON.stringify(result.dataset));
expect(cp2Text.includes(revision) && cp2Text.includes(result.dataset.designDigest) && cp2Text.includes("Inject: tenant_id") && cp2Text.includes("Confirmation"), "CP2 view missing digests or guardrails: " + cp2Text);
const wrap = page.$$(".surla-graph-wrap");
const nodes = page.$$("#surla-c4-svg .surla-svg-node");
const texts = page.$$("#surla-c4-svg text");
expect(wrap.length === 1 && wrap[0].getAttribute("role") === "region" && wrap[0].getAttribute("aria-label") === "C4 architecture diagram" && wrap[0].getAttribute("tabindex") === "0", "the graph is not a focusable region");
expect(nodes.length > 0 && nodes.length === page.$$("#surla-result-view .surla-arch-grid .surla-compact-list")[0].children.length, "the graph does not draw every container");
expect(texts.length > 0 && texts.every((text) => text.getAttribute("fill") === "currentColor"), "SVG text does not follow the theme color");
expect(!page.$("#surla-cp1-editor").dataset.changeId, "a clean editor stayed beside the architecture");
page.close();
`)
}

// TestCheckpointUIArchitectureDrawingGeometry draws a 15-container preview
// with colliding names, an intra-column cycle and a self-loop, served by the
// generated encoder, and checks the drawing's fixed geometry.
func TestCheckpointUIArchitectureDrawingGeometry(t *testing.T) {
	cp := pageControlPlane(t, Options{})
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	mustPropose(t, cp, guide, "chg-geometry", "Draw a large architecture.", refundAnalysis("Draw a large architecture."))
	containers := make([]*gensurla.C4ContainerItem, 0, 15)
	for i := 1; i <= 15; i++ {
		kind := "container"
		switch {
		case i == 1:
			kind = "person"
		case i <= 8:
			kind = "service"
		}
		name := fmt.Sprintf("Container %d", i)
		if i <= 3 {
			name = "Duplicate Display Name"
		}
		containers = append(containers, &gensurla.C4ContainerItem{
			ID: fmt.Sprintf("node-%d", i), Name: name, Kind: kind, Description: fmt.Sprintf("Description for node-%d", i),
		})
	}
	ps := startPageServer(t, &previewFixture{ControlPlane: cp, preview: &gensurla.ArchitecturePreviewResult{
		DesignDigest: "d" + fmt.Sprintf("%063d", 0), SystemName: "orders", Banner: "15 containers evaluated",
		Containers: containers,
		Edges: []*gensurla.C4EdgeItem{
			{FromID: "node-1", ToID: "node-2", Protocol: "HTTPS"},
			{FromID: "node-2", ToID: "node-3", Protocol: "gRPC"},
			{FromID: "node-3", ToID: "node-2", Protocol: "Callback gRPC"},
			{FromID: "node-2", ToID: "node-2", Protocol: "Self Loop"},
			{FromID: "node-8", ToID: "node-15", Protocol: "SQLite"},
		},
		AgentPolicies: []*gensurla.AgentPolicyItem{},
		Tools:         []*gensurla.ToolContractItem{},
	}})
	runPageScript(t, ps, nil, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-geometry", "the change did not open");
page.$("#surla-nav-cp2").click();
await until(() => page.$("#surla-c4-svg"), "the architecture was not drawn");
const svg = page.$$("#surla-c4-svg");
const nodes = page.$$("#surla-c4-svg .surla-svg-node");
const edges = page.$$("#surla-c4-svg .surla-svg-edge");
const scope = page.$("#surla-cp2-scope-note");
const confirmation = page.$("#surla-cp2-confirmation-note");
expect(scope && scope.textContent.includes("does not compare the design with the Change Card") && confirmation && confirmation.textContent.includes("does not show that the service asks the right person") && confirmation.textContent.includes("examples/orders"), "expected the scope and confirmation notes");
const edge23 = edges[1].children[0].getAttribute("d");
const edge32 = edges[2].children[0].getAttribute("d");
const text = page.text("#surla-checkpoint-view");
expect(svg.length === 1 && svg[0].getAttribute("width") === "840" && nodes.length === 15 && edges.length === 5 && edge23 !== edge32 && edge23.includes("Q 526") && edge32.includes("Q 544") && text.includes("node-15") && text.includes("Self Loop"), "expected 840px SVG with 15 nodes and distinct intra-column cyclic/self-loop edges, got width=" + (svg[0] && svg[0].getAttribute("width")) + " e23=" + edge23 + " e32=" + edge32);
page.close();
`)
}

// previewFixture answers preview_architecture with a fixed drawing bound to
// the requested card, through the real generated encoder.
type previewFixture struct {
	*ControlPlane
	// preview is the drawing returned for every request.
	preview *gensurla.ArchitecturePreviewResult
}

// PreviewArchitecture returns the fixed drawing for the requested revision.
func (p *previewFixture) PreviewArchitecture(
	_ context.Context,
	req *gensurla.PreviewArchitecturePayload,
) (*gensurla.ArchitecturePreviewResult, error) {
	out := *p.preview
	out.ChangeID, out.RequestRevision = req.ChangeID, req.RequestRevision
	return &out, nil
}

// architectureTestDesign declares an agent whose tool injects tenant_id and
// requires confirmation, so the preview shows both guardrails.
const architectureTestDesign = `package design

import (
	. "goa.design/goa/v3/dsl"
	. "goa.design/goa-ai/dsl"
)

var _ = API("orders", func() {})

var RefundTools = Toolset("refund_tools", func() {
	Description("Order lookup and payment refund tools")
	Tool("issue_refund", "Issue a payment refund", func() {
		Args(func() {
			Attribute("tenant_id", String)
			Attribute("order_id", String)
			Required("tenant_id", "order_id")
		})
		Return(func() {
			Attribute("status", String)
			Required("status")
		})
		Inject("tenant_id")
		Confirmation(func() {
			Title("Approve refund")
			PromptTemplate("Approve {{ .order_id }}?")
			DeniedResultTemplate(` + "`{\"status\":\"denied\"}`" + `)
		})
	})
})

var _ = Service("orders", func() {
	Description("Public Orders HTTP and MCP service")
	Agent("refund_agent", "Executes durable order refund workflows", func() {
		Use(RefundTools)
		RunPolicy(func() {
			DefaultCaps(MaxToolCalls(6), MaxRecoveryTurns(2))
			TimeBudget("2m")
		})
	})
})
`
