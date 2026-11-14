// Package main implements the Surla views page. The page exchanges the
// one-use link `surla` opened it with for a session held only in this
// script's memory, then reads and acts on the project's changes by calling
// the Surla server at the address it was loaded from. It coordinates the
// stage navigation bar (#surla-checkpoint-nav), the displayed-subject context
// strip (#surla-workspace-context), the two-region layout (#surla-cp1-editor
// and #surla-result-view), the change list, and synchronous React 19
// rendering.

// zod-jitless stays the first import: it configures zod before any imported
// module builds a schema.
import "./zod-jitless";
import React from "react";
import { flushSync } from "react-dom";
import { createRoot, Root } from "react-dom/client";
import { ArchitecturePreviewView, IncomingChangeCardView } from "./components/c4-graph";
import { ChangeListView } from "./components/change-list";
import { CP1EditorView } from "./components/cp1-editor";
import { BuildSectionView, BuildStartReceipt } from "./components/build";
import { VerificationView } from "./components/cp4-verification";
import { ProposedRevisionView } from "./components/proposal";
import { ApproveResultControl } from "./components/cp5-approve";
import { SessionEndedNotice, SignedOutReason, SignedOutView } from "./components/session";
import { StoryStageView } from "./components/story";
import { GlobalTeamAttention, TeamStageProps, TeamStageView, teamAttentionText } from "./components/team";
import { LiveTargetStageView, ModelDraft, ServerAnswer, describeApplied } from "./components/live-target";
import { Alert, Button, Card } from "./components/shadcn";
import { TraceDrawer, TraceFilter } from "./components/trace-drawer";
import { isDraftDirty, PreservedDraft } from "./context/cp1-draft";
import {
  ArchitecturePreviewResult,
  ChangeCardResult,
  ChangeBuild,
  ChangeList,
  ConfigureModelRequest,
  ExampleItem,
  LiveTargetStatus,
  ModelConnection,
  OperationName,
  ProposedRevisionItem,
  Operations,
  ROUTES,
  StoryRefreshResult,
  TraceList,
  TeamRoleState,
  TeamSnapshot,
  TestFindings,
  VerificationListResult,
  VerificationStatusResult,
  isPlainObject,
  isServerError,
} from "./types";

// WAIT_RETRY_MS is how long the page waits before asking again after the
// first failed round of its waiting read: the server did not answer, or
// answered that the list changed but then refused to send it. Each further
// failed round doubles the wait, up to WAIT_RETRY_MAX_MS.
const WAIT_RETRY_MS = 5000;
// WAIT_RETRY_MAX_MS bounds the wait between failed rounds.
const WAIT_RETRY_MAX_MS = 30000;

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
  // proposalAnswers holds the user's choices for the waiting proposal's
  // questions; a re-read keeps those that still fit the proposal.
  proposalAnswers: Record<string, string>;
  // proposalError describes the last refused adopt or decline, or null.
  proposalError: string | null;
}

// ActiveResultState selects the view displayed in #surla-result-view.
type ActiveResultState =
  | { kind: "changes" }
  | { kind: "story" }
  | { kind: "live" }
  | { kind: "team" }
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

// editorDraftFrom starts editable drafts from a saved card's own values, with
// no answers chosen yet for a waiting proposal.
function editorDraftFrom(card: ChangeCardResult): {
  draftAnswers: Record<string, string>;
  draftExamples: ExampleItem[];
  proposalAnswers: Record<string, string>;
  proposalError: string | null;
} {
  return {
    proposalAnswers: {},
    proposalError: null,
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

// keepProposalAnswers keeps the user's choices that still fit next: same
// question ID, same question, and the option still offered. A re-read of the
// same proposal therefore keeps every choice.
function keepProposalAnswers(
  answers: Record<string, string>,
  previous: ProposedRevisionItem | undefined,
  next: ProposedRevisionItem | undefined
): Record<string, string> {
  if (previous === undefined || next === undefined) {
    return {};
  }
  const before = new Map((previous.ambiguities ?? []).map((question) => [question.id, question.question]));
  const kept: Record<string, string> = {};
  for (const question of next.ambiguities ?? []) {
    const chosen = answers[question.id];
    if (chosen !== undefined && before.get(question.id) === question.question && question.options.includes(chosen)) {
      kept[question.id] = chosen;
    }
  }
  return kept;
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

// liveTargetKey names a live target read's case, commit, start and end, so the
// page can tell when the state a message described has changed.
function liveTargetKey(target: LiveTargetStatus): string {
  const live = target.live_target;
  switch (live.state) {
    case "none":
      return "none";
    case "starting":
      return "starting " + live.candidate_commit + " " + live.since;
    case "running":
      return "running " + live.candidate_commit + " " + live.started_at;
    case "ended":
      return "ended " + live.candidate_commit + " " + live.started_at + " " + live.ended_at;
    case "not_confirmed_stopped":
      return "not_confirmed_stopped " + live.candidate_commit;
  }
}

// describeActiveResultSubject derives the subject of #surla-result-view strictly
// from the displayed payload and selected verification run, never falling
// back to the editor's card. The Story stage shows the Story saved with
// storyCard, the change open in the editor, and the owner's current
// evaluation.
function describeActiveResultSubject(
  activeResult: ActiveResultState | null,
  previewState: ArchitecturePreviewResult | null,
  displayedRun: VerificationStatusResult | null,
  selectedVerificationChangeId: string | null,
  bothRegionsShown: boolean,
  storyCard: ChangeCardResult | null,
  storyEvaluation: StoryRefreshResult | null
): WorkspaceSubjectDescriptor | null {
  if (
    !activeResult ||
    activeResult.kind === "cp1-incoming-card" ||
    activeResult.kind === "changes" ||
    activeResult.kind === "live" ||
    activeResult.kind === "team"
  ) {
    return null;
  }
  const label = bothRegionsShown ? "Displayed view" : "Showing";
  if (activeResult.kind === "story") {
    let draft = "current draft not read yet";
    if (storyEvaluation !== null) {
      draft = "current draft " + storyEvaluation.state;
      if (storyEvaluation.snapshot_digest) {
        draft += " · snapshot " + storyEvaluation.snapshot_digest.slice(0, 15);
      }
    }
    return {
      role: "result",
      label,
      stage: "Story",
      changeId: storyCard?.change_id,
      requestRevision: storyCard?.request_revision,
      detail: draft,
    };
  }
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
  // projectName is the physical folder's base name from the authenticated
  // exchange, fixed for this page session and never derived from a team ID.
  let projectName: string | null = null;
  let sessionState: SessionState = "starting";
  let signedOutReason: SignedOutReason = "no-link";
  let signedOutMessage = "";
  const pendingCalls = new Set<PendingCall>();

  let changeList: ChangeList | null = null;
  let listError: string | null = null;
  let listRequest: Promise<boolean> | null = null;
  let waitController: AbortController | null = null;
  let waitLoopRunning = false;
  // waitTroubleShown is true while the status line reports that the waiting
  // read is retrying after a failed round.
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
  // storyEvaluation is the owner's current Story evaluation as last read, or
  // null before the first read; storyEvaluationError is its last failed read.
  let storyEvaluation: StoryRefreshResult | null = null;
  let storyEvaluationError: string | null = null;
  // storyEvaluationStale is true when the change list's version moved since
  // the evaluation was last read while the Story stage was not shown, so
  // opening the stage reads it again.
  let storyEvaluationStale = true;
  // storyReadAgain asks for one more read after the outstanding one, because
  // the version moved while it was in flight.
  let storyReadAgain = false;
  // liveTarget is the live target as last read, or null before the first
  // read; liveTargetError is its last failed read. liveTargetStale and
  // liveReadAgain follow the change list's version as the Story's do.
  let liveTarget: LiveTargetStatus | null = null;
  let liveTargetError: string | null = null;
  let liveTargetStale = true;
  let liveReadAgain = false;
  // liveStopOutcome is the server's answer to the last stop request. It is
  // shown until the live target's state next changes: liveStopBasis is the
  // state the first read issued after the answer found, and only reads
  // numbered above liveStopAfterRead, liveReadCount when the answer came,
  // count, so a read already in flight cannot clear it.
  let liveStopOutcome: ServerAnswer | null = null;
  let liveStopBasis: string | null = null;
  let liveStopAfterRead = 0;
  let liveReadCount = 0;
  // modelConnection is the model connection as last read, or null before
  // the first read; it is not part of the change list's version, so the
  // page reads it when the Live target view opens and after it changes it.
  let modelConnection: ModelConnection | null = null;
  let modelConnectionError: string | null = null;
  // modelApplyOutcome is the server's answer to the last change of model.
  let modelApplyOutcome: ServerAnswer | null = null;
  // modelDraft is the model form's unsent input, kept here so it survives
  // refreshes and leaving the Live target view.
  let modelDraft: ModelDraft = { kind: null, provider: "anthropic", modelName: "", gatewayAddr: "" };
  // teamSnapshot is shared by the Team stage and notices on every stage.
  // Failed reads retain facts, visibly labeled as last read.
  let teamSnapshot: TeamSnapshot | null = null;
  let teamReadError: string | null = null;
  let teamReadAgain = false;
  let teamStartReceipt: TeamStageProps["startReceipt"] = null;
  let teamActionError: string | null = null;
  // buildChangeId identifies the single retained build read. The generation
  // rejects replies begun before a start, its successful answer, or a change
  // of selected card.
  let buildChangeId: string | null = null;
  let buildSnapshot: ChangeBuild["build"] | null = null;
  let buildReadError: string | null = null;
  let buildStartError: string | null = null;
  let buildReadAgain = false;
  let buildGeneration = 0;
  let buildStartReceipt: BuildStartReceipt | null = null;
  // Findings belong to one build/report/verdict, rather than only a change ID.
  let buildFindings: TestFindings | null = null;
  let findingsBasis: string | null = null;
  let findingsError: string | null = null;
  let findingsReadAgain = false;
  const runsById = new Map<string, VerificationStatusResult>();
  // Verification observations follow the page's owner updates independently
  // of immutable terminal reports. Request order also covers overlapping
  // list/get reads of the same run within one update generation.
  let verificationGeneration = 0;
  let verificationReadOrder = 0;
  const runObservationGeneration = new Map<string, number>();
  const runObservationOrder = new Map<string, number>();
  const runReadAgain = new Set<string>();
  const lastLoadedListByChangeId = new Map<string, string[]>();
  let selectedVerificationChangeId: string | null = null;
  let selectedRunId: string | null = null;
  const selectionTouchedDuringList = new Map<string, boolean>();
  const runRequestErrors = new Map<string, string>();
  const listRequestErrors = new Map<string, string>();
  const cardLoadErrors = new Map<string, string>();
  const approveErrors = new Map<string, string>();
  // approvalUnavailable is the server's reason it cannot record approvals,
  // learned from its answer to an approval request. The change list says
  // the same before any request; this answer covers a page that has not
  // read the list, or a server whose answer changed since.
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
      if ((text === "" || parsed !== undefined) && route.decode(parsed)) {
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
    projectName = outcome.result.project_name;
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
    buildStartReceipt = null;
    buildStartError = null;
    teamStartReceipt = null;
    buildGeneration++;
    rootEl.dataset.session = "ended";
    if (waitController !== null) {
      waitController.abort();
    }
    statusEl.textContent = sessionEndedMessage();
    syncAllRegions();
  }

  // loadChanges reads the change list once at a time, then applies what
  // changed to the open change and its runs. It resolves to whether the list
  // was read; a failure is shown on the list and kept in listError.
  function loadChanges(): Promise<boolean> {
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
        return false;
      }
      const previous = changeList;
      changeList = outcome.result;
      listError = null;
      if (previous === null || previous.version !== outcome.result.version) {
        verificationGeneration++;
      }
      applyListChanges(previous, outcome.result);
      if (previous === null || previous.version !== outcome.result.version) {
        readTeam();
        if (activeEditor !== null) {
          readBuild(activeEditor.card.change_id);
          refreshVerificationObservations(activeEditor.card.change_id);
        }
      }
      if (previous !== null && previous.version !== outcome.result.version) {
        // The version also covers the owner's Story evaluation. It is read
        // after the list, so it is never older than the version just read.
        if (activeResult?.kind === "story") {
          readStoryEvaluation();
        } else {
          storyEvaluationStale = true;
        }
        // It also covers the live target: its case, commit, start time and
        // the versions in force.
        if (activeResult?.kind === "live") {
          readLiveTarget();
        } else {
          liveTargetStale = true;
        }
      }
      syncAllRegions();
      startWaiting();
      return true;
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
  // open request; showing it again resumes from the same version. A round
  // fails when the waiting read gets no answer or the list read that follows
  // it fails; the loop then reports the failure once and waits longer after
  // each failed round before asking again, because a list that keeps
  // failing would otherwise be asked for again at once.
  async function waitLoop() {
    let failedRounds = 0;
    while (session !== null && changeList !== null && !document.hidden) {
      const controller = new AbortController();
      waitController = controller;
      const since = changeList.version;
      const outcome = await send("wait_for_changes", { since }, controller.signal);
      waitController = null;
      let trouble: string;
      if (outcome.ok) {
        const changed = changeList !== null && outcome.result.version !== changeList.version;
        if (!changed || (await loadChanges())) {
          failedRounds = 0;
          if (waitTroubleShown) {
            waitTroubleShown = false;
            statusEl.textContent = "This page is up to date with the Surla server again.";
            syncAllRegions();
          }
          continue;
        }
        if (session === null) {
          return;
        }
        trouble =
          "Could not read this project's changes; trying again. Your edits stay on this page. " +
          (listError ?? "");
      } else if (outcome.kind === "aborted") {
        continue;
      } else if (outcome.kind === "session") {
        endSession();
        return;
      } else {
        trouble =
          "Lost contact with the Surla server; trying again. Your edits stay on this page. " +
          outcome.message;
      }
      if (!waitTroubleShown) {
        waitTroubleShown = true;
        statusEl.textContent = trouble;
        syncAllRegions();
      }
      failedRounds += 1;
      const delay = Math.min(WAIT_RETRY_MS * 2 ** (failedRounds - 1), WAIT_RETRY_MAX_MS);
      await new Promise<void>((resume) => window.setTimeout(resume, delay));
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
          cardLoadErrors.set(changeId, outcome.message);
          return;
        }
        if (outcome.result.change_id !== changeId) {
          cardLoadErrors.set(changeId, "The server returned a different Change Card.");
          return;
        }
        cardLoadErrors.delete(changeId);
        receiveLatestCard(outcome.result);
      } while (cardRefreshAgain && session !== null);
    })();
    void cardRefresh.finally(() => {
      cardRefresh = null;
      syncAllRegions();
    });
    syncAllRegions();
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
    cardLoadErrors.delete(latest.change_id);
    const ed = activeEditor;
    if (latest.request_revision !== ed.card.request_revision || latest.status !== ed.card.status) {
      verificationGeneration++;
      refreshVerificationObservations(latest.change_id);
    }
    if (latest.request_revision === ed.card.request_revision) {
      const before = ed.card.proposed_revision;
      const after = latest.proposed_revision;
      activeEditor = {
        ...ed,
        card: latest,
        proposalAnswers: keepProposalAnswers(ed.proposalAnswers, before, after),
        proposalError: before?.request_revision === after?.request_revision ? ed.proposalError : null,
      };
      if (after !== undefined && before?.request_revision !== after.request_revision) {
        statusEl.textContent =
          (before === undefined
            ? "The guide proposed a new revision of this change."
            : "The guide replaced its proposed revision of this change.") +
          " Adopt or decline it under Decisions & Approval; the agreed revision stays in force until then.";
      }
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
    const generation = verificationGeneration;
    const order = ++verificationReadOrder;
    for (const run of runsById.values()) {
      if (run.change_id === changeId) markRunReadPending(run.run_id, order);
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
          upsertRunSnapshot(payload.runs[i], generation, order);
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

  // runInputsKnown means the displayed owner observation survived the latest
  // update and all reads on which its action depends. Saved evidence remains
  // visible while any of these facts are unknown.
  function runInputsKnown(run: VerificationStatusResult): boolean {
    return sessionState === "active" && !document.hidden && changeList !== null && listError === null &&
      !waitTroubleShown && !editorBusy && cardRefresh === null &&
      runObservationGeneration.get(run.run_id) === verificationGeneration &&
      !hasPending("get_verification", run.run_id) &&
      !hasPending("list_verifications", run.change_id) &&
      !hasPending("get_build", run.change_id) &&
      !hasPending("start_build", run.change_id) &&
      !cardLoadErrors.has(run.change_id) &&
      (buildChangeId !== run.change_id || buildReadError === null);
  }

  // refreshVerificationObservations reads exact owner-supplied identities.
  // An older selected run is independent of the latest run's match.
  function refreshVerificationObservations(changeId: string) {
    const latest = changeList?.changes.find((item) => item.change_id === changeId)?.latest_verification;
    if (latest !== undefined) readVerification(latest.run_id, changeId);
    if (selectedVerificationChangeId === changeId && selectedRunId !== null &&
        selectedRunId !== latest?.run_id) readVerification(selectedRunId, changeId);
  }

  // latestVerificationFor uses the run identity supplied by the change owner.
  function latestVerificationFor(changeId: string): VerificationStatusResult | null {
    const id = changeList?.changes.find((item) => item.change_id === changeId)?.latest_verification?.run_id;
    return id === undefined ? null : runsById.get(id) ?? null;
  }

  // reviewSavedCheck opens the exact result whose action the user selected.
  function reviewSavedCheck(run: VerificationStatusResult) {
    selectedVerificationChangeId = run.change_id;
    selectedRunId = run.run_id;
    editorOpen = shouldRetainEditor();
    activeResult = { kind: "cp4-verification" };
    rootEl.dataset.state = "cp4-verification";
    statusEl.textContent = "Review the saved check before approving it.";
    if (!lastLoadedListByChangeId.has(run.change_id)) refreshVerificationsInBackground(run.change_id);
    if (!runInputsKnown(run)) readVerification(run.run_id, run.change_id);
    syncAllRegions();
    focusResult(run.state.type === "completed" && run.state.value.report.passed
      ? "#surla-cp5-approve-title" : "#surla-cp4-title");
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

  // openStory shows the Story stage: the open change's saved Story, if a
  // change is open, and the owner's current Story evaluation, which it reads
  // when it was never read or the change list's version moved since.
  function openStory() {
    editorOpen = shouldRetainEditor();
    activeResult = { kind: "story" };
    rootEl.dataset.state = "story";
    statusEl.textContent =
      activeEditor !== null
        ? "Showing the Story of Change Card " + activeEditor.card.change_id + "."
        : "Showing the current Story draft.";
    if (storyEvaluation === null || storyEvaluationStale) {
      readStoryEvaluation();
    }
    syncAllRegions();
  }

  // readStoryEvaluation reads the owner's current Story evaluation. A read
  // asked for while one is in flight runs once more after it, so the page
  // never keeps an answer older than the last version it saw.
  function readStoryEvaluation() {
    if (hasPending("get_story_refresh", "")) {
      storyReadAgain = true;
      return;
    }
    storyReadAgain = false;
    storyEvaluationStale = false;
    const done = () => {
      if (storyReadAgain) {
        readStoryEvaluation();
      }
    };
    callServer(
      "get_story_refresh",
      {},
      "",
      (evaluation) => {
        storyEvaluation = evaluation;
        storyEvaluationError = null;
        done();
      },
      (message) => {
        storyEvaluationError = message;
        storyEvaluationStale = true;
        if (activeResult?.kind === "story") {
          statusEl.textContent = "Could not read the current Story draft.";
        }
        done();
      }
    );
  }

  // openTeam shows the shared team snapshot without dropping an edited card.
  function openTeam() {
    editorOpen = shouldRetainEditor();
    activeResult = { kind: "team" };
    rootEl.dataset.state = "team";
    statusEl.textContent = "Showing the project team.";
    if (teamSnapshot === null || teamReadError !== null || waitTroubleShown) readTeam();
    syncAllRegions();
    document.getElementById("surla-team-title")?.focus();
  }

  // readTeam follows the existing single version chain. One extra read is
  // retained when another change arrives during an outstanding request.
  function readTeam() {
    if (sessionState !== "active") return;
    if (hasPending("get_team", "")) {
      teamReadAgain = true;
      return;
    }
    teamReadAgain = false;
    const done = () => {
      if (teamReadAgain && sessionState === "active") readTeam();
    };
    callServer(
      "get_team", {}, "",
      (snapshot) => {
        if (sessionState !== "active") return;
        const before = teamSnapshot;
        const wasFailed = teamReadError !== null;
        teamSnapshot = snapshot;
        teamReadError = null;
        const newlyNeedingUser = snapshot.roles.filter((role) => {
          if (role.state !== "needs_you") return false;
          const previous = before?.roles.find((item) => item.role === role.role && item.handle === role.handle);
          return wasFailed || previous?.state !== "needs_you" || previous.attention !== role.attention;
        });
        if (newlyNeedingUser.length > 0) {
          statusEl.textContent = (waitTroubleShown ? "Last read: " : "") +
            newlyNeedingUser.map(teamAttentionText).join(". ");
        }
        done();
      },
      (message) => {
        if (sessionState !== "active") return;
        const firstFailure = teamReadError === null;
        teamReadError = message;
        if (firstFailure) {
          statusEl.textContent = "Could not refresh the project team. " + message + " Open Team to retry.";
        }
        done();
      }
    );
  }

  // teamFactsError distinguishes retained observations from currently followed
  // facts when the read, the update chain or the session is unavailable.
  function teamFactsError(): string | null {
    if (teamReadError !== null) return teamReadError;
    if (sessionState !== "active") return "The session ended; these are last-read facts.";
    if (waitTroubleShown) return "Updates are unavailable; these are last-read facts.";
    return null;
  }

  // showingTeamRoles derives pending controls from the registered snapshot,
  // preserving the generated role type instead of casting arbitrary subjects.
  function showingTeamRoles(): TeamRoleState["role"][] {
    return (teamSnapshot?.roles ?? [])
      .filter((role) => hasPending("show_team_pane", role.role))
      .map((role) => role.role);
  }

  // focusCurrentViewHeading recovers focus only when a focused team control
  // disappears. Each rendered region starts with its current view's heading.
  function focusCurrentViewHeading() {
    const region = activeResult !== null && activeResult.kind !== "cp1-incoming-card"
      ? resultRegionEl : editorRegionEl;
    const heading = region.querySelector<HTMLElement>("h2");
    if (heading !== null) {
      heading.tabIndex = -1;
      heading.focus();
    }
  }

  // startTeamAgents is an explicit window request. Its dated receipt describes
  // what that request returned; current role health always comes from readTeam.
  function startTeamAgents() {
    if (sessionState !== "active" || hasPending("start_team_agents", "")) return;
    const begun = new Date().toISOString();
    teamActionError = null;
    callServer(
      "start_team_agents", {}, "",
      (result) => {
        if (sessionState !== "active") return;
        teamStartReceipt = { at: begun, result };
        if (buildStartReceipt !== null) {
          buildStartReceipt = { ...buildStartReceipt, team: null };
        }
        statusEl.textContent = "This start (begun " + new Date(begun).toLocaleString() +
          ") returned: " + result.step;
        readTeam();
      },
      (message) => {
        if (sessionState !== "active") return;
        teamActionError = message;
        statusEl.textContent = "Could not start agents. " + message;
      }
    );
  }

  // readBuild retains one selected change's user projection. The update chain
  // coalesces a later read instead of creating an independent polling loop.
  function readBuild(changeId: string) {
    if (sessionState !== "active" || activeEditor?.card.change_id !== changeId) return;
    if (buildChangeId !== changeId) {
      buildChangeId = changeId;
      buildSnapshot = null;
      buildReadError = null;
      buildStartReceipt = null;
      buildStartError = null;
      followBuildFindings();
      buildGeneration++;
    }
    if (hasPending("get_build", changeId)) {
      buildReadAgain = true;
      return;
    }
    buildReadAgain = false;
    const generation = buildGeneration;
    const relevant = () => sessionState === "active" && buildChangeId === changeId &&
      activeEditor?.card.change_id === changeId;
    const done = () => {
      if (relevant() && buildReadAgain) readBuild(changeId);
    };
    callServer(
      "get_build", { change_id: changeId }, changeId,
      (result) => {
        if (relevant() && generation === buildGeneration) {
          if (result.change_id !== changeId) {
            buildReadError = "The server returned a build for a different change.";
          } else {
            const before = buildSnapshot;
            buildSnapshot = result.build;
            buildReadError = null;
            const replaced = before?.state === "started"
              ? result.build.state !== "started" || before.started_at !== result.build.started_at
              : result.build.state === "started";
            if (replaced) {
              buildStartReceipt = null;
              buildStartError = null;
            }
            if (before === null || JSON.stringify(before) !== JSON.stringify(result.build)) {
              verificationGeneration++;
              if (before !== null) refreshOpenCard(changeId);
              refreshVerificationObservations(changeId);
            }
            followBuildFindings();
          }
        }
        done();
      },
      (message) => {
        if (relevant() && generation === buildGeneration) buildReadError = message;
        done();
      }
    );
  }

  // currentFindingsBasis names the exact saved verdict whose text is useful.
  // Assignment reads can change independently without invalidating that text.
  function currentFindingsBasis(): string | null {
    if (buildChangeId === null || buildSnapshot?.state !== "started" ||
        buildSnapshot.test_writer.tests.state !== "needs_changes" ||
        hasPending("start_build", buildChangeId)) return null;
    return JSON.stringify([
      buildChangeId, buildSnapshot.request_revision, buildSnapshot.started_at,
      buildSnapshot.test_writer.report, buildSnapshot.test_writer.tests.checked_at,
      buildSnapshot.test_writer.tests.finding_count,
    ]);
  }

  // followBuildFindings starts one user read per new verdict. Read failures
  // remain explicit; an unchanged snapshot never becomes a retry loop.
  function followBuildFindings() {
    const next = currentFindingsBasis();
    if (next === findingsBasis) return;
    findingsBasis = next;
    buildFindings = null;
    findingsError = null;
    if (next !== null) readBuildFindings();
  }

  // readBuildFindings accepts text only when every owner-provided correlation
  // field still matches. A late answer never selects a newer or earlier build.
  function readBuildFindings() {
    const expected = buildSnapshot;
    const changeId = buildChangeId;
    const basis = currentFindingsBasis();
    if (sessionState !== "active" || changeId === null || basis === null ||
        expected?.state !== "started" || expected.test_writer.tests.state !== "needs_changes" ||
        activeEditor?.card.change_id !== changeId) return;
    if (hasPending("get_test_findings", changeId)) {
      findingsReadAgain = true;
      return;
    }
    findingsReadAgain = false;
    findingsError = null;
    const generation = buildGeneration;
    const verdict = expected.test_writer.tests;
    const relevant = () => sessionState === "active" && buildGeneration === generation &&
      activeEditor?.card.change_id === changeId && findingsBasis === basis &&
      currentFindingsBasis() === basis;
    const done = () => {
      if (findingsReadAgain) {
        findingsReadAgain = false;
        readBuildFindings();
      }
    };
    callServer(
      "get_test_findings", { change_id: changeId }, changeId,
      (result) => {
        if (relevant()) {
          if (result.change_id !== changeId || result.request_revision !== expected.request_revision ||
              result.started_at !== expected.started_at || result.report !== expected.test_writer.report ||
              result.checked_at !== verdict.checked_at || result.findings.length !== verdict.finding_count) {
            findingsError = "The findings no longer match this build. Refresh the build to read its current verdict.";
            readBuild(changeId);
          } else {
            buildFindings = result;
          }
        }
        done();
      },
      (message, failure) => {
        if (relevant()) {
          findingsError = message;
          if (failure.kind === "server" && (failure.status === 404 || failure.status === 409)) {
            readBuild(changeId);
          }
        }
        done();
      }
    );
  }

  // startBuild sends the revision displayed by the editor only after its
  // Build control has obtained any required replacement confirmation.
  function startBuild() {
    const ed = activeEditor;
    if (ed === null || sessionState !== "active" || editorBusy ||
        ed.card.status !== "confirmed" || ed.incomingCard !== null ||
        buildChangeId !== ed.card.change_id || buildSnapshot === null ||
        buildReadError !== null || hasPending("start_build", ed.card.change_id)) return;
    const changeId = ed.card.change_id;
    const revision = ed.card.request_revision;
    if (buildSnapshot.state === "started" && buildSnapshot.in_force &&
        buildSnapshot.request_revision !== revision) return;
    const listedRevision = changeList?.changes.find((item) => item.change_id === changeId)?.request_revision;
    if (listedRevision !== undefined && listedRevision !== revision) return;
    const begun = new Date().toISOString();
    buildGeneration++;
    verificationGeneration++;
    buildStartReceipt = null;
    buildStartError = null;
    buildFindings = null;
    findingsBasis = null;
    findingsError = null;
    findingsReadAgain = false;
    callServer(
      "start_build", { change_id: changeId, request_revision: revision }, changeId,
      (result) => {
        if (sessionState !== "active" || activeEditor?.card.change_id !== changeId ||
            activeEditor.card.request_revision !== revision) return;
        // A read begun during the start may still describe its predecessor.
        // The successful answer supersedes it, including transient receipts.
        buildGeneration++;
        verificationGeneration++;
        if (result.build.request_revision !== revision) {
          buildStartError = "The server returned a build for a different revision. Refresh the saved build.";
          statusEl.textContent = buildStartError;
          readBuild(changeId);
          return;
        }
        buildSnapshot = { ...result.build, state: "started" };
        buildReadError = null;
        buildStartReceipt = { at: begun, team: result.team, workingCopy: result.working_copy };
        followBuildFindings();
        statusEl.textContent = "The build started for the agreed revision.";
        readBuild(changeId);
        refreshVerificationObservations(changeId);
        refreshOpenCard(changeId);
        readTeam();
      },
      (message, failure) => {
        if (sessionState === "active" && activeEditor?.card.change_id === changeId) {
          buildStartError = (failure.kind === "server"
            ? "Build did not start. " : "Could not confirm whether the build started. ") + message;
          statusEl.textContent = buildStartError;
          readBuild(changeId);
        }
      }
    );
  }

  // showTeamPane sends only the registered role the user selected; the server
  // rechecks ownership and open dialogs before it changes the window.
  function showTeamPane(role: TeamRoleState["role"]) {
    if (sessionState !== "active" || hasPending("show_team_pane", role)) return;
    teamActionError = null;
    callServer(
      "show_team_pane", { role }, role,
      () => {
        if (sessionState !== "active") return;
        statusEl.textContent = "The " + role.replaceAll("_", " ") + "'s pane is shown in Herdr.";
        readTeam();
      },
      (message) => {
        if (sessionState !== "active") return;
        teamActionError = message;
        statusEl.textContent = "Could not show the " + role.replaceAll("_", " ") + "'s pane. " + message;
      }
    );
  }

  // openLive shows the Live target view: the live target, read when it was
  // never read or the change list's version moved since, and the model
  // connection, read each time the view opens because no version covers it.
  function openLive() {
    editorOpen = shouldRetainEditor();
    activeResult = { kind: "live" };
    rootEl.dataset.state = "live";
    statusEl.textContent = "Showing the live target and the model connection.";
    if (liveTarget === null || liveTargetStale) {
      readLiveTarget();
    }
    readModelConnection();
    syncAllRegions();
  }

  // readLiveTarget reads the live target. A read asked for while one is in
  // flight runs once more after it, so the page never keeps an answer older
  // than the last version it saw.
  function readLiveTarget() {
    if (hasPending("get_live_target", "")) {
      liveReadAgain = true;
      return;
    }
    liveReadAgain = false;
    liveTargetStale = false;
    liveReadCount += 1;
    const read = liveReadCount;
    const done = () => {
      if (liveReadAgain) {
        readLiveTarget();
      }
    };
    callServer(
      "get_live_target",
      {},
      "",
      (target) => {
        liveTarget = target;
        liveTargetError = null;
        // A stop's message describes the state its answer left; it goes once
        // that state changes.
        if (liveStopOutcome !== null && read > liveStopAfterRead) {
          const key = liveTargetKey(target);
          if (liveStopBasis === null) {
            liveStopBasis = key;
          } else if (liveStopBasis !== key) {
            liveStopOutcome = null;
            liveStopBasis = null;
          }
        }
        done();
      },
      (message) => {
        liveTargetError = message;
        liveTargetStale = true;
        if (activeResult?.kind === "live") {
          statusEl.textContent = "Could not read the live target.";
        }
        done();
      }
    );
  }

  // readModelConnection reads the model live targets started afterwards use.
  function readModelConnection() {
    if (hasPending("get_model_connection", "")) {
      return;
    }
    callServer(
      "get_model_connection",
      {},
      "",
      (connection) => {
        modelConnection = connection;
        modelConnectionError = null;
      },
      (message) => {
        modelConnectionError = message;
      }
    );
  }

  // stopLiveTarget sends the one stop request the user confirmed, shows the
  // server's message, and reads the live target again.
  function stopLiveTarget() {
    if (hasPending("stop_live_target", "")) {
      return;
    }
    liveStopOutcome = null;
    callServer(
      "stop_live_target",
      {},
      "",
      (result) => {
        showStopAnswer({ message: result.message, refused: false });
      },
      (message) => {
        showStopAnswer({ message, refused: true });
      }
    );
  }

  // showStopAnswer shows a stop's answer and reads the live target again;
  // the first read issued from now on records the state the answer left.
  function showStopAnswer(answer: ServerAnswer) {
    liveStopOutcome = answer;
    liveStopBasis = null;
    liveStopAfterRead = liveReadCount;
    readLiveTarget();
  }

  // updateModelDraft records one change to the model form's input. Only a
  // change of kind redraws the view; the text fields hold their own text.
  function updateModelDraft(change: Partial<ModelDraft>) {
    modelDraft = { ...modelDraft, ...change };
    if (change.kind !== undefined) {
      syncResultRegion();
    }
  }

  // applyModel asks the server to use choice for live targets started
  // afterwards, shows its answer or refusal, and reads the connection again.
  function applyModel(choice: ConfigureModelRequest) {
    if (hasPending("configure_model", "")) {
      return;
    }
    modelApplyOutcome = null;
    callServer(
      "configure_model",
      choice,
      "",
      (result) => {
        modelApplyOutcome = { message: describeApplied(result), refused: false };
        readModelConnection();
      },
      (message) => {
        modelApplyOutcome = { message, refused: true };
      }
    );
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
    tabs.push({
      id: "surla-nav-team",
      label: "Team",
      active: activeResult?.kind === "team",
      onClick: openTeam,
    });
    tabs.push({
      id: "surla-nav-story",
      label: "Story",
      active: activeResult?.kind === "story",
      onClick: openStory,
    });
    tabs.push({
      id: "surla-nav-live",
      label: "Live target",
      active: activeResult?.kind === "live",
      onClick: openLive,
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
      bothRegionsShown,
      activeEditor !== null ? activeEditor.card : null,
      storyEvaluation
    );
    if (resultSubject !== null) {
      subjects.push(resultSubject);
    }
    if (activeResult?.kind === "team") {
      subjects.push({
        role: "result",
        label: bothRegionsShown ? "Displayed view" : "Showing",
        stage: "Project team",
        detail: teamSnapshot?.team,
      });
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

  // projectTitle owns the tab's next-step contract using only page facts.
  // Recovery outranks retained observations; saved run history does not prove
  // that the current change can be built or approved.
  function projectTitle(): string {
    if (projectName === null) return "Surla";
    let step: string;
    if (sessionState !== "active") {
      step = "Reopen project";
    } else if (waitTroubleShown) {
      step = "Reconnecting";
    } else if (listError !== null) {
      step = "Retry reading changes";
    } else if (teamReadError !== null) {
      step = "Refresh Team";
    } else if (lastOpenAttempt !== null && cardLoadErrors.has(lastOpenAttempt)) {
      step = "Retry opening change";
    } else {
      const attention = ["guide", "builder", "test_writer"].map((role) =>
        teamSnapshot?.roles.find((item) => item.role === role && item.state === "needs_you")
      ).find((role) => role !== undefined);
      if (attention !== undefined) {
        step = teamAttentionText(attention);
      } else if (activeEditor !== null) {
        const ed = activeEditor;
        if (ed.incomingCard !== null || ed.card.proposed_revision !== undefined) {
          step = "Review proposed change";
        } else if (ed.card.status === "needs_confirmation") {
          step = "Confirm change";
        } else if (isDraftDirty(ed.card, ed.draftAnswers, ed.draftExamples)) {
          step = "Refine change";
        } else {
          step = "Review change";
        }
      } else if (changeList === null) {
        step = "Opening project";
      } else {
        step = changeList.changes.length === 0 ? "Ask guide to propose a change" : "Open a change";
      }
    }
    return step + ": " + projectName;
  }

  // syncNoticeRegion keeps the tab title, session notice and role attention visible on
  // every view. Announcements are emitted only by the event/read handlers.
  function syncNoticeRegion() {
    document.title = projectTitle();
    flushSync(() => {
      noticeRoot.render(
        <>
        {sessionState === "ended" ? (
          <SessionEndedNotice
            unsavedLines={
              activeEditor ? snapshotUnsavedDraft(activeEditor)?.lines ?? [] : []
            }
          />
        ) : null}
        {sessionState === "active" || sessionState === "ended" ? (
          <GlobalTeamAttention
            snapshot={teamSnapshot}
            lastRead={teamFactsError() !== null}
            disabled={sessionState !== "active"}
            showing={showingTeamRoles()}
            onShowPane={showTeamPane}
            onFocusedControlRemoved={focusCurrentViewHeading}
          />
        ) : null}
        </>
      );
    });
  }

  // revisionBuildNotice shares the build consequence beside both revision
  // actions. An unread or failed read cannot establish that no build exists.
  function revisionBuildNotice(card: ChangeCardResult): string | null {
    if (card.status !== "confirmed") return null;
    const known = buildChangeId === card.change_id && buildSnapshot !== null &&
      buildReadError === null && !waitTroubleShown;
    if (!known || buildSnapshot === null) {
      return "Changing the agreed revision leaves any existing build on the earlier revision. " +
        "Starting a new build replaces its assignments; when it uses a test writer, its report " +
        "and verdict reset and its tests need acceptance again before verification.";
    }
    if (buildSnapshot.state !== "started" || !buildSnapshot.in_force ||
        buildSnapshot.request_revision !== card.request_revision) return null;
    if (buildSnapshot.test_writer.tests.state === "not_used") {
      return "Changing the agreed revision leaves this build on the earlier revision. " +
        "Start a new build to update the builder’s assignment. " +
        "Verification keeps using the stored verification settings.";
    }
    return "Changing the agreed revision leaves this build on the earlier revision. " +
      "A new build replaces both assignments and clears the test writer’s report and verdict. " +
      "Its tests must be reported, checked and accepted again before verification.";
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
    const proposal = ed.card.proposed_revision;
    const buildRevisionNotice = revisionBuildNotice(ed.card);
    const latestCheck = latestVerificationFor(ed.card.change_id);
    flushSync(() => {
      editorRoot.render(
        <>
        {proposal !== undefined ? (
          <ProposedRevisionView
            card={ed.card}
            proposal={proposal}
            answers={ed.proposalAnswers}
            busy={editorBusy}
            error={ed.proposalError}
            buildRevisionNotice={buildRevisionNotice}
            onAnswer={(questionId, option) => {
              if (!activeEditor || editorBusy) {
                return;
              }
              activeEditor = {
                ...activeEditor,
                proposalAnswers: { ...activeEditor.proposalAnswers, [questionId]: option },
              };
              syncAllRegions();
            }}
            onAdopt={handleAdopt}
            onDecline={handleDecline}
          />
        ) : null}
        <CP1EditorView
          card={ed.card}
          buildRevisionNotice={buildRevisionNotice}
          buildSection={
            <BuildSectionView
              key={activeResult?.kind ?? "decisions"}
              card={ed.card}
              currentRevision={changeList?.changes.find((item) => item.change_id === ed.card.change_id)?.request_revision ??
                ed.incomingCard?.request_revision ?? ed.card.request_revision}
              build={buildChangeId === ed.card.change_id ? buildSnapshot : null}
              loading={hasPending("get_build", ed.card.change_id)}
              error={buildReadError}
              actionError={buildStartError}
              starting={hasPending("start_build", ed.card.change_id)}
              disabled={sessionState !== "active" || editorBusy}
              dirty={isDraftDirty(ed.card, ed.draftAnswers, ed.draftExamples)}
              startReceipt={buildStartReceipt}
              findingTexts={buildFindings?.findings ?? null}
              findingsLoading={hasPending("get_test_findings", ed.card.change_id)}
              findingsError={findingsError}
              check={latestCheck}
              checkKnown={latestCheck !== null && runInputsKnown(latestCheck)}
              checkError={cardLoadErrors.get(ed.card.change_id) ??
                (latestCheck !== null ? runRequestErrors.get(latestCheck.run_id) ?? null : null)}
              approvalAvailable={approvalUnavailable === null && changeList?.approval_available === true}
              onRefresh={() => readBuild(ed.card.change_id)}
              onReadFindings={readBuildFindings}
              onBuild={startBuild}
              onOpenTeam={openTeam}
              onReviewCheck={() => { if (latestCheck !== null) reviewSavedCheck(latestCheck); }}
            />
          }
          proposalWaiting={proposal !== undefined}
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
          onOpenStory={openStory}
        />

        </>
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
    if (res.kind === "story") {
      flushSync(() => {
        resultRoot.render(
          <StoryStageView
            card={activeEditor !== null ? activeEditor.card : null}
            evaluation={storyEvaluation}
            loading={hasPending("get_story_refresh", "")}
            error={storyEvaluationError}
            onRetry={readStoryEvaluation}
          />
        );
      });
      if (activeEditor !== null) {
        resultRegionEl.dataset.changeId = activeEditor.card.change_id;
        resultRegionEl.dataset.requestRevision = activeEditor.card.request_revision;
      }
      return;
    }
    if (res.kind === "team") {
      flushSync(() => {
        resultRoot.render(
          <TeamStageView
            snapshot={teamSnapshot}
            loading={hasPending("get_team", "")}
            error={teamFactsError()}
            starting={hasPending("start_team_agents", "")}
            disabled={sessionState !== "active"}
            showing={showingTeamRoles()}
            startReceipt={teamStartReceipt}
            actionError={teamActionError}
            onRefresh={readTeam}
            onStartAgents={startTeamAgents}
            onShowPane={showTeamPane}
            onFocusedControlRemoved={focusCurrentViewHeading}
          />
        );
      });
      return;
    }
    if (res.kind === "live") {
      flushSync(() => {
        resultRoot.render(
          <LiveTargetStageView
            target={liveTarget}
            targetLoading={hasPending("get_live_target", "")}
            targetError={liveTargetError}
            onRetryTarget={readLiveTarget}
            stopping={hasPending("stop_live_target", "")}
            stopOutcome={liveStopOutcome}
            onStop={stopLiveTarget}
            model={modelConnection}
            modelLoading={hasPending("get_model_connection", "")}
            modelError={modelConnectionError}
            onRetryModel={readModelConnection}
            applying={hasPending("configure_model", "")}
            applyOutcome={modelApplyOutcome}
            onApply={applyModel}
            draft={modelDraft}
            onDraft={updateModelDraft}
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
              inputsKnown={displayedRun !== null && runInputsKnown(displayedRun)}
              listedRuns={listedRuns}
              loadedCard={loadedCard}
              checking={displayedRun ? hasPending("get_verification", displayedRun.run_id) : false}
              loadingList={hasPending("list_verifications", effectiveChangeId)}
              loadingCard={editorBusy || cardRefresh !== null}
              requestError={displayedRun ? runRequestErrors.get(displayedRun.run_id) ?? null : null}
              listError={listRequestErrors.get(effectiveChangeId) ?? null}
              cardLoadError={
                effectiveChangeId ? cardLoadErrors.get(effectiveChangeId) ?? null : null
              }
              onCheckStatus={handleCheckVerificationStatus}
              onLoadVerifications={() => handleListVerifications(effectiveChangeId)}
              onSelectRun={handleSelectVerificationRun}
              onOpenChangeCard={() => {
                if (activeEditor?.card.change_id === effectiveChangeId) handleLoadLatestCard();
                else openChange(effectiveChangeId);
              }}
              onReturnToChangeCard={handleReturnToChangeCard}
            />
            {displayedRun ? (
              <ApproveResultControl
                run={displayedRun}
                inputsKnown={runInputsKnown(displayedRun)}
                card={
                  loadedCard !== null && loadedCard.change_id === displayedRun.change_id
                    ? loadedCard
                    : null
                }
                busy={editorBusy}
                available={
                  approvalUnavailable === null &&
                  (changeList === null || changeList.approval_available)
                }
                unavailableReason={approvalUnavailable}
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
    syncOpenedCheckStatus();
  }

  // syncOpenedCheckStatus names the user's decision on the opened change,
  // without replacing an agent's attention or an operation's failure message.
  function syncOpenedCheckStatus() {
    const ed = activeEditor;
    if (sessionState !== "active" || activeResult !== null || ed?.card.status !== "confirmed" ||
        editorBusy || buildStartError !== null || buildReadError !== null ||
        listError !== null || waitTroubleShown || cardLoadErrors.has(ed.card.change_id) ||
        (teamSnapshot?.roles.some((role) => role.state === "needs_you") ?? false) ||
        isDraftDirty(ed.card, ed.draftAnswers, ed.draftExamples)) return;
    const run = latestVerificationFor(ed.card.change_id);
    if (run === null || !runInputsKnown(run)) return;
    let next: string;
    if (!run.inputs_current) {
      next = "The saved check describes earlier inputs. The builder checks the current agreement and tests again before you approve.";
    } else if (run.state.type === "running") {
      next = "Surla is checking the builder’s code. Your next decision is approval after the check passes.";
    } else if (run.state.type === "completed" && run.state.value.report.passed) {
      next = ed.card.approval?.report_digest === run.state.value.report.report_digest
        ? "The check passed and your approval is recorded."
        : changeList?.approval_available === true && approvalUnavailable === null
        ? "The check passed. Review the result and approve it."
        : "The check passed. Review the result; approval is unavailable on this server.";
    } else {
      next = "The check needs the builder’s attention. Review its saved result.";
    }
    if (statusEl.textContent !== next) statusEl.textContent = next;
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

  // markRunReadPending makes every older response unable to restore a match.
  function markRunReadPending(runId: string, order: number) {
    runObservationOrder.set(runId, order);
    runObservationGeneration.delete(runId);
  }

  // upsertRunSnapshot preserves a terminal report while accepting only a
  // current, non-older observation of its input match.
  function upsertRunSnapshot(
    run: VerificationStatusResult,
    generation: number,
    order: number
  ): VerificationStatusResult {
    const existing = runsById.get(run.run_id);
    const observed = generation === verificationGeneration &&
      order >= (runObservationOrder.get(run.run_id) ?? -1);
    const evidence = existing && existing.state.type !== "running" ? existing : run;
    const snapshot = {
      ...evidence,
      inputs_current: observed ? run.inputs_current : (existing?.inputs_current ?? false),
      candidate_binaries_dir: observed
        ? run.candidate_binaries_dir
        : existing?.candidate_binaries_dir,
    };
    runsById.set(run.run_id, snapshot);
    if (observed) {
      runObservationGeneration.set(run.run_id, generation);
      runObservationOrder.set(run.run_id, order);
      runRequestErrors.delete(run.run_id);
    }
    return snapshot;
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
    if (!runInputsKnown(target)) readVerification(target.run_id, target.change_id);
    syncAllRegions();
  }

  // handleCheckVerificationStatus reads the displayed run and never starts a
  // new verification. A late answer belongs only to that run, even if a
  // different run is displayed by then.
  function handleCheckVerificationStatus() {
    const run = resolveDisplayedVerificationRun();
    if (activeResult?.kind === "cp4-verification" && run !== null) {
      readVerification(run.run_id, run.change_id);
    }
  }

  // readVerification coalesces a read invalidated while pending into one
  // replacement. A same-generation failure waits for an explicit retry.
  function readVerification(runID: string, changeId: string) {
    if (sessionState !== "active") return;
    if (hasPending("get_verification", runID)) {
      runReadAgain.add(runID);
      return;
    }
    runReadAgain.delete(runID);
    const generation = verificationGeneration;
    const order = ++verificationReadOrder;
    markRunReadPending(runID, order);
    const finish = () => {
      const again = runReadAgain.delete(runID) || generation !== verificationGeneration;
      if (again && sessionState === "active" &&
          (activeEditor?.card.change_id === changeId || selectedVerificationChangeId === changeId)) {
        readVerification(runID, changeId);
      }
    };
    runRequestErrors.delete(runID);
    callServer(
      "get_verification",
      { run_id: runID },
      runID,
      (run) => {
        if (run.run_id !== runID || run.change_id !== changeId) {
          if (generation === verificationGeneration && order >= (runObservationOrder.get(runID) ?? -1)) {
            runRequestErrors.set(runID, "The server returned a different verification run.");
          }
          finish();
          return;
        }
        upsertRunSnapshot(run, generation, order);
        if (generation === verificationGeneration && activeResult?.kind === "cp4-verification" && selectedRunId === runID) {
          statusEl.textContent = "Showing the saved verification result.";
        }
        finish();
      },
      (message) => {
        if (generation === verificationGeneration && order >= (runObservationOrder.get(runID) ?? -1)) {
          runRequestErrors.set(runID, message);
          if (activeResult?.kind === "cp4-verification" && selectedRunId === runID) {
            statusEl.textContent = "Could not check verification status; the last result is preserved.";
          }
        }
        finish();
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
    const generation = verificationGeneration;
    const order = ++verificationReadOrder;
    for (const run of runsById.values()) {
      if (run.change_id === requestedChangeId) markRunReadPending(run.run_id, order);
    }

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
          upsertRunSnapshot(payload.runs[i], generation, order);
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
        if (selectedRunId !== null && generation !== verificationGeneration) {
          readVerification(selectedRunId, requestedChangeId);
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
        : "Review the build and its next step. The agreed details stay available below."
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

  // handleAdopt confirms the waiting proposal with the user's answers and its
  // examples: the proposal becomes the agreed revision and the earlier agreed
  // revision is shown as the prior one. Unsaved edits made on the earlier
  // agreed revision are kept as preserved lines, as switching to an incoming
  // revision keeps them. A refused adoption, such as one the guide's newer
  // proposal overtook, reads the change again.
  function handleAdopt() {
    if (!activeEditor || editorBusy) {
      return;
    }
    const card = activeEditor.card;
    const proposal = card.proposed_revision;
    if (proposal === undefined) {
      return;
    }
    const answers: Record<string, string> = {};
    for (const question of proposal.ambiguities ?? []) {
      answers[question.id] = activeEditor.proposalAnswers[question.id];
    }
    statusEl.textContent = "Adopting the proposed revision...";
    callEditorServer(
      "confirm_intent",
      {
        change_id: card.change_id,
        request_revision: proposal.request_revision,
        answers,
        examples: proposal.examples,
      },
      card.change_id,
      (adopted) => {
        const preserved = activeEditor ? snapshotUnsavedDraft(activeEditor) : null;
        mountChangeCard(adopted, preserved, nextPriorCard(activeEditor, adopted));
        statusEl.textContent = "Adopted the proposed revision; it is now the agreed revision.";
        focusResult("#surla-cp1-editor");
      },
      (message, failure) => {
        failProposal(message, failure, card.change_id);
      }
    );
  }

  // handleDecline removes the waiting proposal; the agreed revision and the
  // user's drafts on it stay as they are.
  function handleDecline() {
    if (!activeEditor || editorBusy) {
      return;
    }
    const card = activeEditor.card;
    const proposal = card.proposed_revision;
    if (proposal === undefined) {
      return;
    }
    statusEl.textContent = "Declining the proposed revision...";
    callEditorServer(
      "decline_revision",
      { change_id: card.change_id, request_revision: proposal.request_revision },
      card.change_id,
      (declined) => {
        if (activeEditor && activeEditor.card.request_revision === declined.request_revision) {
          activeEditor = { ...activeEditor, card: declined, proposalAnswers: {}, proposalError: null };
        } else {
          mountChangeCard(declined, null, nextPriorCard(activeEditor, declined));
        }
        statusEl.textContent = "Declined the proposed revision; the agreed revision stays in force.";
        syncAllRegions();
        focusResult("#surla-cp1-editor");
      },
      (message, failure) => {
        failProposal(message, failure, card.change_id);
      }
    );
  }

  // failProposal shows a refused adopt or decline beside the proposal and,
  // when the change moved on meanwhile, reads it again.
  function failProposal(message: string, failure: Failure, changeId: string) {
    if (activeEditor) {
      activeEditor = { ...activeEditor, proposalError: message };
    }
    statusEl.textContent = "The proposed revision was not changed.";
    syncAllRegions();
    focusResult("#surla-proposal-error");
    if (failure.status === 409) {
      refreshOpenCard(changeId);
    }
  }

  // handleApprove asks the server to record approval of run's passed report
  // for the exact card revision shown. A stale revision uses the editor's
  // recovery. Approval can be recorded without promotion settings; a configured
  // promotion target also checks its release inputs before approval.
  function handleApprove(run: VerificationStatusResult) {
    const current = runsById.get(run.run_id);
    const card = activeEditor?.card;
    if (!current || !card || editorBusy || current.state.type !== "completed" ||
        !current.state.value.report.passed || !runInputsKnown(current) || !current.inputs_current ||
        card.change_id !== current.change_id || card.status !== "confirmed" ||
        card.request_revision !== current.request_revision || approvalUnavailable !== null ||
        changeList === null || !changeList.approval_available) {
      return;
    }
    run = current;
    const report = current.state.value.report;
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
          statusEl.textContent = "Approval is unavailable on this server.";
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
        if (latestCard.change_id !== changeId) {
          const message = "The server returned a different Change Card.";
          cardLoadErrors.set(changeId, message);
          showFormError(message);
          return;
        }
        cardLoadErrors.delete(changeId);
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
      (message) => {
        const error = "Could not load the current card and approval: " + message;
        cardLoadErrors.set(changeId, error);
        showFormError(error);
      }
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
    if (activeEditor?.card.change_id !== card.change_id ||
        activeEditor.card.request_revision !== card.request_revision) verificationGeneration++;
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
          : "Review the build and its next step. The agreed details stay available below."
        : "Review the open questions and examples below, then confirm your intent.";
    rootEl.dataset.state = "cp1-change-card";
    readBuild(card.change_id);
    refreshVerificationObservations(card.change_id);
    syncAllRegions();
  }

  // The tab's visibility pauses the waiting read; a hidden tab asks nothing.
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      verificationGeneration++;
      if (waitController !== null) {
        waitController.abort();
      }
      syncAllRegions();
      return;
    }
    if (activeEditor !== null) refreshVerificationObservations(activeEditor.card.change_id);
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
