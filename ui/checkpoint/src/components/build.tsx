// build.tsx presents the saved build and explicit start controls beside a
// Change Card. The page owns requests, revision correlation and receipts.

import React, { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { ChangeBuild, ChangeCardResult, StartBuildResult, StartedBuild, VerificationStatusResult } from "../types";
import { Alert, Badge, Button, Card } from "./shadcn";

// BuildStartReceipt records one start's answers, rather than current health.
// A later Start agents action removes team while retaining the copy receipt.
export interface BuildStartReceipt {
  at: string;
  team: StartBuildResult["team"] | null;
  workingCopy: StartBuildResult["working_copy"];
}

// BuildSectionProps keeps transport, saved-card and findings ownership in the
// page. Null reads are unread; only the generated not_started case means absent.
export interface BuildSectionProps {
  card: ChangeCardResult;
  currentRevision: string | null;
  build: ChangeBuild["build"] | null;
  loading: boolean;
  error: string | null;
  // actionError retains a failed start separately from saved-build read errors.
  actionError: string | null;
  starting: boolean;
  disabled: boolean;
  dirty: boolean;
  startReceipt: BuildStartReceipt | null;
  findingTexts: readonly string[] | null;
  findingsLoading: boolean;
  findingsError: string | null;
  // check is the owner's exact latest saved run for this change.
  check: VerificationStatusResult | null;
  checkKnown: boolean;
  checkError: string | null;
  approvalAvailable: boolean;
  onRefresh: () => void;
  onReadFindings: () => void;
  onBuild: () => void;
  onOpenTeam: () => void;
  onReviewCheck: () => void;
}

// BuildSectionView shows the owner's saved facts without starting work on a
// refresh. Replacing a current build requires a separate explicit confirmation.
export function BuildSectionView({
  card,
  currentRevision,
  build,
  loading,
  error,
  actionError,
  starting,
  disabled,
  dirty,
  startReceipt,
  findingTexts,
  findingsLoading,
  findingsError,
  check,
  checkKnown,
  checkError,
  approvalAvailable,
  onRefresh,
  onReadFindings,
  onBuild,
  onOpenTeam,
  onReviewCheck,
}: BuildSectionProps) {
  const saved = build !== null && build.state === "started" ? build : null;
  const current = saved !== null && saved.in_force;
  const displayedCurrent = currentRevision === card.request_revision &&
    (!current || saved.request_revision === card.request_revision);
  const confirmed = card.status === "confirmed";
  const eligible = confirmed && displayedCurrent && build !== null && error === null;
  const blocked = disabled || starting || loading || !eligible;
  const otherPrimary = !confirmed || !displayedCurrent || dirty || card.proposed_revision !== undefined;
  const report = check?.state.type === "completed" ? check.state.value.report : null;
  const matchedCheck = checkKnown && check?.inputs_current === true &&
    check.change_id === card.change_id && check.request_revision === card.request_revision;
  const recordedCheck = report !== null && card.approval?.report_digest === report.report_digest;
  const sentBack = current && saved.test_writer.tests.state === "needs_changes";
  const reviewPrimary = approvalAvailable && !otherPrimary && !sentBack && matchedCheck &&
    report?.passed === true && !recordedCheck;
  // identity binds the question to the exact subject and start it described.
  const identity = JSON.stringify([card.change_id, card.request_revision, saved?.started_at ?? null]);
  const [questionIdentity, setQuestionIdentity] = useState<string | null>(null);
  const confirming = questionIdentity === identity && current && eligible && !disabled && !starting;
  const headingRef = useRef<HTMLHeadingElement>(null);
  const triggerRef = useRef<HTMLSpanElement>(null);
  const questionRef = useRef<HTMLDivElement | null>(null);
  const removedFocus = useRef(false);
  const returnFocus = useRef(false);
  // sent prevents a second click before the page's pending state is rendered.
  const sent = useRef(false);

  // attachQuestion observes focus before removal, following the Live control
  // pattern. Refreshes never pull focus away from an editor outside this row.
  const attachQuestion = useCallback((node: HTMLDivElement | null) => {
    questionRef.current = node;
    if (node === null) {
      return;
    }
    return () => {
      removedFocus.current = node.contains(document.activeElement);
      questionRef.current = null;
    };
  }, []);

  useLayoutEffect(() => {
    if (confirming) {
      removedFocus.current = false;
      questionRef.current?.focus();
    } else if (returnFocus.current) {
      returnFocus.current = false;
      removedFocus.current = false;
      triggerRef.current?.querySelector("button")?.focus();
    } else if (removedFocus.current) {
      removedFocus.current = false;
      headingRef.current?.focus();
    }
  }, [confirming, identity]);

  // Once invalidated, the old question cannot return when a session/revision
  // becomes eligible again. An outstanding request owns the submission latch.
  useEffect(() => {
    if (!confirming && questionIdentity !== null) {
      setQuestionIdentity(null);
    }
  }, [confirming, questionIdentity]);
  useEffect(() => {
    if (!starting) {
      sent.current = false;
    }
  }, [starting, identity]);

  // cancel sends nothing and restores the separate question trigger.
  function cancel() {
    returnFocus.current = true;
    setQuestionIdentity(null);
  }

  // start is called only by the direct first/earlier action or final confirm.
  function start() {
    if (blocked || sent.current || (current && !confirming)) {
      return;
    }
    sent.current = true;
    setQuestionIdentity(null);
    onBuild();
  }

  if (!confirmed && saved === null) {
    return null;
  }

  const directLabel = card.proposed_revision !== undefined
    ? "Build agreed revision"
    : saved !== null ? "Build current agreed revision" : "Build this change";
  const showDirect = !current && confirmed && displayedCurrent && build !== null;
  const startExplanation = saved?.test_writer.tests.state === "not_used"
    ? "Build starts the builder. This project uses its configured tests. Surla runs those tests on this machine with your permissions: read them before you approve."
    : saved !== null
      ? "Build starts the builder and the test writer. Surla uses the test writer’s accepted tests to check the builder’s work and to start live targets. The tests run on this machine with your permissions: read them before you approve."
      : "Build starts the builder. If this project needs a test writer, it starts that writer too. Surla uses that writer’s accepted tests to check the builder’s work and to start live targets. The tests run on this machine with your permissions: read them before you approve.";

  return (
    <Card
      id="surla-build-section"
      className="surla-card surla-live-wrap"
      role="region"
      aria-labelledby="surla-build-title"
      aria-busy={loading || starting}
      data-state={build?.state ?? "unread"}
    >
      <div className="surla-region-header">
        <h3 id="surla-build-title" tabIndex={-1} ref={headingRef}>Build</h3>
        {saved !== null ? (
          <Badge variant={current ? "accent" : "default"}>
            {current ? "Agreed revision" : "Earlier revision"}
          </Badge>
        ) : null}
      </div>
      {error !== null ? (
        <Alert id="surla-build-error" variant="destructive" role="alert">
          <strong>Could not read the build. </strong>{error}
          <div className="surla-field-row">
            <Button
              id="surla-build-refresh"
              disabled={disabled || starting || loading}
              onClick={onRefresh}
            >Retry build read</Button>
          </div>
        </Alert>
      ) : null}
      {loading ? (
        <p id="surla-build-loading" role="status">
          {build === null ? "Reading this change’s build…" : "Updating build…"}
        </p>
      ) : null}
      {build === null && !loading && error === null ? (
        <p>This change’s build has not been read.</p>
      ) : null}
      {build !== null && error !== null ? <p>Last-read build facts; they could not be refreshed.</p> : null}
      {actionError !== null ? (
        <Alert id="surla-build-action-error" variant="destructive" role="alert">{actionError}</Alert>
      ) : null}
      {check !== null && !sentBack ? (
        <p id="surla-build-state" data-inputs-current={checkKnown ? String(check.inputs_current) : "unknown"}>
          {!checkKnown
            ? "A saved check is available. Its current agreement and tests have not been confirmed."
            : !matchedCheck
            ? "The saved check used an earlier agreement or test selection. The builder checks the current agreement and tests again; then you can approve."
            : check.state.type === "running"
            ? "Surla is checking the builder’s code. Your next decision is approval after the check passes."
            : report?.passed
            ? recordedCheck
              ? "The check passed and your approval is recorded."
              : `The check passed: ${report.counts.passed} of ${report.counts.required} scenarios. ${approvalAvailable ? "Review the result and approve it." : "Review the result; approval is unavailable on this server."}`
            : check.state.type === "not_completed"
            ? "The check could not finish. The builder reads the result and addresses its next step."
            : "The check did not pass. The builder reviews the result before asking for another check."}
          {" Check started "}<time dateTime={check.accepted_at}>{localBuildTime(check.accepted_at)}</time>.
        </p>
      ) : build?.state === "not_started" ? <p id="surla-build-state">Not built yet.</p> : null}
      {checkError !== null ? <p id="surla-build-check-error">Could not refresh the saved check. {checkError}</p> : null}
      {saved !== null ? (
        <>
          {check === null || sentBack ? <BuildProgress build={saved} /> : null}
          <p>
            {saved.required_test_count === 0
              ? "No tests of this change’s Story were required."
              : saved.required_test_count + " required Story " + (saved.required_test_count === 1 ? "test." : "tests.")}
          </p>
          <p id="surla-build-builder">
            Builder: {saved.builder.read
              ? "Assignment read."
              : saved.test_writer.tests.state === "accepted"
                ? "Assignment unread: accepted tests are ready for it to read."
                : "Assignment unread: it has not read this build’s instructions yet."}
          </p>
          <TestWriterFacts
            build={saved}
            findingTexts={findingTexts}
            loading={findingsLoading}
            error={findingsError}
            disabled={disabled || starting}
            onRead={onReadFindings}
          />
        </>
      ) : null}
      {!confirmed ? <p>Confirm the change before building it.</p> : !displayedCurrent ? (
        <p>A newer agreed revision is available. Review it before starting a build.</p>
      ) : dirty ? (
        <p>The build uses the agreed revision; refine the change first to include your edits.</p>
      ) : null}
      {starting ? (
        <p id="surla-build-starting" role="status">
          Starting build… This can take a few minutes and continues if you close the page.
        </p>
      ) : null}
      {showDirect ? <p id="surla-build-start-explanation">{startExplanation}</p> : null}
      <div className="surla-field-row">
        {showDirect ? (
          <Button
            id="surla-build-start"
            variant={otherPrimary || sentBack || reviewPrimary ? "default" : "primary"}
            disabled={blocked}
            onClick={start}
          >{directLabel}</Button>
        ) : null}
        {check !== null ? (
          <Button
            id="surla-build-review-check"
            variant={reviewPrimary ? "primary" : "default"}
            disabled={disabled || starting}
            onClick={onReviewCheck}
          >{reviewPrimary ? "Review and approve" : "Review saved check"}</Button>
        ) : null}
        <Button
          id="surla-build-view-team"
          disabled={disabled || starting}
          onClick={onOpenTeam}
        >View team</Button>
        {current && confirmed && displayedCurrent ? (
          <span ref={triggerRef}>
            <Button
              id="surla-build-again"
              disabled={blocked || confirming}
              onClick={() => setQuestionIdentity(identity)}
            >Build again…</Button>
          </span>
        ) : null}
      </div>
      {confirming ? (
        <div
          id="surla-build-question"
          ref={attachQuestion}
          role="group"
          aria-labelledby="surla-build-question-title"
          tabIndex={-1}
          onKeyDown={(event) => {
            if (event.key === "Escape") {
              event.preventDefault();
              cancel();
            }
          }}
        >
          <Alert variant="warning">
            <p id="surla-build-question-title"><strong>Build the agreed revision again?</strong></p>
            <p>
              {saved?.test_writer.tests.state === "not_used"
                ? "Building again replaces this build and makes the builder’s assignment unread again."
                : "Building again replaces this build, makes both assignments unread, sets the test writer’s work to Assigned, clears its report and verdict, and requires its tests to be reported, judged and accepted again before verification or a new live target."}
            </p>
            <p id="surla-build-confirm-explanation">{startExplanation}</p>
            <div className="surla-field-row">
              <Button id="surla-build-confirm" disabled={blocked} onClick={start}>Build again</Button>
              <Button id="surla-build-cancel" disabled={disabled || starting} onClick={cancel}>Cancel</Button>
            </div>
          </Alert>
        </div>
      ) : null}
      <BuildDetails card={card} build={saved} />
      {startReceipt !== null ? <StartReceipts receipt={startReceipt} /> : null}
    </Card>
  );
}

// BuildProgress describes the saved step without treating a read as proof of
// work, or an earlier revision's tests as progress on the current agreement.
function BuildProgress({ build }: { build: StartedBuild }) {
  const tests = build.test_writer.tests;
  if (!build.in_force) {
    return <p id="surla-build-state">Earlier build, started <time dateTime={build.started_at}>{localBuildTime(build.started_at)}</time>. The agreed revision has changed.</p>;
  }
  switch (tests.state) {
    case "needs_changes":
      return <p id="surla-build-state">Sent back to the test writer. It fixes this and reports again; nothing is yours to do unless it asks you in its pane.</p>;
    case "accepted":
      return <p id="surla-build-state">Tests accepted. The builder finishes the code and asks Surla to check it. Your next decision: approve, after the check passes.</p>;
    case "reported":
      return <p id="surla-build-state">The test writer reported its tests. Surla checks them before the builder can use them.</p>;
    case "could_not_run":
      return <p id="surla-build-state">Surla could not check the test writer’s tests. Read the reason and next step below.</p>;
    case "assigned":
    case "not_used":
      return <p id="surla-build-state">Building since <time dateTime={build.started_at}>{localBuildTime(build.started_at)}</time>. {tests.state === "not_used" ? "The builder uses this project’s configured tests." : "Next: the test writer prepares tests for the builder’s code."} Your next decision: approve, after the builder’s check passes.</p>;
  }
}

// localBuildTime shows local minutes, adding the date for starts before today.
// Exact timestamps remain available in Build details.
function localBuildTime(at: string) {
  const date = new Date(at);
  return date.toLocaleString(undefined, {
    ...(date.toDateString() === new Date().toDateString() ? {} : {
      year: "numeric", month: "short", day: "numeric",
    }),
    hour: "numeric",
    minute: "2-digit",
  });
}

// TestWriterFacts keeps assignment reads separate from declared test progress.
// Findings text comes only from the page's independently correlated user read.
function TestWriterFacts({ build, findingTexts, loading, error, disabled, onRead }: {
  build: StartedBuild;
  findingTexts: readonly string[] | null;
  loading: boolean;
  error: string | null;
  disabled: boolean;
  onRead: () => void;
}) {
  const tests = build.test_writer.tests;
  let progress: string;
  switch (tests.state) {
    case "not_used": progress = "Not needed; this project uses its configured tests."; break;
    case "assigned": progress = build.test_writer.read
      ? "Assignment read. It has not reported tests yet."
      : "Assignment unread: it has not read this build’s instructions yet."; break;
    case "reported": progress = build.test_writer.read
      ? "Assignment read. Surla is checking its reported tests."
      : "Assignment unread: its reported tests are being checked."; break;
    case "needs_changes": progress = build.test_writer.read
      ? "Assignment read. It has the findings to address."
      : "Assignment unread: new findings are ready for it to read."; break;
    case "accepted": progress = build.test_writer.read
      ? "Assignment read. It has read the acceptance of its tests."
      : "Assignment unread: the acceptance of its tests is ready for it to read."; break;
    case "could_not_run": progress = build.test_writer.read
      ? "Assignment read. It has the reason the check could not run."
      : "Assignment unread: the reason the check could not run is ready for it to read."; break;
  }
  const findingCount = tests.state === "needs_changes" ? tests.finding_count : 0;
  const items = findingTexts === null ? null : (
    <ul className="surla-live-prompts">
      {findingTexts.map((text, index) => <li key={index}>{text}</li>)}
    </ul>
  );
  return (
    <div id="surla-build-test-writer" data-state={tests.state}>
      <p>Test writer: {progress}</p>
      {tests.state === "could_not_run" ? <p>{tests.reason} {tests.step}</p> : null}
      {tests.state === "accepted" ? <p id="surla-build-not-judged">{tests.not_judged}</p> : null}
      {tests.state === "needs_changes" ? (
        <div id="surla-build-findings" aria-busy={loading}>
          <p><strong>What Surla told the test writer</strong></p>
          <p>{findingCount} {findingCount === 1 ? "finding" : "findings"}</p>
          {findingCount > 0 ? (
            <>
              {loading ? <p role="status">Reading findings…</p> : null}
              {error !== null ? (
                <Alert id="surla-build-findings-error" variant="destructive">
                  Could not read findings. {error}
                </Alert>
              ) : null}
              {!loading && (error !== null || findingTexts === null) ? (
                <Button id="surla-build-findings-retry" disabled={disabled} onClick={onRead}>
                  {error !== null ? "Retry findings read" : "Read findings"}
                </Button>
              ) : null}
              {!loading && error === null && findingTexts !== null ? (
                findingTexts.length > 3 ? (
                  <details className="surla-details">
                    <summary>Read {findingTexts.length} findings</summary>
                    {items}
                  </details>
                ) : items
              ) : null}
            </>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

// BuildDetails places exact revision, report and verdict evidence behind the
// existing native disclosure, without relabeling acceptance as verification.
function BuildDetails({ card, build }: { card: ChangeCardResult; build: StartedBuild | null }) {
  const tests = build?.test_writer.tests;
  return (
    <details id="surla-build-details" className="surla-details">
      <summary>Build details</summary>
      <p>Agreed revision: <code>{card.request_revision}</code></p>
      {build !== null ? (
        <>
          <p>Built revision: <code>{build.request_revision}</code></p>
          <p>Build started: <time dateTime={build.started_at}>{build.started_at}</time></p>
          {tests?.state !== "not_used" ? <p>Test report number: {build.test_writer.report}</p> : null}
        </>
      ) : null}
      {tests !== undefined && "checked_at" in tests ? (
        <p>Verdict reached: <time dateTime={tests.checked_at}>{tests.checked_at}</time></p>
      ) : null}
      {tests?.state === "accepted" ? <p>Accepted tests commit: <code>{tests.commit}</code></p> : null}
    </details>
  );
}

// StartReceipts describes historical launcher/copy outcomes in the past tense.
// Owner instructions remain quotations returned then, never current advice.
function StartReceipts({ receipt }: { receipt: BuildStartReceipt }) {
  const team = receipt.team;
  const copy = receipt.workingCopy;
  let teamText = "";
  if (team !== null) {
    switch (team.state) {
      case "ready": teamText = "the team was ready"; break;
      case "window_busy": teamText = "the build was saved and the team window was busy"; break;
      case "ambiguous_space": teamText = team.count + " matching team spaces"; break;
      case "unavailable": teamText = "the build was saved and team startup was unavailable"; break;
    }
  }
  let copyText: string;
  switch (copy.state) {
    case "not_used": copyText = "no working copy was used, because the project had stored verification settings"; break;
    case "at_project_commit": copyText = "the working copy was at the project commit"; break;
    case "dirty": copyText = "the working copy was kept with uncommitted work"; break;
    case "not_contained": copyText = "the working copy was kept at its tests commit"; break;
  }
  return (
    <div id="surla-build-start-receipts">
      {team !== null ? (
        <p id="surla-build-start-team-receipt">
          This start (begun <time dateTime={receipt.at}>{receipt.at}</time>) found {teamText}.
          {" "}The instruction returned then was: «{team.step}»
        </p>
      ) : null}
      <p id="surla-build-start-copy-receipt">
        This start (begun <time dateTime={receipt.at}>{receipt.at}</time>) found {copyText}.
        {copy.state === "not_contained" ? <> The instruction returned then was: «{copy.step}»</> : null}
      </p>
      {copy.state !== "not_used" ? (
        <details className="surla-details">
          <summary>Working copy at this start</summary>
          <p>Retained commit: <code>{copy.commit}</code></p>
        </details>
      ) : null}
    </div>
  );
}
