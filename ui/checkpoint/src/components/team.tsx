// team.tsx presents the server's registered team facts and explicit user
// actions. The page owns reads, action results, announcements and window effects.

import React, { useCallback, useLayoutEffect, useRef } from "react";
import type { ProjectTeam, TeamRoleState, TeamSnapshot } from "../types";
import { Alert, Badge, Button, Card } from "./shadcn";

// Role is a registered role from the generated team contract.
type Role = TeamRoleState["role"];

// TeamStageProps supplies the shared team read and the page-owned actions.
export interface TeamStageProps {
  // snapshot is the last successful read, retained when a refresh fails.
  snapshot: TeamSnapshot | null;
  // loading reflects an outstanding team read.
  loading: boolean;
  // error is the last read failure; retained facts are stale while it is set.
  error: string | null;
  // starting prevents a second Start agents request while one is pending.
  starting: boolean;
  // disabled prevents actions after the browser session has ended.
  disabled: boolean;
  // showing identifies the roles with an outstanding Show pane request.
  showing: readonly Role[];
  // startReceipt records what Start agents returned at that action's time.
  startReceipt: { at: string; result: ProjectTeam } | null;
  // actionError is the owner's unchanged refusal or request failure text.
  actionError: string | null;
  // onRefresh asks the page to read the team again.
  onRefresh: () => void;
  // onStartAgents asks the owner to prepare the team after an explicit click.
  onStartAgents: () => void;
  // onShowPane asks the owner to show the selected registered role's pane.
  onShowPane: (role: Role) => void;
  // onFocusedControlRemoved lets the page restore focus to the current view
  // when a focused pane action disappears.
  onFocusedControlRemoved: () => void;
}

// GlobalTeamAttentionProps uses the same retained snapshot and pane actions
// as the Team stage; the page identifies whether these are last-read facts.
export interface GlobalTeamAttentionProps {
  // snapshot is the last successful team read, or null before one succeeds.
  snapshot: TeamSnapshot | null;
  // lastRead labels retained facts when the page cannot establish current status.
  lastRead: boolean;
  // disabled prevents actions after the browser session has ended.
  disabled: boolean;
  // showing identifies the roles with an outstanding Show pane request.
  showing: readonly Role[];
  // onShowPane runs only after the user clicks that role's Show pane button.
  onShowPane: (role: Role) => void;
  // onFocusedControlRemoved lets the page restore focus to the current view
  // when a focused attention action disappears.
  onFocusedControlRemoved: () => void;
}

// roleLabels keep registered handles in Details and human role names visible.
const roleLabels: Record<Role, string> = {
  guide: "Guide",
  builder: "Builder",
  test_writer: "Test writer",
};

// stateLabels describe terminal observations without inferring work from assignments.
const stateLabels: Record<TeamRoleState["state"], string> = {
  not_started: "Not started",
  ended: "Ended",
  starting: "Starting",
  needs_you: "Needs you",
  working: "Working",
  waiting: "Waiting",
  error: "Error",
  unknown: "Status unknown",
};

// attentionLabels add only the reason the owner reported; none adds no reason.
const attentionLabels: Record<TeamRoleState["attention"], string> = {
  none: "",
  permission: "permission",
  question: "question",
  auth: "sign-in required",
  silent: "no status reported",
  assignment: "idle with an unread assignment. Show its pane and tell it to read its assignments and to keep its Chorus watcher running.",
  unknown: "reason unknown",
};

// watcherLabels distinguish a held watcher, observed absence and unknown status.
const watcherLabels: Record<TeamRoleState["watcher"], string> = {
  held: "Chorus watcher held",
  gone: "Chorus watcher absent",
  unknown: "Chorus watcher unknown",
};

// assignmentLabels report assignment-read facts independently of terminal state.
const assignmentLabels: Record<TeamRoleState["assignment"], string> = {
  unread: "Assignment unread",
  nothing_owed: "No unread assignment",
  unknown: "Assignment status unknown",
};

// TeamStageView shows only registered roles, retaining and labeling the last
// successful facts after a failed refresh. It performs no reads or window work.
export function TeamStageView({
  snapshot,
  loading,
  error,
  starting,
  disabled,
  showing,
  startReceipt,
  actionError,
  onRefresh,
  onStartAgents,
  onShowPane,
  onFocusedControlRemoved,
}: TeamStageProps) {
  const needsStart = error === null && snapshot !== null && (
    snapshot.roles.length === 0 ||
    snapshot.roles.some((role) => role.state === "not_started" || role.state === "ended")
  ) && !snapshot.roles.some((role) => role.state === "needs_you");

  return (
    <Card id="surla-team-stage" className="surla-card" role="region" aria-labelledby="surla-team-title">
      <div className="surla-region-header">
        <h2 id="surla-team-title" tabIndex={-1}>Project team</h2>
        <div className="surla-field-row">
          <Button
            id="surla-team-start"
            variant={needsStart ? "primary" : "default"}
            disabled={disabled || starting}
            aria-busy={starting}
            onClick={onStartAgents}
          >
            {starting ? "Starting agents…" : "Start agents"}
          </Button>
          <Button
            id="surla-team-refresh"
            variant={error !== null ? "primary" : "default"}
            disabled={disabled || loading}
            onClick={onRefresh}
          >
            {error !== null ? "Retry team read" : "Refresh team"}
          </Button>
        </div>
      </div>
      {error !== null ? (
        <Alert id="surla-team-read-error" variant="destructive">
          <strong>Could not read the team. </strong>{error}
        </Alert>
      ) : null}
      {loading ? (
        <p id="surla-team-loading">{snapshot === null ? "Reading the project team…" : "Updating team…"}</p>
      ) : null}
      {snapshot === null ? (
        !loading && error === null ? <p>The project team has not been read.</p> : null
      ) : (
        <div id="surla-team-facts" aria-busy={loading} data-stale={error !== null}>
          {error !== null ? <p id="surla-team-stale">Last-read team facts; they could not be refreshed.</p> : null}
          <p id="surla-team-name">Team: <strong>{snapshot.team}</strong></p>
          {snapshot.roles.length === 0 ? (
            <p id="surla-team-empty">
              No agents are registered. Start agents prepares the guide; building adds the required roles.
            </p>
          ) : (
            <ul id="surla-team-roles" className="surla-trace-list">
              {snapshot.roles.map((role) => (
                <li key={role.role} id={"surla-team-role-" + role.role} data-state={role.state}>
                  <div className="surla-trace-heading">
                    <strong>{roleLabels[role.role]}</strong>
                    <Badge
                      variant={
                        role.state === "needs_you" ? "warning" :
                        role.state === "error" ? "destructive" : "default"
                      }
                    >
                      {stateLabels[role.state]}
                      {role.state === "needs_you" && attentionLabels[role.attention] !== ""
                        ? ": " + attentionLabels[role.attention] : ""}
                    </Badge>
                  </div>
                  <p>{watcherLabels[role.watcher]}</p>
                  <p>{role.role === "guide" ? "No assignments" : assignmentLabels[role.assignment]}</p>
                  <div className="surla-field-row">
                    <PaneButton
                      role={role}
                      showing={showing}
                      disabled={disabled}
                      onShowPane={onShowPane}
                      onFocusedControlRemoved={onFocusedControlRemoved}
                      global={false}
                    />
                  </div>
                  <details className="surla-details">
                    <summary>{roleLabels[role.role]} details</summary>
                    <p>Registered handle: <code>{role.handle}</code></p>
                  </details>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
      {startReceipt !== null ? (
        <p id="surla-team-start-receipt">
          This start (begun <time dateTime={startReceipt.at}>{startReceipt.at}</time>) returned:{" "}
          «{startReceipt.result.step}»
        </p>
      ) : null}
      {actionError !== null ? (
        <Alert id="surla-team-action-error" variant="destructive" tabIndex={-1}>{actionError}</Alert>
      ) : null}
    </Card>
  );
}

// GlobalTeamAttention renders one line per needs-you role without announcing
// repeated snapshots, moving focus or deriving attention from a start receipt.
export function GlobalTeamAttention({
  snapshot,
  lastRead,
  showing,
  disabled,
  onShowPane,
  onFocusedControlRemoved,
}: GlobalTeamAttentionProps) {
  const roles = snapshot?.roles.filter((role) => role.state === "needs_you") ?? [];
  if (roles.length === 0) {
    return null;
  }
  return (
    <div id="surla-team-attention">
      {roles.map((role) => (
        <div key={role.role} className="surla-field-row" id={"surla-team-attention-" + role.role}>
          <span className="surla-live-wrap">
            {lastRead ? "Last read: " : ""}
            {teamAttentionText(role)}
          </span>
          <PaneButton
            role={role}
            showing={showing}
            disabled={disabled}
            onShowPane={onShowPane}
            onFocusedControlRemoved={onFocusedControlRemoved}
            global={true}
          />
        </div>
      ))}
    </div>
  );
}

// teamAttentionText supplies the same visible sentence to the global notice
// and the page's once-only announcement. Callers select needs_you roles first.
export function teamAttentionText(role: TeamRoleState): string {
  const reason = attentionLabels[role.attention];
  return roleLabels[role.role] + " needs you" + (reason !== "" ? ": " + reason : "");
}

// PaneButton keeps role-specific names and pending behavior consistent across
// both placements while leaving the actual window effect in the page callback.
function PaneButton({
  role,
  showing,
  disabled,
  onShowPane,
  onFocusedControlRemoved,
  global,
}: {
  role: TeamRoleState;
  showing: readonly Role[];
  disabled: boolean;
  onShowPane: (role: Role) => void;
  onFocusedControlRemoved: () => void;
  global: boolean;
}) {
  const pending = showing.includes(role.role);
  const label = roleLabels[role.role].toLowerCase();
  // notifyRemoval keeps the current page callback without changing the DOM
  // ref's identity when the page renders again.
  const notifyRemoval = useRef(onFocusedControlRemoved);
  useLayoutEffect(() => {
    notifyRemoval.current = onFocusedControlRemoved;
  }, [onFocusedControlRemoved]);

  // attachControl checks focus before React removes this pane action. Ordinary
  // snapshot or pending-state updates retain the same ref and do not notify.
  const attachControl = useCallback((node: HTMLSpanElement | null) => {
    if (node === null) {
      return;
    }
    return () => {
      if (node.contains(document.activeElement)) {
        notifyRemoval.current();
      }
    };
  }, []);

  return (
    <span ref={attachControl}>
      <Button
        id={(global ? "surla-team-attention-show-pane-" : "surla-team-show-") + role.role}
        variant={role.state === "needs_you" ? "primary" : "default"}
        disabled={disabled || pending}
        aria-busy={pending}
        onClick={() => onShowPane(role.role)}
      >
        {pending ? "Showing " + label + " pane…" : "Show " + label + " pane"}
      </Button>
    </span>
  );
}
