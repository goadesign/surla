// Package main implements the Surla views page. The page exchanges the
// one-use link `surla` opened it with for a session held only in this
// script's memory, then reads and acts on the project's changes by calling
// the Surla server at the address it was loaded from. It coordinates the
// stage navigation bar (#surla-checkpoint-nav), the displayed-subject context
// strip (#surla-workspace-context), the two-region layout (#surla-cp1-editor
// and #surla-result-view), the change list, and synchronous React 19
// rendering.

import React from "react";
import { flushSync } from "react-dom";
import { createRoot, Root } from "react-dom/client";
import { ArchitecturePreviewView, IncomingChangeCardView } from "./components/c4-graph";
import { ChangeListView } from "./components/change-list";
import { CP1EditorView } from "./components/cp1-editor";
import { VerificationView } from "./components/cp4-verification";
import { ApproveResultControl } from "./components/cp5-approve";
import { SessionEndedNotice, SignedOutReason, SignedOutView } from "./components/session";
import { Alert, Button, Card } from "./components/shadcn";
import { TraceDrawer, TraceFilter } from "./components/trace-drawer";
import { isDraftDirty, PreservedDraft } from "./context/cp1-draft";
import {
  ArchitecturePreviewResult,
  ChangeCardResult,
  ChangeList,
  ExampleItem,
  OperationName,
  Operations,
  ROUTES,
  TraceList,
  VerificationListResult,
  VerificationStatusResult,
  isPlainObject,
  isServerError,
} from "./types";

// WAIT_RETRY_MS is how long the page waits before asking again after the
// server did not answer its waiting read.
const WAIT_RETRY_MS = 5000;

// Failure describes one request the server refused or never answered.
interface Failure {
  // kind separates a session the server no longer accepts, a refusal or
  // malformed answer, an unreachable server, and a request the page stopped.
  kind: "session" | "server" | "network" | "aborted";
  // status is the HTTP status, or 0 when no answer arrived.
  status: number;
  // message is the server's own error message when it sent one.
  message: string;
}

// Outcome is the page's view of one finished request.
type Outcome<T> = { ok: true; result: T } | ({ ok: false } & Failure);

// PendingCall records one outstanding request so views derive their waiting
// state from the requests actually in flight.
interface PendingCall {
  // op is the operation sent.
  op: OperationName;
  // subject is the change_id or run_id the request is about, or "".
  subject: string;
}

// ActiveEditorState keeps received saved values separate from editable drafts.
interface ActiveEditorState {
  // card is the last saved snapshot received from the server.
  card: ChangeCardResult;
  // priorCard is the immediately prior saved snapshot for this change_id
  // when the editor transitioned between distinct revisions of the same card.
  priorCard: ChangeCardResult | null;
  // incomingCard retains a newer revision that arrived while the editor had
  // unsaved state or an outstanding request, until the user switches to it
  // or a card is mounted directly.
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

// ActiveResultState selects the view displayed in #surla-result-view.
type ActiveResultState =
  | { kind: "changes" }
  | { kind: "cp2-architecture-preview" }
  | { kind: "cp4-verification" }
  | { kind: "cp1-incoming-card" };

// SessionState is the page's session lifecycle: starting while the link is
// exchanged, active with a session, signed out without one, or ended when
// the server stopped accepting the session.
type SessionState = "starting" | "active" | "signed-out" | "ended";

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

// editorDraftFrom starts editable drafts from a saved card's own values.
function editorDraftFrom(card: ChangeCardResult): {
  draftAnswers: Record<string, string>;
  draftExamples: ExampleItem[];
} {
  return {
    draftAnswers: isPlainObject(card.answers) ? { ...card.answers } : {},
    draftExamples: card.examples.map((ex) => ({
      id: ex.id,
      scenario: ex.scenario,
      input_summary: ex.input_summary,
      expected_outcome: ex.expected_outcome,
      verification_kind: ex.verification_kind,
    })),
  };
}

// readLinkFromFragment returns the one-use link carried in the address
// fragment, which a browser never sends to a server.
function readLinkFromFragment(hash: string): string | null {
  const params = new URLSearchParams(hash.startsWith("#") ? hash.slice(1) : hash);
  const link = params.get("link");
  return link !== null && link !== "" ? link : null;
}

// validateVerificationListPayload checks that a VerificationListResult carries
// a non-empty change_id, matches expectedChangeId, and only contains runs
// belonging to that same change_id.
function validateVerificationListPayload(
  payload: VerificationListResult,
  expectedChangeId: string
): string | null {
  if (!payload.change_id || payload.change_id.trim().length === 0) {
    return "The server returned a verification list without a change_id.";
  }
  if (payload.change_id !== expectedChangeId) {
    return (
      "The server returned verification runs for Change Card " +
      payload.change_id +
      " instead of " +
      expectedChangeId +
      "."
    );
  }
  for (let i = 0; i < payload.runs.length; i++) {
    if (payload.runs[i].change_id !== payload.change_id) {
      return (
        "The server returned a verification run for Change Card " +
        payload.runs[i].change_id +
        " inside the list for " +
        payload.change_id +
        "."
      );
    }
  }
  return null;
}

// WorkspaceSubjectDescriptor describes the exact displayed subject of an editor
// or result region without retaining or caching a duplicate copy of session state.
interface WorkspaceSubjectDescriptor {
  // role identifies whether this descriptor describes the Change Card editor,
  // an incoming Change Card banner, or the active result view.
  role: "editor" | "incoming" | "result";
  // label is the human-readable prefix shown in the workspace context strip.
  label: string;
  // stage names the workspace stage being shown.
  stage: string;
  // changeId is set only when the displayed payload itself is bound to a Change Card.
  changeId?: string;
  // requestRevision is set only when the displayed payload itself carries a request_revision.
  requestRevision?: string;
  // detail summarizes secondary state (such as status, design digest, or run ID).
  detail?: string;
}

// describeActiveResultSubject derives the subject of #surla-result-view strictly
// from the displayed payload and selected verification run, never falling
// back to the editor's card.
function describeActiveResultSubject(
  activeResult: ActiveResultState | null,
  previewState: ArchitecturePreviewResult | null,
  displayedRun: VerificationStatusResult | null,
  selectedVerificationChangeId: string | null,
  bothRegionsShown: boolean
): WorkspaceSubjectDescriptor | null {
  if (!activeResult || activeResult.kind === "cp1-incoming-card" || activeResult.kind === "changes") {
    return null;
  }
  const label = bothRegionsShown ? "Displayed view" : "Showing";
  if (activeResult.kind === "cp2-architecture-preview") {
    if (!previewState) {
      return null;
    }
    return {
      role: "result",
      label,
      stage: "Architecture · " + previewState.system_name,
      changeId: previewState.change_id,
      requestRevision: previewState.request_revision,
      detail: "design " + previewState.design_digest.slice(0, 15),
    };
  }
  if (displayedRun) {
    return {
      role: "result",
      label,
      stage: "Proof",
      changeId: displayedRun.change_id,
      requestRevision: displayedRun.request_revision,
      detail:
        "run " +
        displayedRun.run_id.slice(0, 8) +
        " · commit " +
        displayedRun.candidate_commit.slice(0, 7),
    };
  }
  return {
    role: "result",
    label,
    stage: "Proof",
    changeId: selectedVerificationChangeId || undefined,
    detail: "saved verification runs",
  };
}

// bootstrapViewsPage mounts the views page, obtains its session, and starts
// reading the project's changes.
export function bootstrapViewsPage() {
  const rawRootEl = document.getElementById("surla-checkpoint-root");
  const rawStatusEl = document.getElementById("surla-checkpoint-status");
  const rawViewEl = document.getElementById("surla-checkpoint-view");
  const rawNavEl = document.getElementById("surla-checkpoint-nav");
  if (!rawRootEl || !rawStatusEl || !rawViewEl || !rawNavEl) {
    return;
  }
  const rootEl: HTMLElement = rawRootEl;
  const statusEl: HTMLElement = rawStatusEl;
  const viewEl: HTMLElement = rawViewEl;
  const navEl: HTMLElement = rawNavEl;

  const noticeEl = document.createElement("div");
  noticeEl.setAttribute("id", "surla-session-notice");
  statusEl.after(noticeEl);
  const editorRegionEl = document.createElement("div");
  editorRegionEl.setAttribute("id", "surla-cp1-editor");
  editorRegionEl.setAttribute("tabindex", "-1");
  editorRegionEl.setAttribute("aria-label", "Change decisions");
  const resultRegionEl = document.createElement("div");
  resultRegionEl.setAttribute("id", "surla-result-view");
  viewEl.appendChild(editorRegionEl);
  viewEl.appendChild(resultRegionEl);
  const traceRegionEl = document.createElement("aside");
  traceRegionEl.setAttribute("aria-label", "Operation diagnostics");
  viewEl.before(traceRegionEl);

  const navRoot: Root = createRoot(navEl);
  const noticeRoot: Root = createRoot(noticeEl);
  const editorRoot: Root = createRoot(editorRegionEl);
  const resultRoot: Root = createRoot(resultRegionEl);
  const traceRoot: Root = createRoot(traceRegionEl);

  // session is the credential the server issued for this page. It lives only
  // in this closure: never in browser storage or a cookie, so a reload or a
  // closed tab ends it.
  let session: string | null = null;
  let sessionState: SessionState = "starting";
  let signedOutReason: SignedOutReason = "no-link";
  let signedOutMessage = "";
  const pendingCalls = new Set<PendingCall>();

  let changeList: ChangeList | null = null;
  let listError: string | null = null;
  let listRequest: Promise<void> | null = null;
  let waitController: AbortController | null = null;
  let waitLoopRunning = false;
  let waitTroubleShown = false;
  // cardRefresh is the outstanding background read of the open card;
  // cardRefreshAgain asks for one more read after it because the list
  // changed again meanwhile.
  let cardRefresh: Promise<void> | null = null;
  let cardRefreshAgain = false;
  // cardRefreshAfterEditor names the open card to read again once the
  // page's own Change Card request finishes. Reading it during that request
  // could return the page's own save and announce it as another caller's.
  let cardRefreshAfterEditor: string | null = null;

  let traceOpen = false;
  let traceFilter: TraceFilter = "error";
  let tracePage: TraceList | null = null;
  let traceError: string | null = null;

  // editorBusy is true while a Change Card request (confirm, approve or an
  // explicit card read) owns the editor; edits and other card requests wait.
  let editorBusy = false;
  let activeEditor: ActiveEditorState | null = null;
  // editorOpen tracks whether #surla-cp1-editor is mounted. Another stage
  // keeps activeEditor in memory while the editor is not shown.
  let editorOpen = false;
  let previewState: ArchitecturePreviewResult | null = null;
  const previewErrors = new Map<string, string>();
  const runsById = new Map<string, VerificationStatusResult>();
  const lastLoadedListByChangeId = new Map<string, string[]>();
  let selectedVerificationChangeId: string | null = null;
  let selectedRunId: string | null = null;
  const selectionTouchedDuringList = new Map<string, boolean>();
  const runRequestErrors = new Map<string, string>();
  const listRequestErrors = new Map<string, string>();
  const cardLoadErrors = new Map<string, string>();
  const approveErrors = new Map<string, string>();
  // approvalUnavailable is the server's reason this project cannot record
  // approvals, learned from its answer to an approval request.
  let approvalUnavailable: string | null = null;
  // lastOpenAttempt is the change the user last asked to open, whose read
  // failure the change list shows.
  let lastOpenAttempt: string | null = null;
  let activeResult: ActiveResultState | null = null;

  // send encodes one request with the generated route, sends it to the
  // page's own address with the session, and validates the answer with the
  // generated result validator. It is the page's only network call.
  async function send<K extends OperationName>(
    op: K,
    request: Operations[K]["request"],
    signal?: AbortSignal
  ): Promise<Outcome<Operations[K]["result"]>> {
    const route = ROUTES[op];
    const encoded = route.encode(request);
    const headers: Record<string, string> = { Accept: "application/json" };
    if (encoded.body !== undefined) {
      headers["Content-Type"] = "application/json";
    }
    if (session !== null) {
      headers.Authorization = "Bearer " + session;
    }
    let status = 0;
    let text = "";
    try {
      const response = await fetch(encoded.path, {
        method: route.method,
        headers,
        body: encoded.body,
        signal,
        cache: "no-store",
        credentials: "omit",
        redirect: "error",
      });
      status = response.status;
      text = await response.text();
    } catch {
      if (signal?.aborted) {
        return { ok: false, kind: "aborted", status: 0, message: "The request was stopped." };
      }
      return {
        ok: false,
        kind: "network",
        status: 0,
        message: "The Surla server did not answer. Check that surla is still running.",
      };
    }
    let parsed: unknown = undefined;
    try {
      parsed = text === "" ? undefined : JSON.parse(text);
    } catch {
      parsed = undefined;
    }
    if (status === route.success) {
      if (route.decode(parsed)) {
        return { ok: true, result: parsed };
      }
      return {
        ok: false,
        kind: "server",
        status,
        message: "The server's answer to " + op + " did not match its declared result.",
      };
    }
    return {
      ok: false,
      kind: status === 401 ? "session" : "server",
      status,
      message: isServerError(parsed)
        ? parsed.message
        : "The server refused the request (HTTP " + status + ").",
    };
  }

  // hasPending reports whether a request of op about subject is in flight.
  function hasPending(op: OperationName, subject: string): boolean {
    for (const call of pendingCalls) {
      if (call.op === op && call.subject === subject) {
        return true;
      }
    }
    return false;
  }

  // callServer sends one request whose waiting state views derive from
  // pendingCalls, then hands its outcome to exactly one callback and renders.
  function callServer<K extends OperationName>(
    op: K,
    request: Operations[K]["request"],
    subject: string,
    onSuccess: (result: Operations[K]["result"]) => void,
    onError: (message: string, failure: Failure) => void
  ) {
    if (session === null) {
      onError(sessionEndedMessage(), {
        kind: "session",
        status: 0,
        message: sessionEndedMessage(),
      });
      syncAllRegions();
      return;
    }
    const call: PendingCall = { op, subject };
    pendingCalls.add(call);
    syncAllRegions();
    void send(op, request).then((outcome) => {
      pendingCalls.delete(call);
      if (outcome.ok) {
        onSuccess(outcome.result);
      } else {
        if (outcome.kind === "session") {
          endSession();
        }
        onError(outcome.message, outcome);
      }
      syncAllRegions();
    });
  }

  // callEditorServer owns the editable form for one Change Card request;
  // only these requests disable the form.
  function callEditorServer<K extends OperationName>(
    op: K,
    request: Operations[K]["request"],
    subject: string,
    onSuccess: (result: Operations[K]["result"]) => void,
    onError: (message: string, failure: Failure) => void
  ) {
    editorBusy = true;
    // readDeferredCard runs the background read the request held back.
    const readDeferredCard = () => {
      const deferred = cardRefreshAfterEditor;
      cardRefreshAfterEditor = null;
      if (deferred !== null) {
        refreshOpenCard(deferred);
      }
    };
    callServer(
      op,
      request,
      subject,
      (result) => {
        editorBusy = false;
        onSuccess(result);
        readDeferredCard();
      },
      (message, failure) => {
        editorBusy = false;
        onError(message, failure);
        readDeferredCard();
      }
    );
  }

  // sessionEndedMessage tells the user what to do once the session is gone.
  function sessionEndedMessage(): string {
    return "This page's Surla session has ended. Run surla in your project folder to open a new page.";
  }

  // start exchanges the link from the address fragment for a session, removes
  // the link from the address, and reads the project's changes.
  async function start() {
    const link = readLinkFromFragment(window.location.hash);
    if (window.location.hash !== "") {
      window.history.replaceState(null, "", window.location.pathname + window.location.search);
    }
    if (link === null) {
      signOut("no-link", "");
      return;
    }
    statusEl.textContent = "Opening your Surla session...";
    const outcome = await send("exchange_browser_link", { link });
    if (!outcome.ok) {
      signOut(outcome.status === 422 ? "link-used" : "link-failed", outcome.message);
      return;
    }
    session = outcome.result.session;
    sessionState = "active";
    rootEl.dataset.session = "active";
    activeResult = { kind: "changes" };
    rootEl.dataset.state = "changes";
    await loadChanges();
  }

  // signOut shows how to open the page with `surla`; the page then sends no
  // further requests.
  function signOut(reason: SignedOutReason, message: string) {
    sessionState = "signed-out";
    signedOutReason = reason;
    signedOutMessage = message;
    rootEl.dataset.session = "none";
    rootEl.dataset.state = reason === "link-used" ? "link-used" : "signed-out";
    statusEl.textContent =
      reason === "link-used"
        ? "This link was already used. Run surla in your project folder to open a new page."
        : "Run surla in your project folder to open this page.";
    syncAllRegions();
  }

  // endSession forgets a session the server no longer accepts, stops the
  // waiting read, and keeps every view and draft on the page.
  function endSession() {
    if (sessionState !== "active") {
      return;
    }
    session = null;
    sessionState = "ended";
    rootEl.dataset.session = "ended";
    if (waitController !== null) {
      waitController.abort();
    }
    statusEl.textContent = sessionEndedMessage();
    syncAllRegions();
  }

  // loadChanges reads the change list once at a time, then applies what
  // changed to the open change and its runs.
  function loadChanges(): Promise<void> {
    if (listRequest !== null) {
      return listRequest;
    }
    const pending = (async () => {
      const call: PendingCall = { op: "list_changes", subject: "" };
      pendingCalls.add(call);
      syncAllRegions();
      const outcome = await send("list_changes", {});
      pendingCalls.delete(call);
      if (!outcome.ok) {
        if (outcome.kind === "session") {
          endSession();
        }
        listError = outcome.message;
        if (activeResult?.kind === "changes") {
          statusEl.textContent = "Could not read this project's changes.";
        }
        syncAllRegions();
        return;
      }
      const previous = changeList;
      changeList = outcome.result;
      listError = null;
      applyListChanges(previous, outcome.result);
      syncAllRegions();
      startWaiting();
    })();
    listRequest = pending;
    void pending.finally(() => {
      listRequest = null;
    });
    return pending;
  }

  // applyListChanges opens the only change on the first read and otherwise
  // re-reads the open card or its runs only when their list entry changed.
  function applyListChanges(previous: ChangeList | null, next: ChangeList) {
    if (previous === null) {
      if (next.changes.length === 1 && activeEditor === null) {
        openChange(next.changes[0].change_id);
        return;
      }
      if (activeResult?.kind === "changes") {
        statusEl.textContent =
          next.changes.length === 0
            ? "This project has no changes yet."
            : "Showing this project's " + next.changes.length + " changes.";
      }
      return;
    }
    if (activeEditor === null) {
      return;
    }
    const openId = activeEditor.card.change_id;
    const before = previous.changes.find((item) => item.change_id === openId);
    const after = next.changes.find((item) => item.change_id === openId);
    if (!after) {
      return;
    }
    if (
      !before ||
      before.request_revision !== after.request_revision ||
      before.saved_at !== after.saved_at
    ) {
      refreshOpenCard(openId);
    }
    const runBefore = before?.latest_verification;
    const runAfter = after.latest_verification;
    if (
      lastLoadedListByChangeId.has(openId) &&
      (runBefore?.run_id !== runAfter?.run_id || runBefore?.state !== runAfter?.state)
    ) {
      refreshVerificationsInBackground(openId);
    }
  }

  // startWaiting keeps one waiting read open while the page has a session
  // and a list version and the tab is visible.
  function startWaiting() {
    if (waitLoopRunning || session === null || changeList === null || document.hidden) {
      return;
    }
    waitLoopRunning = true;
    void waitLoop().finally(() => {
      waitLoopRunning = false;
    });
  }

  // waitLoop asks the server to answer when the list differs from the last
  // one read, and reads the list again only then. Hiding the tab stops the
  // open request; showing it again resumes from the same version.
  async function waitLoop() {
    while (session !== null && changeList !== null && !document.hidden) {
      const controller = new AbortController();
      waitController = controller;
      const since = changeList.version;
      const outcome = await send("wait_for_changes", { since }, controller.signal);
      waitController = null;
      if (outcome.ok) {
        if (waitTroubleShown) {
          waitTroubleShown = false;
          statusEl.textContent = "Connected to the Surla server again.";
        }
        if (changeList !== null && outcome.result.version !== changeList.version) {
          await loadChanges();
        }
        continue;
      }
      if (outcome.kind === "aborted") {
        continue;
      }
      if (outcome.kind === "session") {
        endSession();
        return;
      }
      if (!waitTroubleShown) {
        waitTroubleShown = true;
        statusEl.textContent =
          "Lost contact with the Surla server; trying again. Your edits stay on this page. " +
          outcome.message;
      }
      await new Promise<void>((resume) => window.setTimeout(resume, WAIT_RETRY_MS));
    }
  }

  // refreshOpenCard reads the open card again because the list says it
  // changed, without taking the editor's request slot. While the page's own
  // Change Card request is in flight, the read waits for it to finish.
  function refreshOpenCard(changeId: string) {
    if (editorBusy) {
      cardRefreshAfterEditor = changeId;
      return;
    }
    if (cardRefresh !== null) {
      cardRefreshAgain = true;
      return;
    }
    cardRefresh = (async () => {
      do {
        cardRefreshAgain = false;
        const outcome = await send("get_change", { change_id: changeId });
        if (!outcome.ok) {
          if (outcome.kind === "session") {
            endSession();
          }
          return;
        }
        receiveLatestCard(outcome.result);
      } while (cardRefreshAgain && session !== null);
    })();
    void cardRefresh.finally(() => {
      cardRefresh = null;
    });
  }

  // receiveLatestCard applies a background read of the open card. The same
  // revision updates saved metadata such as approval and keeps drafts; a new
  // revision is held as an incoming card while the user has unsaved edits or
  // a request in flight, and replaces a clean editor otherwise. It never
  // moves focus and announces a new revision once.
  function receiveLatestCard(latest: ChangeCardResult) {
    if (!activeEditor || activeEditor.card.change_id !== latest.change_id) {
      return;
    }
    const ed = activeEditor;
    if (latest.request_revision === ed.card.request_revision) {
      activeEditor = { ...ed, card: latest };
      syncAllRegions();
      return;
    }
    if (ed.incomingCard && ed.incomingCard.request_revision === latest.request_revision) {
      activeEditor = { ...ed, incomingCard: latest };
      syncAllRegions();
      return;
    }
    if (editorBusy || hasUnsavedEditorState()) {
      activeEditor = { ...ed, incomingCard: latest };
      if (editorOpen && (!activeResult || activeResult.kind === "cp1-incoming-card")) {
        activeResult = { kind: "cp1-incoming-card" };
      }
      statusEl.textContent =
        "A newer revision of this Change Card was saved. Your unsaved edits are kept; use Switch to Incoming Card to review it.";
      syncAllRegions();
      return;
    }
    activeEditor = {
      card: latest,
      priorCard: nextPriorCard(ed, latest),
      incomingCard: null,
      ...editorDraftFrom(latest),
      preservedDraft: null,
      formError: null,
    };
    if (activeResult?.kind === "cp1-incoming-card") {
      activeResult = null;
    }
    statusEl.textContent = "A newer revision of this Change Card was saved and is now shown.";
    syncAllRegions();
  }

  // refreshVerificationsInBackground reads the open change's runs again
  // because the list says its latest run changed, keeping the user's view
  // and selected run.
  function refreshVerificationsInBackground(changeId: string) {
    if (hasPending("list_verifications", changeId)) {
      return;
    }
    callServer(
      "list_verifications",
      { change_id: changeId },
      changeId,
      (payload) => {
        if (validateVerificationListPayload(payload, changeId) !== null) {
          return;
        }
        for (let i = 0; i < payload.runs.length; i++) {
          upsertRunSnapshot(payload.runs[i]);
        }
        const loadedIds = payload.runs.map((r) => r.run_id);
        lastLoadedListByChangeId.set(changeId, loadedIds);
        listRequestErrors.delete(changeId);
        if (selectedVerificationChangeId === changeId && selectedRunId === null && loadedIds.length > 0) {
          selectedRunId = loadedIds[0];
        }
      },
      (message) => {
        listRequestErrors.set(changeId, message);
      }
    );
  }

  // resolveDisplayedVerificationRun returns the selected VerificationStatusResult
  // snapshot when one is currently selected in runsById.
  function resolveDisplayedVerificationRun(): VerificationStatusResult | null {
    return selectedRunId !== null ? runsById.get(selectedRunId) ?? null : null;
  }

  // isViewingChangeResult reports whether the active result view currently
  // displays a CP2 preview or CP4 verification view for changeId.
  function isViewingChangeResult(changeId: string): boolean {
    if (activeResult?.kind === "cp2-architecture-preview") {
      return activeEditor !== null && activeEditor.card.change_id === changeId;
    }
    if (activeResult?.kind === "cp4-verification") {
      const displayedRun = resolveDisplayedVerificationRun();
      const effectiveId = displayedRun
        ? displayedRun.change_id
        : selectedVerificationChangeId || "";
      return effectiveId === changeId;
    }
    return activeResult?.kind === "changes";
  }

  // hasUnsavedEdits reports typed edits the page would lose on unload.
  function hasUnsavedEdits(): boolean {
    if (!activeEditor) {
      return false;
    }
    return (
      isDraftDirty(activeEditor.card, activeEditor.draftAnswers, activeEditor.draftExamples) ||
      activeEditor.preservedDraft !== null
    );
  }

  // hasUnsavedEditorState identifies work that a different stage must leave
  // available for the user to recover or continue editing.
  function hasUnsavedEditorState(): boolean {
    return hasUnsavedEdits() || (activeEditor !== null && activeEditor.formError !== null);
  }

  // shouldRetainEditor keeps drafts and the destination of a pending editor
  // response visible when another stage is shown.
  function shouldRetainEditor(): boolean {
    return activeEditor !== null && ((editorOpen && editorBusy) || hasUnsavedEditorState());
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
    delete resultRegionEl.dataset.runId;
  }

  // showChanges switches to the change list, reading it when none is loaded.
  function showChanges() {
    editorOpen = shouldRetainEditor();
    activeResult = { kind: "changes" };
    rootEl.dataset.state = "changes";
    statusEl.textContent =
      changeList === null
        ? "Reading this project's changes..."
        : changeList.changes.length === 0
        ? "This project has no changes yet."
        : "Showing this project's " + changeList.changes.length + " changes.";
    if (changeList === null) {
      void loadChanges();
    }
    syncAllRegions();
  }

  // openArchitecture shows the open change's architecture stage and asks the
  // server for the architecture of the shown revision when none is loaded.
  function openArchitecture() {
    if (!activeEditor) {
      return;
    }
    const card = activeEditor.card;
    editorOpen = shouldRetainEditor();
    activeResult = { kind: "cp2-architecture-preview" };
    rootEl.dataset.state = "cp2-architecture-preview";
    if (
      previewState === null ||
      previewState.change_id !== card.change_id ||
      previewState.request_revision !== card.request_revision
    ) {
      requestArchitecture(card);
    } else {
      statusEl.textContent = "Showing architecture preview for " + previewState.system_name + ".";
    }
    syncAllRegions();
  }

  // requestArchitecture asks the server to evaluate the bound design for the
  // card's exact revision.
  function requestArchitecture(card: ChangeCardResult) {
    if (hasPending("preview_architecture", card.change_id)) {
      return;
    }
    previewErrors.delete(card.change_id);
    statusEl.textContent = "Evaluating the design for Change Card " + card.change_id + "...";
    callServer(
      "preview_architecture",
      { change_id: card.change_id, request_revision: card.request_revision },
      card.change_id,
      (preview) => {
        if (preview.change_id !== card.change_id) {
          previewErrors.set(
            card.change_id,
            "The server returned the architecture of Change Card " + preview.change_id + "."
          );
          return;
        }
        previewState = preview;
        if (activeResult?.kind === "cp2-architecture-preview") {
          statusEl.textContent = "Showing architecture preview for " + preview.system_name + ".";
        }
      },
      (message) => {
        previewErrors.set(card.change_id, message);
        if (activeResult?.kind === "cp2-architecture-preview") {
          statusEl.textContent = "Could not preview the architecture.";
        }
      }
    );
  }

  // openProof shows the open change's saved verification runs.
  function openProof() {
    const changeId = activeEditor ? activeEditor.card.change_id : selectedVerificationChangeId;
    if (!changeId) {
      return;
    }
    if (selectedVerificationChangeId === changeId && lastLoadedListByChangeId.has(changeId)) {
      editorOpen = shouldRetainEditor();
      activeResult = { kind: "cp4-verification" };
      rootEl.dataset.state = "cp4-verification";
      statusEl.textContent =
        selectedRunId !== null
          ? "Showing the saved verification result."
          : "Showing saved verification runs for Change Card " + changeId + ".";
      syncAllRegions();
      return;
    }
    handleListVerifications(changeId);
  }

  // syncNavRegion renders the stage navigation bar for an active session
  // along with the displayed-subject context strip (#surla-workspace-context).
  function syncNavRegion() {
    if (sessionState === "starting" || sessionState === "signed-out") {
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
    tabs.push({
      id: "surla-nav-changes",
      label: "Changes",
      active: activeResult?.kind === "changes",
      onClick: showChanges,
    });
    const cp1Active =
      editorOpen && (!activeResult || activeResult.kind === "cp1-incoming-card");
    if (activeEditor !== null) {
      tabs.push({
        id: "surla-nav-cp1",
        label: "Decisions & Approval",
        active: cp1Active,
        onClick: handleReturnToChangeCard,
      });
      tabs.push({
        id: "surla-nav-cp2",
        label: "Architecture",
        active: activeResult?.kind === "cp2-architecture-preview",
        onClick: openArchitecture,
      });
    }
    if (activeEditor !== null || selectedVerificationChangeId !== null) {
      tabs.push({
        id: "surla-nav-cp4",
        label: "Proof",
        active: activeResult?.kind === "cp4-verification",
        onClick: openProof,
      });
    }

    const bothRegionsShown =
      activeEditor !== null &&
      editorOpen &&
      activeResult !== null &&
      activeResult.kind !== "cp1-incoming-card";

    const subjects: WorkspaceSubjectDescriptor[] = [];
    if (activeEditor !== null && editorOpen) {
      subjects.push({
        role: "editor",
        label: bothRegionsShown ? "Protected editor" : "Showing",
        stage: "Decisions & Approval",
        changeId: activeEditor.card.change_id,
        requestRevision: activeEditor.card.request_revision,
        detail:
          activeEditor.card.status === "confirmed" ? "confirmed" : "needs confirmation",
      });
      if (activeResult?.kind === "cp1-incoming-card" && activeEditor.incomingCard) {
        subjects.push({
          role: "incoming",
          label: "Incoming card",
          stage: "Updated Change Card",
          changeId: activeEditor.incomingCard.change_id,
          requestRevision: activeEditor.incomingCard.request_revision,
          detail:
            activeEditor.incomingCard.status === "confirmed"
              ? "confirmed"
              : "needs confirmation",
        });
      }
    }
    const shownPreview =
      previewState !== null &&
      activeEditor !== null &&
      previewState.change_id === activeEditor.card.change_id
        ? previewState
        : null;
    const resultSubject = describeActiveResultSubject(
      activeResult,
      shownPreview,
      resolveDisplayedVerificationRun(),
      selectedVerificationChangeId,
      bothRegionsShown
    );
    if (resultSubject !== null) {
      subjects.push(resultSubject);
    }

    navEl.hidden = false;
    flushSync(() => {
      navRoot.render(
        <>
          <div className="surla-nav-tabs">
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
          </div>
          {subjects.length >= 1 ? (
            <div
              id="surla-workspace-context"
              className="surla-workspace-context"
              role="region"
              aria-label="Displayed workspace subject"
            >
              {subjects.map((subj) => (
                <div
                  key={subj.role}
                  id={"surla-context-" + subj.role}
                  className="surla-subject-item"
                  data-subject-role={subj.role}
                  data-change-id={subj.changeId}
                  data-request-revision={subj.requestRevision}
                >
                  <span className="surla-subject-label">{subj.label}</span>{" "}
                  <strong>{subj.stage}</strong>
                  {subj.changeId ? (
                    <>
                      {" · "}
                      <code>{subj.changeId}</code>
                    </>
                  ) : null}
                  {subj.requestRevision ? (
                    <>
                      {" · "}
                      <code>{subj.requestRevision.slice(0, 15)}</code>
                    </>
                  ) : null}
                  {subj.detail ? (
                    <>
                      {" · "}
                      <span>{subj.detail}</span>
                    </>
                  ) : null}
                </div>
              ))}
            </div>
          ) : null}
        </>
      );
    });
  }

  // syncNoticeRegion shows the session-ended notice with the unsaved edits
  // the user can still copy.
  function syncNoticeRegion() {
    flushSync(() => {
      noticeRoot.render(
        sessionState === "ended" ? (
          <SessionEndedNotice
            unsavedLines={
              activeEditor ? snapshotUnsavedDraft(activeEditor)?.lines ?? [] : []
            }
          />
        ) : null
      );
    });
  }

  // syncEditorRegion renders saved values and drafts together when editorOpen
  // is true, with editing disabled while a Change Card request is outstanding.
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
          rpcInFlight={editorBusy}
          loadingVerifications={hasPending("list_verifications", ed.card.change_id)}
          cp4Open={activeResult?.kind === "cp4-verification"}
          preservedDraft={ed.preservedDraft}
          formError={ed.formError}
          onSetAnswer={(ambId, opt) => {
            if (!activeEditor || editorBusy) {
              return;
            }
            activeEditor = {
              ...activeEditor,
              draftAnswers: { ...activeEditor.draftAnswers, [ambId]: opt },
            };
            syncAllRegions();
          }}
          onSetExampleField={(exId, field, value) => {
            if (!activeEditor || editorBusy) {
              return;
            }
            activeEditor = {
              ...activeEditor,
              draftExamples: activeEditor.draftExamples.map((item) =>
                item.id === exId ? { ...item, [field]: value } : item
              ),
            };
            syncAllRegions();
          }}
          onSubmitConfirm={handleConfirmSubmit}
          onDiscardDraft={handleDiscardDraft}
          onLoadLatestCard={handleLoadLatestCard}
          onLoadVerifications={() => handleListVerifications(ed.card.change_id)}
        />
      );
    });
  }

  // syncResultRegion replaces only the stage view beside the editor.
  function syncResultRegion() {
    clearResultDataset();
    if (sessionState === "signed-out") {
      resultRegionEl.dataset.state = "signed-out";
      flushSync(() => {
        resultRoot.render(<SignedOutView reason={signedOutReason} message={signedOutMessage} />);
      });
      return;
    }
    if (!activeResult) {
      flushSync(() => {
        resultRoot.render(null);
      });
      return;
    }
    const res = activeResult;
    resultRegionEl.dataset.state = res.kind;
    if (res.kind === "changes") {
      flushSync(() => {
        resultRoot.render(
          <ChangeListView
            list={changeList}
            loading={hasPending("list_changes", "")}
            error={listError}
            openError={lastOpenAttempt !== null ? cardLoadErrors.get(lastOpenAttempt) ?? null : null}
            openChangeId={activeEditor ? activeEditor.card.change_id : null}
            busy={editorBusy}
            onOpen={openChange}
            onRetry={() => void loadChanges()}
          />
        );
      });
      return;
    }
    if (res.kind === "cp2-architecture-preview") {
      const card = activeEditor ? activeEditor.card : null;
      const changeId = card ? card.change_id : "";
      const preview =
        previewState !== null && previewState.change_id === changeId ? previewState : null;
      const loading = hasPending("preview_architecture", changeId);
      const error = previewErrors.get(changeId) ?? null;
      if (preview) {
        resultRegionEl.dataset.changeId = preview.change_id;
        resultRegionEl.dataset.requestRevision = preview.request_revision;
        resultRegionEl.dataset.designDigest = preview.design_digest;
      }
      flushSync(() => {
        resultRoot.render(
          <>
            {preview ? (
              <ArchitecturePreviewView
                preview={preview}
                loadedCard={card}
                loadingCard={editorBusy}
                cardLoadError={cardLoadErrors.get(preview.change_id) ?? null}
                onOpenChangeCard={() => openChange(preview.change_id)}
              />
            ) : null}
            {loading ? (
              <Card id="surla-cp2-loading" className="surla-card" role="status" aria-busy="true">
                Evaluating the design in Surla's sandbox. This can take a little while.
              </Card>
            ) : null}
            {error !== null ? (
              <Alert id="surla-cp2-error" variant="destructive" role="alert">
                <strong>Could not preview the architecture. </strong>
                {error}
              </Alert>
            ) : null}
            {card !== null ? (
              <div className="surla-field-row">
                <Button
                  id="surla-cp2-refresh"
                  disabled={loading}
                  onClick={() => requestArchitecture(card)}
                >
                  {preview ? "Evaluate the design again" : "Evaluate the design"}
                </Button>
              </div>
            ) : null}
          </>
        );
      });
      return;
    }
    if (res.kind === "cp4-verification") {
      const changeId = selectedVerificationChangeId || "";
      const displayedRun = resolveDisplayedVerificationRun();
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
      const loadedCard = activeEditor ? activeEditor.card : null;
      flushSync(() => {
        resultRoot.render(
          <>
            <VerificationView
              changeId={effectiveChangeId}
              run={displayedRun}
              listedRuns={listedRuns}
              loadedCard={loadedCard}
              checking={displayedRun ? hasPending("get_verification", displayedRun.run_id) : false}
              loadingList={hasPending("list_verifications", effectiveChangeId)}
              loadingCard={editorBusy}
              requestError={displayedRun ? runRequestErrors.get(displayedRun.run_id) ?? null : null}
              listError={listRequestErrors.get(effectiveChangeId) ?? null}
              cardLoadError={
                effectiveChangeId ? cardLoadErrors.get(effectiveChangeId) ?? null : null
              }
              onCheckStatus={handleCheckVerificationStatus}
              onLoadVerifications={() => handleListVerifications(effectiveChangeId)}
              onSelectRun={handleSelectVerificationRun}
              onOpenChangeCard={() => openChange(effectiveChangeId)}
              onReturnToChangeCard={handleReturnToChangeCard}
            />
            {displayedRun ? (
              <ApproveResultControl
                run={displayedRun}
                card={
                  loadedCard !== null && loadedCard.change_id === displayedRun.change_id
                    ? loadedCard
                    : null
                }
                busy={editorBusy}
                unavailable={approvalUnavailable}
                error={approveErrors.get(displayedRun.run_id) ?? null}
                onApprove={() => handleApprove(displayedRun)}
              />
            ) : null}
          </>
        );
      });
      return;
    }
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
            if (editorBusy) {
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
  }

  // syncAllRegions synchronizes the notice, editor, result, nav, and trace regions.
  function syncAllRegions() {
    viewEl.classList.toggle(
      "surla-split-workspace",
      Boolean(activeEditor !== null && editorOpen && activeResult !== null)
    );
    syncNoticeRegion();
    syncEditorRegion();
    syncResultRegion();
    syncNavRegion();
    syncTraceRegion();
  }

  // loadTracePage reads one diagnostics page without taking the editor's
  // request slot. Filtering and paging are serialized by their own pending call.
  function loadTracePage(cursor?: string) {
    if (hasPending("list_traces", "")) return;
    traceError = null;
    callServer(
      "list_traces",
      cursor ? { status: traceFilter, limit: 20, cursor } : { status: traceFilter, limit: 20 },
      "",
      (page) => {
        tracePage = page;
      },
      (message) => {
        traceError = message;
      }
    );
  }

  // syncTraceRegion updates only diagnostics; the CP1 editor stays mounted.
  function syncTraceRegion() {
    if (sessionState === "starting" || sessionState === "signed-out") {
      flushSync(() => traceRoot.render(null));
      return;
    }
    flushSync(() =>
      traceRoot.render(
        <TraceDrawer
          open={traceOpen}
          filter={traceFilter}
          page={tracePage}
          error={traceError}
          busy={hasPending("list_traces", "")}
          onToggle={() => {
            traceOpen = !traceOpen;
            if (traceOpen && tracePage === null) loadTracePage();
            syncAllRegions();
          }}
          onFilter={(filter) => {
            traceFilter = filter;
            tracePage = null;
            loadTracePage();
          }}
          onRefresh={() => loadTracePage()}
          onOlder={() => loadTracePage(tracePage?.next_cursor)}
        />
      )
    );
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

  // handleSelectVerificationRun switches the displayed run inside CP4.
  function handleSelectVerificationRun(runId: string) {
    const target = runsById.get(runId);
    if (!target) {
      return;
    }
    if (hasPending("list_verifications", target.change_id)) {
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
  // new verification. A late answer belongs only to that run, even if a
  // different run is displayed by then.
  function handleCheckVerificationStatus() {
    if (
      activeResult?.kind !== "cp4-verification" ||
      selectedRunId === null ||
      !runsById.has(selectedRunId) ||
      hasPending("get_verification", selectedRunId)
    ) {
      return;
    }
    const runID = selectedRunId;
    runRequestErrors.delete(runID);
    callServer(
      "get_verification",
      { run_id: runID },
      runID,
      (run) => {
        if (run.run_id !== runID) {
          if (activeResult?.kind === "cp4-verification" && selectedRunId === runID) {
            runRequestErrors.set(runID, "The server returned a different verification run.");
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
      }
    );
  }

  // handleListVerifications loads the saved verification runs for requestedChangeId,
  // enforcing at most one in-flight list_verifications request per change_id.
  function handleListVerifications(requestedChangeId: string) {
    if (!requestedChangeId || hasPending("list_verifications", requestedChangeId)) {
      return;
    }
    selectionTouchedDuringList.set(requestedChangeId, false);
    listRequestErrors.delete(requestedChangeId);
    if (!shouldRetainEditor()) {
      editorOpen = false;
    }
    const nextRunId =
      selectedRunId !== null &&
      runsById.get(selectedRunId)?.change_id !== requestedChangeId
        ? (lastLoadedListByChangeId.get(requestedChangeId)?.[0] ?? null)
        : selectedRunId;
    selectedVerificationChangeId = requestedChangeId;
    selectedRunId = nextRunId;
    activeResult = { kind: "cp4-verification" };
    rootEl.dataset.state = "cp4-verification";
    statusEl.textContent = "Loading saved verification runs...";

    callServer(
      "list_verifications",
      { change_id: requestedChangeId },
      requestedChangeId,
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
            // Preserve the run explicitly selected during the list read.
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
      }
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

  // openChange shows requestedChangeId, reading it with get_change when it is
  // not the card already open. A reply that arrives after the user moved to
  // another view, or that names another card, never replaces what is shown.
  function openChange(requestedChangeId: string) {
    if (!requestedChangeId || editorBusy) {
      return;
    }
    if (activeEditor && activeEditor.card.change_id === requestedChangeId) {
      handleReturnToChangeCard();
      return;
    }
    const initialActiveResult = activeResult;
    lastOpenAttempt = requestedChangeId;
    cardLoadErrors.delete(requestedChangeId);
    statusEl.textContent = "Loading Change Card " + requestedChangeId + "...";
    callEditorServer(
      "get_change",
      { change_id: requestedChangeId },
      requestedChangeId,
      (loadedCard) => {
        if (loadedCard.change_id !== requestedChangeId) {
          cardLoadErrors.set(
            requestedChangeId,
            "The server returned Change Card " +
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
        if (
          initialActiveResult !== null &&
          (activeResult !== initialActiveResult || !isViewingChangeResult(requestedChangeId))
        ) {
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

  // focusResult moves keyboard focus to the element that shows an action's
  // result, after the regions rendered it.
  function focusResult(selector: string) {
    const target = document.querySelector(selector);
    if (target instanceof HTMLElement) {
      target.focus();
    }
  }

  // handleConfirmSubmit sends complete answers and examples for the displayed
  // saved revision after checking that the user filled every required value.
  function handleConfirmSubmit() {
    if (!activeEditor || editorBusy) {
      return;
    }
    const ambiguities = Array.isArray(activeEditor.card.ambiguities)
      ? activeEditor.card.ambiguities
      : [];
    for (let i = 0; i < ambiguities.length; i++) {
      const ambId = ambiguities[i].id;
      if (!Object.prototype.hasOwnProperty.call(activeEditor.draftAnswers, ambId) || !activeEditor.draftAnswers[ambId]) {
        showFormError("Select an option for ambiguity " + ambId + " before confirming.");
        focusResult("#surla-confirm-error");
        return;
      }
    }
    for (let j = 0; j < activeEditor.draftExamples.length; j++) {
      const item = activeEditor.draftExamples[j];
      if (!item.scenario.trim() || !item.input_summary.trim() || !item.expected_outcome.trim()) {
        showFormError(
          "Example " + item.id + " requires non-empty scenario, input_summary, and expected_outcome."
        );
        focusResult("#surla-confirm-error");
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

    callEditorServer(
      "confirm_intent",
      {
        change_id: submittedCard.change_id,
        request_revision: submittedCard.request_revision,
        answers: submittedAnswers,
        examples: submittedExamples,
      },
      submittedCard.change_id,
      (updatedCard) => {
        mountChangeCard(updatedCard, null, nextPriorCard(activeEditor, updatedCard));
        focusResult("#surla-cp1-editor");
      },
      (message) => {
        showFormError(message);
        focusResult("#surla-confirm-error");
      }
    );
  }

  // handleApprove asks the server to record approval of run's passed report
  // for the exact card revision shown. A stale revision uses the editor's
  // recovery; a project without promotion settings shows approval as
  // unavailable with the server's reason.
  function handleApprove(run: VerificationStatusResult) {
    if (!activeEditor || editorBusy || run.state.type !== "completed") {
      return;
    }
    const card = activeEditor.card;
    const report = run.state.value.report;
    approveErrors.delete(run.run_id);
    statusEl.textContent = "Recording approval of this result...";
    callEditorServer(
      "approve_merge",
      {
        change_id: card.change_id,
        request_revision: card.request_revision,
        candidate_commit: run.candidate_commit,
        report_digest: report.report_digest,
      },
      card.change_id,
      (approved) => {
        if (activeEditor && activeEditor.card.request_revision === approved.request_revision) {
          activeEditor = { ...activeEditor, card: approved, formError: null };
        } else {
          mountChangeCard(approved, null, nextPriorCard(activeEditor, approved));
        }
        editorOpen = true;
        activeResult = activeEditor && activeEditor.incomingCard ? { kind: "cp1-incoming-card" } : null;
        rootEl.dataset.state = "cp1-change-card";
        statusEl.textContent = "Approval recorded for candidate " + run.candidate_commit.slice(0, 7) + ".";
        syncAllRegions();
        focusResult("#surla-cp5-approval");
      },
      (message, failure) => {
        if (failure.status === 501) {
          approvalUnavailable = message;
          statusEl.textContent = "Approval is unavailable for this project.";
          syncAllRegions();
          focusResult("#surla-cp5-unavailable");
          return;
        }
        if (failure.status === 409) {
          editorOpen = true;
          activeResult = activeEditor && activeEditor.incomingCard ? { kind: "cp1-incoming-card" } : null;
          rootEl.dataset.state = "cp1-change-card";
          showFormError(message);
          focusResult("#surla-confirm-error");
          return;
        }
        approveErrors.set(run.run_id, message);
        statusEl.textContent = "Approval was not recorded.";
        syncAllRegions();
        focusResult("#surla-cp5-approve-error");
      }
    );
  }

  // handleDiscardDraft restores saved values on the retained card and closes
  // the split-screen editor when another stage is displayed beside it.
  function handleDiscardDraft() {
    if (!activeEditor || editorBusy) {
      return;
    }
    const savedCard = activeEditor.card;
    if (activeResult && activeResult.kind !== "cp1-incoming-card") {
      activeEditor = {
        card: savedCard,
        priorCard: activeEditor.priorCard,
        incomingCard: activeEditor.incomingCard,
        ...editorDraftFrom(savedCard),
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
  function handleLoadLatestCard() {
    if (!activeEditor || editorBusy) {
      return;
    }
    const changeId = activeEditor.card.change_id;
    statusEl.textContent = "Loading the latest saved Change Card...";
    callEditorServer(
      "get_change",
      { change_id: changeId },
      changeId,
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
          statusEl.textContent = latestCard.approval
            ? "Showing the recorded approval for these inputs."
            : latestCard.unverified_approval
            ? "Showing the saved approval record (current promotion inputs could not be verified)."
            : "Showing the latest card without a current approval.";
          return;
        }
        const preserved = activeEditor ? snapshotUnsavedDraft(activeEditor) : null;
        const prior = nextPriorCard(activeEditor, latestCard);
        mountChangeCard(latestCard, preserved, prior);
      },
      (message) => showFormError("Could not load the current card and approval: " + message)
    );
  }

  // mountChangeCard starts an editor from a received saved card and optionally
  // shows draft text or the immediately prior saved revision. Any distinct
  // incomingCard that arrived while an editor request was in flight remains.
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
      ...editorDraftFrom(card),
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

  // The tab's visibility pauses the waiting read; a hidden tab asks nothing.
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      if (waitController !== null) {
        waitController.abort();
      }
      return;
    }
    startWaiting();
  });

  // Leaving the page loses edits the server never saved, so the browser asks first.
  window.addEventListener("beforeunload", (event) => {
    if (hasUnsavedEdits()) {
      event.preventDefault();
      event.returnValue = "";
    }
  });

  syncAllRegions();
  void start();
}

bootstrapViewsPage();
