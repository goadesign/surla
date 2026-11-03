// Package main implements the Surla Checkpoint Studio MCP Apps guest bridge,
// coordinating the session navigation bar (#surla-checkpoint-nav), the
// two-region layout (#surla-cp1-editor and #surla-result-view), JSON-RPC 2.0
// host lifecycle notifications, theme synchronization, and synchronous React 19
// rendering.

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
import { Alert, Button, Card } from "./components/shadcn";
import { isDraftDirty, PreservedDraft } from "./context/cp1-draft";
import {
  ArchitecturePreviewResult,
  ChangeCardResult,
  DiffResult,
  DraftOverrideResult,
  ExampleItem,
  LiveTargetResult,
  StopLiveTargetResult,
  VerificationListResult,
  VerificationStatusResult,
  isArchitecturePreviewResult,
  isChangeCardResult,
  isDiffResult,
  isDraftOverrideResult,
  isLiveTargetResult,
  isPlainObject,
  isStopLiveTargetResult,
  isVerificationListResult,
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
  // onResult validates the tool payload before a callback uses it.
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
  // incomingCard retains a newer Change Card notification that arrived while
  // the current editor had unsaved state or an in-flight request, until the
  // user switches to it or mounts a card directly.
  incomingCard: ChangeCardResult | null;
  // draftAnswers contains the user's current choices, including unsaved ones.
  draftAnswers: Record<string, string>;
  // draftExamples contains the exact text the user is editing.
  draftExamples: ExampleItem[];
  // preservedDraft retains unsaved draft edits from an earlier revision.
  preservedDraft: PreservedDraft | null;
  // formError describes the latest failed editor request or invalid input.
  formError: string | null;
}

// ActiveResultState selects the checkpoint result displayed in #surla-result-view.
// Each checkpoint payload is owned once by its dedicated session state variable.
type ActiveResultState =
  | { kind: "cp2-architecture-preview" }
  | { kind: "cp2-diff" }
  | { kind: "cp3-draft-override" }
  | { kind: "cp3-live-target" }
  | { kind: "cp4-verification" }
  | { kind: "cp1-incoming-card" }
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
  const questionByID = new Map<string, string>();
  const answerIDs: string[] = [];
  const seenIDs = new Set<string>();
  for (let i = 0; i < ambiguities.length; i++) {
    const amb = ambiguities[i];
    if (amb && typeof amb.id === "string") {
      if (typeof amb.question === "string") {
        questionByID.set(amb.id, amb.question);
      }
      if (!seenIDs.has(amb.id)) {
        seenIDs.add(amb.id);
        answerIDs.push(amb.id);
      }
    }
  }
  for (const k of Object.keys(savedAnswers).concat(Object.keys(draftAnswers))) {
    if (!seenIDs.has(k)) {
      seenIDs.add(k);
      answerIDs.push(k);
    }
  }

  const lines: string[] = [];
  for (let i = 0; i < answerIDs.length; i++) {
    const k = answerIDs[i];
    const savedVal = Object.prototype.hasOwnProperty.call(savedAnswers, k)
      ? savedAnswers[k]
      : "";
    const draftVal = Object.prototype.hasOwnProperty.call(draftAnswers, k)
      ? draftAnswers[k]
      : "";
    if (draftVal !== savedVal) {
      const q = questionByID.get(k);
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

// validateVerificationListPayload checks that a VerificationListResult carries
// a non-empty change_id, matches expectedChangeId when correlated, and only
// contains runs belonging to that same change_id.
function validateVerificationListPayload(
  payload: VerificationListResult,
  expectedChangeId?: string
): string | null {
  if (!payload.change_id || payload.change_id.trim().length === 0) {
    return "The host returned a verification list without a change_id.";
  }
  if (expectedChangeId !== undefined && payload.change_id !== expectedChangeId) {
    return (
      "The host returned verification runs for Change Card " +
      payload.change_id +
      " instead of " +
      expectedChangeId +
      "."
    );
  }
  for (let i = 0; i < payload.runs.length; i++) {
    if (payload.runs[i].change_id !== payload.change_id) {
      return (
        "The host returned a verification run for Change Card " +
        payload.runs[i].change_id +
        " inside the list for " +
        payload.change_id +
        "."
      );
    }
  }
  return null;
}

// bootstrapCheckpointApp mounts the Checkpoint Studio inside the MCP Apps host
// document and registers the window postMessage bridge.
export function bootstrapCheckpointApp() {
  const rawRootEl = document.getElementById("surla-checkpoint-root");
  const rawStatusEl = document.getElementById("surla-checkpoint-status");
  const viewEl = document.getElementById("surla-checkpoint-view");
  if (!rawRootEl || !rawStatusEl || !viewEl) {
    return;
  }
  const rootEl: HTMLElement = rawRootEl;
  const statusEl: HTMLElement = rawStatusEl;

  let navRegionEl = document.getElementById("surla-checkpoint-nav");
  if (!navRegionEl) {
    navRegionEl = document.createElement("nav");
    navRegionEl.setAttribute("id", "surla-checkpoint-nav");
    navRegionEl.setAttribute("aria-label", "Checkpoint views");
    navRegionEl.hidden = true;
    if (viewEl.parentNode) {
      viewEl.parentNode.insertBefore(navRegionEl, viewEl);
    }
  }
  const navEl: HTMLElement = navRegionEl;

  const editorRegionEl = document.createElement("div");
  editorRegionEl.setAttribute("id", "surla-cp1-editor");
  const resultRegionEl = document.createElement("div");
  resultRegionEl.setAttribute("id", "surla-result-view");
  while (viewEl.firstChild) {
    viewEl.removeChild(viewEl.firstChild);
  }
  viewEl.appendChild(editorRegionEl);
  viewEl.appendChild(resultRegionEl);

  const navRoot: Root = createRoot(navEl);
  const editorRoot: Root = createRoot(editorRegionEl);
  const resultRoot: Root = createRoot(resultRegionEl);

  let initPending = false;
  let nextRequestId = 2;
  const pendingRequests = new Map<string, PendingRPC>();
  // editorRequest completes after the current card request updates the view.
  // CP3 waits for it; independent verification reads do not block editing.
  let editorRequest: Promise<void> | null = null;
  let activeEditor: ActiveEditorState | null = null;
  // editorOpen tracks whether #surla-cp1-editor is mounted in the DOM.
  // Clean CP2/CP3/CP4 notifications set editorOpen = false while keeping
  // activeEditor in memory for session navigation and revision comparison.
  let editorOpen = false;
  let previewState: ArchitecturePreviewResult | null = null;
  let diffState: DiffResult | null = null;
  let draftOverrideState: DraftOverrideResult | null = null;
  let liveTargetState: LiveTargetResult | StopLiveTargetResult | null = null;
  const runsById = new Map<string, VerificationStatusResult>();
  const lastLoadedListByChangeId = new Map<string, string[]>();
  let selectedVerificationChangeId: string | null = null;
  let selectedRunId: string | null = null;
  const selectionTouchedDuringList = new Map<string, boolean>();
  const runRequestErrors = new Map<string, string>();
  const listRequestErrors = new Map<string, string>();
  const cardLoadErrors = new Map<string, string>();
  let activeResult: ActiveResultState | null = null;

  // isViewingChangeResult reports whether the active result view currently
  // displays a CP2 preview or CP4 verification view for changeId.
  function isViewingChangeResult(changeId: string): boolean {
    if (activeResult?.kind === "cp2-architecture-preview") {
      return previewState !== null && previewState.change_id === changeId;
    }
    if (activeResult?.kind === "cp4-verification") {
      const displayedRun =
        selectedRunId !== null ? runsById.get(selectedRunId) ?? null : null;
      const effectiveId = displayedRun
        ? displayedRun.change_id
        : selectedVerificationChangeId || "";
      return effectiveId === changeId;
    }
    return false;
  }

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
      ((editorOpen && editorRequest !== null) || hasUnsavedEditorState())
    );
  }

  // isEditorProtected reports whether incoming tool-input or tool-cancelled
  // notifications must leave the current CP1 editor untouched.
  function isEditorProtected(): boolean {
    return (
      activeEditor !== null &&
      (editorOpen || hasUnsavedEditorState())
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

  // syncNavRegion renders the session navigation bar when two or more
  // checkpoints are available in the session.
  function syncNavRegion() {
    if (rootEl.dataset.state === "terminated") {
      navEl.hidden = true;
      flushSync(() => {
        navRoot.render(null);
      });
      return;
    }

    const tabs: {
      id: string;
      label: string;
      active: boolean;
      onClick: () => void;
    }[] = [];

    const cp1Active =
      editorOpen && (!activeResult || activeResult.kind === "cp1-incoming-card");

    if (activeEditor !== null) {
      tabs.push({
        id: "surla-nav-cp1",
        label: "Change Card (CP1 / CP5)",
        active: cp1Active,
        onClick: handleReturnToChangeCard,
      });
    }
    if (previewState !== null) {
      tabs.push({
        id: "surla-nav-cp2",
        label: "Architecture Preview (CP2)",
        active: activeResult?.kind === "cp2-architecture-preview",
        onClick: () => {
          if (!previewState) {
            return;
          }
          editorOpen = shouldRetainEditor();
          activeResult = { kind: "cp2-architecture-preview" };
          rootEl.dataset.state = "cp2-architecture-preview";
          statusEl.textContent =
            "Showing architecture preview for " + previewState.system_name + ".";
          syncAllRegions();
        },
      });
    }
    if (diffState !== null) {
      tabs.push({
        id: "surla-nav-diff",
        label: "Design Diff (CP2)",
        active: activeResult?.kind === "cp2-diff",
        onClick: () => {
          if (!diffState) {
            return;
          }
          editorOpen = shouldRetainEditor();
          activeResult = { kind: "cp2-diff" };
          rootEl.dataset.state = "cp2-diff";
          statusEl.textContent = diffState.has_changes
            ? "Showing structural design diff with detected contract changes."
            : "Showing structural design diff (no contract changes detected).";
          syncAllRegions();
        },
      });
    }
    if (draftOverrideState !== null || liveTargetState !== null) {
      tabs.push({
        id: "surla-nav-cp3",
        label: "Prompt & Live Target (CP3)",
        active:
          activeResult?.kind === "cp3-draft-override" ||
          activeResult?.kind === "cp3-live-target",
        onClick: () => {
          editorOpen = shouldRetainEditor();
          if (draftOverrideState !== null) {
            activeResult = { kind: "cp3-draft-override" };
            rootEl.dataset.state = "cp3-draft-override";
            statusEl.textContent =
              liveTargetState !== null
                ? "Showing CP3 live target and Fast Draft prompt update."
                : "Showing Fast Draft prompt and rule update.";
          } else if (liveTargetState !== null) {
            activeResult = { kind: "cp3-live-target" };
            rootEl.dataset.state = "cp3-live-target";
            statusEl.textContent = isLiveTargetResult(liveTargetState)
              ? "Showing the running CP3 live target."
              : "Showing the stopped CP3 live target.";
          }
          syncAllRegions();
        },
      });
    }
    if (selectedVerificationChangeId !== null) {
      tabs.push({
        id: "surla-nav-cp4",
        label: "Verification (CP4)",
        active: activeResult?.kind === "cp4-verification",
        onClick: () => {
          if (!selectedVerificationChangeId) {
            return;
          }
          editorOpen = shouldRetainEditor();
          activeResult = { kind: "cp4-verification" };
          rootEl.dataset.state = "cp4-verification";
          statusEl.textContent =
            selectedRunId !== null
              ? "Showing the saved verification result."
              : "Showing saved verification runs for Change Card " +
                selectedVerificationChangeId +
                ".";
          syncAllRegions();
        },
      });
    }

    if (tabs.length < 2) {
      navEl.hidden = true;
      flushSync(() => {
        navRoot.render(null);
      });
      return;
    }

    navEl.hidden = false;
    flushSync(() => {
      navRoot.render(
        <>
          {tabs.map((tab) => (
            <Button
              key={tab.id}
              id={tab.id}
              variant="pill"
              aria-current={tab.active ? "page" : undefined}
              onClick={tab.onClick}
            >
              {tab.label}
            </Button>
          ))}
        </>
      );
    });
  }

  // syncEditorRegion renders saved values and drafts together when editorOpen
  // is true, with editing disabled while the host owns an outstanding editor request.
  function syncEditorRegion() {
    if (!activeEditor || !editorOpen) {
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
          loadingVerifications={hasPendingListVerifications(ed.card.change_id)}
          cp4Open={activeResult?.kind === "cp4-verification"}
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
          onLoadLatestCard={() => handleLoadLatestCard(false)}
          onLoadVerifications={() => handleListVerifications(ed.card.change_id)}
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
      if (!previewState) {
        flushSync(() => {
          resultRoot.render(null);
        });
        return;
      }
      const preview = previewState;
      resultRegionEl.dataset.changeId = preview.change_id;
      resultRegionEl.dataset.requestRevision = preview.request_revision;
      resultRegionEl.dataset.designDigest = preview.design_digest;
      flushSync(() => {
        resultRoot.render(
          <ArchitecturePreviewView
            preview={preview}
            loadedCard={activeEditor ? activeEditor.card : null}
            loadingCard={editorRequest !== null}
            cardLoadError={cardLoadErrors.get(preview.change_id) ?? null}
            onOpenChangeCard={() => handleOpenChangeCard(preview.change_id)}
          />
        );
      });
      return;
    }
    if (res.kind === "cp2-diff") {
      if (!diffState) {
        flushSync(() => {
          resultRoot.render(null);
        });
        return;
      }
      const diff = diffState;
      resultRegionEl.dataset.hasChanges = String(diff.has_changes);
      flushSync(() => {
        resultRoot.render(<DiffResultView diff={diff} />);
      });
      return;
    }
    if (res.kind === "cp3-draft-override" || res.kind === "cp3-live-target") {
      const draft = draftOverrideState;
      const liveTarget = liveTargetState;
      if (!draft && !liveTarget) {
        flushSync(() => {
          resultRoot.render(null);
        });
        return;
      }
      flushSync(() => {
        resultRoot.render(
          <>
            {liveTarget ? <LiveTargetView result={liveTarget} /> : null}
            {draft ? <DraftOverrideView draft={draft} /> : null}
          </>
        );
      });
      return;
    }
    if (res.kind === "cp4-verification") {
      const changeId = selectedVerificationChangeId || "";
      const displayedRun =
        selectedRunId !== null ? runsById.get(selectedRunId) ?? null : null;
      if (displayedRun) {
        resultRegionEl.dataset.runId = displayedRun.run_id;
        resultRegionEl.dataset.changeId = displayedRun.change_id;
        resultRegionEl.dataset.requestRevision = displayedRun.request_revision;
      } else if (changeId) {
        resultRegionEl.dataset.changeId = changeId;
      }
      const effectiveChangeId = displayedRun ? displayedRun.change_id : changeId;
      const listedIds = lastLoadedListByChangeId.get(effectiveChangeId);
      const listedRuns =
        listedIds !== undefined
          ? listedIds
              .map((id) => runsById.get(id))
              .filter((r): r is VerificationStatusResult => Boolean(r))
          : null;
      flushSync(() => {
        resultRoot.render(
          <VerificationView
            changeId={effectiveChangeId}
            run={displayedRun}
            listedRuns={listedRuns}
            loadedCard={activeEditor ? activeEditor.card : null}
            checking={displayedRun ? hasPendingVerification(displayedRun.run_id) : false}
            loadingList={hasPendingListVerifications(effectiveChangeId)}
            loadingCard={editorRequest !== null}
            requestError={displayedRun ? runRequestErrors.get(displayedRun.run_id) ?? null : null}
            listError={listRequestErrors.get(effectiveChangeId) ?? null}
            cardLoadError={
              effectiveChangeId ? cardLoadErrors.get(effectiveChangeId) ?? null : null
            }
            onCheckStatus={handleCheckVerificationStatus}
            onLoadVerifications={() => handleListVerifications(effectiveChangeId)}
            onSelectRun={handleSelectVerificationRun}
            onOpenChangeCard={() => handleOpenChangeCard(effectiveChangeId)}
            onReturnToChangeCard={handleReturnToChangeCard}
          />
        );
      });
      return;
    }
    if (res.kind === "cp1-incoming-card") {
      const incoming = activeEditor ? activeEditor.incomingCard : null;
      if (!incoming) {
        flushSync(() => {
          resultRoot.render(null);
        });
        return;
      }
      resultRegionEl.dataset.changeId = incoming.change_id;
      resultRegionEl.dataset.requestRevision = incoming.request_revision;
      flushSync(() => {
        resultRoot.render(
          <IncomingChangeCardView
            incomingCard={incoming}
            onSwitch={() => {
              if (editorRequest !== null) {
                statusEl.textContent =
                  "Wait for the pending CP1 Change Card request to finish before switching cards.";
                return;
              }
              const preserved = activeEditor ? snapshotUnsavedDraft(activeEditor) : null;
              const prior = nextPriorCard(activeEditor, incoming);
              activeResult = null;
              mountChangeCard(incoming, preserved, prior);
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

  // syncAllRegions synchronizes the editor, result, and session nav regions.
  function syncAllRegions() {
    syncEditorRegion();
    syncResultRegion();
    syncNavRegion();
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
      syncAllRegions();
      return;
    }
    const reqId = nextRequestId++;
    const params = { name: toolName, arguments: args };
    const completion = new Promise<void>((complete) => {
      pendingRequests.set(String(reqId), {
        params,
        onResult: (payload) => {
          if (validateFn(payload)) {
            onSuccess(payload);
          } else {
            onError("Tool response is missing a valid structured checkpoint payload.");
          }
          syncAllRegions();
          complete();
        },
        onError: (message) => {
          onError(message);
          syncAllRegions();
          complete();
        },
      });
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
        syncAllRegions();
      }
    );
  }

  // hasPendingVerification derives a button's waiting state from the original
  // calls already awaiting a host reply, without a second map or status flag.
  function hasPendingVerification(runID: string): boolean {
    for (const request of pendingRequests.values()) {
      if (
        request.params.name === "get_verification" &&
        request.params.arguments.run_id === runID
      ) {
        return true;
      }
    }
    return false;
  }

  // hasPendingListVerifications reports whether a list_verifications request
  // is currently in flight for changeId.
  function hasPendingListVerifications(changeId: string): boolean {
    for (const request of pendingRequests.values()) {
      if (
        request.params.name === "list_verifications" &&
        request.params.arguments.change_id === changeId
      ) {
        return true;
      }
    }
    return false;
  }

  // upsertRunSnapshot stores run in runsById unless runsById.get(run.run_id)
  // already holds a terminal state (state.type !== "running").
  function upsertRunSnapshot(run: VerificationStatusResult): VerificationStatusResult {
    const existing = runsById.get(run.run_id);
    if (existing && existing.state.type !== "running") {
      return existing;
    }
    runsById.set(run.run_id, run);
    return run;
  }

  // displayVerification stores and selects one verification run snapshot.
  // When updating the already-selected (change_id, run_id) in cp4-verification,
  // it preserves the activeResult object reference so an in-flight get_change
  // read for that displayed card is not treated as a view transition.
  function displayVerification(run: VerificationStatusResult) {
    if (hasPendingListVerifications(run.change_id)) {
      selectionTouchedDuringList.set(run.change_id, true);
    }
    const shown = upsertRunSnapshot(run);
    runRequestErrors.delete(shown.run_id);
    const sameDisplayedRun =
      activeResult?.kind === "cp4-verification" &&
      selectedVerificationChangeId === shown.change_id &&
      selectedRunId === shown.run_id;
    selectedVerificationChangeId = shown.change_id;
    selectedRunId = shown.run_id;
    if (!sameDisplayedRun) {
      activeResult = { kind: "cp4-verification" };
    }
    statusEl.textContent = "Showing the saved verification result.";
  }

  // handleSelectVerificationRun switches the displayed run inside CP4.
  function handleSelectVerificationRun(runId: string) {
    const target = runsById.get(runId);
    if (!target) {
      return;
    }
    if (hasPendingListVerifications(target.change_id)) {
      selectionTouchedDuringList.set(target.change_id, true);
    }
    selectedVerificationChangeId = target.change_id;
    selectedRunId = target.run_id;
    activeResult = { kind: "cp4-verification" };
    rootEl.dataset.state = "cp4-verification";
    statusEl.textContent = "Showing the saved verification result.";
    syncAllRegions();
  }

  // handleCheckVerificationStatus reads the displayed run and never starts a
  // new verification. A late reply belongs only to that run, even if another
  // checkpoint or a different run arrived while the host was processing it.
  function handleCheckVerificationStatus() {
    if (
      activeResult?.kind !== "cp4-verification" ||
      selectedRunId === null ||
      !runsById.has(selectedRunId) ||
      hasPendingVerification(selectedRunId)
    ) {
      return;
    }
    const runID = selectedRunId;
    runRequestErrors.delete(runID);
    callHostTool(
      "get_verification",
      { run_id: runID },
      isVerificationStatusResult,
      (run) => {
        if (run.run_id !== runID) {
          if (activeResult?.kind === "cp4-verification" && selectedRunId === runID) {
            runRequestErrors.set(runID, "The host returned a different verification run.");
            statusEl.textContent =
              "Could not check verification status; the last result is preserved.";
          }
          return;
        }
        upsertRunSnapshot(run);
        runRequestErrors.delete(runID);
        if (activeResult?.kind === "cp4-verification" && selectedRunId === runID) {
          statusEl.textContent = "Showing the saved verification result.";
        }
      },
      (message) => {
        if (activeResult?.kind !== "cp4-verification" || selectedRunId !== runID) {
          return;
        }
        runRequestErrors.set(runID, message);
        statusEl.textContent =
          "Could not check verification status; the last result is preserved.";
      },
      syncAllRegions
    );
  }

  // handleListVerifications loads the saved verification runs for requestedChangeId,
  // enforcing at most one in-flight list_verifications request per change_id.
  function handleListVerifications(requestedChangeId: string) {
    if (!requestedChangeId || hasPendingListVerifications(requestedChangeId)) {
      return;
    }
    selectionTouchedDuringList.set(requestedChangeId, false);
    listRequestErrors.delete(requestedChangeId);
    if (!shouldRetainEditor()) {
      editorOpen = false;
    }
    selectedVerificationChangeId = requestedChangeId;
    if (
      selectedRunId !== null &&
      runsById.get(selectedRunId)?.change_id !== requestedChangeId
    ) {
      const prevList = lastLoadedListByChangeId.get(requestedChangeId);
      selectedRunId = prevList && prevList.length > 0 ? prevList[0] : null;
    }
    activeResult = { kind: "cp4-verification" };
    rootEl.dataset.state = "cp4-verification";
    statusEl.textContent = "Loading saved verification runs...";

    callHostTool(
      "list_verifications",
      { change_id: requestedChangeId },
      isVerificationListResult,
      (payload) => {
        const touched = Boolean(selectionTouchedDuringList.get(requestedChangeId));
        selectionTouchedDuringList.delete(requestedChangeId);
        const mismatchErr = validateVerificationListPayload(payload, requestedChangeId);
        if (mismatchErr !== null) {
          listRequestErrors.set(requestedChangeId, mismatchErr);
          if (
            activeResult?.kind === "cp4-verification" &&
            selectedVerificationChangeId === requestedChangeId
          ) {
            statusEl.textContent = "Could not load saved verification runs.";
          }
          return;
        }
        for (let i = 0; i < payload.runs.length; i++) {
          upsertRunSnapshot(payload.runs[i]);
        }
        const loadedIds = payload.runs.map((r) => r.run_id);
        lastLoadedListByChangeId.set(requestedChangeId, loadedIds);
        listRequestErrors.delete(requestedChangeId);

        if (
          activeResult?.kind === "cp4-verification" &&
          selectedVerificationChangeId === requestedChangeId
        ) {
          if (
            touched &&
            selectedRunId !== null &&
            runsById.get(selectedRunId)?.change_id === requestedChangeId
          ) {
            // Preserve the run explicitly selected or notified during the list read.
          } else if (selectedRunId !== null && loadedIds.includes(selectedRunId)) {
            // Preserve the selected run when it remains in the loaded list.
          } else {
            selectedRunId = loadedIds.length > 0 ? loadedIds[0] : null;
          }
          statusEl.textContent =
            loadedIds.length > 0
              ? "Showing saved verification runs for Change Card " + requestedChangeId + "."
              : "No saved verification runs found for Change Card " + requestedChangeId + ".";
        }
      },
      (message) => {
        selectionTouchedDuringList.delete(requestedChangeId);
        listRequestErrors.set(requestedChangeId, message);
        if (
          activeResult?.kind === "cp4-verification" &&
          selectedVerificationChangeId === requestedChangeId
        ) {
          statusEl.textContent = "Could not load saved verification runs.";
        }
      },
      syncAllRegions
    );
  }

  // handleReturnToChangeCard switches the view back to the retained Change Card
  // and restores any pending incoming Change Card banner.
  function handleReturnToChangeCard() {
    if (!activeEditor) {
      return;
    }
    cardLoadErrors.delete(activeEditor.card.change_id);
    editorOpen = true;
    activeResult = activeEditor.incomingCard ? { kind: "cp1-incoming-card" } : null;
    rootEl.dataset.state = "cp1-change-card";
    const card = activeEditor.card;
    statusEl.textContent = activeEditor.incomingCard
      ? "A newer Change Card arrived while you were editing; your unsaved draft is preserved."
      : card.status === "confirmed"
      ? card.approval
        ? "Showing the recorded approval for these inputs."
        : card.unverified_approval
        ? "Showing the saved approval record (current promotion inputs could not be verified)."
        : "Showing confirmed Change Card. You can refine answers or examples below."
      : "Review the open questions and examples below, then confirm your intent.";
    syncAllRegions();
  }

  // handleOpenChangeCard loads requestedChangeId via get_change when not
  // already loaded in activeEditor, correlating completion and error to
  // the exact activeResult view and requestedChangeId while preserving any
  // subsequent navigation or dirty draft.
  function handleOpenChangeCard(requestedChangeId: string) {
    if (!requestedChangeId || editorRequest !== null) {
      return;
    }
    if (activeEditor && activeEditor.card.change_id === requestedChangeId) {
      handleReturnToChangeCard();
      return;
    }
    const initialActiveResult = activeResult;
    if (!initialActiveResult) {
      return;
    }
    cardLoadErrors.delete(requestedChangeId);
    statusEl.textContent = "Loading Change Card " + requestedChangeId + "...";
    callEditorTool(
      "get_change",
      { change_id: requestedChangeId },
      isChangeCardResult,
      (loadedCard) => {
        if (loadedCard.change_id !== requestedChangeId) {
          cardLoadErrors.set(
            requestedChangeId,
            "The host returned Change Card " +
              loadedCard.change_id +
              " instead of " +
              requestedChangeId +
              "."
          );
          if (activeResult === initialActiveResult && isViewingChangeResult(requestedChangeId)) {
            statusEl.textContent = "Could not load the requested Change Card.";
          }
          return;
        }
        cardLoadErrors.delete(requestedChangeId);
        if (activeResult !== initialActiveResult || !isViewingChangeResult(requestedChangeId)) {
          return;
        }
        const preserved = activeEditor ? snapshotUnsavedDraft(activeEditor) : null;
        const prior = nextPriorCard(activeEditor, loadedCard);
        if (activeEditor) {
          activeEditor = {
            ...activeEditor,
            incomingCard: null,
          };
        }
        activeResult = null;
        mountChangeCard(loadedCard, preserved, prior);
      },
      (message) => {
        cardLoadErrors.set(requestedChangeId, "Could not load Change Card: " + message);
        if (activeResult === initialActiveResult && isViewingChangeResult(requestedChangeId)) {
          statusEl.textContent = "Could not load the requested Change Card.";
        }
      }
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
    if (editorRequest === null && activeEditor !== null) {
      handleLoadLatestCard(true);
    }
  }

  // finishPendingRequests stops local waiting when the host ends the viewer.
  // It releases prompt-update continuations without canceling server work.
  function finishPendingRequests(message: string) {
    for (const [key, request] of Array.from(pendingRequests.entries())) {
      pendingRequests.delete(key);
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
    syncAllRegions();
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
      if (!Object.prototype.hasOwnProperty.call(activeEditor.draftAnswers, ambId) || !activeEditor.draftAnswers[ambId]) {
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

  // handleDiscardDraft restores saved values on the retained card and closes
  // the split-screen editor when another checkpoint is displayed beside it.
  function handleDiscardDraft() {
    if (!activeEditor || editorRequest !== null) {
      return;
    }
    const savedCard = activeEditor.card;
    if (activeResult && activeResult.kind !== "cp1-incoming-card") {
      activeEditor = {
        card: savedCard,
        priorCard: activeEditor.priorCard,
        incomingCard: activeEditor.incomingCard,
        draftAnswers: isPlainObject(savedCard.answers) ? { ...savedCard.answers } : {},
        draftExamples: savedCard.examples.map((ex) => ({
          id: ex.id,
          scenario: ex.scenario,
          input_summary: ex.input_summary,
          expected_outcome: ex.expected_outcome,
          verification_kind: ex.verification_kind,
        })),
        preservedDraft: null,
        formError: null,
      };
      editorOpen = false;
      rootEl.dataset.state = activeResult.kind;
      syncAllRegions();
      return;
    }
    mountChangeCard(savedCard, null, activeEditor.priorCard);
  }

  // handleLoadLatestCard refreshes saved values and approval through get_change.
  // The same revision keeps editable drafts; a different revision preserves
  // their text for reconciliation instead of applying it to changed examples.
  function handleLoadLatestCard(keepHiddenIfClosed = false) {
    if (!activeEditor || editorRequest !== null) {
      return;
    }
    const changeId = activeEditor.card.change_id;
    if (editorOpen || !keepHiddenIfClosed) {
      statusEl.textContent = "Loading the latest saved Change Card...";
    }

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
          if (!activeResult && activeEditor.incomingCard) {
            activeResult = { kind: "cp1-incoming-card" };
          }
          if (editorOpen || !keepHiddenIfClosed) {
            statusEl.textContent = latestCard.approval
              ? "Showing the recorded approval for these inputs."
              : latestCard.unverified_approval
              ? "Showing the saved approval record (current promotion inputs could not be verified)."
              : "Showing the latest card without a current approval.";
          }
          syncAllRegions();
          return;
        }
        const shouldOpen = editorOpen || !keepHiddenIfClosed || hasUnsavedEditorState();
        const preserved = activeEditor ? snapshotUnsavedDraft(activeEditor) : null;
        const prior = nextPriorCard(activeEditor, latestCard);
        if (shouldOpen) {
          mountChangeCard(latestCard, preserved, prior);
        } else {
          activeEditor = {
            card: latestCard,
            priorCard: prior,
            incomingCard: null,
            draftAnswers: isPlainObject(latestCard.answers) ? { ...latestCard.answers } : {},
            draftExamples: latestCard.examples.map((ex) => ({
              id: ex.id,
              scenario: ex.scenario,
              input_summary: ex.input_summary,
              expected_outcome: ex.expected_outcome,
              verification_kind: ex.verification_kind,
            })),
            preservedDraft: preserved,
            formError: null,
          };
          syncAllRegions();
        }
      },
      (message) => showFormError("Could not load the current card and approval: " + message)
    );
  }

  // mountChangeCard starts an editor from a received saved card and optionally
  // shows draft text or the immediately prior saved revision. Any distinct
  // incomingCard that arrived while an editor RPC was in flight remains retained.
  function mountChangeCard(
    card: ChangeCardResult,
    preservedDraft: PreservedDraft | null,
    priorCard: ChangeCardResult | null = null
  ) {
    cardLoadErrors.delete(card.change_id);
    const existingIncoming = activeEditor ? activeEditor.incomingCard : null;
    const keepIncoming =
      existingIncoming !== null &&
      (existingIncoming.change_id !== card.change_id ||
        existingIncoming.request_revision !== card.request_revision)
        ? existingIncoming
        : null;
    if (!keepIncoming && activeResult?.kind === "cp1-incoming-card") {
      activeResult = null;
    }
    activeEditor = {
      card,
      priorCard,
      incomingCard: keepIncoming,
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
    editorOpen = true;
    statusEl.textContent =
      card.status === "confirmed"
        ? card.approval
          ? "Showing the recorded approval for these inputs."
          : card.unverified_approval
          ? "Showing the saved approval record (current promotion inputs could not be verified)."
          : "Showing confirmed Change Card. You can refine answers or examples below."
        : "Review the open questions and examples below, then confirm your intent.";
    rootEl.dataset.state = "cp1-change-card";
    syncAllRegions();
  }

  // renderWaiting clears the active result display when the host begins a new
  // checkpoint while preserving any retained session state.
  function renderWaiting(message: string) {
    if (!shouldRetainEditor()) {
      editorOpen = false;
    }
    activeResult = null;
    rootEl.dataset.state = "waiting";
    statusEl.textContent = message;
    syncAllRegions();
  }

  // renderCancelled shows the host's cancellation notice without aborting
  // independent tools/call requests or discarding retained session state.
  function renderCancelled(reason: string) {
    if (!shouldRetainEditor()) {
      editorOpen = false;
    }
    activeResult = { kind: "cancelled", reason };
    rootEl.dataset.state = "cancelled";
    statusEl.textContent = "Checkpoint tool call cancelled";
    syncAllRegions();
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
      editorOpen = false;
    }
    activeResult = { kind: "error", message: String(message) };
    rootEl.dataset.state = "error";
    statusEl.textContent = "Checkpoint payload error";
    syncAllRegions();
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
        editorOpen = false;
      }
      displayVerification(payload);
      rootEl.dataset.state = "cp4-verification";
      syncAllRegions();
      return;
    }
    if (isVerificationListResult(payload)) {
      const listErr = validateVerificationListPayload(payload);
      if (listErr !== null) {
        renderError(listErr);
        return;
      }
      for (let i = 0; i < payload.runs.length; i++) {
        upsertRunSnapshot(payload.runs[i]);
      }
      const listedIds = payload.runs.map((r) => r.run_id);
      lastLoadedListByChangeId.set(payload.change_id, listedIds);
      listRequestErrors.delete(payload.change_id);
      if (!shouldRetainEditor()) {
        editorOpen = false;
      }
      selectedVerificationChangeId = payload.change_id;
      if (listedIds.length === 0) {
        selectedRunId = null;
        statusEl.textContent =
          "No saved verification runs found for Change Card " + payload.change_id + ".";
      } else {
        if (selectedRunId === null || !listedIds.includes(selectedRunId)) {
          selectedRunId = listedIds[0];
        }
        statusEl.textContent =
          "Showing saved verification runs for Change Card " + payload.change_id + ".";
      }
      activeResult = { kind: "cp4-verification" };
      rootEl.dataset.state = "cp4-verification";
      syncAllRegions();
      return;
    }
    if (isArchitecturePreviewResult(payload)) {
      if (!shouldRetainEditor()) {
        editorOpen = false;
      }
      previewState = payload;
      cardLoadErrors.delete(payload.change_id);
      activeResult = { kind: "cp2-architecture-preview" };
      rootEl.dataset.state = "cp2-architecture-preview";
      statusEl.textContent = "Showing architecture preview for " + payload.system_name + ".";
      syncAllRegions();
      return;
    }
    if (isChangeCardResult(payload)) {
      cardLoadErrors.delete(payload.change_id);
      if (activeEditor) {
        if (
          payload.change_id === activeEditor.card.change_id &&
          payload.request_revision === activeEditor.card.request_revision
        ) {
          activeEditor = {
            ...activeEditor,
            card: payload,
          };
          editorOpen = true;
          if (!activeResult && activeEditor.incomingCard) {
            activeResult = { kind: "cp1-incoming-card" };
          }
          if (!activeResult || activeResult.kind === "cp1-incoming-card") {
            rootEl.dataset.state = "cp1-change-card";
          }
          statusEl.textContent =
            payload.status === "confirmed"
              ? payload.approval
                ? "Showing the recorded approval for these inputs."
                : payload.unverified_approval
                ? "Showing the saved approval record (current promotion inputs could not be verified)."
                : "Showing confirmed Change Card. You can refine answers or examples below."
              : "Review the open questions and examples below, then confirm your intent.";
          syncAllRegions();
          return;
        } else if (
          activeEditor.incomingCard &&
          payload.change_id === activeEditor.incomingCard.change_id &&
          payload.request_revision === activeEditor.incomingCard.request_revision
        ) {
          activeEditor = {
            ...activeEditor,
            incomingCard: payload,
          };
          editorOpen = true;
          if (!activeResult || activeResult.kind === "cp1-incoming-card") {
            activeResult = { kind: "cp1-incoming-card" };
            rootEl.dataset.state = "cp1-change-card";
            statusEl.textContent =
              "A newer Change Card arrived while you were editing; your unsaved draft is preserved.";
          }
          syncAllRegions();
          return;
        } else if (shouldRetainEditor()) {
          activeEditor = {
            ...activeEditor,
            incomingCard: payload,
          };
          editorOpen = true;
          activeResult = { kind: "cp1-incoming-card" };
          rootEl.dataset.state = "cp1-change-card";
          statusEl.textContent =
            "A newer Change Card arrived while you were editing; your unsaved draft is preserved.";
          syncAllRegions();
          return;
        }
      }
      const prior = nextPriorCard(activeEditor, payload);
      if (activeEditor) {
        activeEditor = {
          ...activeEditor,
          incomingCard: null,
        };
      }
      activeResult = null;
      mountChangeCard(payload, null, prior);
      return;
    }
    if (isDiffResult(payload)) {
      if (!shouldRetainEditor()) {
        editorOpen = false;
      }
      diffState = payload;
      activeResult = { kind: "cp2-diff" };
      rootEl.dataset.state = "cp2-diff";
      statusEl.textContent = payload.has_changes
        ? "Showing structural design diff with detected contract changes."
        : "Showing structural design diff (no contract changes detected).";
      syncAllRegions();
      return;
    }
    if (isDraftOverrideResult(payload)) {
      if (!shouldRetainEditor()) {
        editorOpen = false;
      }
      draftOverrideState = payload;
      activeResult = { kind: "cp3-draft-override" };
      rootEl.dataset.state = "cp3-draft-override";
      statusEl.textContent = "Showing Fast Draft prompt and rule update.";
      syncAllRegions();
      if (payload.applied_in_store) {
        void refreshAfterPromptUpdate();
      }
      return;
    }
    if (isLiveTargetResult(payload) || isStopLiveTargetResult(payload)) {
      if (!shouldRetainEditor()) {
        editorOpen = false;
      }
      liveTargetState = payload;
      activeResult = { kind: "cp3-live-target" };
      rootEl.dataset.state = "cp3-live-target";
      statusEl.textContent = isLiveTargetResult(payload)
        ? "Showing the running CP3 live target."
        : "Showing the stopped CP3 live target.";
      syncAllRegions();
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
          editorOpen = false;
          previewState = null;
          diffState = null;
          draftOverrideState = null;
          liveTargetState = null;
          selectedVerificationChangeId = null;
          selectedRunId = null;
          runsById.clear();
          lastLoadedListByChangeId.clear();
          selectionTouchedDuringList.clear();
          runRequestErrors.clear();
          listRequestErrors.clear();
          cardLoadErrors.clear();
          finishPendingRequests("The host closed the checkpoint viewer.");
          activeResult = null;
          rootEl.dataset.state = "terminated";
          statusEl.textContent = "Checkpoint resource torn down.";
          syncAllRegions();
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
        if (isEditorProtected() || activeResult?.kind === "cp4-verification") {
          return;
        }
        const partialArgs =
          isPlainObject(msg.params) && isPlainObject(msg.params.arguments)
            ? msg.params.arguments
            : isPlainObject(msg.params)
            ? msg.params
            : {};
        editorOpen = false;
        activeResult = { kind: "partial-input", partialArgs };
        rootEl.dataset.state = "waiting";
        statusEl.textContent = "Receiving checkpoint input...";
        syncAllRegions();
        return;
      }
      if (msg.method === "ui/notifications/tool-input") {
        if (msg.params !== undefined && !isPlainObject(msg.params)) {
          renderError("Malformed ui/notifications/tool-input notification.");
          return;
        }
        if (isEditorProtected() || activeResult?.kind === "cp4-verification") {
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
        if (isEditorProtected()) {
          statusEl.textContent = cancelReason
            ? "Host reported tool cancellation (" + cancelReason + "); CP1 draft preserved."
            : "Host reported tool cancellation; CP1 draft preserved.";
          return;
        }
        if (activeResult?.kind === "cp4-verification") {
          statusEl.textContent =
            "The host canceled a tool call; the saved verification result is still shown.";
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
    if (reqKey && pendingRequests.has(reqKey)) {
      const activeReq = pendingRequests.get(reqKey)!;
      pendingRequests.delete(reqKey);
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
