// live-target.tsx draws the views page's Live target stage: the live target
// the server runs so the builder agent can try prompt wording on a commit,
// with a control that stops it after the user confirms, and the model live
// targets started afterwards use, with a form that changes it. It shows what
// the server reports and never prompt text, which the server does not return.

import React, { useEffect, useRef, useState } from "react";
import { ConfigureModelPayload, LiveTargetStatus, ModelConfigResult, ModelConnection } from "../types";
import { Alert, Badge, Button, Card } from "./shadcn";

// LiveTargetCase is one case of the live target read.
type LiveTargetCase = LiveTargetStatus["live_target"];

// RunningCase is the live target read's running case.
type RunningCase = Extract<LiveTargetCase, { state: "running" }>;

// StoppableCase is a case the page offers to stop: running, or not
// confirmed stopped.
type StoppableCase = Extract<LiveTargetCase, { state: "running" | "not_confirmed_stopped" }>;

// ServerAnswer is what a finished request answered: the server's message,
// and whether it refused.
export interface ServerAnswer {
  // message is the server's text, shown as it is.
  message: string;
  // refused is true when the server refused the request.
  refused: boolean;
}

// LiveTargetStageProps supplies the last reads of the live target and the
// model connection, the state of their requests, and the page's handlers.
export interface LiveTargetStageProps {
  // target is the last live target read, or null before the first read.
  target: LiveTargetStatus | null;
  // targetLoading reflects an outstanding read of the live target.
  targetLoading: boolean;
  // targetError describes the last failed read of the live target, or null.
  targetError: string | null;
  // onRetryTarget reads the live target again.
  onRetryTarget: () => void;
  // stopping reflects an outstanding stop request.
  stopping: boolean;
  // stopOutcome is the answer to the last stop request, or null.
  stopOutcome: ServerAnswer | null;
  // onStop sends the one stop request the user confirmed.
  onStop: () => void;
  // model is the last model connection read, or null before the first read.
  model: ModelConnection | null;
  // modelLoading reflects an outstanding read of the model connection.
  modelLoading: boolean;
  // modelError describes the last failed read of the model connection.
  modelError: string | null;
  // onRetryModel reads the model connection again.
  onRetryModel: () => void;
  // applying reflects an outstanding request to change the model.
  applying: boolean;
  // applyOutcome is the answer to the last change of the model, or null.
  applyOutcome: ServerAnswer | null;
  // onApply asks the server to use choice for live targets started
  // afterwards.
  onApply: (choice: ConfigureModelPayload) => void;
}

// LiveTargetStageView shows the live target and the model connection.
export function LiveTargetStageView(props: LiveTargetStageProps) {
  return (
    <Card
      id="surla-live-stage"
      className="surla-card surla-live-stage"
      role="region"
      aria-labelledby="surla-live-title"
      aria-busy={props.targetLoading || props.modelLoading ? "true" : undefined}
    >
      <div className="surla-region-header">
        <h2 id="surla-live-title">Live target</h2>
      </div>
      <LiveTargetSection {...props} />
      <ModelSection {...props} />
    </Card>
  );
}

// LiveTargetSection shows what runs on the live target and offers to stop
// it while it runs or could not be confirmed stopped.
function LiveTargetSection({
  target,
  targetLoading,
  targetError,
  onRetryTarget,
  stopping,
  stopOutcome,
  onStop,
}: LiveTargetStageProps) {
  const current = target !== null ? target.live_target : null;
  const announcement = useAnnouncement(current);
  const [confirming, setConfirming] = useState(false);
  // stopRowRef holds the row of the Stop control; the shared Button takes
  // no ref, so focus goes to the button inside the row.
  const stopRowRef = useRef<HTMLDivElement>(null);
  const confirmRef = useRef<HTMLDivElement>(null);
  const headingRef = useRef<HTMLHeadingElement>(null);
  // returnFocus is set when the confirmation closes without stopping, so
  // focus goes back to the Stop control once it is drawn again.
  const returnFocus = useRef(false);
  const stoppable: StoppableCase | null =
    current !== null && (current.state === "running" || current.state === "not_confirmed_stopped") ? current : null;

  // A confirmation opened for a target that has since ended closes, so it
  // never reappears for a later target the user did not ask about.
  const nothingToStop = stoppable === null;
  useEffect(() => {
    if (nothingToStop) {
      setConfirming(false);
    }
  }, [nothingToStop]);

  useEffect(() => {
    if (confirming) {
      confirmRef.current?.focus();
    } else if (returnFocus.current) {
      returnFocus.current = false;
      stopRowRef.current?.querySelector("button")?.focus();
    }
  }, [confirming]);

  // keep closes the confirmation and returns focus to the Stop control.
  function keep() {
    returnFocus.current = true;
    setConfirming(false);
  }

  // stop closes the confirmation and sends the one stop request.
  function stop() {
    setConfirming(false);
    headingRef.current?.focus();
    onStop();
  }

  return (
    <section id="surla-live-target" data-state={current !== null ? current.state : "unread"} aria-labelledby="surla-live-target-title">
      <h3 id="surla-live-target-title" ref={headingRef} tabIndex={-1}>
        What runs now
      </h3>
      <p id="surla-live-announcement" className="surla-visually-hidden" role="status">
        {announcement}
      </p>
      {targetError !== null ? (
        <Alert id="surla-live-error" variant="destructive" role="alert">
          <strong>Could not read the live target. </strong>
          <span className="surla-live-wrap">{targetError}</span>
          <div className="surla-field-row">
            <Button id="surla-live-retry" disabled={targetLoading} onClick={onRetryTarget}>
              Read the live target again
            </Button>
          </div>
        </Alert>
      ) : null}
      {current === null ? (
        targetError === null ? <p role="status">Reading the live target from the Surla server...</p> : null
      ) : (
        <LiveTargetBody target={current} />
      )}
      {stoppable !== null && !confirming ? (
        <div className="surla-field-row" ref={stopRowRef}>
          <Button
            id="surla-live-stop"
            disabled={stopping}
            onClick={() => setConfirming(true)}
          >
            {stopping ? "Stopping the live target..." : "Stop live target"}
          </Button>
        </div>
      ) : null}
      {stoppable !== null && confirming ? (
        <div
          id="surla-live-stop-confirm"
          ref={confirmRef}
          role="group"
          aria-labelledby="surla-live-stop-question"
          tabIndex={-1}
          onKeyDown={(event) => {
            if (event.key === "Escape") {
              event.preventDefault();
              keep();
            }
          }}
        >
          <Alert variant="warning">
            <p id="surla-live-stop-question" className="surla-live-wrap">
              Stop the live target for commit <code>{stoppable.candidate_commit.slice(0, 7)}</code>? It
              ends now, and every prompt draft applied to it is discarded; only committed text
              survives.
            </p>
            <div className="surla-field-row">
              <Button id="surla-live-stop-yes" onClick={stop}>
                Stop it
              </Button>
              <Button id="surla-live-stop-keep" onClick={keep}>
                Keep it running
              </Button>
            </div>
          </Alert>
        </div>
      ) : null}
      {stopOutcome !== null ? (
        <Alert
          id="surla-live-stop-outcome"
          variant={stopOutcome.refused ? "destructive" : "default"}
          role={stopOutcome.refused ? "alert" : "status"}
        >
          <span className="surla-live-wrap">{stopOutcome.message}</span>
        </Alert>
      ) : null}
    </section>
  );
}

// LiveTargetBody describes one case of the live target read.
function LiveTargetBody({ target }: { target: LiveTargetCase }) {
  switch (target.state) {
    case "none":
      return (
        <p id="surla-live-none">
          No live target is running. The builder agent starts one from a commit to try prompt
          wording on it.
        </p>
      );
    case "starting":
      return (
        <>
          <p>
            <Badge variant="accent">Starting</Badge>
          </p>
          <p id="surla-live-starting" className="surla-live-wrap">
            Building and starting commit <code>{target.candidate_commit}</code> since{" "}
            <Clock iso={target.since} />. It cannot be stopped until the start finishes.
          </p>
        </>
      );
    case "running":
      return <RunningBody target={target} />;
    case "not_confirmed_stopped":
      return (
        <>
          <p>
            <Badge variant="warning">Not confirmed stopped</Badge>
          </p>
          <p className="surla-live-wrap">
            Commit <code>{target.candidate_commit}</code>.
          </p>
          <p id="surla-live-message" className="surla-live-wrap">
            {target.message}
          </p>
        </>
      );
  }
}

// RunningBody describes a running live target: its commit, since when, its
// address, and the prompts changed in it.
function RunningBody({ target }: { target: RunningCase }) {
  const origin = plainLoopbackOrigin(target.url);
  return (
    <>
      <p>
        <Badge variant="success">Running</Badge>
      </p>
      <p id="surla-live-running" className="surla-live-wrap">
        Commit <code>{target.candidate_commit}</code>, running since <Clock iso={target.started_at} /> at{" "}
        {origin !== null ? (
          <a id="surla-live-address" href={origin} target="_blank" rel="noopener noreferrer">
            {target.url}
          </a>
        ) : (
          <code id="surla-live-address">{target.url}</code>
        )}
        .
      </p>
      {target.prompts.length === 0 ? (
        <p id="surla-live-no-prompts">No prompt has been changed in it yet.</p>
      ) : (
        <>
          <h4>Prompts changed in it</h4>
          <ul id="surla-live-prompts" className="surla-live-prompts">
            {target.prompts.map((prompt) => (
              <li key={prompt.prompt_id} data-prompt-id={prompt.prompt_id} className="surla-live-wrap">
                <code className="surla-live-prompt-id">{prompt.prompt_id}</code> uses version{" "}
                <code className="surla-live-version">{prompt.version}</code>, accepted{" "}
                <Clock iso={prompt.accepted_at} />.
              </li>
            ))}
          </ul>
        </>
      )}
      <p id="surla-live-drafts-note">
        Drafts are not verified. They are discarded when this live target stops, and they survive
        only when their text is committed.
      </p>
    </>
  );
}

// ModelSection shows the model live targets started afterwards use and a
// form that changes it. The form's fields are uncontrolled and both groups
// stay mounted, so what the user typed survives every refresh and a switch
// of kind; only the chosen kind is state.
function ModelSection({
  model,
  modelLoading,
  modelError,
  onRetryModel,
  applying,
  applyOutcome,
  onApply,
}: LiveTargetStageProps) {
  const [kind, setKind] = useState<"direct" | "gateway">("direct");

  // submit sends the choice the form's fields of the chosen kind describe.
  function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const fields = new FormData(event.currentTarget);
    const text = (name: string) => {
      const value = fields.get(name);
      return typeof value === "string" ? value : "";
    };
    if (kind === "gateway") {
      onApply({ gateway_addr: text("gateway_addr") });
      return;
    }
    onApply({ provider: text("provider") === "openai" ? "openai" : "anthropic", model_name: text("model_name") });
  }

  return (
    <section id="surla-model" aria-labelledby="surla-model-title">
      <h3 id="surla-model-title">Model connection</h3>
      {modelError !== null ? (
        <Alert id="surla-model-error" variant="destructive" role="alert">
          <strong>Could not read the model connection. </strong>
          <span className="surla-live-wrap">{modelError}</span>
          <div className="surla-field-row">
            <Button id="surla-model-retry" disabled={modelLoading} onClick={onRetryModel}>
              Read the model connection again
            </Button>
          </div>
        </Alert>
      ) : null}
      {model === null ? (
        modelError === null ? <p role="status">Reading the model connection...</p> : null
      ) : (
        <p id="surla-model-current" data-kind={model.connection.kind} className="surla-live-wrap">
          {describeConnection(model.connection)}
        </p>
      )}
      <p id="surla-model-note">
        The choice applies to live targets started afterwards. A running live target keeps its
        model, and verification is not affected.
      </p>
      <form id="surla-model-form" onSubmit={submit}>
        <fieldset disabled={applying}>
          <legend>Change the model connection</legend>
          <div className="surla-field-row" role="radiogroup" aria-label="Kind of model connection">
            <label>
              <input
                type="radio"
                name="surla-model-kind"
                value="direct"
                checked={kind === "direct"}
                onChange={() => setKind("direct")}
              />{" "}
              A provider and model
            </label>
            <label>
              <input
                type="radio"
                name="surla-model-kind"
                value="gateway"
                checked={kind === "gateway"}
                onChange={() => setKind("gateway")}
              />{" "}
              A model gateway
            </label>
          </div>
          <div className="surla-field-row" hidden={kind !== "direct"}>
            <label htmlFor="surla-model-provider">Provider</label>
            <select id="surla-model-provider" name="provider" defaultValue="anthropic">
              <option value="anthropic">anthropic</option>
              <option value="openai">openai</option>
            </select>
            <label htmlFor="surla-model-name">Model name</label>
            <input id="surla-model-name" name="model_name" type="text" required={kind === "direct"} />
          </div>
          <div className="surla-field-row" hidden={kind !== "gateway"}>
            <label htmlFor="surla-model-gateway">Gateway address (host:port)</label>
            <input id="surla-model-gateway" name="gateway_addr" type="text" required={kind === "gateway"} />
          </div>
          <div className="surla-field-row">
            <Button id="surla-model-apply" type="submit" variant="primary">
              {applying ? "Applying..." : "Apply"}
            </Button>
          </div>
        </fieldset>
      </form>
      {applyOutcome !== null ? (
        <Alert
          id="surla-model-outcome"
          variant={applyOutcome.refused ? "destructive" : "default"}
          role={applyOutcome.refused ? "alert" : "status"}
        >
          <span className="surla-live-wrap">{applyOutcome.message}</span>
        </Alert>
      ) : null}
    </section>
  );
}

// Clock shows a time the server reported in the reader's clock, keeping the
// exact value machine-readable.
function Clock({ iso }: { iso: string }) {
  const parsed = new Date(iso);
  const text = Number.isNaN(parsed.getTime())
    ? iso
    : parsed.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  return <time dateTime={iso}>{text}</time>;
}

// describeConnection says which model live targets started afterwards use.
function describeConnection(connection: ModelConnection["connection"]): string {
  switch (connection.kind) {
    case "none":
      return "No model is chosen: live targets start without a model setting.";
    case "direct":
      return "Live targets started from now on use " + connection.provider + " / " + connection.model_name + ".";
    case "gateway":
      return "Live targets started from now on use the model gateway at " + connection.gateway_addr + ".";
  }
}

// describeApplied says what an accepted change of the model now means.
export function describeApplied(result: ModelConfigResult): string {
  if (result.gateway_addr !== undefined) {
    return "Live targets started from now on use the model gateway at " + result.gateway_addr + ".";
  }
  return "Live targets started from now on use " + (result.provider ?? "") + " / " + (result.model_name ?? "") + ".";
}

// plainLoopbackOrigin returns url's origin when it is a plain local address
// the page may link to: http, a loopback IP literal, an explicit port, and no
// user, path, query or fragment. Anything else returns null and is shown as
// text, because the server passes on the address its evaluator suite reported.
export function plainLoopbackOrigin(url: string): string | null {
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return null;
  }
  const loopback = parsed.hostname === "[::1]" || /^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(parsed.hostname);
  if (
    parsed.protocol !== "http:" ||
    !loopback ||
    parsed.port === "" ||
    parsed.username !== "" ||
    parsed.password !== "" ||
    (parsed.pathname !== "" && parsed.pathname !== "/") ||
    parsed.search !== "" ||
    parsed.hash !== ""
  ) {
    return null;
  }
  return parsed.origin;
}

// useAnnouncement returns what assistive technology announces when the live
// target changes after the page first read it: a start, a target that runs,
// one that ends, one not confirmed stopped, or a prompt whose version
// changed. The first read announces nothing.
function useAnnouncement(current: LiveTargetCase | null): string {
  const previous = useRef<LiveTargetCase | null>(null);
  const [announcement, setAnnouncement] = useState("");
  useEffect(() => {
    const before = previous.current;
    previous.current = current;
    if (before === null || current === null) {
      return;
    }
    const next = announce(before, current);
    if (next !== "") {
      setAnnouncement(next);
    }
  }, [current]);
  return announcement;
}

// announce describes how the live target changed from before to after, or
// returns "" when nothing a reader needs to hear changed.
function announce(before: LiveTargetCase, after: LiveTargetCase): string {
  switch (after.state) {
    case "none":
      return before.state === "none" ? "" : "No live target is running.";
    case "starting":
      return before.state === "starting" && before.since === after.since
        ? ""
        : "A live target is starting from commit " + after.candidate_commit.slice(0, 7) + ".";
    case "not_confirmed_stopped":
      return before.state === "not_confirmed_stopped" ? "" : "The live target could not be confirmed stopped.";
    case "running": {
      if (before.state !== "running" || before.started_at !== after.started_at) {
        return "The live target for commit " + after.candidate_commit.slice(0, 7) + " is running.";
      }
      const versions = new Map(before.prompts.map((prompt) => [prompt.prompt_id, prompt.version]));
      const changed = after.prompts.filter((prompt) => versions.get(prompt.prompt_id) !== prompt.version);
      return changed.map((prompt) => "Prompt " + prompt.prompt_id + " now uses version " + prompt.version + ".").join(" ");
    }
  }
}
