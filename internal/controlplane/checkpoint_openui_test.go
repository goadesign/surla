// Package controlplane tests the Phase 10 OpenUI Decision Studio, shadcn/ui
// component rendering, host theme synchronization, partial tool-input preview,
// and build-artifact drift invariants of `ui://surla/checkpoint`.
package controlplane

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
)

var checkpointUIDepsOnce sync.Once

// ensureCheckpointUIDeps installs ui/checkpoint npm dependencies once when
// running tests in a clean checkout where node_modules is not yet populated.
func ensureCheckpointUIDeps(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("../../ui/checkpoint/node_modules/jsdom"); err == nil {
		return
	}
	npmPath, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not found in PATH and ui/checkpoint/node_modules is missing")
	}
	checkpointUIDepsOnce.Do(func() {
		cmd := exec.Command(npmPath, "ci", "--prefix", "../../ui/checkpoint", "--no-audit", "--no-fund")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("npm ci --prefix ../../ui/checkpoint: %v\n%s", err, string(out))
		}
	})
}

// TestCheckpointUIOpenUIDecisionStudioAndStreaming verifies that:
//  1. `checkpoint.html` and `decision_catalog.md` are byte-for-byte in sync
//     with `ui/checkpoint/src` (`node build.mjs --check`) and no authored source
//     uses `innerHTML` or `dangerouslySetInnerHTML`.
//  2. A Change Card carrying a valid `ui_composition` renders the real
//     `@openuidev/react-lang` `<Renderer />` (`DecisionWorkspace`,
//     `PolicySimulatorTable`, `PolicySimulatorRow`, `AmbiguityChoiceCard`,
//     `WorkflowTimeline`, `WorkflowStep`, `ImpactCallout`) inside
//     `#surla-openui-workspace` over the single `CP1DraftContext` draft owner.
//  3. Clicking an option in `PolicySimulatorTable` updates the canonical CP1
//     draft answer, highlights the active table column, and submits the
//     selected answer through `confirm_intent`.
//  4. Editing a confirmation example locally surfaces `#surla-openui-local-edit-notice`
//     qualifying the model-proposed simulator illustration.
//  5. Malformed, incomplete, out-of-range-ordinal, mismatched-outcomes, or
//     revision-skewed `ui_composition` payloads render `#surla-openui-error`
//     while keeping the canonical shadcn/ui CP1 inspector functional.
//  6. `ui/notifications/host-context-changed` and `ui/notifications/tool-input-partial`
//     update host theme and partial input previews without clobbering dirty
//     CP1 drafts.
func TestCheckpointUIOpenUIDecisionStudioAndStreaming(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found in PATH")
	}
	ensureCheckpointUIDeps(t)

	// 1. Verify zero build drift and no innerHTML in authored UI sources.
	checkCmd := exec.Command(nodePath, "../../ui/checkpoint/build.mjs", "--check")
	if out, err := checkCmd.CombinedOutput(); err != nil {
		t.Fatalf("checkpoint UI bundle drift check failed: %v\n%s", err, string(out))
	}
	err = filepath.Walk("../../ui/checkpoint/src", func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return walkErr
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(raw)
		if strings.Contains(src, "innerHTML") || strings.Contains(src, "dangerouslySetInnerHTML") {
			t.Fatalf("authored UI source %s must not use innerHTML or dangerouslySetInnerHTML", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk ui/checkpoint/src: %v", err)
	}

	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	validOpenUIProgram := strings.Join([]string{
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

	analysis := &gensurla.CP1Analysis{
		Summary: "Route refunds >= $50 to manager approval.",
		Ambiguities: []*gensurla.CP1AnalysisAmbiguity{
			{
				Question: "Does the $50 limit include tax?",
				Options:  []string{"subtotal_only", "total_with_tax"},
			},
		},
		Examples: []*gensurla.CP1AnalysisExample{
			{
				Scenario:         "Borderline $48 order",
				InputSummary:     "$48 subtotal + $4 tax",
				ExpectedOutcome:  "Auto-approved",
				VerificationKind: "deterministic",
			},
			{
				Scenario:         "Large $75 order",
				InputSummary:     "$75 subtotal + $6 tax",
				ExpectedOutcome:  "Manager approval required",
				VerificationKind: "deterministic",
			},
		},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
		UIComposition:      &validOpenUIProgram,
	}

	cp := newControlPlane(t, Options{})
	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)

	resRead, err := adapter.ResourcesRead(ctx, &genmcpadapter.ResourcesReadPayload{URI: "ui://surla/checkpoint"})
	if err != nil {
		t.Fatalf("ResourcesRead(ui://surla/checkpoint): %v", err)
	}
	completeRes, ok := resRead.Outcome.AsComplete()
	if !ok || len(completeRes.Contents) != 1 || completeRes.Contents[0].Text == nil {
		t.Fatalf("unexpected ResourcesRead outcome: %+v", resRead)
	}
	htmlDoc := *completeRes.Contents[0].Text
	for _, legacyV3Marker := range []string{"ZodEffects", "ZodParsedType"} {
		if strings.Contains(htmlDoc, legacyV3Marker) {
			t.Fatalf("checkpointUIHTML still bundles legacy Zod v3 marker %q", legacyV3Marker)
		}
	}
	scriptBody := extractCheckpointScript(t, htmlDoc)

	proposeArgs, err := gensurla.EncodeProposeChangePayload(&gensurla.ProposeChangePayload{
		ChangeID: "chg-openui",
		Prompt:   "Route refunds >= $50 to manager approval.",
		Analysis: analysis,
	})
	if err != nil {
		t.Fatalf("marshal proposeArgs: %v", err)
	}
	proposeCall, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{
		Name:      "propose_change",
		Arguments: proposeArgs,
	})
	if err != nil {
		t.Fatalf("ToolsCall(propose_change): %v", err)
	}
	proposeOut, ok := proposeCall.Outcome.AsComplete()
	if !ok || len(proposeOut.StructuredContent) == 0 {
		t.Fatalf("unexpected propose_change outcome: %+v", proposeCall)
	}

	harnessInput, err := json.Marshal(map[string]any{
		"htmlDoc":       htmlDoc,
		"script":        scriptBody,
		"cp1Structured": json.RawMessage(proposeOut.StructuredContent),
	})
	if err != nil {
		t.Fatalf("marshal harnessInput: %v", err)
	}

	nodeHarness := `
const fs = require("fs");
const { JSDOM } = require("../../ui/checkpoint/node_modules/jsdom");
const input = JSON.parse(fs.readFileSync(0, "utf8"));

const dom = new JSDOM(input.htmlDoc, {
  url: "https://sandbox.mcp.local/",
  runScripts: "outside-only",
  pretendToBeVisual: true
});
const win = dom.window;
const doc = win.document;
const root = doc.getElementById("surla-checkpoint-root");
const statusEl = doc.getElementById("surla-checkpoint-status");
const viewEl = doc.getElementById("surla-checkpoint-view");

const hostMessages = [];
let messageListener = null;
const parentWindow = {
  postMessage(msg, targetOrigin) {
    hostMessages.push({ msg, targetOrigin });
  }
};
Object.defineProperty(win, "parent", { value: parentWindow, configurable: true });
const origAddEventListener = win.addEventListener.bind(win);
win.addEventListener = function(type, fn, opts) {
  if (type === "message") messageListener = fn;
  return origAddEventListener(type, fn, opts);
};

win.eval(input.script);

// 1. Handshake with initial dark theme in hostContext.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    id: 1,
    result: {
      protocolVersion: "2026-01-26",
      hostInfo: { name: "surla-openui-host", version: "1.0.0" },
      hostCapabilities: {},
      hostContext: { theme: "dark" }
    }
  }
});
if (doc.documentElement.dataset.theme !== "dark") {
  throw new Error("expected initial dark theme on documentElement, got: " + doc.documentElement.dataset.theme);
}

// 2. Host switches theme via ui/notifications/host-context-changed.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/host-context-changed",
    params: { theme: "light" }
  }
});
if (doc.documentElement.dataset.theme !== "light") {
  throw new Error("expected light theme after host-context-changed, got: " + doc.documentElement.dataset.theme);
}

// 3. Host streams partial tool arguments via ui/notifications/tool-input-partial.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-input-partial",
    params: {
      arguments: {
        change_id: "chg-openui",
        prompt: "Route refunds >= $50..."
      }
    }
  }
});
const resultRegion = doc.getElementById("surla-result-view");
const editorRegion = doc.getElementById("surla-cp1-editor");
if (root.dataset.state !== "waiting" ||
    resultRegion.dataset.state !== "partial-input" ||
    !doc.getElementById("surla-partial-input-preview") ||
    !resultRegion.textContent.includes("Route refunds >= $50...")) {
  throw new Error("expected tool-input-partial preview in result region, got: " + resultRegion.textContent);
}

// 4. Deliver settled CP1 ChangeCardResult with valid OpenUI ui_composition.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-result",
    params: { structuredContent: input.cp1Structured }
  }
});
const openuiWorkspace = doc.getElementById("surla-openui-workspace");
if (!openuiWorkspace ||
    openuiWorkspace.getAttribute("data-composed-for-revision") !== input.cp1Structured.request_revision ||
    doc.getElementById("surla-openui-error")) {
  throw new Error("expected #surla-openui-workspace to render valid OpenUI composition");
}
const wsText = openuiWorkspace.textContent;
for (const want of [
  "Refund Threshold Decision Studio",
  "Tax Inclusion Matrix",
  "Borderline $48 order + $4 tax",
  "Auto-approved (< $50 subtotal)",
  "Subtotal keeps tax fluctuations out of approval routing.",
  "Refund Execution Flow",
  "Tenant Isolation Invariant"
]) {
  if (!wsText.includes(want)) {
    throw new Error("OpenUI workspace missing expected content " + JSON.stringify(want) + " in: " + wsText);
  }
}
const tableRegion = openuiWorkspace.querySelector('[role="region"][tabindex="0"] table.shadcn-table');
if (!tableRegion) {
  throw new Error("expected responsive shadcn-table inside focusable region");
}
const rowCells = tableRegion.querySelectorAll("tbody tr:first-child td[data-label]");
if (rowCells.length !== 4 ||
    rowCells[0].getAttribute("data-label") !== "Scenario" ||
    rowCells[1].getAttribute("data-label") !== "Input" ||
    rowCells[2].getAttribute("data-label") !== "subtotal_only" ||
    rowCells[3].getAttribute("data-label") !== "total_with_tax") {
  throw new Error("expected PolicySimulatorTable cells to carry data-label attributes for narrow responsive layouts");
}

// 5. Click an option button inside OpenUI PolicySimulatorTable -> updates canonical CP1 draft and active column highlight.
const simOptionBtn = openuiWorkspace.querySelector('[data-openui-ambiguity-id="chg-openui-ambiguity-01"][data-openui-option="subtotal_only"]');
if (!simOptionBtn) {
  throw new Error("missing PolicySimulatorTable option button");
}
simOptionBtn.click();
const canonicalPill = editorRegion.querySelector('[data-ambiguity-id="chg-openui-ambiguity-01"][data-option="subtotal_only"]');
const activeCells = openuiWorkspace.querySelectorAll(".shadcn-cell-active");
if (!canonicalPill ||
    canonicalPill.getAttribute("aria-pressed") !== "true" ||
    activeCells.length !== 3 ||
    doc.getElementById("surla-confirmed-decisions") ||
    !editorRegion.textContent.includes("Summary: ") ||
    !editorRegion.textContent.includes("[Selected draft answer: subtotal_only]") ||
    editorRegion.textContent.includes("[Confirmed answer: subtotal_only]")) {
  throw new Error("OpenUI option click did not synchronize canonical pill, draft label, or active column cells (activeCells=" + activeCells.length + ")");
}

// 6. Edit a confirmation example locally -> surfaces #surla-openui-local-edit-notice qualification alert.
if (doc.getElementById("surla-openui-local-edit-notice")) {
  throw new Error("unexpected local-edit notice before editing examples");
}
const outcomeTextarea = editorRegion.querySelector('textarea[data-example-id="check_01"][data-field="expected_outcome"]');
outcomeTextarea.value = "Auto-approved with audit log";
outcomeTextarea.dispatchEvent(new win.Event("input", { bubbles: true }));
if (!doc.getElementById("surla-openui-local-edit-notice")) {
  throw new Error("expected #surla-openui-local-edit-notice after editing example locally");
}

// 7. Partial tool input arriving while editor is dirty must NOT clobber the active CP1 editor.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-input-partial",
    params: { arguments: { change_id: "other" } }
  }
});
if (editorRegion.dataset.requestRevision !== input.cp1Structured.request_revision ||
    outcomeTextarea.value !== "Auto-approved with audit log") {
  throw new Error("tool-input-partial clobbered dirty CP1 editor");
}

// 8. Submit confirm_intent via #surla-confirm-submit -> sends OpenUI-selected answer and edited example.
const msgCountBeforeSubmit = hostMessages.length;
doc.getElementById("surla-confirm-submit").click();
if (hostMessages.length !== msgCountBeforeSubmit + 1) {
  throw new Error("expected confirm_intent tools/call message");
}
const confirmReq = hostMessages[msgCountBeforeSubmit].msg;

// 9. Verify fail-closed #surla-openui-error fallback on invalid/mismatched compositions while keeping canonical CP1 inspector working:
const badCases = [
  {
    name: "out-of-bounds ambiguityIndex",
    card: Object.assign({}, input.cp1Structured, {
      change_id: "chg-bad-1",
      ui_composition: {
        program: 'root = DecisionWorkspace("Bad", [AmbiguityChoiceCard(99, "oob")])',
        composed_for_revision: input.cp1Structured.request_revision
      }
    }),
    wantErr: "out of bounds"
  },
  {
    name: "outcomes length mismatch",
    card: Object.assign({}, input.cp1Structured, {
      change_id: "chg-bad-2",
      ui_composition: {
        program: 'root = DecisionWorkspace("Bad", [PolicySimulatorTable("Sim", 1, [PolicySimulatorRow("S", "I", ["Only one outcome"])])])',
        composed_for_revision: input.cp1Structured.request_revision
      }
    }),
    wantErr: "outcomes must have exactly 2 non-empty strings"
  },
  {
    name: "revision mismatch",
    card: Object.assign({}, input.cp1Structured, {
      change_id: "chg-bad-3",
      ui_composition: {
        program: input.cp1Structured.ui_composition.program,
        composed_for_revision: "rev-stale-mismatch"
      }
    }),
    wantErr: "does not match card.request_revision"
  },
  {
    name: "incomplete openui-lang",
    card: Object.assign({}, input.cp1Structured, {
      change_id: "chg-bad-4",
      ui_composition: {
        program: 'root = DecisionWorkspace("Unclosed", [AmbiguityChoiceCard(1',
        composed_for_revision: input.cp1Structured.request_revision
      }
    }),
    wantErr: "incomplete"
  },
  {
    name: "disallowed root child PolicySimulatorRow",
    card: Object.assign({}, input.cp1Structured, {
      change_id: "chg-bad-5",
      ui_composition: {
        program: 'root = DecisionWorkspace("D", [PolicySimulatorRow("S", "I", ["X", "Y"])])',
        composed_for_revision: input.cp1Structured.request_revision
      }
    }),
    wantErr: "Unsupported component type PolicySimulatorRow"
  },
  {
    name: "disallowed WorkflowTimeline child AmbiguityChoiceCard",
    card: Object.assign({}, input.cp1Structured, {
      change_id: "chg-bad-6",
      ui_composition: {
        program: 'root = DecisionWorkspace("D", [WorkflowTimeline("Flow", [AmbiguityChoiceCard(99)])])',
        composed_for_revision: input.cp1Structured.request_revision
      }
    }),
    wantErr: "Unsupported component type AmbiguityChoiceCard"
  },
  {
    name: "empty or whitespace-only program",
    card: Object.assign({}, input.cp1Structured, {
      change_id: "chg-bad-7",
      ui_composition: {
        program: " \n\t",
        composed_for_revision: input.cp1Structured.request_revision
      }
    }),
    wantErr: "OpenUI program is empty"
  },
  {
    name: "forged ElementNode object literal in component slot",
    card: Object.assign({}, input.cp1Structured, {
      change_id: "chg-bad-8",
      ui_composition: {
        program: 'root = DecisionWorkspace("D", [WorkflowTimeline("Flow", [{type: "element", typeName: "WorkflowStep", props: {}, partial: false, hasDynamicProps: false}])])',
        composed_for_revision: input.cp1Structured.request_revision
      }
    }),
    wantErr: "WorkflowStep props failed schema validation"
  },
  {
    name: "WorkflowStep with object-valued detail prop",
    card: Object.assign({}, input.cp1Structured, {
      change_id: "chg-bad-9",
      ui_composition: {
        program: 'root = DecisionWorkspace("D", [WorkflowTimeline("Flow", [WorkflowStep("S", "A", {bad: "x"})])])',
        composed_for_revision: input.cp1Structured.request_revision
      }
    }),
    wantErr: "validation failed"
  },
  {
    name: "forged ImpactCallout with unexpected extra property",
    card: Object.assign({}, input.cp1Structured, {
      change_id: "chg-bad-10",
      ui_composition: {
        program: 'root = DecisionWorkspace("D", [{type: "element", typeName: "ImpactCallout", props: {variant: "info", title: "Shown", summary: "Shown", unexpected: "Dropped model field"}, partial: false}])',
        composed_for_revision: input.cp1Structured.request_revision
      }
    }),
    wantErr: "ImpactCallout props failed schema validation"
  }
];

// Complete the pending confirm_intent first so editor is clean before testing badCases.
const confirmedWithoutUI = Object.assign({}, input.cp1Structured, {
  status: "confirmed",
  request_revision: "rev-after-confirm",
  answers: { "chg-openui-ambiguity-01": "subtotal_only" },
  ui_composition: undefined
});
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    id: confirmReq.id,
    result: { content: [], structuredContent: confirmedWithoutUI }
  }
});
if (doc.getElementById("surla-openui-workspace") ||
    !editorRegion.textContent.includes("[Confirmed answer: subtotal_only]")) {
  throw new Error("expected #surla-openui-workspace to unmount and confirmed answer label to appear after confirm_intent");
}
const confirmedDecisionsEl = doc.getElementById("surla-confirmed-decisions");
if (!confirmedDecisionsEl ||
    !confirmedDecisionsEl.textContent.includes("Confirmed Decisions:") ||
    !confirmedDecisionsEl.textContent.includes("Does the $50 limit include tax?") ||
    !confirmedDecisionsEl.textContent.includes("subtotal_only") ||
    !editorRegion.textContent.includes("Initial Proposal Summary: ")) {
  throw new Error("expected #surla-confirmed-decisions and Initial Proposal Summary heading on confirmed card with ambiguities, got: " + editorRegion.textContent);
}
const confirmedZeroAmbiguities = Object.assign({}, confirmedWithoutUI, {
  change_id: "chg-zero-amb",
  request_revision: "rev-zero-amb",
  ambiguities: [],
  answers: undefined
});
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-result",
    params: { structuredContent: confirmedZeroAmbiguities }
  }
});
if (doc.getElementById("surla-confirmed-decisions") ||
    !editorRegion.textContent.includes("Summary: ") ||
    editorRegion.textContent.includes("Initial Proposal Summary: ")) {
  throw new Error("expected zero-ambiguity confirmed card to omit #surla-confirmed-decisions and keep Summary heading, got: " + editorRegion.textContent);
}

for (const tc of badCases) {
  messageListener({
    source: parentWindow,
    data: {
      jsonrpc: "2.0",
      method: "ui/notifications/tool-result",
      params: { structuredContent: tc.card }
    }
  });
  const errEl = doc.getElementById("surla-openui-error");
  const errDetails = errEl && errEl.querySelector("details.surla-details");
  if (!errEl ||
      !errEl.textContent.includes("Interactive decision preview unavailable:") ||
      !errDetails ||
      !errDetails.textContent.includes(tc.wantErr) ||
      doc.getElementById("surla-openui-workspace")) {
    throw new Error("case " + tc.name + " did not render expected #surla-openui-error with Details disclosure (" + tc.wantErr + "), got: " + (errEl && errEl.textContent));
  }
  const fallbackPill = editorRegion.querySelector('[data-ambiguity-id="chg-openui-ambiguity-01"][data-option="total_with_tax"]');
  if (!fallbackPill) {
    throw new Error("case " + tc.name + " broke canonical CP1 inspector fallback");
  }
}

// 9b. Verify generated JSON Schema validation rejects undeclared wire properties (additionalProperties: false).
const extraPropCard = Object.assign({}, input.cp1Structured, {
  unexpected_wire_field: "must be rejected by generated schema"
});
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-result",
    params: { structuredContent: extraPropCard }
  }
});
if (root.dataset.state !== "error") {
  throw new Error("expected generated schema validator to reject undeclared property on ChangeCardResult, got state=" + root.dataset.state);
}

// 10. Workspace with only WorkflowTimeline + ImpactCallout still displays Model-Proposed Illustration and local-example-edit notice.
const timelineOnlyCard = Object.assign({}, input.cp1Structured, {
  change_id: "chg-timeline-only",
  ui_composition: {
    program: 'root = DecisionWorkspace("Flow Only", [WorkflowTimeline("Steps", [WorkflowStep("S1", "api", "D1")]), ImpactCallout("info", "Invariant", "Preserved")])',
    composed_for_revision: input.cp1Structured.request_revision
  }
});
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-result",
    params: { structuredContent: timelineOnlyCard }
  }
});
const timelineWs = doc.getElementById("surla-openui-workspace");
if (!timelineWs || !timelineWs.textContent.includes("Model-Proposed Illustration")) {
  throw new Error("expected workspace-level Model-Proposed Illustration badge on timeline/callout workspace");
}
const timelineTextarea = editorRegion.querySelector('textarea[data-example-id="check_01"][data-field="expected_outcome"]');
timelineTextarea.value = "Locally edited outcome on timeline workspace";
timelineTextarea.dispatchEvent(new win.Event("input", { bubbles: true }));
if (!doc.getElementById("surla-openui-local-edit-notice")) {
  throw new Error("expected workspace-level #surla-openui-local-edit-notice on timeline/callout workspace after example edit");
}

process.stdout.write(JSON.stringify({ confirmCall: confirmReq.params }));
`

	cmd := exec.CommandContext(ctx, nodePath, "-e", nodeHarness)
	cmd.Stdin = strings.NewReader(string(harnessInput))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("OpenUI Decision Studio harness failed: %v\n%s", err, string(out))
	}

	var captured struct {
		ConfirmCall struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"confirmCall"`
	}
	if err := json.Unmarshal(out, &captured); err != nil {
		t.Fatalf("unmarshal captured confirmCall: %v (raw=%s)", err, string(out))
	}
	confirmRes, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{
		Name:      captured.ConfirmCall.Name,
		Arguments: captured.ConfirmCall.Arguments,
	})
	if err != nil {
		t.Fatalf("adapter.ToolsCall(confirm_intent from OpenUI): %v", err)
	}
	confirmComplete, ok := confirmRes.Outcome.AsComplete()
	if !ok || len(confirmComplete.StructuredContent) == 0 {
		t.Fatalf("unexpected confirm_intent outcome: %+v", confirmRes)
	}
	var confirmedCard struct {
		Status        string          `json:"status"`
		Answers       map[string]any  `json:"answers"`
		UIComposition json.RawMessage `json:"ui_composition"`
	}
	if err := json.Unmarshal(confirmComplete.StructuredContent, &confirmedCard); err != nil {
		t.Fatalf("unmarshal confirmedCard: %v", err)
	}
	if confirmedCard.Status != StatusConfirmed ||
		confirmedCard.Answers["chg-openui-ambiguity-01"] != "subtotal_only" ||
		len(confirmedCard.UIComposition) != 0 {
		t.Fatalf("unexpected confirmedCard after OpenUI selection: %+v", confirmedCard)
	}
}
