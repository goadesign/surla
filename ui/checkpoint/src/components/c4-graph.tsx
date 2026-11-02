// Package components renders Checkpoint 2 C4 Architecture Previews, Structural
// Design Diffs, Checkpoint 3 Fast Draft Overrides, and Incoming CP1 Change Card
// notifications using shadcn/ui primitives.

import React from "react";
import {
  ArchitecturePreviewResult,
  C4ContainerItem,
  C4EdgeItem,
  ChangeCardResult,
  DiffResult,
  DraftOverrideResult,
  LiveTargetResult,
  StopLiveTargetResult,
} from "../types";
import { Alert, Badge, Button, Card } from "./shadcn";

function columnForKind(kind: string): number {
  if (kind === "person") {
    return 0;
  }
  if (kind === "service" || kind === "agent" || kind === "component") {
    return 1;
  }
  return 2;
}

interface NodePosition {
  id: string;
  name: string;
  kind: string;
  col: number;
  row: number;
  x: number;
  y: number;
}

// C4GraphSVG renders the 3-column deterministic SVG architecture topology with
// Bezier intra-column arcs and cubic self-loops.
export function C4GraphSVG({
  containers,
  edges,
}: {
  containers: C4ContainerItem[];
  edges: C4EdgeItem[];
}) {
  const colRows = [0, 0, 0];
  const colX = [20, 290, 560];
  const boxW = 210;
  const boxH = 54;
  const rowStep = 76;
  const posById: Record<string, NodePosition> = {};

  containers.forEach((c) => {
    const col = columnForKind(c.kind);
    const row = colRows[col]++;
    posById[c.id] = {
      id: c.id,
      name: c.name,
      kind: c.kind,
      col,
      row,
      x: colX[col],
      y: 24 + row * rowStep,
    };
  });

  const maxRows = Math.max(1, colRows[0], colRows[1], colRows[2]);
  const svgWidth = 840;
  const svgHeight = Math.max(140, maxRows * rowStep + 36);
  const pairCounts: Record<string, number> = {};

  return (
    <div
      className="surla-graph-wrap"
      role="region"
      aria-label="C4 architecture diagram"
      tabIndex={0}
    >
      <svg
        id="surla-c4-svg"
        role="img"
        aria-label={
          "C4 Architecture Directed Graph (" +
          containers.length +
          " containers, " +
          edges.length +
          " edges)"
        }
        viewBox={"0 0 " + svgWidth + " " + svgHeight}
        width={String(svgWidth)}
        height={String(svgHeight)}
      >
        <defs>
          <marker
            id="surla-arrow"
            viewBox="0 0 10 10"
            refX="9"
            refY="5"
            markerWidth="6"
            markerHeight="6"
            orient="auto-start-reverse"
          >
            <path d="M 0 0 L 10 5 L 0 10 z" fill="currentColor" />
          </marker>
        </defs>
        {edges.map((e, idx) => {
          const src = posById[e.from_id];
          const dst = posById[e.to_id];
          if (!src || !dst) {
            return null;
          }
          const pairKey =
            e.from_id < e.to_id ? e.from_id + "::" + e.to_id : e.to_id + "::" + e.from_id;
          const seen = pairCounts[pairKey] || 0;
          pairCounts[pairKey] = seen + 1;
          const curveOffset = (seen % 2 === 0 ? 1 : -1) * (12 + seen * 10);

          let pathD = "";
          let labelX = 0;
          let labelY = 0;
          if (e.from_id === e.to_id) {
            const sx = src.x + boxW;
            const sy = src.y + boxH / 2;
            pathD =
              "M " +
              sx +
              " " +
              (sy - 10) +
              " C " +
              (sx + 44) +
              " " +
              (sy - 32) +
              ", " +
              (sx + 44) +
              " " +
              (sy + 32) +
              ", " +
              sx +
              " " +
              (sy + 10);
            labelX = sx + 24;
            labelY = sy;
          } else if (src.col === dst.col) {
            const ix1 = src.x + boxW;
            const iy1 = src.y + boxH / 2;
            const ix2 = dst.x + boxW;
            const iy2 = dst.y + boxH / 2;
            const intraArc = 26 + seen * 18;
            const imx = ix1 + intraArc;
            const imy = (iy1 + iy2) / 2;
            pathD = "M " + ix1 + " " + iy1 + " Q " + imx + " " + imy + " " + ix2 + " " + iy2;
            labelX = imx + 4;
            labelY = imy + (seen % 2 === 0 ? -4 : 10);
          } else {
            const x1 = src.x <= dst.x ? src.x + boxW : src.x;
            const y1 = src.y + boxH / 2;
            const x2 = src.x <= dst.x ? dst.x : dst.x + boxW;
            const y2 = dst.y + boxH / 2;
            const mx = (x1 + x2) / 2;
            const my = (y1 + y2) / 2 + curveOffset;
            pathD = "M " + x1 + " " + y1 + " Q " + mx + " " + my + " " + x2 + " " + y2;
            labelX = mx;
            labelY = my - 4;
          }

          return (
            <g
              key={String(idx)}
              className="surla-svg-edge"
              data-from-id={e.from_id}
              data-to-id={e.to_id}
              data-edge-index={String(idx)}
            >
              <path
                d={pathD}
                fill="none"
                stroke="currentColor"
                strokeWidth="1.5"
                markerEnd="url(#surla-arrow)"
              />
              <text
                x={String(Math.round(labelX))}
                y={String(Math.round(labelY))}
                fill="currentColor"
                textAnchor="middle"
                fontSize="11"
              >
                {e.protocol}
              </text>
            </g>
          );
        })}
        {containers.map((c) => {
          const pos = posById[c.id];
          return (
            <g
              key={c.id}
              className="surla-svg-node"
              data-container-id={c.id}
              data-container-kind={c.kind}
              transform={"translate(" + pos.x + "," + pos.y + ")"}
            >
              <rect
                width={String(boxW)}
                height={String(boxH)}
                rx="6"
                ry="6"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.5"
              />
              <text x="10" y="21" fill="currentColor" fontSize="12" fontWeight="600">
                {c.name}
              </text>
              <text x="10" y="40" fill="currentColor" fontSize="11">
                {"[" + c.kind + "] " + c.id}
              </text>
            </g>
          );
        })}
      </svg>
    </div>
  );
}

// ArchitecturePreviewView renders a Checkpoint 2 C4 Architecture Preview.
export function ArchitecturePreviewView({ preview }: { preview: ArchitecturePreviewResult }) {
  return (
    <>
      <Card className="surla-meta">
        <h2>{"Architecture Preview: " + preview.system_name}</h2>
        <p>{preview.banner}</p>
        <details className="surla-details">
          <summary>Details</summary>
          <p>
            change_id: <code>{preview.change_id}</code> · request_revision:{" "}
            <code>{preview.request_revision}</code> · design_digest:{" "}
            <code>{preview.design_digest}</code>
          </p>
        </details>
      </Card>
      <Card className="surla-card">
        <h3>
          {"C4 Directed Graph (" +
            preview.containers.length +
            " nodes, " +
            preview.edges.length +
            " edges)"}
        </h3>
        <C4GraphSVG containers={preview.containers} edges={preview.edges} />
        <h3>{"C4 Containers (" + preview.containers.length + ")"}</h3>
        <ul>
          {preview.containers.map((c) => (
            <li key={c.id}>
              <Badge>{c.kind}</Badge>
              <code>{c.id}</code>
              {" (" + c.name + "): " + c.description}
            </li>
          ))}
        </ul>
        <h3>{"Directed Contract Edges (" + preview.edges.length + ")"}</h3>
        <ul>
          {preview.edges.map((e, idx) => (
            <li key={String(idx)}>
              <code>{e.from_id}</code>
              {" -> "}
              <code>{e.to_id}</code>
              {" [" + e.protocol + "]"}
            </li>
          ))}
        </ul>
        <h3>{"Evaluated Agent Policies (" + preview.agent_policies.length + ")"}</h3>
        <ul>
          {preview.agent_policies.map((p) => (
            <li key={p.agent_id}>
              <code>{p.agent_id}</code>
              {": time_budget=" +
                p.max_execution_seconds +
                "s, max_tool_calls=" +
                p.max_tool_calls +
                ", max_recovery_turns=" +
                p.max_recovery_turns +
                ", history=" +
                p.compress_at_turns +
                "/" +
                p.keep_recent_turns +
                ", cache_after_system=" +
                p.cache_after_system +
                ", cache_after_tools=" +
                p.cache_after_tools}
            </li>
          ))}
        </ul>
        <h3>{"Evaluated Tool Guardrails (" + preview.tools.length + ")"}</h3>
        <ul>
          {preview.tools.map((t) => (
            <li key={t.id}>
              <code>{t.id}</code>
              {" (" + t.toolset + "): " + t.description + " "}
              {t.injected_fields.length > 0 ? (
                <Badge>{"Inject: " + t.injected_fields.join(", ")}</Badge>
              ) : null}
              {t.requires_confirmation ? <Badge>Confirmation</Badge> : null}
              {t.bounded_result ? <Badge>BoundedResult</Badge> : null}
              {t.requires_ui ? <Badge>UI required</Badge> : null}
            </li>
          ))}
        </ul>
      </Card>
      <Card className="surla-card">
        <h3>{"Proposed Chorus Swarm (" + preview.swarm.status + ")"}</h3>
        <p>{preview.swarm.summary}</p>
        <p>
          <Badge>{"cross_family_review=" + preview.swarm.cross_family_review}</Badge>
          <Badge>{"cross_family_eval=" + preview.swarm.cross_family_eval}</Badge>
          <Badge>{"single_family=" + preview.swarm.single_family}</Badge>
        </p>
        {preview.swarm.assignments.length > 0 ? (
          <ul>
            {preview.swarm.assignments.map((r) => (
              <li key={r.handle}>
                <code>{"@" + r.handle}</code>
                {" [" +
                  r.role +
                  " / " +
                  r.scope_name +
                  "] -> " +
                  r.provider +
                  ":" +
                  r.model +
                  " (" +
                  r.family +
                  "), access=" +
                  r.access_mode +
                  ", paths=" +
                  r.owned_paths.join(", ")}
              </li>
            ))}
          </ul>
        ) : null}
        {preview.swarm.manifest ? (
          <details className="surla-details" id="surla-cp2-swarm-manifest">
            <summary>Chorus collaboration manifest</summary>
            <pre tabIndex={0}>
              <code>{preview.swarm.manifest}</code>
            </pre>
          </details>
        ) : null}
      </Card>
    </>
  );
}

function DiffList({
  prefix,
  items,
  emptyText,
}: {
  prefix: string;
  items: string[];
  emptyText: string;
}) {
  if (items.length === 0) {
    return <p>{emptyText}</p>;
  }
  return (
    <ul>
      {items.map((decl, idx) => (
        <li key={String(idx)}>
          <Badge>{prefix}</Badge>
          <code>{decl}</code>
        </li>
      ))}
    </ul>
  );
}

// DiffResultView renders an unbound Checkpoint 2 structural diff.
export function DiffResultView({ diff }: { diff: DiffResult }) {
  return (
    <>
      <Card className="surla-meta">
        <h2>Structural Goa &amp; Goa-AI Design Diff</h2>
        <p>{diff.summary}</p>
        <p>
          Standalone structural comparison (unbound to a CP1 Change Card or request_revision).
        </p>
        <details className="surla-details">
          <summary>Details</summary>
          <p>
            <Badge>{"has_changes=" + diff.has_changes}</Badge>
          </p>
        </details>
      </Card>
      <Card className="surla-card">
        <h3>{"Added Declarations (" + diff.added_declarations.length + ")"}</h3>
        <DiffList
          prefix="+"
          items={diff.added_declarations}
          emptyText="No added declarations."
        />
        <h3>{"Modified Declarations (" + diff.modified_declarations.length + ")"}</h3>
        <DiffList
          prefix="~"
          items={diff.modified_declarations}
          emptyText="No modified declarations."
        />
        <h3>{"Removed Declarations (" + diff.removed_declarations.length + ")"}</h3>
        <DiffList
          prefix="-"
          items={diff.removed_declarations}
          emptyText="No removed declarations."
        />
        <h3>{"Module Version Deltas (" + diff.version_deltas.length + ")"}</h3>
        <DiffList
          prefix="go.mod"
          items={diff.version_deltas}
          emptyText="No go.mod directive changes."
        />
      </Card>
    </>
  );
}

// DraftOverrideView renders a Checkpoint 3 Fast Draft Override result.
export function DraftOverrideView({ draft }: { draft: DraftOverrideResult }) {
  return (
    <Card className="surla-meta">
      <h2>Fast Draft Update</h2>
      <p>{draft.message}</p>
      <details className="surla-details">
        <summary>Details</summary>
        <p>
          <Badge>{"applied_in_store=" + draft.applied_in_store}</Badge>
          <Badge>{"requires_rebuild=" + draft.requires_rebuild}</Badge>
          {draft.prompt_version ? (
            <span>
              prompt_version: <code>{draft.prompt_version}</code>
            </span>
          ) : null}
        </p>
      </details>
    </Card>
  );
}

// LiveTargetView renders the CP3 live target a start_live_target call started
// or the outcome of stop_live_target. The commit stays in Details; the
// heading and message say what the operator can do next.
export function LiveTargetView({ result }: { result: LiveTargetResult | StopLiveTargetResult }) {
  if ("url" in result) {
    return (
      <Card className="surla-meta">
        <h2>Live Target Running</h2>
        <p>{result.message}</p>
        <p>
          The target listens at <code>{result.url}</code>, a loopback address on the Surla server's host. Send prompt text
          with draft_override. Drafts are not verified, and starting another target discards them: commit the accepted
          text and run verification.
        </p>
        <details className="surla-details">
          <summary>Details</summary>
          <p>
            candidate_commit: <code>{result.candidate_commit}</code>
          </p>
        </details>
      </Card>
    );
  }
  return (
    <Card className="surla-meta">
      <h2>{result.stopped ? "Live Target Stopped" : "No Live Target Running"}</h2>
      <p>{result.message}</p>
    </Card>
  );
}

// IncomingChangeCardView renders a notification banner when a new CP1 Change Card
// arrives while the operator has unsaved draft edits or an in-flight RPC.
export function IncomingChangeCardView({
  incomingCard,
  onSwitch,
}: {
  incomingCard: ChangeCardResult;
  onSwitch: () => void;
}) {
  return (
    <Card
      id="surla-incoming-change-card"
      className="surla-card"
      role="region"
      aria-label="Incoming Change Card"
    >
      <h3>Updated Change Card Available</h3>
      <p>{incomingCard.summary}</p>
      <div className="surla-field-row">
        <Button id="surla-replace-incoming-card" onClick={onSwitch}>
          Switch to Incoming Card (Keep Unsaved Edits)
        </Button>
      </div>
      <details className="surla-details">
        <summary>Details</summary>
        <p>
          change_id: <code>{incomingCard.change_id}</code> ({incomingCard.status}) · incoming
          request_revision: <code>{incomingCard.request_revision}</code>
        </p>
      </details>
    </Card>
  );
}

// ToolInputPartialView renders a live preview of partial tool arguments streamed
// via ui/notifications/tool-input-partial before the tool completes.
export function ToolInputPartialView({
  partialArgs,
}: {
  partialArgs: Record<string, unknown>;
}) {
  const keys = Object.keys(partialArgs);
  return (
    <Alert id="surla-partial-input-preview" role="status">
      <div className="surla-field-row">
        <strong>Streaming Tool Input Preview</strong>
        <Badge variant="accent">tool-input-partial</Badge>
      </div>
      {keys.length > 0 ? (
        <ul>
          {keys.map((k) => {
            const val = partialArgs[k];
            const text = typeof val === "string" ? val : JSON.stringify(val);
            return (
              <li key={k}>
                <code>{k}</code>: {text}
              </li>
            );
          })}
        </ul>
      ) : (
        <p>Streaming tool arguments...</p>
      )}
    </Alert>
  );
}
