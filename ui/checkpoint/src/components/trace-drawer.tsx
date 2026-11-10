// The diagnostic view uses the checkpoint's existing controls and typography.
// Reading a page never takes ownership of the Change Card's editable state.
import React from "react";
import { TraceList, TraceSpan } from "../types";
import { Alert, Button } from "./shadcn";

// TraceFilter adds the API's all-status option to the generated span statuses.
export type TraceFilter = "all" | Exclude<TraceSpan["status"], "unset">;

// TraceDrawerProps carries only this view's current page and request actions.
interface TraceDrawerProps {
  open: boolean;
  filter: TraceFilter;
  page: TraceList | null;
  error: string | null;
  busy: boolean;
  onToggle: () => void;
  onFilter: (filter: TraceFilter) => void;
  onRefresh: () => void;
  onOlder: () => void;
}

// TraceDrawer renders a nonmodal, keyboard-accessible view beside the editor.
// Unset remains visible as unset; a missing error status never proves success.
export function TraceDrawer(props: TraceDrawerProps) {
  return (
    <div className="surla-trace-view">
      <Button
        id="surla-trace-drawer-toggle"
        aria-expanded={props.open}
        aria-controls="surla-trace-drawer"
        onClick={props.onToggle}
      >
        {props.open ? "Hide traces" : "Traces"}
      </Button>
      <section
        id="surla-trace-drawer"
        hidden={!props.open}
        aria-labelledby="surla-trace-title"
        aria-busy={props.busy}
        className="surla-trace-panel"
      >
        <h2 id="surla-trace-title">Operation diagnostics</h2>
        <p className="surla-muted">
          Saved diagnostics for your instrumented operations, newest first.
          Raw errors and inputs stay with the server operator.
        </p>
        <div className="surla-trace-actions">
          <div role="group" aria-label="Trace status filter" className="surla-trace-actions">
            <Button variant="pill" aria-pressed={props.filter === "error"} disabled={props.busy} onClick={() => props.onFilter("error")}>
              Errors
            </Button>
            <Button variant="pill" aria-pressed={props.filter === "all"} disabled={props.busy} onClick={() => props.onFilter("all")}>
              All statuses
            </Button>
            <Button variant="pill" aria-pressed={props.filter === "ok"} disabled={props.busy} onClick={() => props.onFilter("ok")}>
              Without error status
            </Button>
          </div>
          <Button id="surla-trace-refresh" disabled={props.busy} onClick={props.onRefresh}>Refresh</Button>
        </div>
        {props.busy && <p role="status">Loading diagnostics…</p>}
        {props.error && (
          <Alert role="alert" variant="destructive">
            <p>{props.error}</p>
            <Button disabled={props.busy} onClick={props.onRefresh}>Retry from newest</Button>
          </Alert>
        )}
        {props.page && (
          <>
            {props.page.spans.length === 0 ? (
              <p role="status">
                {props.page.has_more
                  ? "No matching diagnostics in this part of history. Load older traces to continue."
                  : "No matching diagnostics in the remaining history. Refresh after another operation."}
              </p>
            ) : (
              <ol className="surla-trace-list" aria-label="Completed operation spans">
                {props.page.spans.map((span) => <TraceRow key={span.trace_id + ":" + span.span_id} span={span} />)}
              </ol>
            )}
            {props.page.has_more && (
              <Button id="surla-trace-load-more" disabled={props.busy} onClick={props.onOlder}>
                Load older traces
              </Button>
            )}
            <p className="surla-muted">Each page replaces the previous page. Refresh returns to the newest records.</p>
          </>
        )}
      </section>
    </div>
  );
}

// TraceRow puts the operation and safe summary before expandable identifiers.
function TraceRow({ span }: { span: TraceSpan }) {
  return (
    <li>
      <div className="surla-trace-heading">
        <strong>{span.name || "Unnamed operation"}</strong>
        <span>{span.status === "error" ? "Error" : span.status === "ok" ? "OK" : "Status unset"}</span>
      </div>
      <p>{span.summary}</p>
      <p className="surla-muted">
        <time dateTime={span.end_time}>{new Date(span.end_time).toLocaleString()}</time>
        {" · "}{span.duration_ms.toLocaleString()} ms
      </p>
      {span.shortened && <p className="surla-muted">Some text was shortened. Ask the operator for full telemetry if needed.</p>}
      <details className="surla-details">
        <summary>Identifiers and details</summary>
        <dl className="surla-trace-details">
          <dt>Trace</dt><dd><code>{span.trace_id}</code></dd>
          <dt>Span</dt><dd><code>{span.span_id}</code></dd>
          {span.parent_span_id && <><dt>Parent span</dt><dd><code>{span.parent_span_id}</code></dd></>}
          {Object.entries(span.attributes).map(([key, value]) => (
            <React.Fragment key={key}><dt>{key}</dt><dd><code>{value}</code></dd></React.Fragment>
          ))}
        </dl>
      </details>
    </li>
  );
}
