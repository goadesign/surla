// Package main implements the Surla Checkpoint Studio MCP Apps guest bridge,
// coordinating the two-region layout (#surla-cp1-editor and #surla-result-view),
// JSON-RPC 2.0 host lifecycle notifications, theme synchronization, and
// synchronous React 19 rendering.

import React from "react";
import { flushSync } from "react-dom";
import { createRoot, Root } from "react-dom/client";
import {
  ArchitecturePreviewView,
  DiffResultView,
  DraftOverrideView,
  IncomingChangeCardView,
  LiveTargetView,
  ToolInputPartialView,
} from "./components/c4-graph";
import { CP1EditorView } from "./components/cp1-editor";
import { VerificationView } from "./components/cp4-verification";
import { Alert, Card } from "./components/shadcn";
import { isDraftDirty, PreservedDraft } from "./context/cp1-draft";
import {
  ArchitecturePreviewResult,
  ChangeCardResult,
  DiffResult,
  DraftOverrideResult,
  ExampleItem,
  LiveTargetResult,
  StopLiveTargetResult,
  VerificationStatusResult,
  isArchitecturePreviewResult,
  isChangeCardResult,
  isDiffResult,
  isDraftOverrideResult,
  isLiveTargetResult,
  isPlainObject,
  isStopLiveTargetResult,
  isVerificationStatusResult,
} from "./types";

// Initialization has its own request ID; editor calls start after it.
const INIT_REQUEST_ID = 1;
// PROTOCOL_VERSION is the MCP Apps version advertised to the parent host.
const PROTOCOL_VERSION = "2026-01-26";

// PendingRPC consumes the host response associated with one request ID.
interface PendingRPC {
  // params is the exact call sent to the host, retained only until its reply.
  params: { name: string; arguments: Record<string, unknown> };
  // onResult validates the tool payload before an editor callback uses it.
  onResult: (payload: unknown) => void;
  // onError reports the failed request and completes its local lifetime.
  onError: (message: string) => void;
}

// ActiveEditorState keeps received saved values separate from editable drafts.
interface ActiveEditorState {
  // card is the last saved snapshot received from the server.
  card: ChangeCardResult;
  // priorCard is the immediately prior saved snapshot for this change_id
  // when the editor transitioned between distinct revisions of the same card.
  priorCard: ChangeCardResult | null;
  // draftAnswers contains the user's current choices, including unsaved ones.
  draftAnswers: Record<string, string>;
  // draftExamples contains the exact text the user is editing.
  draftExamples: ExampleItem[];
  // preservedDraft retains unsaved draft edits from an earlier revision.
  preservedDraft: PreservedDraft | null;
  // formError describes the latest failed editor request or invalid input.
  formError: string | null;
}

// ActiveResultState is the checkpoint displayed alongside a retained editor.
type ActiveResultState =
  | { kind: "cp2-architecture-preview"; preview: ArchitecturePreviewResult }
  | { kind: "cp2-diff"; diff: DiffResult }
  | { kind: "cp3-draft-override"; draft: DraftOverrideResult }
  | { kind: "cp3-live-target"; live: LiveTargetResult | StopLiveTargetResult }
  | { kind: "cp4-verification"; run: VerificationStatusResult; requestError: string | null }
  | { kind: "cp1-incoming-card"; card: ChangeCardResult }
  | { kind: "partial-input"; partialArgs: Record<string, unknown> }
  | { kind: "error"; message: string }
  | { kind: "cancelled"; reason: string };

// formatChangedDraftLines formats only the ambiguity answers and acceptance
// examples that differ from the saved Change Card, including cleared values,
// so unchanged saved content is not relabeled as unsaved draft edits.
function formatChangedDraftLines(ed: ActiveEditorState): string[] {
  const rev = ed.card.request_revision;
  const ambiguities = Array.isArray(ed.card.ambiguities) ? ed.card.ambiguities : [];
  const savedAnswers = isPlainObject(ed.card.answers) ? ed.card.answers : {};
  const draftAnswers = ed.draftAnswers || {};
  const questionByID: Record<string, string> = {};
  const answerIDs: string[] = [];
  const seenIDs: Record<string, boolean> = {};
  for (let i = 0; i < ambiguities.length; i++) {
    const amb = ambiguities[i];
    if (amb && typeof amb.id === "string") {
      if (typeof amb.question === "string") {
        questionByID[amb.id] = amb.question;
      }
      if (!seenIDs[amb.id]) {
        seenIDs[amb.id] = true;
        answerIDs.push(amb.id);
      }
    }
  }
  for (const k of Object.keys(savedAnswers).concat(Object.keys(draftAnswers))) {
    if (!seenIDs[k]) {
      seenIDs[k] = true;
      answerIDs.push(k);
    }
  }

  const lines: string[] = [];
  for (let i = 0; i < answerIDs.length; i++) {
    const k = answerIDs[i];
    const savedVal = savedAnswers[k] || "";
    const draftVal = draftAnswers[k] || "";
    if (draftVal !== savedVal) {
      const q = questionByID[k];
      lines.push("[" + rev + "] " + k + (q ? " (" + q + ")" : "") + "=" + draftVal);
    }
  }

  const savedExamples = Array.isArray(ed.card.examples) ? ed.card.examples : [];
  const draftExamples = Array.isArray(ed.draftExamples) ? ed.draftExamples : [];
  for (let i = 0; i < draftExamples.length; i++) {
    const ex = draftExamples[i];
    const savedEx = savedExamples.find((s) => s.id === ex.id);
    if (
      !savedEx ||
      savedEx.scenario !== ex.scenario ||
      savedEx.input_summary !== ex.input_summary ||
      savedEx.expected_outcome !== ex.expected_outcome ||
      savedEx.verification_kind !== ex.verification_kind
    ) {
      lines.push(
        "[" +
          rev +
          "] " +
          ex.id +
          ": " +
          ex.scenario +
          " | " +
          ex.input_summary +
          " -> " +
          ex.expected_outcome +
          " [" +
          ex.verification_kind +
          "]"
      );
    }
  }
  return lines;
}

// snapshotUnsavedDraft preserves unsaved local edits and any earlier unsaved
// draft lines before a different saved revision replaces the editable form.
function snapshotUnsavedDraft(ed: ActiveEditorState): PreservedDraft | null {
  const priorLines = ed.preservedDraft ? ed.preservedDraft.lines : [];
  const currentLines = formatChangedDraftLines(ed);
  if (currentLines.length === 0 && priorLines.length === 0) {
    return null;
  }
  return {
    fromRevision:
      currentLines.length > 0
        ? ed.card.request_revision
        : ed.preservedDraft!.fromRevision,
    lines: priorLines.concat(currentLines),
  };
}

// nextPriorCard returns the immediately prior saved card when transitioning
// between distinct revisions of the same change_id, preserves the existing
// priorCard when refreshing the same revision, and returns null when
// mounting an initial card or switching to a different change_id.
function nextPriorCard(
  current: ActiveEditorState | null,
  next: ChangeCardResult
): ChangeCardResult | null {
  if (!current || current.card.change_id !== next.change_id) {
    return null;
  }
  if (current.card.request_revision === next.request_revision) {
    return current.priorCard;
  }
  return current.card;
}

// extractErrorText uses the host's text error when provided and otherwise
// explains that the tool failed without inventing a cause.
function extractErrorText(params: Record<string, unknown>): string {
  if (Array.isArray(params.content)) {
    for (let i = 0; i < params.content.length; i++) {
      const item = params.content[i];
      if (
        isPlainObject(item) &&
        item.type === "text" &&
        typeof item.text === "string" &&
        item.text.length > 0
      ) {
        return item.text;
      }
    }
  }
  return "Tool execution failed.";
}

// applyHostTheme accepts supported themes from initialization or host updates.
function applyHostTheme(candidate: unknown) {
  if (!isPlainObject(candidate)) {
    return;
  }
  const direct = candidate.theme;
  const nested = isPlainObject(candidate.hostContext) ? candidate.hostContext.theme : undefined;
  const theme = direct === "dark" || direct === "light" ? direct : nested;
  if (theme === "dark" || theme === "light") {
    if (document.documentElement && document.documentElement.dataset) {
      document.documentElement.dataset.theme = theme;
    }
  }
}

// bootstrapCheckpointApp mounts the two-region Checkpoint Studio inside the
// MCP Apps host document and registers the window postMessage bridge.
export function bootstrapCheckpointApp() {
  const rawRootEl = document.getElementById("surla-checkpoint-root");
  const rawStatusEl = document.getElementById("surla-checkpoint-status");
  const viewEl = document.getElementById("surla-checkpoint-view");
  if (!rawRootEl || !rawStatusEl || !viewEl) {
    return;
  }
  const rootEl: HTMLElement = rawRootEl;
  const statusEl: HTMLElement = rawStatusEl;

  const editorRegionEl = document.createElement("div");
  editorRegionEl.setAttribute("id", "surla-cp1-editor");
  const resultRegionEl = document.createElement("div");
  resultRegionEl.setAttribute("id", "surla-result-view");
  while (viewEl.firstChild) {
    viewEl.removeChild(viewEl.firstChild);
  }
  viewEl.appendChild(editorRegionEl);
  viewEl.appendChild(resultRegionEl);

  const editorRoot: Root = createRoot(editorRegionEl);
  const resultRoot: Root = createRoot(resultRegionEl);

  let initPending = false;
  let nextRequestId = 2;
  const pendingRequests: Record<string, PendingRPC> = {};
  // editorRequest completes after the current card request updates the view.
  // CP3 waits for it; independent verification reads do not block editing.
  let editorRequest: Promise<void> | null = null;
  let activeEditor: ActiveEditorState | null = null;
  let activeResult: ActiveResultState | null = null;

  // hasUnsavedEditorState identifies work that a different checkpoint must
  // leave available for the user to recover or continue editing.
  function hasUnsavedEditorState(): boolean {
    if (!activeEditor) {
      return false;
    }
    return (
      isDraftDirty(
        activeEditor.card,
        activeEditor.draftAnswers,
        activeEditor.draftExamples
      ) ||
      activeEditor.preservedDraft !== null ||
      activeEditor.formError !== null
    );
  }

  // shouldRetainEditor keeps drafts and the destination of a pending editor
  // response visible when a separate checkpoint notification arrives.
  function shouldRetainEditor(): boolean {
    return (
      activeEditor !== null &&
      (editorRequest !== null || hasUnsavedEditorState())
    );
  }

  // clearEditorDataset removes DOM metadata when no card is displayed.
  function clearEditorDataset() {
    delete editorRegionEl.dataset.state;
    delete editorRegionEl.dataset.changeId;
    delete editorRegionEl.dataset.requestRevision;
  }

  // clearResultDataset prevents a new result from inheriting old DOM metadata.
  function clearResultDataset() {
    delete resultRegionEl.dataset.state;
    delete resultRegionEl.dataset.changeId;
    delete resultRegionEl.dataset.requestRevision;
    delete resultRegionEl.dataset.designDigest;
    delete resultRegionEl.dataset.hasChanges;
    delete resultRegionEl.dataset.runId;
  }

  // syncEditorRegion renders saved values and drafts together, with editing
  // disabled while the host owns an outstanding editor request.
  function syncEditorRegion() {
    if (!activeEditor) {
      clearEditorDataset();
      flushSync(() => {
        editorRoot.render(null);
      });
      return;
    }
    const ed = activeEditor;
    editorRegionEl.dataset.state = "cp1-change-card";
    editorRegionEl.dataset.changeId = ed.card.change_id;
    editorRegionEl.dataset.requestRevision = ed.card.request_revision;
    flushSync(() => {
      editorRoot.render(
        <CP1EditorView
          card={ed.card}
          priorCard={ed.priorCard}
          draftAnswers={ed.draftAnswers}
          draftExamples={ed.draftExamples}
          rpcInFlight={editorRequest !== null}
          preservedDraft={ed.preservedDraft}
          formError={ed.formError}
          onSetAnswer={(ambId, opt) => {
            if (!activeEditor || editorRequest !== null) {
              return;
            }
            activeEditor = {
              ...activeEditor,
              draftAnswers: { ...activeEditor.draftAnswers, [ambId]: opt },
            };
            syncEditorRegion();
          }}
          onSetExampleField={(exId, field, value) => {
            if (!activeEditor || editorRequest !== null) {
              return;
            }
            activeEditor = {
              ...activeEditor,
              draftExamples: activeEditor.draftExamples.map((item) =>
                item.id === exId ? { ...item, [field]: value } : item
              ),
            };
            syncEditorRegion();
          }}
          onSubmitConfirm={handleConfirmSubmit}
          onDiscardDraft={handleDiscardDraft}
          onLoadLatestCard={handleLoadLatestCard}
        />
      );
    });
  }

  // syncResultRegion replaces only the checkpoint result beside the editor.
  function syncResultRegion() {
    clearResultDataset();
    if (!activeResult) {
      flushSync(() => {
        resultRoot.render(null);
      });
      return;
    }
    const res = activeResult;
    resultRegionEl.dataset.state = res.kind;
    if (res.kind === "cp2-architecture-preview") {
      resultRegionEl.dataset.changeId = res.preview.change_id;
      resultRegionEl.dataset.requestRevision = res.preview.request_revision;
      resultRegionEl.dataset.designDigest = res.preview.design_digest;
      flushSync(() => {
        resultRoot.render(<ArchitecturePreviewView preview={res.preview} />);
      });
      return;
    }
    if (res.kind === "cp2-diff") {
      resultRegionEl.dataset.hasChanges = String(res.diff.has_changes);
      flushSync(() => {
        resultRoot.render(<DiffResultView diff={res.diff} />);
      });
      return;
    }
    if (res.kind === "cp3-draft-override") {
      flushSync(() => {
        resultRoot.render(<DraftOverrideView draft={res.draft} />);
      });
      return;
    }
    if (res.kind === "cp3-live-target") {
      flushSync(() => {
        resultRoot.render(<LiveTargetView result={res.live} />);
      });
      return;
    }
    if (res.kind === "cp4-verification") {
      resultRegionEl.dataset.runId = res.run.run_id;
      resultRegionEl.dataset.changeId = res.run.change_id;
      resultRegionEl.dataset.requestRevision = res.run.request_revision;
      flushSync(() => {
        resultRoot.render(
          <VerificationView
            run={res.run}
            displayedCardRevision={activeEditor?.card.change_id === res.run.change_id
              ? activeEditor.card.request_revision : null}
            checking={hasPendingVerification(res.run.run_id)}
            requestError={res.requestError}
            onCheckStatus={handleCheckVerificationStatus}
          />
        );
      });
      return;
    }
    if (res.kind === "cp1-incoming-card") {
      resultRegionEl.dataset.changeId = res.card.change_id;
      resultRegionEl.dataset.requestRevision = res.card.request_revision;
      flushSync(() => {
        resultRoot.render(
          <IncomingChangeCardView
            incomingCard={res.card}
            onSwitch={() => {
              if (editorRequest !== null) {
                statusEl.textContent =
                  "Wait for the pending CP1 Change Card request to finish before switching cards.";
                return;
              }
              const preserved = activeEditor ? snapshotUnsavedDraft(activeEditor) : null;
              const prior = nextPriorCard(activeEditor, res.card);
              activeResult = null;
              syncResultRegion();
              mountChangeCard(res.card, preserved, prior);
            }}
          />
        );
      });
      return;
    }
    if (res.kind === "partial-input") {
      flushSync(() => {
        resultRoot.render(<ToolInputPartialView partialArgs={res.partialArgs} />);
      });
      return;
    }
    if (res.kind === "error") {
      flushSync(() => {
        resultRoot.render(
          <Alert variant="destructive" role="alert">
            <strong>Error: </strong>
            {res.message}
          </Alert>
        );
      });
      return;
    }
    if (res.kind === "cancelled") {
      const detail =
        res.reason.length > 0
          ? "Tool call was cancelled (" +
            res.reason +
            "). Re-run the Surla checkpoint tool to inspect a fresh result."
          : "Tool call was cancelled before completion. Re-run the Surla checkpoint tool to inspect a fresh result.";
      flushSync(() => {
        resultRoot.render(
          <Card className="surla-meta" role="status">
            <strong>Cancelled: </strong>
            {detail}
          </Card>
        );
      });
    }
  }

  // callHostTool correlates one request and validates its response once. Its
  // caller receives the completion before dispatch, so the caller can own its
  // waiting state without a second protocol path or an unrelated busy flag.
  function callHostTool<T>(
    toolName: string,
    args: Record<string, unknown>,
    validateFn: (payload: unknown) => payload is T,
    onSuccess: (payload: T) => void,
    onError: (message: string) => void,
    onStart: (completion: Promise<void>) => void
  ) {
    if (!window.parent || window.parent === window) {
      onError("Cannot invoke " + toolName + ": no MCP Apps host frame is attached.");
      syncResultRegion();
      return;
    }
    const reqId = nextRequestId++;
    const params = { name: toolName, arguments: args };
    const completion = new Promise<void>((complete) => {
      pendingRequests[String(reqId)] = {
        params,
        onResult: (payload) => {
          if (validateFn(payload)) {
            onSuccess(payload);
          } else {
            onError("Tool response is missing a valid structured checkpoint payload.");
          }
          syncResultRegion();
          complete();
        },
        onError: (message) => {
          onError(message);
          syncResultRegion();
          complete();
        },
      };
    });
    onStart(completion);
    window.parent.postMessage(
      {
        jsonrpc: "2.0",
        id: reqId,
        method: "tools/call",
        params,
      },
      "*"
    );
  }

  // callEditorTool owns CP1's editable form through its request. Only card
  // requests disable that form or supply the completion CP3 needs to await.
  function callEditorTool<T>(
    toolName: string,
    args: Record<string, unknown>,
    validateFn: (payload: unknown) => payload is T,
    onSuccess: (payload: T) => void,
    onError: (message: string) => void
  ) {
    callHostTool(
      toolName,
      args,
      validateFn,
      (payload) => {
        editorRequest = null;
        onSuccess(payload);
      },
      (message) => {
        editorRequest = null;
        onError(message);
      },
      (completion) => {
        editorRequest = completion;
        syncEditorRegion();
      }
    );
  }

  // hasPendingVerification derives a button's waiting state from the original
  // calls already awaiting a host reply, without a second map or status flag.
  function hasPendingVerification(runID: string): boolean {
    return Object.values(pendingRequests).some((request) =>
      request.params.name === "get_verification" &&
      request.params.arguments.run_id === runID
    );
  }

  // displayVerification keeps the terminal result already observed for this
  // run. Its owner writes that result once and never reuses the run ID, so an
  // older notification or status reply cannot put it back into progress.
  // Another run can always be displayed without retaining a result history.
  function displayVerification(run: VerificationStatusResult) {
    const displayed = activeResult?.kind === "cp4-verification" ? activeResult.run : null;
    const shown = displayed?.run_id === run.run_id && displayed.state.type !== "running"
      ? displayed : run;
    activeResult = { kind: "cp4-verification", run: shown, requestError: null };
    statusEl.textContent = "Showing the saved verification result.";
  }

  // handleCheckVerificationStatus reads the displayed run and never starts a
  // new verification. A late reply belongs only to that run, even if another
  // checkpoint or a different run arrived while the host was processing it.
  function handleCheckVerificationStatus() {
    if (
      activeResult?.kind !== "cp4-verification" ||
      hasPendingVerification(activeResult.run.run_id)
    ) {
      return;
    }
    const runID = activeResult.run.run_id;
    activeResult = { ...activeResult, requestError: null };
    callHostTool(
      "get_verification",
      { run_id: runID },
      isVerificationStatusResult,
      (run) => {
        if (activeResult?.kind !== "cp4-verification" || activeResult.run.run_id !== runID) {
          return;
        }
        if (run.run_id !== runID) {
          activeResult = { ...activeResult, requestError: "The host returned a different verification run." };
          statusEl.textContent = "Could not check verification status; the last result is preserved.";
          return;
        }
        displayVerification(run);
      },
      (message) => {
        if (activeResult?.kind !== "cp4-verification" || activeResult.run.run_id !== runID) {
          return;
        }
        activeResult = { ...activeResult, requestError: message };
        statusEl.textContent = "Could not check verification status; the last result is preserved.";
      },
      syncResultRegion
    );
  }

  // refreshAfterPromptUpdate waits for the request active when the prompt
  // update arrived. If another request has since begun, it already reads
  // after that update; otherwise this notification starts a fresh card read.
  // Later checkpoint views do not erase the notification's continuation.
  async function refreshAfterPromptUpdate() {
    const pending = editorRequest;
    if (pending !== null) {
      await pending;
    }
    if (editorRequest === null) {
      handleLoadLatestCard();
    }
  }

  // finishPendingRequests stops local waiting when the host ends the viewer.
  // It releases prompt-update continuations without canceling server work.
  function finishPendingRequests(message: string) {
    for (const key of Object.keys(pendingRequests)) {
      const request = pendingRequests[key];
      delete pendingRequests[key];
      request.onError(message);
    }
  }

  // showFormError reports a failed read, save or input check without changing
  // the saved snapshot or discarding draft text.
  function showFormError(msg: string) {
    if (!activeEditor) {
      return;
    }
    statusEl.textContent = "Change Card request failed; unsaved edits preserved.";
    activeEditor = {
      ...activeEditor,
      formError: String(msg),
    };
    syncEditorRegion();
  }

  // handleConfirmSubmit sends complete answers and examples for the displayed
  // saved revision after checking that the user filled every required value.
  function handleConfirmSubmit() {
    if (!activeEditor || editorRequest !== null) {
      return;
    }
    const ambiguities = Array.isArray(activeEditor.card.ambiguities)
      ? activeEditor.card.ambiguities
      : [];
    for (let i = 0; i < ambiguities.length; i++) {
      const ambId = ambiguities[i].id;
      if (!activeEditor.draftAnswers[ambId]) {
        showFormError("Select an option for ambiguity " + ambId + " before confirming.");
        return;
      }
    }
    for (let j = 0; j < activeEditor.draftExamples.length; j++) {
      const item = activeEditor.draftExamples[j];
      if (!item.scenario.trim() || !item.input_summary.trim() || !item.expected_outcome.trim()) {
        showFormError(
          "Example " + item.id + " requires non-empty scenario, input_summary, and expected_outcome."
        );
        return;
      }
    }
    activeEditor = {
      ...activeEditor,
      formError: null,
    };
    statusEl.textContent = "Saving your confirmed answers and examples...";

    const submittedCard = activeEditor.card;
    const submittedAnswers = { ...activeEditor.draftAnswers };
    const submittedExamples = activeEditor.draftExamples.map((item) => ({
      id: item.id,
      scenario: item.scenario,
      input_summary: item.input_summary,
      expected_outcome: item.expected_outcome,
      verification_kind: item.verification_kind,
    }));

    callEditorTool(
      "confirm_intent",
      {
        change_id: submittedCard.change_id,
        request_revision: submittedCard.request_revision,
        answers: submittedAnswers,
        examples: submittedExamples,
      },
      isChangeCardResult,
      (updatedCard) => {
        mountChangeCard(updatedCard, null, nextPriorCard(activeEditor, updatedCard));
      },
      showFormError
    );
  }

  // handleDiscardDraft restores saved values or closes the retained editor
  // while leaving the newer checkpoint result available.
  function handleDiscardDraft() {
    if (!activeEditor || editorRequest !== null) {
      return;
    }
    if (activeResult && activeResult.kind !== "cp1-incoming-card") {
      activeEditor = null;
      syncEditorRegion();
      syncResultRegion();
      rootEl.dataset.state = activeResult.kind;
      return;
    }
    mountChangeCard(activeEditor.card, null, activeEditor.priorCard);
  }

  // handleLoadLatestCard refreshes saved values and approval through get_change.
  // The same revision keeps editable drafts; a different revision preserves
  // their text for reconciliation instead of applying it to changed examples.
  function handleLoadLatestCard() {
    if (!activeEditor || editorRequest !== null) {
      return;
    }
    const changeId = activeEditor.card.change_id;
    statusEl.textContent = "Loading the latest saved Change Card...";

    callEditorTool(
      "get_change",
      { change_id: changeId },
      isChangeCardResult,
      (latestCard) => {
        if (
          activeEditor &&
          latestCard.change_id === activeEditor.card.change_id &&
          latestCard.request_revision === activeEditor.card.request_revision
        ) {
          activeEditor = {
            ...activeEditor,
            card: latestCard,
            formError: null,
          };
          statusEl.textContent = latestCard.approval
            ? "Showing the recorded approval for these inputs."
            : latestCard.unverified_approval
            ? "Showing the saved approval record (current promotion inputs could not be verified)."
            : "Showing the latest card without a current approval.";
          syncEditorRegion();
          return;
        }
        mountChangeCard(
          latestCard,
          activeEditor ? snapshotUnsavedDraft(activeEditor) : null,
          nextPriorCard(activeEditor, latestCard)
        );
      },
      (message) => showFormError("Could not load the current card and approval: " + message)
    );
  }

  // mountChangeCard starts an editor from a received saved card and optionally
  // shows draft text or the immediately prior saved revision.
  function mountChangeCard(
    card: ChangeCardResult,
    preservedDraft: PreservedDraft | null,
    priorCard: ChangeCardResult | null = null
  ) {
    activeEditor = {
      card,
      priorCard,
      draftAnswers: isPlainObject(card.answers) ? { ...card.answers } : {},
      draftExamples: card.examples.map((ex) => ({
        id: ex.id,
        scenario: ex.scenario,
        input_summary: ex.input_summary,
        expected_outcome: ex.expected_outcome,
        verification_kind: ex.verification_kind,
      })),
      preservedDraft,
      formError: null,
    };
    statusEl.textContent =
      card.status === "confirmed"
        ? card.approval
          ? "Showing the recorded approval for these inputs."
          : card.unverified_approval
          ? "Showing the saved approval record (current promotion inputs could not be verified)."
          : "Showing confirmed Change Card. You can refine answers or examples below."
        : "Review the open questions and examples below, then confirm your intent.";
    rootEl.dataset.state = "cp1-change-card";
    syncEditorRegion();
  }

  // renderWaiting clears prior results when the host begins a new checkpoint.
  function renderWaiting(message: string) {
    activeEditor = null;
    activeResult = null;
    syncEditorRegion();
    syncResultRegion();
    rootEl.dataset.state = "waiting";
    statusEl.textContent = message;
  }

  // renderCancelled ends local waiting and shows the host's cancellation.
  function renderCancelled(reason: string) {
    activeEditor = null;
    finishPendingRequests("The host ended the checkpoint tool call.");
    activeResult = { kind: "cancelled", reason };
    syncEditorRegion();
    syncResultRegion();
    rootEl.dataset.state = "cancelled";
    statusEl.textContent = "Checkpoint tool call cancelled";
  }

  // renderError displays an uncorrelated notification failure without
  // attributing it to a pending status read or replacing its saved run.
  // Other checkpoints retain their existing draft and error behavior.
  function renderError(message: string) {
    if (activeResult?.kind === "cp4-verification") {
      statusEl.textContent = "Could not display the new checkpoint result: " + message;
      return;
    }
    if (!shouldRetainEditor()) {
      activeEditor = null;
      syncEditorRegion();
    }
    activeResult = { kind: "error", message: String(message) };
    syncResultRegion();
    rootEl.dataset.state = "error";
    statusEl.textContent = "Checkpoint payload error";
  }

  // handleToolResult validates untyped host data and updates the region that
  // owns it. Prompt updates refresh retained cards independently of whichever
  // checkpoint result is displayed when the current editor request finishes.
  function handleToolResult(params: unknown) {
    if (!isPlainObject(params)) {
      renderError("Malformed ui/notifications/tool-result: params must be an object.");
      return;
    }
    if (params.isError === true) {
      renderError(extractErrorText(params));
      return;
    }
    if (!isPlainObject(params.structuredContent)) {
      renderError("Tool result is missing structuredContent object.");
      return;
    }
    const payload = params.structuredContent;
    if (isVerificationStatusResult(payload)) {
      if (!shouldRetainEditor()) {
        activeEditor = null;
        syncEditorRegion();
      }
      displayVerification(payload);
      syncResultRegion();
      rootEl.dataset.state = "cp4-verification";
      return;
    }
    if (isArchitecturePreviewResult(payload)) {
      if (!shouldRetainEditor()) {
        activeEditor = null;
        syncEditorRegion();
      }
      activeResult = { kind: "cp2-architecture-preview", preview: payload };
      syncResultRegion();
      rootEl.dataset.state = "cp2-architecture-preview";
      statusEl.textContent = "Showing architecture preview for " + payload.system_name + ".";
      return;
    }
    if (isChangeCardResult(payload)) {
      if (activeEditor) {
        if (
          payload.change_id === activeEditor.card.change_id &&
          payload.request_revision === activeEditor.card.request_revision
        ) {
          activeEditor = {
            ...activeEditor,
            card: payload,
          };
          statusEl.textContent =
            payload.status === "confirmed"
              ? payload.approval
                ? "Showing the recorded approval for these inputs."
                : payload.unverified_approval
                ? "Showing the saved approval record (current promotion inputs could not be verified)."
                : "Showing confirmed Change Card. You can refine answers or examples below."
              : "Review the open questions and examples below, then confirm your intent.";
          syncEditorRegion();
          return;
        } else if (shouldRetainEditor()) {
          activeResult = { kind: "cp1-incoming-card", card: payload };
          syncResultRegion();
          rootEl.dataset.state = "cp1-change-card";
          statusEl.textContent =
            "A newer Change Card arrived while you were editing; your unsaved draft is preserved.";
          return;
        }
      }
      const prior = nextPriorCard(activeEditor, payload);
      activeResult = null;
      syncResultRegion();
      mountChangeCard(payload, null, prior);
      return;
    }
    if (isDiffResult(payload)) {
      if (!shouldRetainEditor()) {
        activeEditor = null;
        syncEditorRegion();
      }
      activeResult = { kind: "cp2-diff", diff: payload };
      syncResultRegion();
      rootEl.dataset.state = "cp2-diff";
      statusEl.textContent = payload.has_changes
        ? "Showing structural design diff with detected contract changes."
        : "Showing structural design diff (no contract changes detected).";
      return;
    }
    if (isDraftOverrideResult(payload)) {
      if (!shouldRetainEditor()) {
        activeEditor = null;
        syncEditorRegion();
      }
      activeResult = { kind: "cp3-draft-override", draft: payload };
      syncResultRegion();
      rootEl.dataset.state = "cp3-draft-override";
      statusEl.textContent = "Showing Fast Draft prompt and rule update.";
      if (payload.applied_in_store) {
        void refreshAfterPromptUpdate();
      }
      return;
    }
    if (isLiveTargetResult(payload) || isStopLiveTargetResult(payload)) {
      if (!shouldRetainEditor()) {
        activeEditor = null;
        syncEditorRegion();
      }
      activeResult = { kind: "cp3-live-target", live: payload };
      syncResultRegion();
      rootEl.dataset.state = "cp3-live-target";
      statusEl.textContent = isLiveTargetResult(payload)
        ? "Showing the running CP3 live target."
        : "Showing the stopped CP3 live target.";
      return;
    }
    renderError("Unsupported or malformed Surla checkpoint payload.");
  }

  // handleMessage accepts only the parent host, separates protocol requests
  // from notifications, and correlates responses with their editor request.
  function handleMessage(event: MessageEvent) {
    if (!event || event.source !== window.parent) {
      return;
    }
    const msg = event.data;
    if (!isPlainObject(msg) || msg.jsonrpc !== "2.0") {
      return;
    }
    if (typeof msg.method === "string") {
      if (msg.id !== undefined && msg.id !== null) {
        if (msg.method === "ui/resource-teardown") {
          activeEditor = null;
          finishPendingRequests("The host closed the checkpoint viewer.");
          activeResult = null;
          syncEditorRegion();
          syncResultRegion();
          rootEl.dataset.state = "terminated";
          statusEl.textContent = "Checkpoint resource torn down.";
          window.parent.postMessage({ jsonrpc: "2.0", id: msg.id, result: {} }, "*");
          return;
        }
        if (msg.method === "ping") {
          window.parent.postMessage({ jsonrpc: "2.0", id: msg.id, result: {} }, "*");
          return;
        }
        window.parent.postMessage(
          {
            jsonrpc: "2.0",
            id: msg.id,
            error: { code: -32601, message: "Method not found: " + msg.method },
          },
          "*"
        );
        return;
      }
      if (msg.method === "ui/notifications/host-context-changed") {
        if (msg.params !== undefined && !isPlainObject(msg.params)) {
          return;
        }
        applyHostTheme(msg.params);
        return;
      }
      if (msg.method === "ui/notifications/tool-input-partial") {
        if (msg.params !== undefined && !isPlainObject(msg.params)) {
          renderError("Malformed ui/notifications/tool-input-partial notification.");
          return;
        }
        if (activeEditor !== null || activeResult?.kind === "cp4-verification") {
          return;
        }
        const partialArgs =
          isPlainObject(msg.params) && isPlainObject(msg.params.arguments)
            ? msg.params.arguments
            : isPlainObject(msg.params)
            ? msg.params
            : {};
        activeEditor = null;
        activeResult = { kind: "partial-input", partialArgs };
        syncEditorRegion();
        syncResultRegion();
        rootEl.dataset.state = "waiting";
        statusEl.textContent = "Receiving checkpoint input...";
        return;
      }
      if (msg.method === "ui/notifications/tool-input") {
        if (msg.params !== undefined && !isPlainObject(msg.params)) {
          renderError("Malformed ui/notifications/tool-input notification.");
          return;
        }
        if (activeEditor !== null || activeResult?.kind === "cp4-verification") {
          return;
        }
        renderWaiting("Preparing checkpoint view...");
        return;
      }
      if (msg.method === "ui/notifications/tool-cancelled") {
        if (msg.params !== undefined && !isPlainObject(msg.params)) {
          renderError("Malformed ui/notifications/tool-cancelled notification.");
          return;
        }
        const cancelReason =
          isPlainObject(msg.params) && typeof msg.params.reason === "string"
            ? msg.params.reason
            : "";
        if (activeEditor) {
          statusEl.textContent = cancelReason
            ? "Host reported tool cancellation (" + cancelReason + "); CP1 draft preserved."
            : "Host reported tool cancellation; CP1 draft preserved.";
          return;
        }
        if (activeResult?.kind === "cp4-verification") {
          statusEl.textContent = "The host canceled a tool call; the saved verification result is still shown.";
          return;
        }
        renderCancelled(cancelReason);
        return;
      }
      if (msg.method === "ui/notifications/tool-result") {
        handleToolResult(msg.params);
      }
      return;
    }

    const reqKey = msg.id !== undefined && msg.id !== null ? String(msg.id) : "";
    if (reqKey && Object.prototype.hasOwnProperty.call(pendingRequests, reqKey)) {
      const activeReq = pendingRequests[reqKey];
      delete pendingRequests[reqKey];
      if (msg.error) {
        const rpcErr =
          isPlainObject(msg.error) && typeof msg.error.message === "string"
            ? msg.error.message
            : "Host rejected tool call.";
        activeReq.onError(rpcErr);
        return;
      }
      if (!isPlainObject(msg.result)) {
        activeReq.onError("Host returned malformed tools/call result.");
        return;
      }
      if (msg.result.isError === true) {
        activeReq.onError(extractErrorText(msg.result));
        return;
      }
      activeReq.onResult(msg.result.structuredContent);
      return;
    }

    if (initPending && msg.id === INIT_REQUEST_ID) {
      initPending = false;
      if (msg.error) {
        const errMsg =
          isPlainObject(msg.error) && typeof msg.error.message === "string"
            ? msg.error.message
            : "MCP Apps host rejected ui/initialize.";
        renderError(errMsg);
        return;
      }
      if (!isPlainObject(msg.result)) {
        renderError("MCP Apps host returned invalid ui/initialize result.");
        return;
      }
      applyHostTheme(msg.result);
      rootEl.dataset.initialized = "true";
      statusEl.textContent = "Ready. Waiting for the next checkpoint...";
      window.parent.postMessage(
        { jsonrpc: "2.0", method: "ui/notifications/initialized", params: {} },
        "*"
      );
    }
  }

  window.addEventListener("message", handleMessage);

  if (window.parent && window.parent !== window) {
    initPending = true;
    window.parent.postMessage(
      {
        jsonrpc: "2.0",
        id: INIT_REQUEST_ID,
        method: "ui/initialize",
        params: {
          protocolVersion: PROTOCOL_VERSION,
          appInfo: { name: "surla-checkpoint", version: "1.0.0" },
          appCapabilities: {},
        },
      },
      "*"
    );
  }
}

bootstrapCheckpointApp();
