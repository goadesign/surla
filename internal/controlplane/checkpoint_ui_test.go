// Package controlplane tests the stateless MCP Apps (`ui://surla/checkpoint`)
// viewer and its JSON-RPC 2.0 host lifecycle bridge.
package controlplane

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/swarm"
	"github.com/goadesign/surla/internal/testmodule"
	"goa.design/goa-ai/runtime/agent/prompt"
)

// TestCheckpointUIMCPAppsHandshakeAndToolResultRendering verifies that
// `ui://surla/checkpoint` serves a stateless MCP Apps viewer that performs
// the `ui/initialize` -> `ui/notifications/initialized` postMessage handshake
// with `window.parent`, rejects cross-origin messages where `event.source !==
// window.parent`, replaces prior views on each new tool result without server
// preview state, and renders exact `request_revision` and `design_digest`
// values for CP1, CP2, and CP3 tool results.
func TestCheckpointUIMCPAppsHandshakeAndToolResultRendering(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	analysisJSON, err := json.Marshal(CP1SemanticAnalysis{
		Summary: "Route refunds >= $50 to manager approval.",
		Ambiguities: []CP1ModelAmbiguity{
			{
				Question: "Does the $50 limit include tax?",
				Options:  []string{"subtotal_only", "total_with_tax"},
			},
		},
		Examples: []CP1ModelExample{
			{
				Scenario:         "Refund under $50",
				InputSummary:     "$40 subtotal",
				ExpectedOutcome:  "Auto-approved",
				VerificationKind: "deterministic",
			},
		},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
	})
	if err != nil {
		t.Fatalf("marshal analysis: %v", err)
	}

	targetStore := prompt.NewInMemoryStore()
	promotionTarget := &promotionTargetStub{
		inputs: PromotionInputs{
			SuiteID:     "orders_acceptance",
			SuiteCommit: "2222222222222222222222222222222222222222",
			Environment: "sim",
			Artifacts:   map[string]string{"orders": strings.Repeat("a", 64)},
		},
	}
	cp := newControlPlane(t, Options{
		Analyzer:        NewModelCP1Analyzer(newStubModelClient(t, string(analysisJSON))),
		TargetUpdater:   &boundTargetPromptUpdater{store: targetStore},
		PromotionTarget: promotionTarget,
		SwarmProfiles: []swarm.ProviderProfile{
			{
				ID:            "claude-opus",
				Provider:      "claude",
				Model:         "claude-opus-4-6",
				Family:        "anthropic",
				Authenticated: true,
				Capabilities:  []swarm.RoleKind{swarm.RoleArchitect, swarm.RoleBuilder, swarm.RoleEvalAuthor, swarm.RoleReviewer},
			},
			{
				ID:            "codex-gpt5",
				Provider:      "codex",
				Model:         "gpt-5.4",
				Family:        "openai",
				Authenticated: true,
				Capabilities:  []swarm.RoleKind{swarm.RoleBuilder, swarm.RoleEvalAuthor, swarm.RoleReviewer},
			},
		},
	})
	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)

	resRead, err := adapter.ResourcesRead(ctx, &genmcpadapter.ResourcesReadPayload{URI: "ui://surla/checkpoint"})
	if err != nil {
		t.Fatalf("ResourcesRead(ui://surla/checkpoint): %v", err)
	}
	completeRes, ok := resRead.Outcome.AsComplete()
	if !ok || len(completeRes.Contents) != 1 || completeRes.Contents[0].Text == nil || completeRes.Contents[0].MimeType == nil {
		t.Fatalf("unexpected ResourcesRead outcome: %+v", resRead)
	}
	if got := *completeRes.Contents[0].MimeType; got != "text/html;profile=mcp-app" {
		t.Fatalf("MimeType = %q, want %q", got, "text/html;profile=mcp-app")
	}
	htmlDoc := *completeRes.Contents[0].Text
	for _, want := range []string{
		`http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline';"`,
		`data-profile="mcp-app"`,
		`"ui/initialize"`,
		`"ui/notifications/initialized"`,
		`"ui/notifications/tool-input"`,
		`"ui/notifications/tool-input-partial"`,
		`"ui/notifications/host-context-changed"`,
		`"ui/notifications/tool-cancelled"`,
		`"ui/notifications/tool-result"`,
		`"ui/resource-teardown"`,
		`event.source !== window.parent`,
	} {
		if !strings.Contains(htmlDoc, want) {
			t.Fatalf("checkpointUIHTML missing %q", want)
		}
	}

	// Execute real MCP ToolsCall operations for CP1, CP2, and CP3 to obtain wire results.
	proposeArgs, err := json.Marshal(map[string]any{
		"change_id": "chg-ui-test",
		"prompt":    "Route refunds >= $50 to manager approval.",
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
	var proposedWire struct {
		ChangeID        string `json:"change_id"`
		RequestRevision string `json:"request_revision"`
	}
	if err := json.Unmarshal(proposeOut.StructuredContent, &proposedWire); err != nil {
		t.Fatalf("unmarshal proposedWire: %v", err)
	}

	confirmArgs, err := json.Marshal(map[string]any{
		"change_id":        proposedWire.ChangeID,
		"request_revision": proposedWire.RequestRevision,
		"answers": map[string]string{
			"chg-ui-test-ambiguity-01": "subtotal_only",
		},
		"examples": []map[string]any{
			{
				"id":                "chg-ui-test-check-01",
				"scenario":          "Refund under $50",
				"input_summary":     "$40 subtotal",
				"expected_outcome":  "  Instant store credit\nup to $50.00  ",
				"verification_kind": "deterministic",
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal confirmArgs: %v", err)
	}
	confirmCall, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{
		Name:      "confirm_intent",
		Arguments: confirmArgs,
	})
	if err != nil {
		t.Fatalf("ToolsCall(confirm_intent): %v", err)
	}
	confirmOut, ok := confirmCall.Outcome.AsComplete()
	if !ok || len(confirmOut.StructuredContent) == 0 {
		t.Fatalf("unexpected confirm_intent outcome: %+v", confirmCall)
	}
	var confirmedWire struct {
		ChangeID        string `json:"change_id"`
		RequestRevision string `json:"request_revision"`
	}
	if err := json.Unmarshal(confirmOut.StructuredContent, &confirmedWire); err != nil {
		t.Fatalf("unmarshal confirmedWire: %v", err)
	}

	getChangeArgs, err := json.Marshal(map[string]any{
		"change_id": confirmedWire.ChangeID,
	})
	if err != nil {
		t.Fatalf("marshal getChangeArgs: %v", err)
	}
	getChangeCall, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{
		Name:      "get_change",
		Arguments: getChangeArgs,
	})
	if err != nil {
		t.Fatalf("ToolsCall(get_change): %v", err)
	}
	getChangeOut, ok := getChangeCall.Outcome.AsComplete()
	if !ok || len(getChangeOut.StructuredContent) == 0 {
		t.Fatalf("unexpected get_change outcome: %+v", getChangeCall)
	}

	const candidateCommit = "1111111111111111111111111111111111111111"
	promotionTarget.report = passingReport(t, confirmedWire.RequestRevision, candidateCommit, promotionTarget.inputs)
	approveArgs, err := json.Marshal(map[string]any{
		"change_id":        confirmedWire.ChangeID,
		"request_revision": confirmedWire.RequestRevision,
		"candidate_commit": candidateCommit,
		"report_digest":    promotionTarget.report.Digest,
	})
	if err != nil {
		t.Fatalf("marshal approveArgs: %v", err)
	}
	approveCall, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{
		Name:      "approve_merge",
		Arguments: approveArgs,
	})
	if err != nil {
		t.Fatalf("ToolsCall(approve_merge): %v", err)
	}
	approveOut, ok := approveCall.Outcome.AsComplete()
	if !ok || len(approveOut.StructuredContent) == 0 {
		t.Fatalf("unexpected approve_merge outcome: %+v", approveCall)
	}

	candidateDesign := `package design

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
	previewArgs, err := json.Marshal(map[string]any{
		"change_id":        confirmedWire.ChangeID,
		"request_revision": confirmedWire.RequestRevision,
		"design": map[string]any{
			"files": testmodule.Snapshot(t, "", candidateDesign),
		},
		"builders": []map[string]any{
			{
				"name":  "orders",
				"paths": []string{"internal/**"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal previewArgs: %v", err)
	}
	previewCall, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{
		Name:      "preview_architecture",
		Arguments: previewArgs,
	})
	if err != nil {
		t.Fatalf("ToolsCall(preview_architecture): %v", err)
	}
	previewOut, ok := previewCall.Outcome.AsComplete()
	if !ok || len(previewOut.StructuredContent) == 0 {
		t.Fatalf("unexpected preview_architecture outcome: %+v", previewCall)
	}
	var previewWire struct {
		DesignDigest string `json:"design_digest"`
	}
	if err := json.Unmarshal(previewOut.StructuredContent, &previewWire); err != nil {
		t.Fatalf("unmarshal previewWire: %v", err)
	}

	draftArgs, err := json.Marshal(map[string]any{
		"target_kind": TargetKindPromptTemplate,
		"prompt_id":   "orders.refund.system",
		"new_content": "You are a concise refund specialist.",
	})
	if err != nil {
		t.Fatalf("marshal draftArgs: %v", err)
	}
	draftCall, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{
		Name:      "draft_override",
		Arguments: draftArgs,
	})
	if err != nil {
		t.Fatalf("ToolsCall(draft_override): %v", err)
	}
	draftOut, ok := draftCall.Outcome.AsComplete()
	if !ok || len(draftOut.StructuredContent) == 0 {
		t.Fatalf("unexpected draft_override outcome: %+v", draftCall)
	}

	stopLiveCall, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{
		Name:      "stop_live_target",
		Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("ToolsCall(stop_live_target): %v", err)
	}
	stopLiveOut, ok := stopLiveCall.Outcome.AsComplete()
	if !ok || len(stopLiveOut.StructuredContent) == 0 {
		t.Fatalf("unexpected stop_live_target outcome: %+v", stopLiveCall)
	}

	staleConfirmCall, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{
		Name:      "confirm_intent",
		Arguments: confirmArgs,
	})
	if err != nil {
		t.Fatalf("ToolsCall(confirm_intent stale): %v", err)
	}
	staleConfirmOut, ok := staleConfirmCall.Outcome.AsComplete()
	if !ok || staleConfirmOut.IsError == nil || !*staleConfirmOut.IsError || len(staleConfirmOut.Content) == 0 || staleConfirmOut.Content[0].Text == nil {
		t.Fatalf("expected stale confirm_intent tool error outcome, got: %+v", staleConfirmCall)
	}

	baselineDesign := `package design

import . "goa.design/goa/v3/dsl"

var _ = API("orders", func() {})

var _ = Service("orders", func() {
	Description("Baseline Orders service")
})
`
	diffArgs, err := json.Marshal(map[string]any{
		"old": map[string]any{
			"files": testmodule.Snapshot(t, "", baselineDesign),
		},
		"new": map[string]any{
			"files": testmodule.Snapshot(t, "", candidateDesign),
		},
	})
	if err != nil {
		t.Fatalf("marshal diffArgs: %v", err)
	}
	diffCall, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{
		Name:      "diff",
		Arguments: diffArgs,
	})
	if err != nil {
		t.Fatalf("ToolsCall(diff): %v", err)
	}
	diffOut, ok := diffCall.Outcome.AsComplete()
	if !ok || len(diffOut.StructuredContent) == 0 {
		t.Fatalf("unexpected diff outcome: %+v", diffCall)
	}

	nodePath, err := exec.LookPath("node")
	if err != nil {
		return
	}
	ensureCheckpointUIDeps(t)

	scriptBody := extractCheckpointScript(t, htmlDoc)
	harnessInput, err := json.Marshal(map[string]any{
		"htmlDoc":                htmlDoc,
		"script":                 scriptBody,
		"cp1Structured":          json.RawMessage(proposeOut.StructuredContent),
		"cp1ConfirmedStructured": json.RawMessage(confirmOut.StructuredContent),
		"cp5ApprovedStructured":  json.RawMessage(approveOut.StructuredContent),
		"getChangeStructured":    json.RawMessage(getChangeOut.StructuredContent),
		"cp2Structured":          json.RawMessage(previewOut.StructuredContent),
		"diffStructured":         json.RawMessage(diffOut.StructuredContent),
		"cp3Structured":          json.RawMessage(draftOut.StructuredContent),
		"cp3StopLiveStructured":  json.RawMessage(stopLiveOut.StructuredContent),
		"staleErrorText":         *staleConfirmOut.Content[0].Text,
		"wantCP1Revision":        proposedWire.RequestRevision,
		"wantCP2Revision":        confirmedWire.RequestRevision,
		"wantDesignDigest":       previewWire.DesignDigest,
		"wantReportDigest":       promotionTarget.report.Digest,
		"wantCandidateCommit":    candidateCommit,
		"wantAmbiguityText":      "Does the $50 limit include tax?",
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

guestWindow.Element.prototype.dispatch = function(type) {
  if (type === "click") {
    this.click();
    return;
  }
  this.dispatchEvent(new guestWindow.Event(type, { bubbles: true, cancelable: true }));
};

function wrapNode(node) {
  if (!node) return node;
  const raw = node.__rawNode || node;
  if (raw.nodeType !== 1) return raw;
  return new Proxy(raw, {
    get(target, prop) {
      if (prop === "__rawNode") return target;
      if (prop === "attributes") {
        const m = {};
        for (let i = 0; i < target.attributes.length; i++) {
          const a = target.attributes[i];
          m[a.name] = a.value;
        }
        return m;
      }
      if (prop === "tagName") {
        return String(target.tagName || "").toUpperCase();
      }
      if (prop === "className" && target.namespaceURI === "http://www.w3.org/2000/svg") {
        return { baseVal: target.getAttribute("class") || "" };
      }
      if (prop === "children") {
        return Array.from(target.children || []).map(wrapNode);
      }
      const val = target[prop];
      return typeof val === "function" ? val.bind(target) : val;
    },
    set(target, prop, value) {
      target[prop] = value;
      return true;
    }
  });
}

function findAll(node, pred, acc) {
  const out = acc || [];
  const raw = node && (node.__rawNode || node);
  if (!raw || raw.nodeType !== 1) return out;
  const wrapped = wrapNode(raw);
  if (pred(wrapped)) out.push(wrapped);
  const children = raw.children || [];
  for (let i = 0; i < children.length; i++) {
    findAll(children[i], pred, out);
  }
  return out;
}

function renderText(node) {
  if (!node) return "";
  return node.textContent || "";
}

guestWindow.eval(input.script);

if (typeof messageListener !== "function") {
  throw new Error("guest did not register window message listener");
}
if (hostMessages.length !== 1 || hostMessages[0].msg.method !== "ui/initialize" || hostMessages[0].msg.id !== 1) {
  throw new Error("expected initial ui/initialize request, got: " + JSON.stringify(hostMessages));
}

// 1. Forged message from non-parent source must be ignored.
messageListener({
  source: {},
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-result",
    params: { structuredContent: input.cp1Structured }
  }
});
if (root.dataset.state !== "waiting") {
  throw new Error("forged non-parent message mutated state: " + root.dataset.state);
}

// 2. Host replies to ui/initialize -> guest sends ui/notifications/initialized.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    id: 1,
    result: {
      protocolVersion: "2026-01-26",
      hostInfo: {
        name: "surla-test-host",
        version: "1.0.0"
      },
      hostCapabilities: {}
    }
  }
});
if (hostMessages.length !== 2 || hostMessages[1].msg.method !== "ui/notifications/initialized") {
  throw new Error("expected ui/notifications/initialized after init response, got: " + JSON.stringify(hostMessages));
}
if (root.dataset.initialized !== "true") {
  throw new Error("expected root.dataset.initialized === 'true'");
}

// 3. Deliver CP1 ChangeCardResult via ui/notifications/tool-result and exercise interactive confirm_intent form.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-result",
    params: { structuredContent: input.cp1Structured }
  }
});
const editorRegion = findAll(viewEl, (n) => n.attributes["id"] === "surla-cp1-editor")[0];
const resultRegion = findAll(viewEl, (n) => n.attributes["id"] === "surla-result-view")[0];
const cp1Text = renderText(viewEl);
if (root.dataset.state !== "cp1-change-card" || editorRegion.dataset.requestRevision !== input.wantCP1Revision) {
  throw new Error("unexpected CP1 dataset: " + JSON.stringify(editorRegion.dataset));
}
if (!cp1Text.includes(input.wantCP1Revision) ||
    !cp1Text.includes(input.wantAmbiguityText) ||
    !cp1Text.includes("Scenario:") ||
    !cp1Text.includes("Input:") ||
    !cp1Text.includes("Expected result:")) {
  throw new Error("CP1 view missing revision, ambiguity question, or visible field labels: " + cp1Text);
}

// 3a. Click an ambiguity option pill, edit an example outcome input, and submit confirm_intent via tools/call.
const optionPills = findAll(viewEl, (n) => n.attributes["data-ambiguity-id"] === "chg-ui-test-ambiguity-01" && n.attributes["data-option"] === "subtotal_only");
if (optionPills.length !== 1) {
  throw new Error("expected ambiguity option pill button, got: " + optionPills.length);
}
optionPills[0].dispatch("click");
const outcomeInputs = findAll(viewEl, (n) => n.attributes["data-example-id"] === "chg-ui-test-check-01" && n.attributes["data-field"] === "expected_outcome");
if (outcomeInputs.length !== 1 || outcomeInputs[0].tagName !== "TEXTAREA") {
  throw new Error("expected example expected_outcome textarea, got: " + outcomeInputs.length);
}
outcomeInputs[0].value = "  Instant store credit\nup to $50.00  ";
outcomeInputs[0].dispatch("input", { target: outcomeInputs[0] });
const submitBtns = findAll(viewEl, (n) => n.attributes["id"] === "surla-confirm-submit");
if (submitBtns.length !== 1) {
  throw new Error("expected #surla-confirm-submit button");
}
const msgCountBeforeConfirm = hostMessages.length;
submitBtns[0].dispatch("click");
// Duplicate click or input edits while in flight must be ignored and all edit controls disabled.
submitBtns[0].dispatch("click");
const inFlightPills = findAll(viewEl, (n) => n.attributes["data-ambiguity-id"] === "chg-ui-test-ambiguity-01");
const inFlightFormCards = findAll(viewEl, (n) => n.attributes["id"] === "surla-cp1-form-card");
outcomeInputs[0].value = "typed while saving";
outcomeInputs[0].dispatch("input", { target: outcomeInputs[0] });
if (hostMessages.length !== msgCountBeforeConfirm + 1 ||
    inFlightFormCards.length !== 1 ||
    inFlightFormCards[0].attributes["aria-busy"] !== "true" ||
    submitBtns[0].disabled !== true ||
    outcomeInputs[0].disabled !== true ||
    !inFlightPills.every((p) => p.disabled === true) ||
    outcomeInputs[0].value !== "  Instant store credit\nup to $50.00  ") {
  throw new Error("expected single tools/call request, aria-busy=true on #surla-cp1-form-card, and all edit controls disabled while in flight");
}
const confirmReq = hostMessages[msgCountBeforeConfirm].msg;
if (confirmReq.method !== "tools/call" ||
    confirmReq.params.name !== "confirm_intent" ||
    confirmReq.params.arguments.request_revision !== input.wantCP1Revision ||
    confirmReq.params.arguments.answers["chg-ui-test-ambiguity-01"] !== "subtotal_only" ||
    confirmReq.params.arguments.examples[0].expected_outcome !== "  Instant store credit\nup to $50.00  ") {
  throw new Error("unexpected confirm_intent tools/call payload: " + JSON.stringify(confirmReq));
}

// Uncorrelated tool-cancelled while confirm is pending must NOT clear pendingRequests or replace the active CP1 editor.
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", method: "ui/notifications/tool-cancelled", params: { reason: "uncorrelated cancel" } } });
if (root.dataset.state !== "cp1-change-card" || outcomeInputs[0].disabled !== true) {
  throw new Error("tool-cancelled while confirm pending deleted CP1 editor or re-enabled controls");
}
// Host replies with confirmed ChangeCardResult -> guest transitions to confirmed revision.
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", id: confirmReq.id, result: { content: [], structuredContent: input.cp1ConfirmedStructured } } });
if (root.dataset.state !== "cp1-change-card" ||
    editorRegion.dataset.requestRevision !== input.wantCP2Revision ||
    findAll(viewEl, (n) => n.attributes["id"] === "surla-cp1-form-card")[0].attributes["aria-busy"] !== undefined) {
  throw new Error("expected confirmed CP1 revision and cleared aria-busy after tools/call reply, got: " + JSON.stringify(editorRegion.dataset));
}
// Typing into confirmed card and receiving a late tool-result notification must NOT overwrite post-reply edits.
const postConfirmInputs = findAll(viewEl, (n) => n.attributes["data-example-id"] === "chg-ui-test-check-01" && n.attributes["data-field"] === "expected_outcome");
postConfirmInputs[0].value = "post-confirm draft edit";
postConfirmInputs[0].dispatch("input", { target: postConfirmInputs[0] });
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.cp1ConfirmedStructured } } });
if (postConfirmInputs[0].value !== "post-confirm draft edit") {
  throw new Error("late tool-result notification overwrote post-confirm edit");
}
postConfirmInputs[0].value = "  Instant store credit\nup to $50.00  ";
postConfirmInputs[0].dispatch("input", { target: postConfirmInputs[0] });
// Clicking Refine with no edits on a whitespace/newline-bearing saved card must preserve verbatim text.
const noEditSubmitBtns = findAll(viewEl, (n) => n.attributes["id"] === "surla-confirm-submit");
const msgCountBeforeNoEdit = hostMessages.length;
noEditSubmitBtns[0].dispatch("click");
const noEditRefineReq = hostMessages[msgCountBeforeNoEdit].msg;
if (noEditRefineReq.params.arguments.examples[0].expected_outcome !== "  Instant store credit\nup to $50.00  ") {
  throw new Error("no-edit refine mutated multiline/whitespace field: " + JSON.stringify(noEditRefineReq));
}
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", id: noEditRefineReq.id, result: { content: [], structuredContent: input.cp1ConfirmedStructured } } });

// 3a-cp5. Deliver CP5 approve_merge ChangeCardResult on the same request_revision -> updates active editor in place, renders #surla-cp5-approval, and shows #surla-cp5-approval-dirty-warning when local draft edits would supersede the approved revision.
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.cp5ApprovedStructured } } });
const approvalPanels = findAll(viewEl, (n) => n.attributes["id"] === "surla-cp5-approval");
const approvalText = approvalPanels.length === 1 ? renderText(approvalPanels[0]) : "";
if (approvalPanels.length !== 1 ||
    !approvalText.includes("Approval recorded") ||
    approvalText.includes("In-memory approval") ||
    approvalText.includes("in control-plane memory") ||
    !approvalText.includes("No merge or deploy side effect") ||
    !approvalText.includes("alice@example.com") ||
    !approvalText.includes(input.wantCandidateCommit) ||
    !approvalText.includes(input.wantReportDigest) ||
    !approvalText.includes("orders_acceptance") ||
    findAll(approvalPanels[0], (n) => n.attributes["id"] === "surla-cp5-approval-details").length !== 1 ||
    findAll(viewEl, (n) => n.attributes["id"] === "surla-cp5-approval-dirty-warning").length !== 0) {
  throw new Error("expected #surla-cp5-approval panel without dirty warning on clean approved card, got: " + approvalText);
}
const approvedOutcomeInputs = findAll(viewEl, (n) => n.attributes["data-example-id"] === "chg-ui-test-check-01" && n.attributes["data-field"] === "expected_outcome");
approvedOutcomeInputs[0].value = "edited after approval";
approvedOutcomeInputs[0].dispatch("input", { target: approvedOutcomeInputs[0] });
if (findAll(viewEl, (n) => n.attributes["id"] === "surla-cp5-approval-dirty-warning").length !== 1) {
  throw new Error("expected #surla-cp5-approval-dirty-warning when approved card has unsaved draft edits");
}
approvedOutcomeInputs[0].value = "  Instant store credit\nup to $50.00  ";
approvedOutcomeInputs[0].dispatch("input", { target: approvedOutcomeInputs[0] });
if (findAll(viewEl, (n) => n.attributes["id"] === "surla-cp5-approval-dirty-warning").length !== 0) {
  throw new Error("expected #surla-cp5-approval-dirty-warning to clear when draft matches saved approved card");
}

// 3b. Exercise stale-revision CP1 form submission with broadcast error notification arriving BEFORE direct RPC error reply.
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.cp1Structured } } });
const stalePills = findAll(viewEl, (n) => n.attributes["data-ambiguity-id"] === "chg-ui-test-ambiguity-01" && n.attributes["data-option"] === "total_with_tax");
stalePills[0].dispatch("click");
const staleOutcomeInputs = findAll(viewEl, (n) => n.attributes["data-example-id"] === "chg-ui-test-check-01" && n.attributes["data-field"] === "expected_outcome");
staleOutcomeInputs[0].value = "Edited outcome preserved across stale error";
staleOutcomeInputs[0].dispatch("input", { target: staleOutcomeInputs[0] });
const staleSubmitBtns = findAll(viewEl, (n) => n.attributes["id"] === "surla-confirm-submit");
const msgCountBeforeStale = hostMessages.length;
staleSubmitBtns[0].dispatch("click");
const staleReq = hostMessages[msgCountBeforeStale].msg;
// Host broadcasts uncorrelated ui/notifications/tool-result {isError:true} BEFORE delivering the matching RPC error response -> renders error in resultRegion while retaining CP1 editor and keeping controls disabled until matching RPC error response arrives.
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { isError: true, content: [{ type: "text", text: "preview_architecture failed: invalid design" }] } } });
if (resultRegion.dataset.state !== "error" || staleOutcomeInputs[0].disabled !== true || renderText(editorRegion).includes("Change Card request failed:")) {
  throw new Error("uncorrelated error notification replaced CP1 editor, routed into editor feedback, or prematurely re-enabled controls");
}
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", id: staleReq.id, result: { isError: true, content: [{ type: "text", text: input.staleErrorText }] } } });
const staleViewText = renderText(editorRegion);
if (editorRegion.dataset.state !== "cp1-change-card" ||
    staleOutcomeInputs[0].disabled !== false ||
    !statusEl.textContent.includes("Change Card request failed; unsaved edits preserved.") ||
    !staleViewText.includes(input.staleErrorText) ||
    !staleViewText.includes("Your edited answers and examples remain in this form") ||
    staleOutcomeInputs[0].value !== "Edited outcome preserved across stale error") {
  throw new Error("stale form submission did not update status, re-enable controls, preserve edits, or show recovery guidance: " + statusEl.textContent + " / " + staleViewText);
}

// 3c. Click #surla-load-latest-card after stale error -> guest calls get_change and renders latest card alongside #surla-preserved-draft.
const loadLatestBtns = findAll(viewEl, (n) => n.attributes["id"] === "surla-load-latest-card");
if (loadLatestBtns.length !== 1) {
  throw new Error("expected #surla-load-latest-card button after stale error");
}
const msgCountBeforeGetChange = hostMessages.length;
loadLatestBtns[0].dispatch("click");
if (hostMessages.length !== msgCountBeforeGetChange + 1) {
  throw new Error("expected tools/call get_change request after clicking load latest card");
}
const getChangeReq = hostMessages[msgCountBeforeGetChange].msg;
if (getChangeReq.method !== "tools/call" ||
    getChangeReq.params.name !== "get_change" ||
    getChangeReq.params.arguments.change_id !== "chg-ui-test") {
  throw new Error("unexpected get_change tools/call payload: " + JSON.stringify(getChangeReq));
}
// Host broadcasts ui/notifications/tool-result BEFORE delivering the tools/call get_change JSON-RPC response -> must retain active editor while get_change is pending, then render latest card + #surla-preserved-draft on the matching tools/call reply.
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.cp1ConfirmedStructured } } });
if (editorRegion.dataset.requestRevision !== input.wantCP1Revision) {
  throw new Error("interleaved ui/notifications/tool-result replaced active editor before get_change reply");
}
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", id: getChangeReq.id, result: { content: [], structuredContent: input.getChangeStructured } } });
// Same successful host tool-result notification arriving AFTER get_change reply must NOT drop #surla-preserved-draft.
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.getChangeStructured } } });
const recoveredViewText = renderText(viewEl);
const preservedPanels = findAll(viewEl, (n) => n.attributes["id"] === "surla-preserved-draft");
if (root.dataset.state !== "cp1-change-card" ||
    editorRegion.dataset.requestRevision !== input.wantCP2Revision ||
    preservedPanels.length !== 1 ||
    !recoveredViewText.includes("Preserved Unsaved Draft Edits") ||
    !recoveredViewText.includes("total_with_tax") ||
    !recoveredViewText.includes("Edited outcome preserved across stale error")) {
  throw new Error("get_change recovery did not retain latest card + preserved draft panel after late tool-result: " + recoveredViewText);
}

// 3d. Cross-view draft preservation, per-region dataset bindings, and pending-RPC result retention:
// Delivering diff while editor has preserved/dirty state keeps CP1 editor + preserved draft mounted AND renders diff in result region with region-scoped dataset bindings.
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.diffStructured } } });
if (root.dataset.state !== "cp2-diff" ||
    editorRegion.dataset.requestRevision !== input.wantCP2Revision ||
    resultRegion.dataset.state !== "cp2-diff" ||
    resultRegion.dataset.changeId !== undefined ||
    resultRegion.dataset.requestRevision !== undefined ||
    root.dataset.changeId !== undefined ||
    root.dataset.requestRevision !== undefined ||
    findAll(viewEl, (n) => n.attributes["id"] === "surla-preserved-draft").length !== 1 ||
    !renderText(viewEl).includes("refund_agent")) {
  throw new Error("diff notification during recovery dropped preserved draft, failed to render diff, or leaked dataset bindings");
}
// Repeated get_change recovery without new form edits must retain the already-preserved draft lines.
findAll(viewEl, (n) => n.attributes["id"] === "surla-confirm-submit")[0].dispatch("click");
const staleReq2 = hostMessages[hostMessages.length - 1].msg;
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", id: staleReq2.id, result: { isError: true, content: [{ type: "text", text: input.staleErrorText }] } } });
findAll(viewEl, (n) => n.attributes["id"] === "surla-load-latest-card")[0].dispatch("click");
const getChangeReq2 = hostMessages[hostMessages.length - 1].msg;
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", id: getChangeReq2.id, result: { content: [], structuredContent: input.getChangeStructured } } });
if (!renderText(editorRegion).includes("Edited outcome preserved across stale error")) {
  throw new Error("repeated get_change recovery lost prior preserved draft");
}
// Switching to an incoming card while the editor has both prior preserved draft lines and new dirty edits must preserve both in #surla-preserved-draft and initialize the form from the incoming card's own saved values.
const dirtyBeforeSwitchInputs = findAll(editorRegion, (n) => n.attributes["data-example-id"] === "chg-ui-test-check-01" && n.attributes["data-field"] === "expected_outcome");
dirtyBeforeSwitchInputs[0].value = "Dirty edit on confirmed card before incoming switch";
dirtyBeforeSwitchInputs[0].dispatch("input", { target: dirtyBeforeSwitchInputs[0] });
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.cp1Structured } } });
const incomingSwitchBtns = findAll(resultRegion, (n) => n.attributes["id"] === "surla-replace-incoming-card");
if (incomingSwitchBtns.length !== 1 || !renderText(incomingSwitchBtns[0]).includes("Keep Unsaved Edits")) {
  throw new Error("expected #surla-replace-incoming-card button with Keep Unsaved Edits label");
}
incomingSwitchBtns[0].dispatch("click");
const switchedOutcomeInputs = findAll(editorRegion, (n) => n.attributes["data-example-id"] === "chg-ui-test-check-01" && n.attributes["data-field"] === "expected_outcome");
const switchedPreservedPanels = findAll(editorRegion, (n) => n.attributes["id"] === "surla-preserved-draft");
const switchedPreservedText = switchedPreservedPanels.length === 1 ? renderText(switchedPreservedPanels[0]) : "";
if (editorRegion.dataset.requestRevision !== input.wantCP1Revision ||
    editorRegion.dataset.changeId !== "chg-ui-test" ||
    resultRegion.dataset.state !== undefined ||
    findAll(resultRegion, (n) => n.attributes["id"] === "surla-replace-incoming-card").length !== 0 ||
    switchedPreservedPanels.length !== 1 ||
    !switchedPreservedText.includes("Edited outcome preserved across stale error") ||
    !switchedPreservedText.includes("Dirty edit on confirmed card before incoming switch") ||
    !switchedPreservedText.includes(input.wantCP2Revision) ||
    switchedOutcomeInputs[0].value !== input.cp1Structured.examples[0].expected_outcome) {
  throw new Error("switching to incoming card did not preserve prior+dirty draft lines or initialize incoming card form values: " + switchedPreservedText);
}
// Clicking Discard Unsaved Edits after switching clears #surla-preserved-draft while keeping the switched card mounted.
findAll(editorRegion, (n) => n.attributes["id"] === "surla-discard-draft")[0].dispatch("click");
if (editorRegion.dataset.requestRevision !== input.wantCP1Revision ||
    findAll(editorRegion, (n) => n.attributes["id"] === "surla-preserved-draft").length !== 0) {
  throw new Error("Discard Unsaved Edits after incoming switch did not clear #surla-preserved-draft");
}
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.cp1ConfirmedStructured } } });
// Submitting confirm_intent while an incoming card B arrives in flight retains B and its switch button after A's reply completes.
findAll(viewEl, (n) => n.attributes["id"] === "surla-confirm-submit")[0].dispatch("click");
const pendingIncomingConfirmReq = hostMessages[hostMessages.length - 1].msg;
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.cp1Structured } } });
findAll(resultRegion, (n) => n.attributes["id"] === "surla-replace-incoming-card")[0].dispatch("click");
if (!statusEl.textContent.includes("Wait for the pending CP1 Change Card request")) {
  throw new Error("clicking switch button during pending RPC did not update status message");
}
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", id: pendingIncomingConfirmReq.id, result: { content: [], structuredContent: input.cp1ConfirmedStructured } } });
if (findAll(resultRegion, (n) => n.attributes["id"] === "surla-replace-incoming-card").length !== 1 ||
    editorRegion.dataset.requestRevision !== input.wantCP2Revision ||
    resultRegion.dataset.requestRevision !== input.wantCP1Revision) {
  throw new Error("matching confirm_intent reply cleared incoming card B or broke per-region dataset bindings");
}
// Submitting confirm_intent while a diff arrives in flight retains the diff and per-region dataset bindings after the matching confirm_intent reply completes.
const pendingConfirmSubmit = findAll(viewEl, (n) => n.attributes["id"] === "surla-confirm-submit")[0];
const msgCountBeforePendingDiff = hostMessages.length;
pendingConfirmSubmit.dispatch("click");
const pendingDiffConfirmReq = hostMessages[msgCountBeforePendingDiff].msg;
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", method: "ui/notifications/tool-result", params: { structuredContent: input.diffStructured } } });
messageListener({ source: parentWindow, data: { jsonrpc: "2.0", id: pendingDiffConfirmReq.id, result: { content: [], structuredContent: input.cp1ConfirmedStructured } } });
if (root.dataset.state !== "cp1-change-card" ||
    editorRegion.dataset.requestRevision !== input.wantCP2Revision ||
    resultRegion.dataset.state !== "cp2-diff" ||
    resultRegion.dataset.requestRevision !== undefined ||
    root.dataset.requestRevision !== undefined ||
    !renderText(viewEl).includes("refund_agent")) {
  throw new Error("diff arriving during pending confirm_intent was lost or leaked root dataset bindings after matching confirm_intent reply");
}

// 4. Deliver CP2 ArchitecturePreviewResult when CP1 editor is clean -> replaces clean CP1 view completely and renders both SVG graph and inspectable list.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-result",
    params: { structuredContent: input.cp2Structured }
  }
});
const cp2Text = renderText(viewEl);
if (root.dataset.state !== "cp2-architecture-preview" ||
    resultRegion.dataset.requestRevision !== input.wantCP2Revision ||
    resultRegion.dataset.designDigest !== input.wantDesignDigest) {
  throw new Error("unexpected CP2 dataset: " + JSON.stringify(resultRegion.dataset));
}
if (cp2Text.includes(input.wantAmbiguityText)) {
  throw new Error("CP2 view retained stale CP1 ambiguity text: " + cp2Text);
}
if (!cp2Text.includes(input.wantCP2Revision) ||
    !cp2Text.includes(input.wantDesignDigest) ||
    !cp2Text.includes("Inject: tenant_id") ||
    !cp2Text.includes("Confirmation")) {
  throw new Error("CP2 view missing expected digests or guardrails: " + cp2Text);
}
const cp2GraphWrap = findAll(viewEl, (n) => n.attributes["class"] === "surla-graph-wrap");
const cp2Svg = findAll(viewEl, (n) => n.attributes["id"] === "surla-c4-svg");
const cp2SvgNodes = findAll(viewEl, (n) => n.attributes["class"] === "surla-svg-node" && n.className && n.className.baseVal === "surla-svg-node");
const cp2SvgTexts = findAll(viewEl, (n) => n.tagName === "TEXT" && n.namespaceURI === "http://www.w3.org/2000/svg");
if (cp2GraphWrap.length !== 1 ||
    cp2GraphWrap[0].attributes["role"] !== "region" ||
    cp2GraphWrap[0].attributes["aria-label"] !== "C4 architecture diagram" ||
    cp2GraphWrap[0].attributes["tabindex"] !== "0" ||
    cp2Svg.length !== 1 ||
    cp2SvgNodes.length !== input.cp2Structured.containers.length ||
    cp2SvgTexts.length === 0 ||
    !cp2SvgTexts.every((t) => t.attributes["fill"] === "currentColor")) {
  throw new Error("expected focusable CP2 SVG graph region with all containers and fill=currentColor on all SVG text, got svg=" + cp2Svg.length + " nodes=" + cp2SvgNodes.length);
}

// 4a. Deliver 15-node cyclic + colliding-name + self-loop CP2 fixture -> no node cutoff, all 15 nodes and cyclic/self-loop edges rendered alongside inspectable list.
const largeContainers = [];
for (let i = 1; i <= 15; i++) {
  largeContainers.push({
    id: "node-" + i,
    name: i <= 3 ? "Duplicate Display Name" : "Container " + i,
    kind: i === 1 ? "person" : (i <= 8 ? "service" : "container"),
    description: "Description for node-" + i
  });
}
const cyclicEdges = [
  { from_id: "node-1", to_id: "node-2", protocol: "HTTPS" },
  { from_id: "node-2", to_id: "node-3", protocol: "gRPC" },
  { from_id: "node-3", to_id: "node-2", protocol: "Callback gRPC" },
  { from_id: "node-2", to_id: "node-2", protocol: "Self Loop" },
  { from_id: "node-8", to_id: "node-15", protocol: "SQLite" }
];
const largePreview = Object.assign({}, input.cp2Structured, {
  containers: largeContainers,
  edges: cyclicEdges,
  swarm: Object.assign({}, input.cp2Structured.swarm, {
    manifest: "# Chorus Collaboration Manifest\n@architect -> claude"
  })
});
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-result",
    params: { structuredContent: largePreview }
  }
});
const largeSvg = findAll(viewEl, (n) => n.attributes["id"] === "surla-c4-svg");
const largeSvgNodes = findAll(viewEl, (n) => n.attributes["class"] === "surla-svg-node");
const largeSvgEdges = findAll(viewEl, (n) => n.attributes["class"] === "surla-svg-edge");
const swarmManifestDetails = findAll(viewEl, (n) => n.attributes["id"] === "surla-cp2-swarm-manifest" && n.tagName === "DETAILS");
const largeText = renderText(viewEl);
const edge23Path = largeSvgEdges[1].children[0].attributes["d"];
const edge32Path = largeSvgEdges[2].children[0].attributes["d"];
if (largeSvg.length !== 1 ||
    largeSvg[0].attributes["width"] !== "840" ||
    largeSvgNodes.length !== 15 ||
    largeSvgEdges.length !== 5 ||
    swarmManifestDetails.length !== 1 ||
    !renderText(swarmManifestDetails[0]).includes("Chorus collaboration manifest") ||
    !renderText(swarmManifestDetails[0]).includes("# Chorus Collaboration Manifest") ||
    edge23Path === edge32Path ||
    !edge23Path.includes("Q 526") ||
    !edge32Path.includes("Q 544") ||
    !largeText.includes("node-15") ||
    !largeText.includes("Self Loop")) {
  throw new Error("expected 840px SVG with 15 nodes, #surla-cp2-swarm-manifest details, and distinct intra-column cyclic/self-loop edges, got width=" + (largeSvg[0] && largeSvg[0].attributes["width"]) + " e23=" + edge23Path + " e32=" + edge32Path);
}

// 4b. Deliver CP2 DiffResult from MCP diff codec -> renders unbound structural diff and clears CP1/CP2 dataset keys.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-result",
    params: { structuredContent: input.diffStructured }
  }
});
const diffText = renderText(viewEl);
if (root.dataset.state !== "cp2-diff" ||
    resultRegion.dataset.hasChanges !== "true" ||
    resultRegion.dataset.changeId !== undefined ||
    resultRegion.dataset.requestRevision !== undefined ||
    resultRegion.dataset.designDigest !== undefined) {
  throw new Error("unexpected cp2-diff dataset: " + JSON.stringify(resultRegion.dataset));
}
if (!diffText.includes("Standalone structural comparison") || !diffText.includes("refund_agent")) {
  throw new Error("cp2-diff view missing unbound label or added declaration: " + diffText);
}

// 5. Deliver CP3 DraftOverrideResult -> replaces CP2 view.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-result",
    params: { structuredContent: input.cp3Structured }
  }
});
const cp3Text = renderText(viewEl);
if (root.dataset.state !== "cp3-draft-override" || resultRegion.dataset.designDigest !== undefined) {
  throw new Error("unexpected CP3 dataset: " + JSON.stringify(resultRegion.dataset));
}
if (!cp3Text.includes("applied_in_store=true") || !cp3Text.includes("requires_rebuild=false")) {
  throw new Error("CP3 view missing fast-draft badges: " + cp3Text);
}

// 5b. Deliver a start_live_target result -> renders the running live target.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-result",
    params: { structuredContent: {
      candidate_commit: "4f0b3c1d2e5a6978877665544332211009988776",
      url: "http://127.0.0.1:41234",
      message: "Live target for commit 4f0b3c1d2e5a6978877665544332211009988776 is running at http://127.0.0.1:41234."
    } }
  }
});
const liveText = renderText(viewEl);
if (root.dataset.state !== "cp3-live-target" || !liveText.includes("Live Target Running") ||
    !liveText.includes("http://127.0.0.1:41234") || !liveText.includes("Drafts are not verified")) {
  throw new Error("unexpected live target view: " + root.dataset.state + " / " + liveText);
}

// 5c. Deliver the stop_live_target result the MCP codec produced -> no target ran.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-result",
    params: { structuredContent: input.cp3StopLiveStructured }
  }
});
if (root.dataset.state !== "cp3-live-target" || !renderText(viewEl).includes("No Live Target Running")) {
  throw new Error("unexpected stop live target view: " + renderText(viewEl));
}

// 6. Deliver tool-input notification -> transitions back to waiting state.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-input",
    params: { arguments: { change_id: "chg-next" } }
  }
});
if (root.dataset.state !== "waiting") {
  throw new Error("expected waiting state on tool-input notification, got: " + root.dataset.state);
}

// 7. Deliver isError tool-result -> transitions to explicit error state with error text.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-result",
    params: {
      isError: true,
      content: [{ type: "text", text: "stale request_revision" }]
    }
  }
});
if (root.dataset.state !== "error" || !renderText(viewEl).includes("stale request_revision")) {
  throw new Error("expected error state with tool error text, got: " + root.dataset.state + " / " + renderText(viewEl));
}

// 8. Deliver malformed tool-result -> transitions to explicit error state.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-result",
    params: { structuredContent: { bogus: true } }
  }
});
if (root.dataset.state !== "error") {
  throw new Error("expected error state on malformed payload, got: " + root.dataset.state);
}

// 9. Deliver tool-cancelled notification -> clears attribution and transitions to cancelled state.
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    method: "ui/notifications/tool-cancelled",
    params: { reason: "user aborted" }
  }
});
if (root.dataset.state !== "cancelled" || root.dataset.requestRevision !== undefined || !renderText(viewEl).includes("user aborted")) {
  throw new Error("expected cancelled state with cleared attribution, got: " + JSON.stringify(root.dataset) + " / " + renderText(viewEl));
}

// 10. Deliver host ping and ui/resource-teardown requests with id === 1 (same as INIT_REQUEST_ID) -> must respond with JSON-RPC results, not misclassify as init response.
const beforeCount = hostMessages.length;
messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    id: 1,
    method: "ping",
    params: {}
  }
});
if (hostMessages.length !== beforeCount + 1 || hostMessages[beforeCount].msg.id !== 1 || !hostMessages[beforeCount].msg.result) {
  throw new Error("expected ping result for id=1, got: " + JSON.stringify(hostMessages.slice(beforeCount)));
}

messageListener({
  source: parentWindow,
  data: {
    jsonrpc: "2.0",
    id: 1,
    method: "ui/resource-teardown",
    params: {}
  }
});
if (hostMessages.length !== beforeCount + 2 || hostMessages[beforeCount + 1].msg.id !== 1 || !hostMessages[beforeCount + 1].msg.result || root.dataset.state !== "terminated") {
  throw new Error("expected resource-teardown result for id=1 and terminated state, got: " + JSON.stringify(hostMessages.slice(beforeCount)) + " / " + root.dataset.state);
}

process.stdout.write(JSON.stringify({
  confirmCall: noEditRefineReq.params,
  staleCall: staleReq.params,
  getChangeCall: getChangeReq.params
}));
`
	cmd := exec.CommandContext(ctx, nodePath, "-e", nodeHarness)
	cmd.Stdin = strings.NewReader(string(harnessInput))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("MCP Apps JS bridge harness failed: %v\n%s", err, string(out))
	}

	// Execute the exact guest-emitted tools/call payloads through the real MCP adapter.
	var captured struct {
		ConfirmCall struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"confirmCall"`
		StaleCall struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"staleCall"`
		GetChangeCall struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"getChangeCall"`
	}
	if err := json.Unmarshal(out, &captured); err != nil {
		t.Fatalf("unmarshal guest tools/call output: %v (raw=%s)", err, string(out))
	}
	guestGetRes, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{
		Name:      captured.GetChangeCall.Name,
		Arguments: captured.GetChangeCall.Arguments,
	})
	if err != nil {
		t.Fatalf("adapter.ToolsCall(guest get_change): %v", err)
	}
	if getComplete, ok := guestGetRes.Outcome.AsComplete(); !ok || len(getComplete.StructuredContent) == 0 {
		t.Fatalf("unexpected guest get_change outcome: %+v", guestGetRes)
	}
	guestStaleRes, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{
		Name:      captured.StaleCall.Name,
		Arguments: captured.StaleCall.Arguments,
	})
	if err != nil {
		t.Fatalf("adapter.ToolsCall(guest stale confirm_intent): %v", err)
	}
	if staleComplete, ok := guestStaleRes.Outcome.AsComplete(); !ok || staleComplete.IsError == nil || !*staleComplete.IsError {
		t.Fatalf("expected guest stale confirm_intent to return tool error, got: %+v", guestStaleRes)
	}
	guestRefineRes, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{
		Name:      captured.ConfirmCall.Name,
		Arguments: captured.ConfirmCall.Arguments,
	})
	if err != nil {
		t.Fatalf("adapter.ToolsCall(guest confirm_intent no-edit refine): %v", err)
	}
	refineComplete, ok := guestRefineRes.Outcome.AsComplete()
	if !ok || len(refineComplete.StructuredContent) == 0 {
		t.Fatalf("unexpected guest confirm_intent refine outcome: %+v", guestRefineRes)
	}
	var refinedWire struct {
		RequestRevision string `json:"request_revision"`
	}
	if err := json.Unmarshal(refineComplete.StructuredContent, &refinedWire); err != nil {
		t.Fatalf("unmarshal refinedWire: %v", err)
	}
	if refinedWire.RequestRevision != confirmedWire.RequestRevision || !strings.Contains(string(refineComplete.StructuredContent), `  Instant store credit\nup to $50.00  `) {
		t.Fatalf("no-edit refine altered request_revision or multiline/whitespace field: got %q, want %q", refinedWire.RequestRevision, confirmedWire.RequestRevision)
	}
}

// extractCheckpointScript extracts the embedded <script>...</script> body from
// the CheckpointUI HTML document.
func extractCheckpointScript(t *testing.T, htmlDoc string) string {
	t.Helper()
	const openTag = "<script>"
	const closeTag = "</script>"
	start := strings.Index(htmlDoc, openTag)
	end := strings.LastIndex(htmlDoc, closeTag)
	if start < 0 || end < 0 || end <= start+len(openTag) {
		t.Fatal("checkpointUIHTML missing <script>...</script> block")
	}
	return htmlDoc[start+len(openTag) : end]
}
