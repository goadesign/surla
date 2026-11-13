// build.tsx presents the saved build and explicit start controls beside a
// Change Card. The page owns requests, revision correlation and receipts.

import React, { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { ChangeBuild, ChangeCardResult, StartBuildResult, StartedBuild } from "../types";
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
  starting: boolean;
  disabled: boolean;
  dirty: boolean;
  startReceipt: BuildStartReceipt | null;
  findingTexts: readonly string[] | null;
  findingsLoading: boolean;
  findingsError: string | null;
  onRefresh: () => void;
  onReadFindings: () => void;
  onBuild: () => void;
  onOpenTeam: () => void;
}

// startExplanation belongs immediately before each control that sends a start.
const startExplanation = "Building starts a builder and, unless this project has stored verification settings, a test writer whose tests Surla runs on this machine outside the sandbox to check the builder’s work.";

// BuildSectionView shows the owner's saved facts without starting work on a
// refresh. Replacing a current build requires a separate explicit confirmation.
export function BuildSectionView({
  card,
  currentRevision,
  build,
  loading,
  error,
  starting,
  disabled,
  dirty,
  startReceipt,
  findingTexts,
  findingsLoading,
  findingsError,
  onRefresh,
  onReadFindings,
  onBuild,
  onOpenTeam,
}: BuildSectionProps) {
  const saved = build !== null && build.state === "started" ? build : null;
  const current = saved !== null && saved.in_force && saved.request_revision === card.request_revision;
  const displayedCurrent = currentRevision === card.request_revision;
  const confirmed = card.status === "confirmed";
  const eligible = confirmed && displayedCurrent && build !== null && error === null;
  const blocked = disabled || starting || loading || !eligible;
  const otherPrimary = !confirmed || !displayedCurrent || dirty || card.proposed_revision !== undefined;
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
      {build?.state === "not_started" ? <p>No build is ready for this change.</p> : null}
      {saved !== null ? (
        <>
          <p>{current ? "Built from the agreed revision." : "Built for an earlier revision."}</p>
          <p>Started <time dateTime={saved.started_at}>{saved.started_at}</time></p>
          <p>
            {saved.required_test_count === 0
              ? "No tests of this change’s Story were required."
              : saved.required_test_count + " required Story " + (saved.required_test_count === 1 ? "test." : "tests.")}
          </p>
          <p id="surla-build-builder">Builder: Assignment {saved.builder.read ? "read" : "unread"}.</p>
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
            variant={otherPrimary ? "default" : "primary"}
            disabled={blocked}
            onClick={start}
          >{directLabel}</Button>
        ) : null}
        <Button
          id="surla-build-view-team"
          variant={current && !otherPrimary ? "primary" : "default"}
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
                : "Building again replaces this build, makes both assignments unread, sets the test writer’s work to Assigned, clears its report and verdict, and requires its tests to be reported, judged and accepted again."}
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
  let label: string;
  switch (tests.state) {
    case "not_used": label = "Not used — stored verification settings"; break;
    case "assigned": label = "Assigned"; break;
    case "reported": label = "Reported"; break;
    case "needs_changes": label = "Needs changes"; break;
    case "accepted": label = "Accepted"; break;
    case "could_not_run": label = "Could not run"; break;
  }
  // Only the count is consumed from this older generated build projection.
  const findingCount = tests.state === "needs_changes" ? tests.findings.length : 0;
  const items = findingTexts === null ? null : (
    <ul className="surla-live-prompts">
      {findingTexts.map((text, index) => <li key={index}>{text}</li>)}
    </ul>
  );
  return (
    <div id="surla-build-test-writer" data-state={tests.state}>
      <p>
        Test writer: <Badge variant={tests.state === "needs_changes" ? "warning" : "default"}>{label}</Badge>
        {tests.state !== "not_used" ? (
          <> · Assignment {build.test_writer.read ? "read" : "unread"}</>
        ) : null}
      </p>
      {tests.state === "could_not_run" ? <p>{tests.reason} {tests.step}</p> : null}
      {tests.state === "needs_changes" ? (
        <div id="surla-build-findings" aria-busy={loading}>
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
