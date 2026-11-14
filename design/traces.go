// This file declares the caller's saved, safe view of completed operations.
package design

import (
	. "goa.design/goa-ai/dsl"
	. "goa.design/goa/v3/dsl"
)

// TraceSpan describes one finished span without raw errors, events, or inputs.
var TraceSpan = Type("TraceSpan", func() {
	Description("Safe diagnostic details of one completed operation belonging to the authenticated caller. Raw errors, prompts, events, credentials, and filesystem paths are not included. OpenTelemetry export, when configured by the operator, retains the original spans.")
	Attribute("trace_id", String, "OpenTelemetry trace identifier.")
	Attribute("span_id", String, "OpenTelemetry span identifier.")
	Attribute("parent_span_id", String, "Parent span identifier; empty when this span has no parent.")
	Attribute("name", String, "Operation name, at most 128 bytes.")
	Attribute("start_time", String, "Operation start time in UTC.", func() { Format(FormatDateTime) })
	Attribute("end_time", String, "Operation finish time in UTC.", func() { Format(FormatDateTime) })
	Attribute("duration_ms", Int64, "Elapsed milliseconds.")
	Attribute("status", String, "Original span status: unset, ok, or error. An unset status does not establish success.", func() {
		Enum("unset", "ok", "error")
	})
	Attribute("summary", String, "Safe operation summary, at most 2048 bytes; never a raw error message.")
	Attribute("attributes", MapOf(String, String), "Selected operation identifiers, results, and private caller attribution; at most seven values of 1024 bytes each.")
	Attribute("shortened", Boolean, "True when text was shortened to fit this diagnostic record; use operator telemetry for the full values.")
	Required("trace_id", "span_id", "parent_span_id", "name", "start_time", "end_time", "duration_ms", "status", "summary", "attributes", "shortened")
})

// ListTracesPayload selects one bounded, newest-first diagnostic page.
var ListTracesPayload = Type("ListTracesPayload", func() {
	Description("Read a bounded page of saved operation diagnostics for the authenticated caller, newest first. Keep the filter unchanged when reading older pages.")
	Attribute("status", String, "all for every status, error for recorded failures, or ok for spans without an error status (including unset).", func() {
		Enum("all", "error", "ok")
		Default("all")
	})
	Attribute("limit", Int, "Maximum matching spans returned; a page may contain fewer because each call scans at most 256 KiB.", func() {
		Minimum(1)
		Maximum(100)
		Default(20)
	})
	Attribute("cursor", String, "Opaque next_cursor from the previous page; omit to refresh from the newest records.")
})

// TraceList reports one completed disk window, including sparse empty pages.
var TraceList = Type("TraceList", func() {
	Description("One newest-first diagnostic page. An empty spans array with has_more true means older records remain, not that the complete history contains no matches.")
	Attribute("spans", ArrayOf(TraceSpan), "Matching spans found in this page's bounded disk window.")
	Attribute("has_more", Boolean, "True when older records have not yet been examined.")
	Attribute("next_cursor", String, "Cursor to examine the next older window; present only when has_more is true.")
	Required("spans", "has_more")
})

// traceMethods exposes the card-store owner's diagnostic history over HTTP/MCP.
func traceMethods() {
	Method("list_traces", func() {
		Description("Read the project owner's saved operation diagnostics from the control plane's private storage; the user, the guide and the builder act for the owner and read the same history. Return bounded safe summaries newest first, without reading the entire file. Older pages and later refreshes work on the same running server and after restart. Recording is automatic in the project server. Authenticated HTTP/MCP domain calls have a surla.request.<operation> boundary span, with child domain operations recorded separately. Reads by the user's views page that succeed or are refused (sent with GET, such as the reads the page repeats whenever the project changes) are exported but not added to this private history; such a read that ends with an error status or a panic is added. Waiting reads are never added, whoever sends them; every other agent call and every operation that changes something is added. Requests rejected before authentication or decoding and separate verification processes are absent.")
		Payload(ListTracesPayload)
		Result(TraceList)
		Error("unprocessable", ErrorResult, "The cursor is invalid; omit it to refresh the history.")
		Error("not_implemented", ErrorResult, "This server does not record operations; a project server started with surla always does.")
		Tool("list_traces", "Read the project's saved operation diagnostics, newest first. status filters all spans, recorded errors, or spans without an error status. Empty spans with has_more true still leaves older history to examine: pass next_cursor as cursor with the same filter. Omit cursor to refresh. Summaries exclude raw errors and inputs; unset status does not prove success. Authenticated HTTP/MCP domain calls have a request boundary span and may also have child domain spans. Reads by the user's views page that succeed or are refused, and all waiting reads, do not enter the private history; a user's read that ends with an error status or a panic does, and so do agent calls other than waiting reads. Requests rejected before authentication or decoding and separate verification processes are absent. not_implemented means this server does not record operations, which a project server started with surla always does; it is not an empty history.")
		HTTP(func() {
			GET("/traces")
			Param("status")
			Param("limit")
			Param("cursor")
			Response(StatusOK)
			Response("unprocessable", StatusUnprocessableEntity)
			Response("not_implemented", StatusNotImplemented)
		})
	})
}
