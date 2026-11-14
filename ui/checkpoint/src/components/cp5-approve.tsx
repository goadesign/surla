// Package components offers the user's Checkpoint 5 control that approves a
// passed verification result for the exact Change Card revision shown.

import React from "react";
import { ChangeCardResult, VerificationStatusResult } from "../types";
import { Alert, Button, Card } from "./shadcn";

// ApproveResultControlProps supplies the displayed run, the card revision the
// page shows, and the state of the approval request.
export interface ApproveResultControlProps {
  // run is the displayed verification run; only a passed report is offered.
  run: VerificationStatusResult;
  // card is the Change Card the page shows for run's change, or null when it
  // is not loaded.
  card: ChangeCardResult | null;
  // busy reflects an outstanding Change Card request, including approval.
  busy: boolean;
  // available is false when the server cannot record approvals: its change
  // list says so, or it refused an approval as not set up.
  available: boolean;
  // unavailableReason is the server's message from a refused approval, or
  // null when only the change list said approval is unavailable.
  unavailableReason: string | null;
  // error is the last refused approval of this run, or null.
  error: string | null;
  // onApprove asks the server to record approval of this run's report.
  onApprove: () => void;
}

// ApproveResultControl renders nothing for a run without a passed report. It
// explains, rather than hides, why approval cannot be sent for this run.
export function ApproveResultControl({
  run,
  card,
  busy,
  available,
  unavailableReason,
  error,
  onApprove,
}: ApproveResultControlProps) {
  if (run.state.type !== "completed" || !run.state.value.report.passed) {
    return null;
  }
  const digest = run.state.value.report.report_digest;
  const recorded = card !== null && card.approval?.report_digest === digest;
  let blocked: string | null = null;
  if (card === null || card.change_id !== run.change_id) {
    blocked = "Open this result's Change Card to approve it.";
  } else if (card.status !== "confirmed") {
    blocked = "Confirm the Change Card before approving a result.";
  } else if (card.request_revision !== run.request_revision) {
    blocked =
      "This result verified a different revision of the Change Card than the one shown. Verify the shown revision, then approve its result.";
  }
  return (
    <Card
      id="surla-cp5-approve-region"
      className="surla-card"
      role="region"
      aria-labelledby="surla-cp5-approve-title"
      aria-busy={busy ? "true" : undefined}
    >
      <h3 id="surla-cp5-approve-title">Approve this result</h3>
      <p>
        Approval records that this passed result may be promoted for the Change Card revision
        shown. It does not merge or deploy anything.
      </p>
      {!available ? (
        <Alert id="surla-cp5-unavailable" variant="warning" role="alert" tabIndex={-1}>
          <strong>Approval is unavailable on this server. </strong>
          {unavailableReason !== null ? `${unavailableReason} ` : ""}
          Start Surla from a project directory so verification is configured.
        </Alert>
      ) : recorded ? (
        <p id="surla-cp5-approve-recorded" role="status">
          Approval of this result is recorded on the Change Card.
        </p>
      ) : (
        <>
          {blocked !== null ? <p id="surla-cp5-approve-blocked">{blocked}</p> : null}
          <div className="surla-field-row">
            <Button
              id="surla-cp5-approve"
              variant="primary"
              disabled={busy || blocked !== null}
              aria-describedby={blocked !== null ? "surla-cp5-approve-blocked" : undefined}
              onClick={onApprove}
            >
              {busy ? "Approving..." : "Approve this result"}
            </Button>
          </div>
        </>
      )}
      {error !== null ? (
        <Alert id="surla-cp5-approve-error" variant="destructive" role="alert" tabIndex={-1}>
          <strong>Approval was not recorded. </strong>
          {error}
        </Alert>
      ) : null}
    </Card>
  );
}
