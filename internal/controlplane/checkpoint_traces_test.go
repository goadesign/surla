// This file exercises diagnostics with the checkpoint's existing jsdom host
// protocol and generated wire codecs, including unsaved Change Card edits.
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

	gensurlatools "github.com/goadesign/surla/gen/surla/toolsets/surla"
	"github.com/stretchr/testify/require"
)

// checkpointTraceHost verifies this view's requests and UI while CP1 stays live.
const checkpointTraceHost = `
const fs = require("fs");
const {JSDOM} = require("../../ui/checkpoint/node_modules/jsdom");
const input = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
function expect(value, message) { if (!value) throw new Error(message); }
const dom = new JSDOM(input.html, {url:"https://checkpoint.example.invalid",runScripts:"outside-only",pretendToBeVisual:true});
const win = dom.window, doc = win.document, calls = [];
let listener;
const parent = {postMessage(message) { if (message.method === "tools/call") calls.push(message); }};
Object.defineProperty(win,"parent",{value:parent,configurable:true});
const add = win.addEventListener.bind(win);
win.addEventListener = (type, callback, options) => { if (type === "message") listener = callback; add(type,callback,options); };
win.eval(input.script);
const send = (data) => listener({source:parent,data});
send({jsonrpc:"2.0",id:1,result:{hostContext:{theme:"light"}}});
send({jsonrpc:"2.0",method:"ui/notifications/tool-result",params:{structuredContent:input.card}});
const field = () => doc.querySelector('textarea[data-field="expected_outcome"]');
const original = field();
original.value = "Unsaved outcome stays editable";
original.dispatchEvent(new win.Event("input",{bubbles:true}));
const toggle = () => doc.querySelector("#surla-trace-drawer-toggle");
const region = () => doc.querySelector("#surla-trace-drawer:not([hidden])");
const checkDraft = () => {
  expect(field() === original, "diagnostic refresh replaced the editor field");
  expect(field().value === "Unsaved outcome stays editable", "diagnostic read lost unsaved text");
  expect(!doc.querySelector("#surla-confirm-submit").disabled, "diagnostic read disabled CP1 saving");
};
const reply = (request, payload) => send({jsonrpc:"2.0",id:request.id,result:{structuredContent:payload}});
const last = () => calls[calls.length-1];
toggle().click();
expect(toggle().getAttribute("aria-expanded") === "true", "toggle did not announce expanded state");
expect(last().params.name === "list_traces", "opened diagnostics through another tool");
expect(JSON.stringify(last().params.arguments) === JSON.stringify({status:"error",limit:20}), "initial read did not default to recorded errors");
expect(region().getAttribute("aria-busy") === "true", "missing diagnostic loading state");
checkDraft();
doc.querySelector("#surla-trace-refresh").click();
expect(calls.length === 1, "duplicate refresh was sent while pending");
send({jsonrpc:"2.0",id:last().id,error:{code:-32000,message:"History unavailable. Retry."}});
expect(region().textContent.includes("History unavailable. Retry."), "read failure had no actionable message");
checkDraft();
Array.from(region().querySelectorAll("button")).find(b => b.textContent === "Retry from newest").click();
expect(!("cursor" in last().params.arguments), "retry reused an invalid older cursor");
reply(last(),input.first);
expect(region().getAttribute("aria-busy") === "false", "read stayed busy after reply");
expect(region().textContent.includes("Safe diagnostic <img src=x onerror=alert(1)>"), "summary was dropped");
expect(!region().querySelector("img"), "diagnostic text became executable HTML");
const details = region().querySelector("details");
expect(details && !details.open, "opaque identifiers were not progressively disclosed");
details.querySelector("summary").click();
expect(details.open, "details did not expand by its native summary control");
checkDraft();
doc.querySelector("#surla-trace-load-more").click();
expect(last().params.arguments.cursor === "100", "older page did not use the returned cursor");
expect(last().params.arguments.status === "error", "paging changed the active filter");
reply(last(),input.sparse);
expect(region().textContent.includes("this part of history"), "empty partial evidence claimed a complete result");
expect(doc.querySelector("#surla-trace-load-more"), "sparse page hid older history");
doc.querySelector("#surla-trace-load-more").click();
expect(last().params.arguments.cursor === "50", "sparse page did not advance");
reply(last(),input.empty);
expect(!doc.querySelector("#surla-trace-load-more"), "completed history still offered a cursor");
Array.from(region().querySelectorAll("button")).find(b => b.textContent === "All statuses").click();
expect(last().params.arguments.status === "all" && !("cursor" in last().params.arguments), "filter reused another filter's continuation");
reply(last(),input.unset);
expect(region().textContent.includes("Status unset"), "unset was relabeled as successful");
checkDraft();
toggle().click();
expect(!region(), "diagnostics did not collapse");
toggle().click();
expect(region(), "diagnostics did not reopen");
checkDraft();
send({jsonrpc:"2.0",id:77,method:"ui/resource-teardown",params:{}});
expect(!region(), "teardown retained diagnostic details");
dom.window.close();
process.stdout.write(JSON.stringify({passed:true}));
`

// TestCheckpointTracesPreserveUnsavedEdits exercises failures, sparse paging,
// refresh and teardown using the same host-call boundary as the real viewer.
func TestCheckpointTracesPreserveUnsavedEdits(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to exercise the checkpoint viewer")
	}
	ensureCheckpointUIDeps(t)
	card, _ := approvalRefreshCards(t)
	cursor := "100"
	span := &gensurlatools.TraceSpan{
		TraceID: strings.Repeat("1", 32), SpanID: strings.Repeat("2", 16),
		ParentSpanID: "", Name: "surla.controlplane.propose_change",
		StartTime: "2026-01-02T03:04:05Z", EndTime: "2026-01-02T03:04:06Z",
		DurationMs: 1000, Status: "error",
		Summary:    "Safe diagnostic <img src=x onerror=alert(1)>",
		Attributes: map[string]string{"surla.change_id": "change-1"},
	}
	first, err := gensurlatools.MarshalListTracesResult(&gensurlatools.ListTracesResult{
		Spans: []*gensurlatools.TraceSpan{span}, HasMore: true, NextCursor: &cursor,
	})
	require.NoError(t, err)
	cursor = "50"
	sparse, err := gensurlatools.MarshalListTracesResult(&gensurlatools.ListTracesResult{
		Spans: []*gensurlatools.TraceSpan{}, HasMore: true, NextCursor: &cursor,
	})
	require.NoError(t, err)
	empty, err := gensurlatools.MarshalListTracesResult(&gensurlatools.ListTracesResult{
		Spans: []*gensurlatools.TraceSpan{},
	})
	require.NoError(t, err)
	span.Status = "unset"
	unset, err := gensurlatools.MarshalListTracesResult(&gensurlatools.ListTracesResult{
		Spans: []*gensurlatools.TraceSpan{span},
	})
	require.NoError(t, err)
	fixture, err := json.Marshal(map[string]any{
		"html": checkpointUIHTML, "script": extractCheckpointScript(t, checkpointUIHTML),
		"card": card, "first": json.RawMessage(first), "sparse": json.RawMessage(sparse),
		"empty": json.RawMessage(empty), "unset": json.RawMessage(unset),
	})
	require.NoError(t, err)
	fixturePath := filepath.Join(t.TempDir(), "trace-host.json")
	require.NoError(t, os.WriteFile(fixturePath, fixture, 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "-e", checkpointTraceHost, fixturePath)
	cmd.Env = append(os.Environ(), "CI=true")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "diagnostic viewer failed:\n%s", out)
	require.JSONEq(t, `{"passed":true}`, string(out))
}
