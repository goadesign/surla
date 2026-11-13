// This file exercises the views page's diagnostics drawer through its real
// list_traces requests, including failures, sparse paging and unsaved
// Change Card edits that must survive every read.
package controlplane

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// traceFixtures answers list_traces with fixed pages through the real
// generated encoder; its first read fails as an unreadable history would.
type traceFixtures struct {
	*ControlPlane
	// mu guards failed.
	mu sync.Mutex
	// failed records that the first read already failed.
	failed bool
	// pages holds each page by "<status>|<cursor>".
	pages map[string]*gensurla.TraceList
}

// ListTraces fails once, then returns the page for the requested filter and cursor.
func (s *traceFixtures) ListTraces(_ context.Context, p *gensurla.ListTracesPayload) (*gensurla.TraceList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.failed {
		s.failed = true
		return nil, errors.New("History unavailable. Retry.")
	}
	key := p.Status + "|"
	if p.Cursor != nil {
		key += *p.Cursor
	}
	page, ok := s.pages[key]
	if !ok {
		return nil, gensurla.MakeUnprocessable(errors.New("no trace page " + key))
	}
	return page, nil
}

// TestCheckpointTracesPreserveUnsavedEdits exercises failures, sparse paging,
// filters and refresh while a Change Card holds unsaved edits.
func TestCheckpointTracesPreserveUnsavedEdits(t *testing.T) {
	cp := pageControlPlane(t, Options{})
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	mustPropose(t, cp, guide, "chg-traces", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
	cursor100, cursor50 := "100", "50"
	span := func(status string) *gensurla.TraceSpan {
		return &gensurla.TraceSpan{
			TraceID: strings.Repeat("1", 32), SpanID: strings.Repeat("2", 16),
			ParentSpanID: "", Name: "surla.controlplane.propose_change",
			StartTime: "2026-01-02T03:04:05Z", EndTime: "2026-01-02T03:04:06Z",
			DurationMs: 1000, Status: status,
			Summary:    "Safe diagnostic <img src=x onerror=alert(1)>",
			Attributes: map[string]string{"surla.change_id": "change-1"},
		}
	}
	svc := &traceFixtures{ControlPlane: cp, pages: map[string]*gensurla.TraceList{
		"error|":    {Spans: []*gensurla.TraceSpan{span("error")}, HasMore: true, NextCursor: &cursor100},
		"error|100": {Spans: []*gensurla.TraceSpan{}, HasMore: true, NextCursor: &cursor50},
		"error|50":  {Spans: []*gensurla.TraceSpan{}},
		"all|":      {Spans: []*gensurla.TraceSpan{span("unset")}},
	}}
	ps := startPageServer(t, svc)
	runPageScript(t, ps, nil, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor").dataset.changeId === "chg-traces", "the change did not open");
const field = () => page.$('textarea[data-field="expected_outcome"]');
const original = field();
page.type(original, "Unsaved outcome stays editable");
const toggle = () => page.$("#surla-trace-drawer-toggle");
const region = () => page.$("#surla-trace-drawer:not([hidden])");
const reads = () => page.sent("GET", "/traces");
const last = () => new URLSearchParams(reads()[reads().length - 1].search);
const checkDraft = () => {
  expect(field() === original, "diagnostic refresh replaced the editor field");
  expect(field().value === "Unsaved outcome stays editable", "diagnostic read lost unsaved text");
  expect(!page.$("#surla-confirm-submit").disabled, "diagnostic read disabled CP1 saving");
};
await control(origin, "hold", { key: "GET /traces" });
toggle().click();
expect(toggle().getAttribute("aria-expanded") === "true", "toggle did not announce expanded state");
expect(reads().length === 1 && last().get("status") === "error" && last().get("limit") === "20" && !last().has("cursor"), "initial read did not default to recorded errors");
expect(region().getAttribute("aria-busy") === "true", "missing diagnostic loading state");
checkDraft();
page.$("#surla-trace-refresh").click();
expect(reads().length === 1, "duplicate refresh was sent while pending");
await control(origin, "release", { key: "GET /traces" });
await until(() => region().textContent.includes("History unavailable. Retry."), "read failure had no actionable message");
checkDraft();
Array.from(region().querySelectorAll("button")).find((b) => b.textContent === "Retry from newest").click();
expect(!last().has("cursor"), "retry reused an invalid older cursor");
await until(() => region().getAttribute("aria-busy") === "false" && region().textContent.includes("Safe diagnostic <img src=x onerror=alert(1)>"), "summary was dropped");
expect(!region().querySelector("img"), "diagnostic text became executable HTML");
const details = region().querySelector("details");
expect(details && !details.open, "opaque identifiers were not progressively disclosed");
details.querySelector("summary").click();
expect(details.open, "details did not expand by its native summary control");
checkDraft();
page.$("#surla-trace-load-more").click();
expect(last().get("cursor") === "100", "older page did not use the returned cursor");
expect(last().get("status") === "error", "paging changed the active filter");
await until(() => region().textContent.includes("this part of history"), "empty partial evidence claimed a complete result");
expect(page.$("#surla-trace-load-more"), "sparse page hid older history");
page.$("#surla-trace-load-more").click();
expect(last().get("cursor") === "50", "sparse page did not advance");
await until(() => !page.$("#surla-trace-load-more"), "completed history still offered a cursor");
Array.from(region().querySelectorAll("button")).find((b) => b.textContent === "All statuses").click();
expect(last().get("status") === "all" && !last().has("cursor"), "filter reused another filter's continuation");
await until(() => region().textContent.includes("Status unset"), "unset was relabeled as successful");
checkDraft();
toggle().click();
expect(!region(), "diagnostics did not collapse");
toggle().click();
expect(region(), "diagnostics did not reopen");
checkDraft();
page.close();
`)
}
