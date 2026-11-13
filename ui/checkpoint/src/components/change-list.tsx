// Package components lists the project's saved Change Cards so the user can
// find work and open one change.

import React from "react";
import { ChangeList, ChangeListItem, LatestVerificationItem } from "../types";
import { Alert, Badge, Button, Card } from "./shadcn";

// ChangeListViewProps supplies the last list the server returned and the
// user's way to open one change or read the list again.
export interface ChangeListViewProps {
  // list is the last change list read, or null before the first read ends.
  list: ChangeList | null;
  // loading reflects an outstanding list_changes request.
  loading: boolean;
  // error describes the last failed list read, or null.
  error: string | null;
  // openError describes why the change the user last tried to open could
  // not be read, or null.
  openError: string | null;
  // openChangeId is the change currently open in the editor, if any.
  openChangeId: string | null;
  // busy disables opening while a Change Card request is outstanding.
  busy: boolean;
  // onOpen opens one change by its change_id.
  onOpen: (changeId: string) => void;
  // onRetry reads the list again after a failure.
  onRetry: () => void;
}

// verificationLabels describes saved history without claiming current proof.
const verificationLabels: Record<LatestVerificationItem["state"], string> = {
  running: "Latest saved run: in progress",
  passed: "Latest saved run: passed",
  failed: "Latest saved run: did not pass",
  not_completed: "Latest saved run: did not finish",
};

// ChangeListView renders every listed change as one keyboard-operable
// button, most recently saved first, or the empty state that explains how a
// change appears.
export function ChangeListView({
  list,
  loading,
  error,
  openError,
  openChangeId,
  busy,
  onOpen,
  onRetry,
}: ChangeListViewProps) {
  return (
    <Card
      id="surla-change-list"
      className="surla-card"
      aria-labelledby="surla-change-list-title"
      aria-busy={loading ? "true" : undefined}
    >
      <h2 id="surla-change-list-title">Changes</h2>
      {list?.changes.some((item) => item.latest_verification !== undefined) ? (
        <p id="surla-change-run-history" className="surla-muted">
          Saved run results describe their recorded inputs. The agreed revision or tests may
          have changed since. Open a change, then Proof, to inspect what its runs checked.
        </p>
      ) : null}
      {error !== null ? (
        <Alert id="surla-change-list-error" variant="destructive" role="alert">
          <strong>Could not read this project's changes. </strong>
          {error}
          <div className="surla-field-row">
            <Button id="surla-change-list-retry" disabled={loading} onClick={onRetry}>
              Read the changes again
            </Button>
          </div>
        </Alert>
      ) : null}
      {openError !== null ? (
        <Alert id="surla-change-open-error" variant="destructive" role="alert">
          {openError}
        </Alert>
      ) : null}
      {list === null ? (
        error === null ? <p role="status">Reading this project's changes...</p> : null
      ) : list.changes.length === 0 ? (
        <p id="surla-change-list-empty">
          This project has no changes yet. Ask the guide to propose a change; it appears here
          by itself.
        </p>
      ) : (
        <ul className="surla-compact-list" aria-label="Project changes">
          {list.changes.map((item) => (
            <ChangeListRow
              key={item.change_id}
              item={item}
              open={item.change_id === openChangeId}
              busy={busy}
              onOpen={onOpen}
            />
          ))}
        </ul>
      )}
    </Card>
  );
}

// ChangeListRow shows one change's summary and state before its identifiers.
function ChangeListRow({
  item,
  open,
  busy,
  onOpen,
}: {
  item: ChangeListItem;
  open: boolean;
  busy: boolean;
  onOpen: (changeId: string) => void;
}) {
  return (
    <li data-change-id={item.change_id}>
      <Button
        id={"surla-open-change-" + item.change_id}
        className="surla-change-open"
        aria-current={open ? "true" : undefined}
        disabled={busy}
        onClick={() => onOpen(item.change_id)}
      >
        {"Open: " + item.summary}
      </Button>{" "}
      <Badge variant={item.status === "confirmed" ? "success" : "warning"}>
        {item.status === "confirmed" ? "Confirmed" : "Needs confirmation"}
      </Badge>{" "}
      {item.proposed_revision ? (
        <>
          <Badge variant="warning" className="surla-proposal-badge">
            New revision proposed
          </Badge>{" "}
        </>
      ) : null}
      {item.latest_verification ? (
        <Badge aria-describedby="surla-change-run-history">
          {verificationLabels[item.latest_verification.state]}
        </Badge>
      ) : (
        <Badge>No saved runs</Badge>
      )}{" "}
      <span className="surla-muted">
        Saved <time dateTime={item.saved_at}>{new Date(item.saved_at).toLocaleString()}</time>
      </span>
      <details className="surla-details">
        <summary>Details</summary>
        <p>
          change_id: <code>{item.change_id}</code> · request_revision:{" "}
          <code>{item.request_revision}</code>
          {item.proposed_revision ? (
            <>
              {" "}
              · proposed revision: <code>{item.proposed_revision}</code>
            </>
          ) : null}
          {item.latest_verification ? (
            <>
              {" "}
              · latest run: <code>{item.latest_verification.run_id}</code>
            </>
          ) : null}
        </p>
      </details>
    </li>
  );
}
