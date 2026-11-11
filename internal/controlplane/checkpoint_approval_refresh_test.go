// Package controlplane tests the checkpoint viewer's approval refresh through
// the same host messages and generated Change Card results used by MCP Apps.
package controlplane

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	gensurlatools "github.com/goadesign/surla/gen/surla/toolsets/surla"
)

// approvalRefreshHost runs isolated jsdom guests with a parent host that records
// each outgoing request and supplies explicit responses. Every wait advances a
// JavaScript event turn; no polling or timer-based retry drives the behavior.
const approvalRefreshHost = `
const fs = require("fs");
const { JSDOM } = require("../../ui/checkpoint/node_modules/jsdom");
const input = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
const turn = () => new Promise((resolve) => setImmediate(resolve));
function expect(value, message) { if (!value) throw new Error(message); }
function draft(applied = true) {
  return {applied_in_store: applied, requires_rebuild: !applied,
    prompt_version: "sha256:" + "b".repeat(64), message: "Prompt update result"};
}
function open() {
  const dom = new JSDOM(input.html, {url:"https://checkpoint.example.invalid",runScripts:"outside-only",pretendToBeVisual:true});
  const win = dom.window, doc = win.document, calls = [];
  let listener;
  const parent = {postMessage(message) { if (message.method === "tools/call") calls.push(message); }};
  Object.defineProperty(win, "parent", {value:parent,configurable:true});
  const add = win.addEventListener.bind(win);
  win.addEventListener = (type, callback, options) => { if (type === "message") listener = callback; add(type,callback,options); };
  win.eval(input.script);
  const send = (data) => listener({source:parent,data});
  send({jsonrpc:"2.0",id:1,result:{hostContext:{theme:"light"}}});
  const result = (payload) => send({jsonrpc:"2.0",method:"ui/notifications/tool-result",params:{structuredContent:payload}});
  result(input.approved);
  const field = () => doc.querySelector('textarea[data-field="expected_outcome"]');
  function edit(value) {
    expect(field(), "example editor is missing");
    field().value = value;
    field().dispatchEvent(new win.Event("input", {bubbles:true}));
  }
  function reply(request, payload = input.approved) {
    send({jsonrpc:"2.0",id:request.id,result:{structuredContent:payload}});
  }
  const withoutApproval = {...input.approved};
  delete withoutApproval.approval;
  return {dom,win,doc,calls,send,result,field,edit,reply,withoutApproval};
}
function cardRead(request) {
  expect(request?.params.name === "get_change", "expected a card read");
  expect(request.params.arguments.change_id === input.approved.change_id, "read targets a different card");
}
async function main() {
  // A successful prompt edit refreshes the saved card; a compiled rule does not.
  {
    const h = open(); await turn(); h.edit("Unsaved revised outcome");
    h.result(draft(false)); expect(h.calls.length === 0, "compiled rule triggered an approval read");
    h.result(draft()); expect(h.calls.length === 1, "prompt edit did not request one card read");
    cardRead(h.calls[0]); h.reply(h.calls[0], h.withoutApproval); await turn();
    expect(!h.doc.querySelector("#surla-cp5-approval"), "stale approval remains after the card read");
    expect(h.field().value === "Unsaved revised outcome", "same-revision refresh erased editable draft text");
    expect(h.calls.length === 1, "approval refresh looped"); h.dom.window.close();
  }
  // Matching inputs keep a recorded approval and the editable draft.
  {
    const h = open(); await turn(); h.edit("Keep this draft"); h.result(draft());
    h.reply(h.calls[0]); await turn();
    expect(h.doc.querySelector("#surla-cp5-approval"), "matching inputs removed the approval");
    expect(h.field().value === "Keep this draft", "matching approval reset the draft");
    expect(!h.doc.body.textContent.includes("Approved for promotion"), "recorded snapshot claims continuing authority");
    expect(h.calls.length === 1, "matching result caused another read"); h.dom.window.close();
  }
  // A failed owner read is visible, retains the record and draft, and can be retried.
  {
    const h = open(); await turn(); h.edit("Preserve after error"); h.result(draft());
    h.send({jsonrpc:"2.0",id:h.calls[0].id,result:{isError:true,content:[{type:"text",text:"Target inputs are unavailable"}]}}); await turn();
    const alert = h.doc.querySelector("#surla-confirm-feedback [role=alert]");
    expect(alert?.textContent.includes("Target inputs are unavailable"), "owner-read error is not visible");
    expect(h.doc.querySelector("#surla-cp5-approval"), "failed read discarded the recorded check");
    expect(h.field().value === "Preserve after error", "failed read erased the draft");
    h.doc.querySelector("#surla-load-latest-card").click(); expect(h.calls.length === 2, "retry action did not read the card");
    h.reply(h.calls[1], h.withoutApproval); await turn();
    expect(!h.doc.querySelector("#surla-cp5-approval") && h.field().value === "Preserve after error", "retry did not retain the draft and replace saved metadata");
    h.dom.window.close();
  }
  // CP3 during confirmation waits for the save, even when a different view follows it.
  {
    const h = open(); await turn(); h.edit("Saved edited outcome"); h.doc.querySelector("#surla-confirm-submit").click();
    expect(h.calls.length === 1 && h.calls[0].params.name === "confirm_intent", "confirmation request is missing");
    expect(h.calls[0].params.arguments.examples[0].expected_outcome === "Saved edited outcome", "confirmation omitted edited text");
    h.result(draft()); h.result({has_changes:false,added_declarations:[],removed_declarations:[],modified_declarations:[],version_deltas:[],summary:"No schema changes"});
    expect(h.doc.querySelector("#surla-result-view").dataset.state === "cp2-diff", "intervening checkpoint fixture was rejected");
    expect(h.calls.length === 1, "prompt update ran a parallel card read");
    h.reply(h.calls[0], input.saved); await turn();
    expect(h.calls.length === 2, "prompt update was lost when another checkpoint arrived"); cardRead(h.calls[1]);
    h.reply(h.calls[1], input.saved); await turn();
    expect(h.field().value === "Saved edited outcome", "post-confirm refresh lost saved text");
    expect(h.calls.length === 2, "post-confirm refresh repeated"); h.dom.window.close();
  }
  // A later prompt update waits for an older card read; simultaneous updates share a new read.
  {
    const h = open(); await turn(); h.edit("Keep during read"); h.doc.querySelector("#surla-cp5-refresh-approval").click();
    expect(h.calls.length === 1, "explicit approval check is missing");
    h.result(draft()); h.result(draft()); expect(h.calls.length === 1, "prompt updates started parallel reads");
    h.reply(h.calls[0]); await turn(); expect(h.calls.length === 2, "updates did not produce one subsequent read");
    h.reply(h.calls[1], h.withoutApproval); await turn();
    expect(h.calls.length === 2, "coalesced updates caused duplicate reads");
    expect(h.field().value === "Keep during read" && !h.doc.querySelector("#surla-cp5-approval"), "read completion lost the draft or retained stale approval");
    h.dom.window.close();
  }
  // Tearing down the viewer releases pending continuations without dispatching another call.
  {
    const h = open(); await turn(); h.doc.querySelector("#surla-cp5-refresh-approval").click(); h.result(draft());
    h.send({jsonrpc:"2.0",id:"end-viewer",method:"ui/resource-teardown",params:{}}); await turn();
    expect(h.calls.length === 1, "teardown dispatched an approval read");
    expect(h.doc.querySelector("#surla-checkpoint-root").dataset.state === "terminated", "request continuation revived a closed viewer");
    h.dom.window.close();
  }
  process.stdout.write(JSON.stringify({cases:6,passed:true}));
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
`

// TestCheckpointUIRefreshesApprovalWithoutDiscardingEdits drives the built
// viewer through its real postMessage interface. Prompt updates must refresh
// retained cards, serialize behind an existing editor request, preserve draft
// text, and report an unreadable target without presenting continuing authority.
func TestCheckpointUIRefreshesApprovalWithoutDiscardingEdits(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to exercise the checkpoint viewer")
	}
	ensureCheckpointUIDeps(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	approved, saved := approvalRefreshCards(t)
	input, err := json.Marshal(map[string]any{
		"html":     checkpointUIHTML,
		"script":   extractCheckpointScript(t, checkpointUIHTML),
		"approved": approved,
		"saved":    saved,
	})
	require.NoError(t, err)
	fixture := filepath.Join(t.TempDir(), "host-input.json")
	require.NoError(t, os.WriteFile(fixture, input, 0o600))
	cmd := exec.CommandContext(ctx, node, "-e", approvalRefreshHost, fixture)
	cmd.Env = append(os.Environ(), "CI=true")
	// The host reads a finite fixture file; exec's nil stdin is /dev/null.
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "checkpoint approval refresh failed:\n%s", out)
	require.JSONEq(t, `{"cases":6,"passed":true}`, string(out))
}

// approvalRefreshCards obtains fixture results through the generated MCP
// adapter. The saved card contains the edit the host later confirms, so the
// test never substitutes an impossible unchanged response to a changed form.
func approvalRefreshCards(t *testing.T) (json.RawMessage, json.RawMessage) {
	t.Helper()
	ctx := WithPrincipal(t.Context(), Principal{ID: "approval-owner@example.invalid"})
	analysis := &gensurla.CP1Analysis{
		Summary:     "Refunds above $50 require a manager.",
		Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
		Examples: []*gensurla.CP1AnalysisExample{{
			Scenario: "Refund above $50", InputSummary: "Order total $75",
			ExpectedOutcome: "Wait for manager approval", VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{},
	}
	inputs := PromotionInputs{
		SuiteID: "orders_acceptance", SuiteCommit: strings.Repeat("2", 40),
		Environment: "sim", Artifacts: map[string]string{"orders": strings.Repeat("a", 64)},
	}
	target := &promotionTargetStub{inputs: inputs}
	cp := newControlPlane(t, Options{PromotionTarget: target})
	card, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{
		ChangeID: "approval-refresh", Prompt: "Refunds above $50 require a manager.",
		Analysis: analysis,
	})
	require.NoError(t, err)
	commit := strings.Repeat("1", 40)
	target.report = passingReport(t, card.RequestRevision, commit, inputs)
	_, err = cp.ApproveMerge(ctx, &gensurla.ApproveMergePayload{
		ChangeID: card.ChangeID, RequestRevision: card.RequestRevision,
		CandidateCommit: commit, ReportDigest: target.report.Digest,
	})
	require.NoError(t, err)
	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)
	args, err := json.Marshal(map[string]string{"change_id": card.ChangeID})
	require.NoError(t, err)
	result, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{Name: "get_change", Arguments: args})
	require.NoError(t, err)
	wire, ok := result.Outcome.AsComplete()
	require.True(t, ok)
	approved := json.RawMessage(wire.StructuredContent)
	example := card.Examples[0]
	confirm, err := gensurlatools.MarshalConfirmIntentPayload(&gensurlatools.ConfirmIntentPayload{
		ChangeID: card.ChangeID, RequestRevision: card.RequestRevision,
		Answers: map[string]string{},
		Examples: []*gensurlatools.ExampleItem{{
			ID: example.ID, Scenario: example.Scenario, InputSummary: example.InputSummary,
			ExpectedOutcome: "Saved edited outcome", VerificationKind: example.VerificationKind,
		}},
	})
	require.NoError(t, err)
	result, err = adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{Name: "confirm_intent", Arguments: confirm})
	require.NoError(t, err)
	wire, ok = result.Outcome.AsComplete()
	require.True(t, ok)
	content, err := json.Marshal(wire.Content)
	require.NoError(t, err)
	require.False(t, wire.IsError != nil && *wire.IsError, "confirmation failed: %s", content)
	require.NotEmpty(t, wire.StructuredContent, "confirmation returned no structured card: %s", content)
	savedCard, err := gensurlatools.UnmarshalConfirmIntentResult(wire.StructuredContent)
	require.NoError(t, err)
	require.NotEqual(t, card.RequestRevision, savedCard.RequestRevision, "saved edits must advance the card revision")
	return approved, json.RawMessage(wire.StructuredContent)
}
