// This file tests the MCP Apps (`ui://surla/checkpoint`) viewer for CP1
// `revise_change` prior-revision comparison, host input notification retention,
// question-bearing preserved draft lines, and CP5 unverified-approval recovery.

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
)

// TestCheckpointUIReviseLifecyclePriorComparisonAndUnverifiedApproval verifies
// in jsdom against the compiled `ui://surla/checkpoint` bundle:
//  1. Full host notification lifecycle (`tool-input-partial` -> `tool-input` ->
//     `tool-result`) across clean auto-confirmed `R1 -> R2 -> R3` keeps `activeEditor`
//     mounted during input notifications, renders `#surla-prior-revision` with
//     `R1` on `R2` and `R2` on `R3`, and omits `#surla-preserved-draft` when clean.
//  2. Dirty unsaved edits on `R2` when `R3` arrives render both
//     `#surla-prior-revision` (`R2`) and `#surla-preserved-draft` with the
//     ambiguity question text `[<rev>] <id> (<question>)=<answer>`.
//  3. Pending `confirm_intent` (`R1 -> R2`) and pending `get_change` hold
//     interleaved `tool-result` cards (`R3` for same `change_id`, `cardOther`
//     for different `change_id`) in `#surla-incoming-change-card`, block
//     switching until the RPC completes, and set `priorCard` to `R2` on same-card
//     switch or `null` on cross-card switch.
//  4. All 5 CP5 approval states render honestly (`#surla-cp5-approval` vs.
//     `#surla-cp5-unverified-approval`), and `#surla-cp5-refresh-approval`
//     invokes `get_change` to re-check promotion inputs.
func TestCheckpointUIReviseLifecyclePriorComparisonAndUnverifiedApproval(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	const commit = "2222222222222222222222222222222222222222"

	client := &queuedModelClient{}
	inputs := PromotionInputs{
		SuiteID:     "orders_acceptance",
		SuiteCommit: "3333333333333333333333333333333333333333",
		Environment: "sim",
		Artifacts:   map[string]string{"orders": strings.Repeat("b", 64)},
	}
	target := &promotionTargetStub{inputs: inputs}
	cp := newControlPlane(t, Options{
		Analyzer:        NewModelCP1Analyzer(newQueuedModelClient(t, client)),
		PromotionTarget: target,
	})
	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)

	resRead, err := adapter.ResourcesRead(ctx, &genmcpadapter.ResourcesReadPayload{URI: "ui://surla/checkpoint"})
	require.NoError(t, err)
	completeRes, ok := resRead.Outcome.AsComplete()
	require.True(t, ok)
	htmlDoc := *completeRes.Contents[0].Text

	callMCPTool := func(t *testing.T, name string, args map[string]any) json.RawMessage {
		t.Helper()
		rawArgs, err := json.Marshal(args)
		require.NoError(t, err)
		res, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{Name: name, Arguments: rawArgs})
		require.NoError(t, err)
		complete, ok := res.Outcome.AsComplete()
		require.True(t, ok)
		require.False(t, complete.IsError != nil && *complete.IsError)
		require.NotEmpty(t, complete.StructuredContent)
		return complete.StructuredContent
	}

	type wireCardRef struct {
		RequestRevision string           `json:"request_revision"`
		Examples        []map[string]any `json:"examples"`
	}

	// Mint R1 (0 ambiguities -> auto-confirmed).
	client.enqueueJSON(t, CP1SemanticAnalysis{
		Summary:     "Refunds up to $50 are instant.",
		Ambiguities: []CP1ModelAmbiguity{},
		Examples: []CP1ModelExample{
			{Scenario: "Refund under $50", InputSummary: "$40 subtotal", ExpectedOutcome: "Instant refund", VerificationKind: "deterministic"},
		},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
	})
	r1Raw := callMCPTool(t, "propose_change", map[string]any{
		"change_id": "chg-rev-ui",
		"prompt":    "Refunds up to $50 are instant.",
	})
	var r1Card wireCardRef
	require.NoError(t, json.Unmarshal(r1Raw, &r1Card))

	// Mint R2 via revise_change (1 ambiguity, 2 examples -> needs_confirmation, no ui_composition).
	r2Analysis := CP1RevisionSemanticAnalysis{
		Summary: "Refunds up to $50 are instant; refunds over $50 require approval.",
		Ambiguities: []CP1ModelAmbiguity{
			{Question: "Does the $50 limit include tax?", Options: []string{"subtotal_only", "total_with_tax"}},
		},
		Examples: []CP1RevisionModelExample{
			{PriorID: "check_01", Scenario: "Refund under $50", InputSummary: "$40 subtotal", ExpectedOutcome: "Instant refund", VerificationKind: "deterministic"},
			{Scenario: "Refund over $50", InputSummary: "$75 subtotal", ExpectedOutcome: "Manager approval", VerificationKind: "deterministic"},
		},
		RemovedExampleIDs:  []string{},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
	}
	client.enqueueJSON(t, r2Analysis)
	r2Raw := callMCPTool(t, "revise_change", map[string]any{
		"change_id":        "chg-rev-ui",
		"request_revision": r1Card.RequestRevision,
		"prompt":           "Refunds up to $50 are instant; refunds over $50 require approval.",
	})
	var r2Card wireCardRef
	require.NoError(t, json.Unmarshal(r2Raw, &r2Card))

	// Mint same-hash R2 updates with ui_composition nil -> programA -> programB.
	r2ProgramA := strings.Join([]string{
		`root = DecisionWorkspace("Refund Studio V1", [callout])`,
		`callout = ImpactCallout("info", "Illustration V1", "Initial OpenUI callout.")`,
	}, "\n")
	r2AnalysisWithProgramA := r2Analysis
	r2AnalysisWithProgramA.Examples = []CP1RevisionModelExample{
		{PriorID: "check_01", Scenario: "Refund under $50", InputSummary: "$40 subtotal", ExpectedOutcome: "Instant refund", VerificationKind: "deterministic"},
		{PriorID: "check_02", Scenario: "Refund over $50", InputSummary: "$75 subtotal", ExpectedOutcome: "Manager approval", VerificationKind: "deterministic"},
	}
	r2AnalysisWithProgramA.UIComposition = &r2ProgramA
	client.enqueueJSON(t, r2AnalysisWithProgramA)
	r2WithProgramARaw := callMCPTool(t, "revise_change", map[string]any{
		"change_id":        "chg-rev-ui",
		"request_revision": r2Card.RequestRevision,
		"prompt":           "Refunds up to $50 are instant; refunds over $50 require approval.",
	})
	var r2ProgramACard wireCardRef
	require.NoError(t, json.Unmarshal(r2WithProgramARaw, &r2ProgramACard))
	require.Equal(t, r2Card.RequestRevision, r2ProgramACard.RequestRevision)

	r2ProgramB := strings.Join([]string{
		`root = DecisionWorkspace("Refund Studio V2", [callout])`,
		`callout = ImpactCallout("warning", "Illustration V2", "Updated OpenUI callout.")`,
	}, "\n")
	r2AnalysisWithProgramB := r2AnalysisWithProgramA
	r2AnalysisWithProgramB.UIComposition = &r2ProgramB
	client.enqueueJSON(t, r2AnalysisWithProgramB)
	r2WithProgramBRaw := callMCPTool(t, "revise_change", map[string]any{
		"change_id":        "chg-rev-ui",
		"request_revision": r2Card.RequestRevision,
		"prompt":           "Refunds up to $50 are instant; refunds over $50 require approval.",
	})
	var r2ProgramBCard wireCardRef
	require.NoError(t, json.Unmarshal(r2WithProgramBRaw, &r2ProgramBCard))
	require.Equal(t, r2Card.RequestRevision, r2ProgramBCard.RequestRevision)

	// Mint R2Confirmed via confirm_intent on R2.
	r2ConfirmedRaw := callMCPTool(t, "confirm_intent", map[string]any{
		"change_id":        "chg-rev-ui",
		"request_revision": r2Card.RequestRevision,
		"answers":          map[string]string{"chg-rev-ui-ambiguity-01": "subtotal_only"},
		"examples":         r2Card.Examples,
	})
	var r2ConfirmedCard wireCardRef
	require.NoError(t, json.Unmarshal(r2ConfirmedRaw, &r2ConfirmedCard))

	// Mint CP5 states on R2Confirmed: current approval, changed inputs, unavailable target, and absent target.
	target.report = passingReport(t, r2ConfirmedCard.RequestRevision, commit, inputs)
	r2ApprovedRaw := callMCPTool(t, "approve_merge", map[string]any{
		"change_id":        "chg-rev-ui",
		"request_revision": r2ConfirmedCard.RequestRevision,
		"candidate_commit": commit,
		"report_digest":    target.report.Digest,
	})
	target.inputs = PromotionInputs{
		SuiteID:     inputs.SuiteID,
		SuiteCommit: inputs.SuiteCommit,
		Environment: inputs.Environment,
		Artifacts:   map[string]string{"orders": strings.Repeat("c", 64)},
	}
	r2ChangedInputsRaw := callMCPTool(t, "get_change", map[string]any{"change_id": "chg-rev-ui"})

	target.inputs = inputs
	target.inputsErr = errors.New("open /var/lib/surla/releases/orders: no such file or directory")
	r2UnavailableTargetRaw := callMCPTool(t, "get_change", map[string]any{"change_id": "chg-rev-ui"})

	cp.promotionTarget = nil
	r2AbsentTargetRaw := callMCPTool(t, "get_change", map[string]any{"change_id": "chg-rev-ui"})
	cp.promotionTarget = target
	target.inputsErr = nil

	// Mint R3 via revise_change on R2Confirmed.
	client.enqueueJSON(t, CP1RevisionSemanticAnalysis{
		Summary: "Refunds up to $50 (subtotal only) are instant and emit a webhook.",
		Ambiguities: []CP1ModelAmbiguity{
			{Question: "Should webhook retries use exponential backoff?", Options: []string{"exponential_backoff", "fixed_delay"}},
		},
		Examples: []CP1RevisionModelExample{
			{PriorID: "check_01", Scenario: "Refund under $50", InputSummary: "$40 subtotal", ExpectedOutcome: "Instant refund + webhook", VerificationKind: "deterministic"},
		},
		RemovedExampleIDs: []string{"check_02"},
		PreservedBehaviors: []string{
			"Cross-tenant lookups return 404.",
			"The $50 limit applies to subtotal_only.",
		},
	})
	r3Raw := callMCPTool(t, "revise_change", map[string]any{
		"change_id":        "chg-rev-ui",
		"request_revision": r2ConfirmedCard.RequestRevision,
		"prompt":           "Refunds up to $50 are instant, require approval over $50, and emit a webhook.",
	})
	var r3Card wireCardRef
	require.NoError(t, json.Unmarshal(r3Raw, &r3Card))

	// Mint cardOther for a different change_id.
	client.enqueueJSON(t, CP1SemanticAnalysis{
		Summary:     "Other change card summary.",
		Ambiguities: []CP1ModelAmbiguity{},
		Examples: []CP1ModelExample{
			{Scenario: "Other scenario", InputSummary: "Other input", ExpectedOutcome: "Other outcome", VerificationKind: "deterministic"},
		},
		PreservedBehaviors: []string{},
	})
	otherRaw := callMCPTool(t, "propose_change", map[string]any{
		"change_id": "chg-other",
		"prompt":    "Other change card prompt.",
	})

	nodePath, err := exec.LookPath("node")
	if err != nil {
		return
	}
	ensureCheckpointUIDeps(t)

	harnessInput, err := json.Marshal(map[string]any{
		"htmlDoc":             htmlDoc,
		"script":              extractCheckpointScript(t, htmlDoc),
		"r1":                  r1Raw,
		"r2":                  r2Raw,
		"r2WithProgramA":      r2WithProgramARaw,
		"r2WithProgramB":      r2WithProgramBRaw,
		"r2Confirmed":         r2ConfirmedRaw,
		"r2Approved":          r2ApprovedRaw,
		"r2ChangedInputs":     r2ChangedInputsRaw,
		"r2UnavailableTarget": r2UnavailableTargetRaw,
		"r2AbsentTarget":      r2AbsentTargetRaw,
		"r3":                  r3Raw,
		"cardOther":           otherRaw,
	})
	require.NoError(t, err)

	nodeHarness := `
const fs = require("fs");
const { JSDOM } = require("../../ui/checkpoint/node_modules/jsdom");
const input = JSON.parse(fs.readFileSync(0, "utf8"));

const dom = new JSDOM(input.htmlDoc, {
  url: "https://sandbox.mcp.local/",
  runScripts: "outside-only",
  pretendToBeVisual: true
});
const guestWindow = dom.window;
const documentObj = guestWindow.document;
const root = documentObj.getElementById("surla-checkpoint-root");
const statusEl = documentObj.getElementById("surla-checkpoint-status");
const viewEl = documentObj.getElementById("surla-checkpoint-view");

const hostMessages = [];
let messageListener = null;
const parentWindow = {
  postMessage(msg, targetOrigin) {
    hostMessages.push({ msg, targetOrigin });
  }
};
Object.defineProperty(guestWindow, "parent", { value: parentWindow, configurable: true });
const origAddEventListener = guestWindow.addEventListener.bind(guestWindow);
guestWindow.addEventListener = function(type, fn, opts) {
  if (type === "message") messageListener = fn;
  return origAddEventListener(type, fn, opts);
};

guestWindow.eval(input.script);
const editorRegion = documentObj.getElementById("surla-cp1-editor");
const resultRegion = documentObj.getElementById("surla-result-view");
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", id: 1, result: { protocolVersion: "2026-01-26" } }
});

// 1. Clean auto-confirmed R1 -> tool-input-partial -> tool-input -> tool-result (R2) -> tool-input-partial -> tool-input -> tool-result (R3).
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r1 } }
});
if (editorRegion.dataset.requestRevision !== input.r1.request_revision ||
    documentObj.getElementById("surla-prior-revision") !== null) {
  throw new Error("expected clean R1 mounted without #surla-prior-revision");
}

messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-input-partial",
    params: { arguments: { change_id: "chg-rev-ui", request_revision: input.r1.request_revision, prompt: "partial revise..." } }
  }
});
if (editorRegion.dataset.requestRevision !== input.r1.request_revision || root.dataset.state !== "cp1-change-card") {
  throw new Error("tool-input-partial cleared or hid activeEditor R1");
}

messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-input",
    params: { arguments: { change_id: "chg-rev-ui", request_revision: input.r1.request_revision, prompt: "full revise..." } }
  }
});
if (editorRegion.dataset.requestRevision !== input.r1.request_revision || root.dataset.state !== "cp1-change-card") {
  throw new Error("tool-input cleared activeEditor R1 before R2 arrived");
}

messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r2 } }
});
const priorOnR2 = documentObj.getElementById("surla-prior-revision");
if (editorRegion.dataset.requestRevision !== input.r2.request_revision ||
    priorOnR2 === null ||
    !priorOnR2.textContent.includes("Previous Saved Revision") ||
    !priorOnR2.textContent.includes(input.r1.request_revision) ||
    !priorOnR2.textContent.includes(input.r1.summary) ||
    documentObj.getElementById("surla-preserved-draft") !== null ||
    documentObj.getElementById("surla-openui-workspace") !== null) {
  throw new Error("expected R2 to render #surla-prior-revision with R1 and no #surla-preserved-draft or #surla-openui-workspace, got: " + (priorOnR2 && priorOnR2.textContent));
}

// 1b. Same-hash ui_composition updates (nil -> programA -> programB at input.r2.request_revision) apply in place while preserving drafts, priorCard, pending request ownership, and incoming result.
const r2Check2Input = editorRegion.querySelector('textarea[data-example-id="check_02"][data-field="expected_outcome"]');
r2Check2Input.value = "Draft edit kept across same-hash OpenUI update";
r2Check2Input.dispatchEvent(new guestWindow.Event("input", { bubbles: true }));
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r3 } }
});
if (documentObj.getElementById("surla-incoming-change-card") === null) {
  throw new Error("expected R3 held in #surla-incoming-change-card before same-hash update");
}
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r2WithProgramA } }
});
const openuiV1 = documentObj.getElementById("surla-openui-workspace");
if (openuiV1 === null ||
    !openuiV1.textContent.includes("Refund Studio V1") ||
    !openuiV1.textContent.includes("Illustration V1") ||
    r2Check2Input.value !== "Draft edit kept across same-hash OpenUI update" ||
    documentObj.getElementById("surla-prior-revision") === null ||
    !documentObj.getElementById("surla-prior-revision").textContent.includes(input.r1.request_revision) ||
    documentObj.getElementById("surla-incoming-change-card") === null) {
  throw new Error("same-hash nil -> programA did not render #surla-openui-workspace in place while preserving draft, priorCard, and incoming result");
}
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r2WithProgramB } }
});
const openuiV2 = documentObj.getElementById("surla-openui-workspace");
if (openuiV2 === null ||
    !openuiV2.textContent.includes("Refund Studio V2") ||
    !openuiV2.textContent.includes("Illustration V2") ||
    openuiV2.textContent.includes("Refund Studio V1") ||
    r2Check2Input.value !== "Draft edit kept across same-hash OpenUI update" ||
    documentObj.getElementById("surla-prior-revision") === null ||
    !documentObj.getElementById("surla-prior-revision").textContent.includes(input.r1.request_revision) ||
    documentObj.getElementById("surla-incoming-change-card") === null) {
  throw new Error("same-hash programA -> programB did not update #surla-openui-workspace in place while preserving draft, priorCard, and incoming result");
}
// Restore clean R2 before continuing clean R2 -> R3 check.
documentObj.getElementById("surla-discard-draft").click();
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r1 } }
});
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r2 } }
});

// Continue clean R2 -> tool-input-partial -> tool-input -> tool-result (R3).
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-input-partial",
    params: { arguments: { change_id: "chg-rev-ui", request_revision: input.r2.request_revision } }
  }
});
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-input",
    params: { arguments: { change_id: "chg-rev-ui", request_revision: input.r2.request_revision } }
  }
});
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r3 } }
});
const priorOnR3 = documentObj.getElementById("surla-prior-revision");
if (editorRegion.dataset.requestRevision !== input.r3.request_revision ||
    priorOnR3 === null ||
    !priorOnR3.textContent.includes(input.r2.request_revision) ||
    !priorOnR3.textContent.includes(input.r2.summary) ||
    documentObj.getElementById("surla-preserved-draft") !== null) {
  throw new Error("expected R3 to render #surla-prior-revision with R2 and no #surla-preserved-draft");
}

// 2. Dirty unsaved edits on R2 when R3 arrives -> holds R3 in #surla-incoming-change-card, and clicking #surla-replace-incoming-card mounts R3 with BOTH #surla-prior-revision (R2) AND #surla-preserved-draft with question text.
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r2 } }
});
const r2Pill = editorRegion.querySelector('[data-ambiguity-id="chg-rev-ui-ambiguity-01"][data-option="total_with_tax"]');
if (!r2Pill) {
  throw new Error("missing ambiguity option pill on R2");
}
r2Pill.click();
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r3 } }
});
if (editorRegion.dataset.requestRevision !== input.r2.request_revision ||
    documentObj.getElementById("surla-incoming-change-card") === null) {
  throw new Error("expected dirty R2 to stay mounted and R3 held in #surla-incoming-change-card");
}
documentObj.getElementById("surla-replace-incoming-card").click();
const priorDirtyR3 = documentObj.getElementById("surla-prior-revision");
const preservedDirtyR3 = documentObj.getElementById("surla-preserved-draft");
if (editorRegion.dataset.requestRevision !== input.r3.request_revision ||
    priorDirtyR3 === null ||
    !priorDirtyR3.textContent.includes(input.r2.request_revision) ||
    preservedDirtyR3 === null ||
    !preservedDirtyR3.textContent.includes("[" + input.r2.request_revision + "] chg-rev-ui-ambiguity-01 (Does the $50 limit include tax?)=total_with_tax") ||
    preservedDirtyR3.textContent.includes("check_01") ||
    preservedDirtyR3.textContent.includes("check_02")) {
  throw new Error("expected only changed ambiguity answer in #surla-preserved-draft on R3 (without unchanged examples), got: " + (preservedDirtyR3 && preservedDirtyR3.textContent));
}
documentObj.getElementById("surla-discard-draft").click();

// 2b. Editing ONE example on R2Confirmed while leaving saved answer (subtotal_only) and first example (check_01) untouched preserves ONLY the changed example in #surla-preserved-draft, and retains it across successive revision R3 -> R1 when R3 has a cleared example field.
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r2Confirmed } }
});
const r2ConfEx2 = editorRegion.querySelector('textarea[data-example-id="check_02"][data-field="expected_outcome"]');
r2ConfEx2.value = "VP finance approval over $50";
r2ConfEx2.dispatchEvent(new guestWindow.Event("input", { bubbles: true }));
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r3 } }
});
documentObj.getElementById("surla-replace-incoming-card").click();
const preservedOneEx = documentObj.getElementById("surla-preserved-draft");
if (preservedOneEx === null ||
    !preservedOneEx.textContent.includes("[" + input.r2Confirmed.request_revision + "] check_02: Refund over $50 | $75 subtotal -> VP finance approval over $50 [deterministic]") ||
    preservedOneEx.textContent.includes("chg-rev-ui-ambiguity-01") ||
    preservedOneEx.textContent.includes("subtotal_only") ||
    preservedOneEx.textContent.includes("check_01")) {
  throw new Error("expected #surla-preserved-draft to include ONLY changed check_02 and omit unchanged saved answer/example, got: " + (preservedOneEx && preservedOneEx.textContent));
}
// On R3, clear the example's expected_outcome to "" and switch to incoming R2 -> both the earlier changed check-02 line and R3's cleared check-01 line are retained.
const r3Ex1 = editorRegion.querySelector('textarea[data-example-id="check_01"][data-field="expected_outcome"]');
r3Ex1.value = "";
r3Ex1.dispatchEvent(new guestWindow.Event("input", { bubbles: true }));
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r2 } }
});
documentObj.getElementById("surla-replace-incoming-card").click();
const preservedSuccessive = documentObj.getElementById("surla-preserved-draft");
if (preservedSuccessive === null ||
    !preservedSuccessive.textContent.includes("[" + input.r2Confirmed.request_revision + "] check_02: Refund over $50 | $75 subtotal -> VP finance approval over $50 [deterministic]") ||
    !preservedSuccessive.textContent.includes("[" + input.r3.request_revision + "] check_01: Refund under $50 | $40 subtotal ->  [deterministic]") ||
    preservedSuccessive.textContent.includes("chg-rev-ui-ambiguity-01")) {
  throw new Error("expected successive revision #surla-preserved-draft to retain both R2Confirmed changed example and R3 cleared example without unchanged fields, got: " + (preservedSuccessive && preservedSuccessive.textContent));
}
documentObj.getElementById("surla-discard-draft").click();

// 3. Pending confirm_intent (R2 -> R2Confirmed) while tool-input-partial, tool-input, and tool-result (R3) arrive before RPC reply.
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r2 } }
});
editorRegion.querySelector('[data-ambiguity-id="chg-rev-ui-ambiguity-01"][data-option="subtotal_only"]').click();
const beforeConfirmCount = hostMessages.length;
documentObj.getElementById("surla-confirm-submit").click();
const confirmReq = hostMessages[beforeConfirmCount].msg;

messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-input-partial", params: { arguments: { change_id: "chg-rev-ui" } } }
});
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-input", params: { arguments: { change_id: "chg-rev-ui" } } }
});
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r3 } }
});
const incomingCardPanel = documentObj.getElementById("surla-incoming-change-card");
const replaceBtn = documentObj.getElementById("surla-replace-incoming-card");
if (editorRegion.dataset.requestRevision !== input.r2.request_revision ||
    incomingCardPanel === null ||
    replaceBtn === null) {
  throw new Error("expected R3 held in #surla-incoming-change-card while confirm_intent is pending");
}
replaceBtn.click();
if (!statusEl.textContent.includes("Wait for the pending CP1 Change Card request")) {
  throw new Error("expected switch button to wait while confirm_intent is pending");
}
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", id: confirmReq.id, result: { content: [], structuredContent: input.r2Confirmed } }
});
if (editorRegion.dataset.requestRevision !== input.r2Confirmed.request_revision ||
    documentObj.getElementById("surla-prior-revision") === null ||
    !documentObj.getElementById("surla-prior-revision").textContent.includes(input.r2.request_revision) ||
    documentObj.getElementById("surla-incoming-change-card") === null) {
  throw new Error("expected confirm_intent reply to mount R2Confirmed with prior R2 while keeping R3 in #surla-incoming-change-card");
}
documentObj.getElementById("surla-replace-incoming-card").click();
if (editorRegion.dataset.requestRevision !== input.r3.request_revision ||
    documentObj.getElementById("surla-prior-revision") === null ||
    !documentObj.getElementById("surla-prior-revision").textContent.includes(input.r2Confirmed.request_revision)) {
  throw new Error("expected switching to incoming R3 to set priorCard to R2Confirmed");
}

// 3b. Pending get_change (#surla-load-latest-card after stale error) while tool-input-partial, tool-input, and tool-result (R3) arrive before get_change RPC reply.
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r2 } }
});
editorRegion.querySelector('[data-ambiguity-id="chg-rev-ui-ambiguity-01"][data-option="total_with_tax"]').click();
const beforeStaleConfirm = hostMessages.length;
documentObj.getElementById("surla-confirm-submit").click();
const staleConfirmReq = hostMessages[beforeStaleConfirm].msg;
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", id: staleConfirmReq.id, result: { isError: true, content: [{ type: "text", text: "request_revision mismatch" }] } }
});
const beforeLoadLatest = hostMessages.length;
documentObj.getElementById("surla-load-latest-card").click();
const loadLatestReq = hostMessages[beforeLoadLatest].msg;
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-input-partial", params: { arguments: { change_id: "chg-rev-ui" } } }
});
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-input", params: { arguments: { change_id: "chg-rev-ui" } } }
});
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r3 } }
});
if (editorRegion.dataset.requestRevision !== input.r2.request_revision ||
    documentObj.getElementById("surla-incoming-change-card") === null) {
  throw new Error("expected R3 held in #surla-incoming-change-card while get_change is pending");
}
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", id: loadLatestReq.id, result: { content: [], structuredContent: input.r2Confirmed } }
});
if (editorRegion.dataset.requestRevision !== input.r2Confirmed.request_revision ||
    documentObj.getElementById("surla-prior-revision") === null ||
    !documentObj.getElementById("surla-prior-revision").textContent.includes(input.r2.request_revision) ||
    documentObj.getElementById("surla-preserved-draft") === null ||
    !documentObj.getElementById("surla-preserved-draft").textContent.includes("[" + input.r2.request_revision + "] chg-rev-ui-ambiguity-01 (Does the $50 limit include tax?)=total_with_tax") ||
    documentObj.getElementById("surla-incoming-change-card") === null) {
  throw new Error("expected get_change reply to mount R2Confirmed with prior R2 and question-annotated preserved draft while retaining incoming R3");
}
documentObj.getElementById("surla-discard-draft").click();
documentObj.getElementById("surla-replace-incoming-card").click();

// Switching to a different change_id clears priorCard (#surla-prior-revision is not shown).
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.cardOther } }
});
if (editorRegion.dataset.changeId !== "chg-other" ||
    documentObj.getElementById("surla-prior-revision") !== null) {
  throw new Error("expected switching to a different change_id to clear #surla-prior-revision");
}

// 4. Verify all 5 CP5 approval states and #surla-cp5-refresh-approval recovery.
// State 1: no record.
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r2Confirmed } }
});
if (documentObj.getElementById("surla-cp5-approval") !== null ||
    documentObj.getElementById("surla-cp5-unverified-approval") !== null) {
  throw new Error("State 1 (no record) must not render approval or unverified approval");
}

// State 2: current approval.
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r2Approved } }
});
if (documentObj.getElementById("surla-cp5-approval") === null ||
    documentObj.getElementById("surla-cp5-unverified-approval") !== null) {
  throw new Error("State 2 (current) must render #surla-cp5-approval only");
}

// State 3: changed inputs.
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r2ChangedInputs } }
});
if (documentObj.getElementById("surla-cp5-approval") !== null ||
    documentObj.getElementById("surla-cp5-unverified-approval") !== null) {
  throw new Error("State 3 (changed inputs) must not render #surla-cp5-approval or #surla-cp5-unverified-approval");
}

// State 4: unavailable target.
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r2UnavailableTarget } }
});
const unverifiedUnavailable = documentObj.getElementById("surla-cp5-unverified-approval");
const unavailableReason = documentObj.getElementById("surla-cp5-unverified-approval-reason");
if (documentObj.getElementById("surla-cp5-approval") !== null ||
    unverifiedUnavailable === null ||
    unavailableReason === null ||
    !unavailableReason.textContent.includes("could not report its current inputs") ||
    unavailableReason.textContent.includes("/var/lib/surla") ||
    documentObj.getElementById("surla-cp5-unverified-approval-details") === null ||
    documentObj.getElementById("surla-cp5-refresh-approval") === null) {
  throw new Error("State 4 (unavailable target) did not render honest path-free #surla-cp5-unverified-approval");
}

// State 5: absent target + refresh button recovery back to State 2 (current).
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.r2AbsentTarget } }
});
const absentReason = documentObj.getElementById("surla-cp5-unverified-approval-reason");
const refreshBtn = documentObj.getElementById("surla-cp5-refresh-approval");
if (absentReason === null || !absentReason.textContent.includes("-promotion-config") || refreshBtn === null) {
  throw new Error("State 5 (absent target) did not render -promotion-config reason and refresh button");
}
const beforeRefreshCount = hostMessages.length;
refreshBtn.click();
const refreshReq = hostMessages[beforeRefreshCount].msg;
if (refreshReq.method !== "tools/call" ||
    refreshReq.params.name !== "get_change" ||
    refreshReq.params.arguments.change_id !== "chg-rev-ui") {
  throw new Error("unexpected refresh button tools/call: " + JSON.stringify(refreshReq));
}
messageListener({
  source: parentWindow,
  data: { jsonrpc: "2.0", id: refreshReq.id, result: { content: [], structuredContent: input.r2Approved } }
});
if (documentObj.getElementById("surla-cp5-approval") === null ||
    documentObj.getElementById("surla-cp5-unverified-approval") !== null) {
  throw new Error("refreshing approval after target recovery did not transition to #surla-cp5-approval");
}
`
	cmd := exec.CommandContext(ctx, nodePath, "-e", nodeHarness)
	cmd.Stdin = strings.NewReader(string(harnessInput))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("MCP Apps revise/approval JS harness failed: %v\n%s", err, string(out))
	}
}
