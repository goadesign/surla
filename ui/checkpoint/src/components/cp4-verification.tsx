// Package cp4-verification displays the evaluator's declared scenario outcomes,
// the saved verification run list for a Change Card, and the exact saved
// revision each run describes, without exposing private evidence.

import React from "react";
import {
  BuilderVerificationReport,
  ChangeCardResult,
  VerificationNotCompleted,
  VerificationScenario,
  VerificationStatusResult,
} from "../types";
import { Alert, Badge, Button, Card } from "./shadcn";

// VerificationViewProps supplies the active verification state, last loaded
// server list, and explicit user-triggered status, list, and card reads.
export interface VerificationViewProps {
  // changeId is the Change Card whose verification runs are being viewed.
  changeId: string;
  // run is the currently selected VerificationStatusResult snapshot, or null
  // when loading, empty, or failed before any run was selected.
  run: VerificationStatusResult | null;
  // listedRuns is the exact server-ordered array of runs from the last loaded
  // VerificationListResult for changeId, or null if no list has been loaded yet.
  listedRuns: VerificationStatusResult[] | null;
  // loadedCard is the Change Card currently retained in the session, if any.
  loadedCard: ChangeCardResult | null;
  // checking reflects an outstanding get_verification request for run.run_id.
  checking: boolean;
  // loadingList reflects an outstanding list_verifications request for changeId.
  loadingList: boolean;
  // loadingCard reflects an outstanding editor request (confirm_intent or get_change).
  loadingCard: boolean;
  // requestError leaves the last valid run visible when a status read fails.
  requestError: string | null;
  // listError reports a failed list_verifications request for changeId.
  listError: string | null;
  // cardLoadError reports a failed get_change request from CP4.
  cardLoadError: string | null;
  // onCheckStatus reads this exact run; it never restarts verification.
  onCheckStatus: () => void;
  // onLoadVerifications loads the saved verification runs for changeId.
  onLoadVerifications: () => void;
  // onSelectRun selects one run from the saved verification list.
  onSelectRun: (runId: string) => void;
  // onOpenChangeCard loads the Change Card for changeId when not already loaded.
  onOpenChangeCard: () => void;
  // onReturnToChangeCard switches the view back to the already-loaded Change Card.
  onReturnToChangeCard: () => void;
}

// scenarioOutcomeLabels translates the evaluator's outcomes without treating
// missing or unusable scenario evidence as a failed product assertion.
const scenarioOutcomeLabels: Record<VerificationScenario["outcome"], string> = {
  passed: "Passed",
  failed: "Failed",
  not_run: "Not run",
  harness_error: "Could not evaluate",
};

// failureStageLabels explains which operation stopped before a report existed.
const failureStageLabels: Record<VerificationNotCompleted["stage"], string> = {
  checkout: "Loading the saved source",
  build: "Building the programs",
  describe: "Reading the declared checks",
  suite: "Running the evaluator",
  report: "Saving the verification report",
  supervise: "Running the verification",
};

// runStatusHeading returns the user-facing status heading for a verification run.
function runStatusHeading(run: VerificationStatusResult): string {
  const state = run.state;
  if (state.type === "running") {
    return "Verification in progress";
  }
  if (state.type === "completed") {
    return state.value.report.passed
      ? "Verification passed"
      : "Verification did not pass";
  }
  return state.value.kind === "interrupted"
    ? "Verification interrupted"
    : "Verification could not finish";
}

// VerificationView renders the selected run or the loading/empty/error list
// state when no run is currently selected. Only a completed report supplies a
// verdict; a failed status or list read preserves already received snapshots.
export function VerificationView({
  changeId,
  run,
  listedRuns,
  loadedCard,
  checking,
  loadingList,
  loadingCard,
  requestError,
  listError,
  cardLoadError,
  onCheckStatus,
  onLoadVerifications,
  onSelectRun,
  onOpenChangeCard,
  onReturnToChangeCard,
}: VerificationViewProps) {
  const effectiveChangeId = run ? run.change_id : changeId;
  const sameCardLoaded = loadedCard !== null && loadedCard.change_id === effectiveChangeId;
  const displayedCardRevision = sameCardLoaded ? loadedCard.request_revision : null;
  const differentChangeLoaded =
    loadedCard !== null && loadedCard.change_id !== effectiveChangeId;

  if (run === null) {
    if (loadingList) {
      return (
        <Card
          id="surla-cp4-verification"
          aria-labelledby="surla-cp4-title"
          aria-busy="true"
        >
          <h2 id="surla-cp4-title">Loading saved verifications</h2>
          <p>Reading saved verification runs for this Change Card...</p>
          <div className="surla-field-row">
            <Button id="surla-load-verifications" disabled={true} onClick={onLoadVerifications}>
              Loading saved verifications...
            </Button>
            {sameCardLoaded ? (
              <Button id="surla-return-change-card" onClick={onReturnToChangeCard}>
                Return to Change Card (CP1 / CP5)
              </Button>
            ) : (
              <Button
                id="surla-open-change-card"
                disabled={loadingCard}
                onClick={onOpenChangeCard}
              >
                {loadingCard ? "Loading Change Card..." : "Load Change Card (CP1 / CP5)"}
              </Button>
            )}
          </div>
          <details className="surla-details" id="surla-cp4-details">
            <summary>Candidate and run details</summary>
            <dl className="surla-verification-details">
              <dt>Change Card</dt><dd><code>{effectiveChangeId}</code></dd>
            </dl>
          </details>
        </Card>
      );
    }

    if (listError !== null) {
      return (
        <Card
          id="surla-cp4-verification"
          aria-labelledby="surla-cp4-title"
        >
          <h2 id="surla-cp4-title">Could not load saved verifications</h2>
          <Alert id="surla-cp4-list-error" variant="destructive" role="alert">
            <strong>Could not load saved verifications. </strong>
            {listError}
          </Alert>
          {cardLoadError !== null && !sameCardLoaded ? (
            <Alert id="surla-cp4-card-error" variant="destructive" role="alert">
              {cardLoadError}
            </Alert>
          ) : null}
          <div className="surla-field-row">
            <Button
              id="surla-load-verifications"
              disabled={loadingList}
              onClick={onLoadVerifications}
            >
              Retry loading verifications
            </Button>
            {sameCardLoaded ? (
              <Button id="surla-return-change-card" onClick={onReturnToChangeCard}>
                Return to Change Card (CP1 / CP5)
              </Button>
            ) : (
              <Button
                id="surla-open-change-card"
                disabled={loadingCard}
                onClick={onOpenChangeCard}
              >
                {loadingCard ? "Loading Change Card..." : "Load Change Card (CP1 / CP5)"}
              </Button>
            )}
          </div>
          <details className="surla-details" id="surla-cp4-details">
            <summary>Candidate and run details</summary>
            <dl className="surla-verification-details">
              <dt>Change Card</dt><dd><code>{effectiveChangeId}</code></dd>
            </dl>
          </details>
        </Card>
      );
    }

    return (
      <Card
        id="surla-cp4-verification"
        aria-labelledby="surla-cp4-title"
      >
        <h2 id="surla-cp4-title">No saved verifications</h2>
        <p id="surla-cp4-empty">
          No verification runs are currently saved for this Change Card. Run verify_change on a
          confirmed Change Card revision, then load saved verifications here.
        </p>
        {cardLoadError !== null && !sameCardLoaded ? (
          <Alert id="surla-cp4-card-error" variant="destructive" role="alert">
            {cardLoadError}
          </Alert>
        ) : null}
        <div className="surla-field-row">
          <Button
            id="surla-load-verifications"
            disabled={loadingList}
            onClick={onLoadVerifications}
          >
            Load saved verifications
          </Button>
          {sameCardLoaded ? (
            <Button id="surla-return-change-card" onClick={onReturnToChangeCard}>
              Return to Change Card (CP1 / CP5)
            </Button>
          ) : (
            <Button
              id="surla-open-change-card"
              disabled={loadingCard}
              onClick={onOpenChangeCard}
            >
              {loadingCard ? "Loading Change Card..." : "Load Change Card (CP1 / CP5)"}
            </Button>
          )}
        </div>
        <details className="surla-details" id="surla-cp4-details">
          <summary>Candidate and run details</summary>
          <dl className="surla-verification-details">
            <dt>Change Card</dt><dd><code>{effectiveChangeId}</code></dd>
          </dl>
        </details>
      </Card>
    );
  }

  const state = run.state;
  const title = runStatusHeading(run);
  const unlistedInLastLoadedList =
    listedRuns !== null && !listedRuns.some((r) => r.run_id === run.run_id);

  return (
    <Card
      id="surla-cp4-verification"
      aria-labelledby="surla-cp4-title"
      aria-busy={checking || loadingList ? "true" : undefined}
    >
      <h2 id="surla-cp4-title">{title}</h2>
      {displayedCardRevision !== null && displayedCardRevision !== run.request_revision ? (
        <Alert variant="warning" role="status" id="surla-cp4-different-revision">
          This result applies to a different saved Change Card revision
          than the one shown here.
        </Alert>
      ) : null}
      {differentChangeLoaded ? (
        <Alert variant="warning" role="status" id="surla-cp4-different-change">
          {"This verification run belongs to Change Card " +
            run.change_id +
            " (currently loaded: " +
            loadedCard!.change_id +
            ")."}
        </Alert>
      ) : null}
      {unlistedInLastLoadedList ? (
        <p id="surla-cp4-unlisted-run" role="status">
          Not included in the last loaded list; reload to check.
        </p>
      ) : null}
      {listedRuns !== null && listedRuns.length > 0 ? (
        <div id="surla-cp4-run-list" role="group" aria-label="Saved verification runs">
          {listedRuns.map((r) => {
            const revShort = r.request_revision.slice(0, 15);
            const revLabel =
              displayedCardRevision !== null
                ? r.request_revision === displayedCardRevision
                  ? "Matches loaded card revision (" + revShort + ")"
                  : "Different card revision (" + revShort + ")"
                : "Card revision " + revShort;
            return (
              <Button
                key={r.run_id}
                id={"surla-cp4-run-" + r.run_id}
                variant="pill"
                data-run-id={r.run_id}
                aria-pressed={r.run_id === run.run_id ? "true" : "false"}
                onClick={() => onSelectRun(r.run_id)}
              >
                {runStatusHeading(r) +
                  " — candidate " +
                  r.candidate_commit.slice(0, 7) +
                  " — accepted " +
                  r.accepted_at +
                  " · " +
                  revLabel}
              </Button>
            );
          })}
        </div>
      ) : null}
      {state.type === "running" ? (
        <p>
          Building and evaluating the saved candidate. You can leave this view
          and check the result later.
        </p>
      ) : state.type === "completed" ? (
        <CompletedReport report={state.value.report} />
      ) : (
        <>
          <p>{state.value.summary}</p>
          <p>
            <strong>Stopped during: </strong>{failureStageLabels[state.value.stage]}.
          </p>
        </>
      )}

      {requestError !== null ? (
        <Alert variant="destructive" role="alert">
          <strong>Could not check verification status. </strong>
          {requestError}
          <p>The last received result is still shown.</p>
        </Alert>
      ) : null}
      {listError !== null ? (
        <Alert id="surla-cp4-list-error" variant="destructive" role="alert">
          <strong>Could not load saved verifications. </strong>
          {listError}
        </Alert>
      ) : null}
      {cardLoadError !== null && !sameCardLoaded ? (
        <Alert id="surla-cp4-card-error" variant="destructive" role="alert">
          {cardLoadError}
        </Alert>
      ) : null}
      <div className="surla-field-row">
        {state.type === "running" || requestError !== null ? (
          <Button
            id="surla-cp4-check-status"
            variant="primary"
            disabled={checking}
            onClick={onCheckStatus}
          >
            {checking ? "Checking status..." : "Check status"}
          </Button>
        ) : null}
        <Button
          id="surla-load-verifications"
          disabled={loadingList}
          onClick={onLoadVerifications}
        >
          {loadingList
            ? "Loading saved verifications..."
            : listError !== null
            ? "Retry loading verifications"
            : "Load saved verifications"}
        </Button>
        {!sameCardLoaded ? (
          <Button
            id="surla-open-change-card"
            disabled={loadingCard}
            onClick={onOpenChangeCard}
          >
            {loadingCard ? "Loading Change Card..." : "Load Change Card (CP1 / CP5)"}
          </Button>
        ) : null}
      </div>

      <details className="surla-details" id="surla-cp4-details">
        <summary>Candidate and run details</summary>
        <dl className="surla-verification-details">
          <dt>Candidate commit</dt><dd><code>{run.candidate_commit}</code></dd>
          <dt>Change Card revision</dt><dd><code>{run.request_revision}</code></dd>
          <dt>Evaluator commit</dt><dd><code>{run.suite_commit}</code></dd>
          <dt>Verification run</dt><dd><code>{run.run_id}</code></dd>
          <dt>Change Card</dt><dd><code>{run.change_id}</code></dd>
          <dt>Accepted</dt>
          <dd><time dateTime={run.accepted_at}>{run.accepted_at}</time></dd>
          {state.type === "running" ? (
            <>
              <dt>Started</dt>
              <dd><time dateTime={state.value.started_at}>{state.value.started_at}</time></dd>
            </>
          ) : state.type === "completed" ? (
            <>
              <dt>Evaluator suite</dt><dd><code>{state.value.suite_id}</code></dd>
              <dt>Environment</dt><dd>{state.value.environment}</dd>
              <dt>Report digest</dt><dd><code>{state.value.report.report_digest}</code></dd>
            </>
          ) : null}
        </dl>
      </details>
    </Card>
  );
}

// CompletedReport shows the entire declared denominator and every builder-safe
// outcome. It never derives a verdict from counts or missing diagnostics.
function CompletedReport({ report }: { report: BuilderVerificationReport }) {
  const counts = report.counts;
  return (
    <>
      <p>{counts.passed} of {counts.required} required scenarios passed.</p>
      {report.suite_failed ? (
        <Alert variant="destructive" role="status">
          The evaluation failed or ended unexpectedly.
          Individual scenario outcomes are shown below.
        </Alert>
      ) : null}
      <dl className="surla-verification-counts" aria-label="Scenario outcome counts">
        <div><dt>Required</dt><dd>{counts.required}</dd></div>
        <div><dt>Passed</dt><dd>{counts.passed}</dd></div>
        <div><dt>Failed</dt><dd>{counts.failed}</dd></div>
        <div><dt>Not run</dt><dd>{counts.not_run}</dd></div>
        <div><dt>Could not evaluate</dt><dd>{counts.harness_error}</dd></div>
      </dl>
      <h3>Declared scenarios</h3>
      <p>Unseen audit scenarios show only an anonymous outcome.</p>
      <ul className="surla-verification-scenarios">
        {report.scenarios.map((scenario) => (
          <li key={scenario.id}>
            {scenario.checks?.length || scenario.error ? (
              <details>
                <summary><ScenarioOutcome scenario={scenario} /></summary>
                {scenario.error ? <p>{scenario.error}</p> : null}
                {scenario.checks?.length ? (
                  <ul>
                    {scenario.checks.map((check, index) => (
                      <li key={index}>
                        <strong>{check.name}</strong>{" "}
                        <Badge variant={check.passed ? "default" : "destructive"}>
                          {check.passed ? "Passed" : "Failed"}
                        </Badge>
                        {check.diagnostic ? <p>{check.diagnostic}</p> : null}
                      </li>
                    ))}
                  </ul>
                ) : null}
              </details>
            ) : <ScenarioOutcome scenario={scenario} />}
          </li>
        ))}
      </ul>
      <h3>Not tested</h3>
      <p>
        Behavior outside the suite&apos;s declared scenarios. Adding an example
        to the Change Card does not add it to this report.
      </p>
    </>
  );
}

// ScenarioOutcome keeps the evaluator's scenario name and outcome together
// whether its check details were provided or deliberately withheld.
function ScenarioOutcome({ scenario }: { scenario: VerificationScenario }) {
  return (
    <>
      <span>{scenario.id}</span>{" "}
      <Badge variant={scenario.outcome === "passed" ? "default" : "destructive"}>
        {scenarioOutcomeLabels[scenario.outcome]}
      </Badge>
    </>
  );
}
