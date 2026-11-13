// Package components provides the Change Card decision and approval editor,
// combining the OpenUI Decision Workspace with the always-available canonical
// shadcn/ui inspector over a single shared draft context.

import React, { useEffect, useRef } from "react";
import { flushSync } from "react-dom";
import {
  areExamplesModified,
  CP1DraftContext,
  CP1DraftContextValue,
  isDraftDirty,
  PreservedDraft,
} from "../context/cp1-draft";
import {
  Renderer,
  surlaDecisionLibrary,
  validateCompletedOpenUI,
} from "../openui/library";
import { ChangeCardResult, ExampleItem } from "../types";
import { Alert, Badge, Button, Card } from "./shadcn";
import { countsPhrase, storyCounts } from "./story";

export interface CP1EditorProps {
  card: ChangeCardResult;
  // buildSection follows the saved summary before the long editable fields.
  buildSection: React.ReactNode;
  // buildRevisionNotice explains the effect of changing the agreed revision.
  buildRevisionNotice: string | null;
  // proposalWaiting is true while a proposed revision waits beside the
  // agreed card; refining the agreed card is then refused, so it is offered
  // only after the user adopts or declines the proposal.
  proposalWaiting: boolean;
  priorCard: ChangeCardResult | null;
  draftAnswers: Record<string, string>;
  draftExamples: ExampleItem[];
  rpcInFlight: boolean;
  loadingVerifications: boolean;
  cp4Open: boolean;
  preservedDraft: PreservedDraft | null;
  formError: string | null;
  onSetAnswer: (ambiguityId: string, option: string) => void;
  onSetExampleField: (
    exampleId: string,
    field: "scenario" | "input_summary" | "expected_outcome" | "verification_kind",
    value: string
  ) => void;
  onSubmitConfirm: () => void;
  onDiscardDraft: () => void;
  onLoadLatestCard: () => void;
  onLoadVerifications: () => void;
  // onOpenStory shows the Story stage for this change.
  onOpenStory: () => void;
}

interface FieldTextareaProps {
  exampleId: string;
  field: "scenario" | "input_summary" | "expected_outcome";
  label: string;
  value: string;
  disabled: boolean;
  onChangeValue: (next: string) => void;
}

// FieldTextarea renders a controlled multiline textarea that preserves exact
// whitespace and newlines while rejecting mutations when an RPC is in flight.
function FieldTextarea({
  exampleId,
  field,
  label,
  value,
  disabled,
  onChangeValue,
}: FieldTextareaProps) {
  const elRef = useRef<HTMLTextAreaElement | null>(null);
  const valueRef = useRef(value);
  const disabledRef = useRef(disabled);
  const onChangeRef = useRef(onChangeValue);
  valueRef.current = value;
  disabledRef.current = disabled;
  onChangeRef.current = onChangeValue;

  useEffect(() => {
    const node = elRef.current;
    if (!node) {
      return;
    }
    const handleInput = (evt: Event) => {
      const target = (evt.target as HTMLTextAreaElement) || node;
      if (disabledRef.current) {
        target.value = valueRef.current;
        return;
      }
      if (typeof target.value === "string" && target.value !== valueRef.current) {
        flushSync(() => {
          onChangeRef.current(target.value);
        });
      }
    };
    node.addEventListener("input", handleInput);
    return () => {
      node.removeEventListener("input", handleInput);
    };
  }, []);

  useEffect(() => {
    if (elRef.current && elRef.current.value !== value) {
      elRef.current.value = value;
    }
  }, [value]);

  return (
    <label className="surla-field-label">
      <span>{label + ": "}</span>
      <textarea
        ref={elRef}
        rows={2}
        className="surla-input shadcn-textarea"
        aria-label={exampleId + " " + label}
        data-example-id={exampleId}
        data-field={field}
        disabled={disabled}
        value={value}
        onChange={(e) => {
          if (disabled) {
            e.target.value = value;
            return;
          }
          onChangeValue(e.target.value);
        }}
      />
    </label>
  );
}

// CP1EditorView renders the active CP1 Change Card, its OpenUI Decision Studio
// workspace (when present), and the canonical shadcn/ui CP1 inspector.
export function CP1EditorView({
  card,
  buildSection,
  buildRevisionNotice,
  proposalWaiting,
  priorCard,
  draftAnswers,
  draftExamples,
  rpcInFlight,
  loadingVerifications,
  cp4Open,
  preservedDraft,
  formError,
  onSetAnswer,
  onSetExampleField,
  onSubmitConfirm,
  onDiscardDraft,
  onLoadLatestCard,
  onLoadVerifications,
  onOpenStory,
}: CP1EditorProps) {
  const ambiguities = Array.isArray(card.ambiguities) ? card.ambiguities : [];
  const stories = card.stories ?? [];
  const localExamplesEdited = areExamplesModified(card.examples, draftExamples);
  const draftDirty = isDraftDirty(card, draftAnswers, draftExamples);
  const artifactNames = card.approval
    ? Object.keys(card.approval.artifacts || {}).sort()
    : [];
  const unverifiedArtifactNames = card.unverified_approval
    ? Object.keys(card.unverified_approval.approval.artifacts || {}).sort()
    : [];

  const ctxValue: CP1DraftContextValue = {
    card,
    draftAnswers,
    draftExamples,
    rpcInFlight,
    localExamplesEdited,
    setAnswer: onSetAnswer,
  };

  let openUINode: React.ReactNode = null;
  if (card.ui_composition) {
    const check = validateCompletedOpenUI(card.ui_composition.program, card);
    if (check.ok) {
      openUINode = (
        <div
          id="surla-openui-workspace"
          data-composed-for-revision={card.ui_composition.composed_for_revision}
        >
          <Renderer
            library={surlaDecisionLibrary}
            response={card.ui_composition.program}
            isStreaming={false}
            publishObservability={false}
          />
        </div>
      );
    } else {
      openUINode = (
        <Alert id="surla-openui-error" variant="destructive" role="alert">
          <strong>Interactive decision preview unavailable: </strong>
          You can still answer every open question and edit or confirm every example directly in the
          form below.
          <details className="surla-details">
            <summary>Details</summary>
            <p>{check.error}</p>
          </details>
        </Alert>
      );
    }
  }

  return (
    <CP1DraftContext.Provider value={ctxValue}>
      <Card className="surla-meta">
        <div className="surla-region-header">
          <div className="surla-region-title-group">
            <h2>
              {card.status === "confirmed"
                ? "Confirmed Change Decision"
                : "Review & Confirm Change Intent"}
            </h2>
          </div>
          <div className="surla-toolbar-actions">
            <Badge variant={card.status === "confirmed" ? "success" : "warning"}>
              {card.status === "confirmed" ? "Confirmed" : "Needs confirmation"}
            </Badge>
            {card.approval ? <Badge variant="success">Approval recorded</Badge> : null}
            {card.unverified_approval ? <Badge variant="warning">Approval unverified</Badge> : null}
            {!cp4Open ? (
              <Button
                id="surla-load-verifications"
                disabled={loadingVerifications}
                onClick={onLoadVerifications}
              >
                {loadingVerifications ? "Loading saved verifications..." : "View saved verifications"}
              </Button>
            ) : null}
          </div>
        </div>
        {card.status === "confirmed" && ambiguities.length > 0 ? (
          <div id="surla-confirmed-decisions" role="region" aria-label="Confirmed Decisions">
            <p>
              <strong>Confirmed Decisions:</strong>
            </p>
            <ul>
              {ambiguities.map((amb) => (
                <li key={amb.id}>
                  <strong>{amb.question}</strong>{" "}
                  <code>{card.answers?.[amb.id] ?? ""}</code>
                </li>
              ))}
            </ul>
          </div>
        ) : null}
        <p>
          <strong>
            {card.status === "confirmed" && ambiguities.length > 0
              ? "Initial Proposal Summary: "
              : "Summary: "}
          </strong>
          {card.summary}
        </p>
        <p>
          <strong>Requested change: </strong>
          {card.prompt}
        </p>
        {card.confirmed_by ? (
          <p>
            <strong>Confirmed by: </strong>
            {card.confirmed_by}
          </p>
        ) : null}
        <details className="surla-details">
          <summary>Details</summary>
          <p>
            change_id: <code>{card.change_id}</code> · status: <code>{card.status}</code> ·
            request_revision: <code>{card.request_revision}</code>
          </p>
        </details>
      </Card>

      {buildSection}

      {card.approval ? (
        <Card
          id="surla-cp5-approval"
          className="surla-card"
          role="region"
          aria-label="Recorded Promotion Approval"
          tabIndex={-1}
        >
          <div className="surla-region-header">
            <div className="surla-field-row">
              <h3>Recorded Promotion Approval</h3>
              <Badge variant="success">Approval recorded</Badge>
              <Badge>No merge or deploy side effect</Badge>
            </div>
            <Button
              id="surla-cp5-refresh-approval"
              disabled={rpcInFlight}
              onClick={onLoadLatestCard}
            >
              Check current approval
            </Button>
          </div>
          <p>
            Approval recorded by bearer principal <code>{card.approval.approved_by}</code> for this
            confirmed <code>request_revision</code> on the saved Change Card. Recording approval
            does not merge branches or deploy binaries. This record describes the verified inputs
            below; check the current approval to see whether they still match the promotion target.
          </p>
          {draftDirty ? (
            <Alert id="surla-cp5-approval-dirty-warning" variant="warning" role="status">
              <strong>Unsaved draft edits detected: </strong>
              Saving refined answers or examples will mint a new <code>request_revision</code> and
              clear this approval.
            </Alert>
          ) : null}
          <p>
            <strong>Approved by: </strong>
            <code>{card.approval.approved_by}</code> · <strong>Environment: </strong>
            <code>{card.approval.environment}</code> · <strong>Evaluator suite: </strong>
            <code>{card.approval.suite_id}</code> · <strong>Scenarios passed: </strong>
            <code>{String(card.approval.scenario_count)}</code> ·{" "}
            <strong>Verified artifacts: </strong>
            <code>{String(artifactNames.length)}</code>
          </p>
          <details className="surla-details" id="surla-cp5-approval-details">
            <summary>Verified inputs and digests</summary>
            <p>
              <strong>Candidate commit: </strong>
              <code>{card.approval.candidate_commit}</code>
            </p>
            <p>
              <strong>Verification report digest: </strong>
              <code>{card.approval.report_digest}</code>
            </p>
            <p>
              <strong>Evaluator suite commit: </strong>
              <code>{card.approval.suite_id}</code> @ <code>{card.approval.suite_commit}</code>
            </p>
            <h4>{"Verified Candidate Artifacts (" + artifactNames.length + ")"}</h4>
            <ul>
              {artifactNames.map((name) => (
                <li key={name}>
                  <code>{name}</code>: <code>{card.approval!.artifacts[name]}</code>
                </li>
              ))}
            </ul>
          </details>
        </Card>
      ) : null}

      {card.unverified_approval ? (
        <Card
          id="surla-cp5-unverified-approval"
          className="surla-card"
          role="region"
          aria-label="Unverified Promotion Approval"
        >
          <div className="surla-region-header">
            <div className="surla-field-row">
              <h3>Saved Promotion Approval (Unverified)</h3>
              <Badge variant="warning">Inputs not verified on this read</Badge>
              <Badge>No merge or deploy side effect</Badge>
            </div>
            <Button
              id="surla-cp5-refresh-approval"
              disabled={rpcInFlight}
              onClick={onLoadLatestCard}
            >
              Check current approval
            </Button>
          </div>
          <Alert id="surla-cp5-unverified-approval-reason" variant="warning" role="status">
            <strong>Saved approval could not be verified: </strong>
            {card.unverified_approval.reason}
          </Alert>
          {draftDirty ? (
            <Alert id="surla-cp5-approval-dirty-warning" variant="warning" role="status">
              <strong>Unsaved draft edits detected: </strong>
              Saving refined answers or examples will mint a new <code>request_revision</code> and
              clear this saved approval.
            </Alert>
          ) : null}
          <p>
            <strong>Recorded approval by: </strong>
            <code>{card.unverified_approval.approval.approved_by}</code> ·{" "}
            <strong>Environment: </strong>
            <code>{card.unverified_approval.approval.environment}</code> ·{" "}
            <strong>Evaluator suite: </strong>
            <code>{card.unverified_approval.approval.suite_id}</code> ·{" "}
            <strong>Scenarios passed: </strong>
            <code>{String(card.unverified_approval.approval.scenario_count)}</code> ·{" "}
            <strong>Recorded artifacts: </strong>
            <code>{String(unverifiedArtifactNames.length)}</code>
          </p>
          <details className="surla-details" id="surla-cp5-unverified-approval-details">
            <summary>Recorded inputs and digests</summary>
            <p>
              <strong>Candidate commit: </strong>
              <code>{card.unverified_approval.approval.candidate_commit}</code>
            </p>
            <p>
              <strong>Verification report digest: </strong>
              <code>{card.unverified_approval.approval.report_digest}</code>
            </p>
            <p>
              <strong>Evaluator suite commit: </strong>
              <code>{card.unverified_approval.approval.suite_id}</code> @{" "}
              <code>{card.unverified_approval.approval.suite_commit}</code>
            </p>
            <h4>{"Recorded Candidate Artifacts (" + unverifiedArtifactNames.length + ")"}</h4>
            <ul>
              {unverifiedArtifactNames.map((name) => (
                <li key={name}>
                  <code>{name}</code>:{" "}
                  <code>{card.unverified_approval!.approval.artifacts[name]}</code>
                </li>
              ))}
            </ul>
          </details>
        </Card>
      ) : null}

      {priorCard ? (
        <Card
          id="surla-prior-revision"
          className="surla-card"
          role="region"
          aria-label="Previous Saved Revision"
        >
          <div className="surla-field-row">
            <h3>Previous Saved Revision</h3>
            <Badge>{priorCard.status === "confirmed" ? "Confirmed" : "Needs confirmation"}</Badge>
            <code>{priorCard.request_revision}</code>
          </div>
          <p>
            <strong>Previous request: </strong>
            {priorCard.prompt}
          </p>
          <p>
            <strong>Previous summary: </strong>
            {priorCard.summary}
          </p>
          <h4>
            {"Previous Questions & Decisions (" +
              (Array.isArray(priorCard.ambiguities) ? priorCard.ambiguities.length : 0) +
              ")"}
          </h4>
          {Array.isArray(priorCard.ambiguities) && priorCard.ambiguities.length > 0 ? (
            <ul>
              {priorCard.ambiguities.map((amb) => {
                const ans =
                  priorCard.answers &&
                  Object.prototype.hasOwnProperty.call(priorCard.answers, amb.id)
                    ? priorCard.answers[amb.id]
                    : "";
                return (
                  <li key={amb.id}>
                    <strong>{amb.question}</strong>
                    {ans
                      ? " — Confirmed answer: " + ans
                      : " — Unanswered (options: " + amb.options.join(", ") + ")"}
                  </li>
                );
              })}
            </ul>
          ) : (
            <p>No prior domain questions.</p>
          )}
          <h4>{"Previous Examples (" + priorCard.examples.length + ")"}</h4>
          <ul>
            {priorCard.examples.map((ex) => {
              const currentEx = card.examples.find((c) => c.id === ex.id);
              let statusText = "Removed";
              let badgeVariant: "default" | "accent" | "destructive" = "destructive";
              if (currentEx) {
                const unchanged =
                  currentEx.scenario === ex.scenario &&
                  currentEx.input_summary === ex.input_summary &&
                  currentEx.expected_outcome === ex.expected_outcome &&
                  currentEx.verification_kind === ex.verification_kind;
                if (unchanged) {
                  statusText = "Kept";
                  badgeVariant = "default";
                } else {
                  statusText = "Edited";
                  badgeVariant = "accent";
                }
              }
              return (
                <li key={ex.id}>
                  <code>{ex.id}</code> <Badge variant={badgeVariant}>{statusText}</Badge>{" "}
                  <strong>{ex.scenario}</strong>
                  {" — Input: " +
                    ex.input_summary +
                    " -> Expected: " +
                    ex.expected_outcome +
                    " [" +
                    ex.verification_kind +
                    "]"}
                </li>
              );
            })}
          </ul>
          <h4>
            {"Previous Preserved Invariants (" + priorCard.preserved_behaviors.length + ")"}
          </h4>
          {priorCard.preserved_behaviors.length > 0 ? (
            <ul>
              {priorCard.preserved_behaviors.map((pb, idx) => (
                <li key={String(idx)}>{pb}</li>
              ))}
            </ul>
          ) : (
            <p>None declared.</p>
          )}
        </Card>
      ) : null}

      {preservedDraft && Array.isArray(preservedDraft.lines) ? (
        <Card
          id="surla-preserved-draft"
          className="surla-card"
          role="region"
          aria-label="Preserved Unsaved Draft Edits"
        >
          <h3>Preserved Unsaved Draft Edits</h3>
          <p>
            Review the latest saved Change Card below before re-applying any of your unsaved edits:
          </p>
          <ul>
            {preservedDraft.lines.map((line, idx) => (
              <li key={String(idx)}>{line}</li>
            ))}
          </ul>
          <details className="surla-details">
            <summary>Details</summary>
            <p>
              Superseded revision: <code>{preservedDraft.fromRevision}</code>
            </p>
          </details>
        </Card>
      ) : null}

      {openUINode}

      <Card
        id="surla-cp1-form-card"
        className="surla-card"
        aria-busy={rpcInFlight ? "true" : undefined}
      >
        <h3>{"Open Questions (" + ambiguities.length + ")"}</h3>
        {ambiguities.length > 0 ? (
          <ul>
            {ambiguities.map((amb) => {
              const selected = Object.prototype.hasOwnProperty.call(draftAnswers, amb.id)
                ? draftAnswers[amb.id]
                : "";
              const saved =
                card.answers && Object.prototype.hasOwnProperty.call(card.answers, amb.id)
                  ? card.answers[amb.id]
                  : "";
              const answerBadge = !selected
                ? ""
                : card.status === "confirmed" && selected === saved
                ? " [Confirmed answer: " + selected + "]"
                : " [Selected draft answer: " + selected + "]";
              return (
                <li key={amb.id}>
                  <div>
                    <strong>{amb.question}</strong>
                    {answerBadge}
                  </div>
                  <div role="group" aria-label={amb.question}>
                    {amb.options.map((opt) => (
                      <Button
                        key={opt}
                        variant="pill"
                        disabled={rpcInFlight}
                        data-ambiguity-id={amb.id}
                        data-option={opt}
                        aria-pressed={selected === opt ? "true" : "false"}
                        onClick={() => onSetAnswer(amb.id, opt)}
                      >
                        {opt}
                      </Button>
                    ))}
                  </div>
                  <details className="surla-details">
                    <summary>Details</summary>
                    <p>
                      <code>{amb.id}</code>
                      {" (options: " + amb.options.join(", ") + ")"}
                    </p>
                  </details>
                </li>
              );
            })}
          </ul>
        ) : (
          <p>No open domain questions.</p>
        )}

        <h3>{"Concrete Examples (" + draftExamples.length + ")"}</h3>
        <ul className="surla-compact-list">
          {draftExamples.map((ex, idx) => (
            <li key={ex.id} className="surla-example-card" data-example-index={String(idx)}>
              <div>
                <strong>{ex.scenario}</strong>
                {" — Input: " + ex.input_summary + " -> Expected: " + ex.expected_outcome + " "}
                <Badge>
                  {ex.verification_kind === "deterministic"
                    ? "Deterministic check"
                    : "Model-judged check"}
                </Badge>
              </div>
              <div className="surla-field-row">
                <FieldTextarea
                  exampleId={ex.id}
                  field="scenario"
                  label="Scenario"
                  value={ex.scenario}
                  disabled={rpcInFlight}
                  onChangeValue={(v) => onSetExampleField(ex.id, "scenario", v)}
                />
                <FieldTextarea
                  exampleId={ex.id}
                  field="input_summary"
                  label="Input"
                  value={ex.input_summary}
                  disabled={rpcInFlight}
                  onChangeValue={(v) => onSetExampleField(ex.id, "input_summary", v)}
                />
                <FieldTextarea
                  exampleId={ex.id}
                  field="expected_outcome"
                  label="Expected result"
                  value={ex.expected_outcome}
                  disabled={rpcInFlight}
                  onChangeValue={(v) => onSetExampleField(ex.id, "expected_outcome", v)}
                />
                <Button
                  variant="pill"
                  disabled={rpcInFlight}
                  data-example-id={ex.id}
                  data-field="verification_kind"
                  onClick={() =>
                    onSetExampleField(
                      ex.id,
                      "verification_kind",
                      ex.verification_kind === "deterministic" ? "model_judged" : "deterministic"
                    )
                  }
                >
                  {"Kind: " + ex.verification_kind}
                </Button>
              </div>
              <details className="surla-details">
                <summary>Details</summary>
                <p>
                  <code>{ex.id}</code>
                  {" [" + ex.verification_kind + "]"}
                </p>
              </details>
            </li>
          ))}
        </ul>

        <h3>{"Preserved Contract Invariants (" + card.preserved_behaviors.length + ")"}</h3>
        {card.preserved_behaviors.length > 0 ? (
          <ul>
            {card.preserved_behaviors.map((pb, idx) => (
              <li key={String(idx)}>{pb}</li>
            ))}
          </ul>
        ) : (
          <p>None declared.</p>
        )}

        <div
          id="surla-confirm-story"
          className="surla-panel-section"
          role="region"
          aria-label="Story of this change"
        >
          <p>
            {stories.length === 0
              ? "This change has no Story."
              : "This change's Story has " + countsPhrase(storyCounts(stories)) + "."}{" "}
            Only complete flows are tested end to end; drafts do not block confirming.
          </p>
          <Button id="surla-confirm-open-story" onClick={onOpenStory}>
            Show the Story
          </Button>
        </div>

        {proposalWaiting ? (
          <p id="surla-confirm-blocked">
            A proposed revision waits above. Adopt or decline it before refining the agreed
            revision.
          </p>
        ) : null}
        {card.status === "confirmed" && draftDirty && !proposalWaiting && buildRevisionNotice !== null ? (
          <p id="surla-confirm-build-consequence">{buildRevisionNotice}</p>
        ) : null}
        <div className="surla-field-row">
          <Button
            id="surla-confirm-submit"
            variant={card.status === "needs_confirmation" || draftDirty ? "primary" : "default"}
            disabled={rpcInFlight || proposalWaiting}
            aria-describedby={proposalWaiting ? "surla-confirm-blocked" :
              card.status === "confirmed" && draftDirty && buildRevisionNotice !== null
                ? "surla-confirm-build-consequence" : undefined}
            onClick={onSubmitConfirm}
          >
            {card.status === "needs_confirmation"
              ? "Confirm Intent & Lock Examples"
              : "Refine Confirmed Answers & Examples"}
          </Button>
          <Button
            id="surla-discard-draft"
            disabled={rpcInFlight}
            onClick={onDiscardDraft}
          >
            Discard Unsaved Edits
          </Button>
        </div>

        <div id="surla-confirm-feedback">
          {formError ? (
            <Alert id="surla-confirm-error" variant="destructive" role="alert" tabIndex={-1}>
              <strong>Change Card request failed: </strong>
              <p>{formError}</p>
              Your edited answers and examples remain in this form. Click{" "}
              <strong>Load Latest Card</strong> to inspect the current saved Change Card alongside
              your unsaved edits.
              <div className="surla-field-row">
                <Button
                  id="surla-load-latest-card"
                  disabled={rpcInFlight}
                  onClick={onLoadLatestCard}
                >
                  Load Latest Card
                </Button>
              </div>
            </Alert>
          ) : null}
        </div>
      </Card>
    </CP1DraftContext.Provider>
  );
}
