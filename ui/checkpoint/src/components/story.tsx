// story.tsx draws the views page's Story stage, read-only: the Story saved
// with the open change's revision and the project owner's current Story
// evaluation. Each flow is a row of step cards by actor role, marked as
// tested end to end or as an untested draft with the reason its own data
// gives. It also compares a draft with the change's Story by content.

import React from "react";
import {
  ChangeCardResult,
  StoryFlowItem,
  StoryQuestionItem,
  StoryRefreshResult,
  StoryStepItem,
  StorySummaryItem,
  isPlainObject,
} from "../types";
import { Alert, Badge, Button, Card } from "./shadcn";

// StoryCounts is how many flows a set of Stories has and how many of them
// are drafts.
export interface StoryCounts {
  // flows counts every flow of every Story.
  flows: number;
  // drafts counts the flows the server does not report as complete.
  drafts: number;
}

// StoryDifference names how a draft's Stories differ from a change's
// Stories. A Story is identified by its name and a flow by its Story's name
// and its key; flows are named by title.
export interface StoryDifference {
  // same is true when both have the same content.
  same: boolean;
  // added names flows only the draft has.
  added: string[];
  // removed names flows only the change has.
  removed: string[];
  // changed names flows both have whose content differs other than by the
  // order of their steps.
  changed: string[];
  // stepOrder names flows both have whose steps are the same in another order.
  stepOrder: string[];
  // flowOrder names Stories whose flows present in both appear in another order.
  flowOrder: string[];
  // details names Stories whose description or roles differ, or that have no
  // flows and exist on one side only.
  details: string[];
  // storyOrder is true when the Stories present in both appear in another order.
  storyOrder: boolean;
}

// StoryStageProps supplies the open change, if any, and the owner's current
// Story evaluation as the page last read it.
export interface StoryStageProps {
  // card is the change shown in the editor, or null with no change open.
  card: ChangeCardResult | null;
  // evaluation is the last evaluation read, or null before the first read.
  evaluation: StoryRefreshResult | null;
  // loading reflects an outstanding get_story_refresh request.
  loading: boolean;
  // error describes the last failed read of the evaluation, or null.
  error: string | null;
  // onRetry reads the evaluation again after a failure.
  onRetry: () => void;
}

// STATE_ANNOUNCEMENTS is what assistive technology announces, once, when the
// current draft's evaluation reaches each state.
const STATE_ANNOUNCEMENTS: Record<StoryRefreshResult["state"], string> = {
  none: "",
  running: "The Story evaluation is running.",
  succeeded: "The Story evaluation succeeded.",
  failed: "The Story evaluation failed.",
};

// STEP_KIND_LABELS names a step's position in its flow.
const STEP_KIND_LABELS: Record<StoryStepItem["kind"], string> = {
  start: "Starts the flow",
  step: "Step",
  outcome: "Outcome",
};

// StoryStageView shows the Story of the open change and the current draft,
// and, when both exist, whether the draft differs from the change's Story.
export function StoryStageView({ card, evaluation, loading, error, onRetry }: StoryStageProps) {
  const changeStories = card !== null ? card.stories ?? [] : [];
  return (
    <Card
      id="surla-story-stage"
      className="surla-card surla-story-stage"
      role="region"
      aria-labelledby="surla-story-title"
      aria-busy={loading ? "true" : undefined}
    >
      <div className="surla-region-header">
        <h2 id="surla-story-title">Story</h2>
      </div>
      {card !== null ? (
        <section id="surla-story-change" aria-labelledby="surla-story-change-title">
          <h3 id="surla-story-change-title">Story of this change</h3>
          <p>
            Saved with revision <code>{card.request_revision.slice(0, 15)}</code> of Change Card{" "}
            <code>{card.change_id}</code>.{" "}
            {changeStories.length > 0
              ? "Its Story has " + countsPhrase(storyCounts(changeStories)) + "."
              : null}
          </p>
          {changeStories.length === 0 ? (
            <p id="surla-story-change-empty">
              This change has no Story. The guide adds one by revising the change with a refreshed
              Story.
            </p>
          ) : (
            <StoryDrawing stories={changeStories} label={"Story of Change Card " + card.change_id} />
          )}
        </section>
      ) : null}
      <section
        id="surla-story-draft"
        data-state={evaluation !== null ? evaluation.state : "unread"}
        aria-labelledby="surla-story-draft-title"
      >
        <h3 id="surla-story-draft-title">Current Story draft</h3>
        <p>
          The guide's latest Story evaluation of the project's design files. You, the guide and
          the builder all read this same evaluation; the server keeps it in memory only.
        </p>
        <p id="surla-story-draft-announcement" className="surla-visually-hidden" role="status">
          {evaluation !== null ? STATE_ANNOUNCEMENTS[evaluation.state] : ""}
        </p>
        {error !== null ? (
          <Alert id="surla-story-draft-error" variant="destructive" role="alert">
            <strong>Could not read the current draft. </strong>
            {error}
            <div className="surla-field-row">
              <Button id="surla-story-draft-retry" disabled={loading} onClick={onRetry}>
                Read the draft again
              </Button>
            </div>
          </Alert>
        ) : null}
        {evaluation === null ? (
          error === null ? <p role="status">Reading the current draft...</p> : null
        ) : (
          <DraftBody card={card} evaluation={evaluation} />
        )}
      </section>
    </Card>
  );
}

// StoryDrawing draws stories in declaration order: each flow a row with its
// title, its mark, and its steps as cards in order.
export function StoryDrawing({ stories, label }: { stories: StorySummaryItem[]; label: string }) {
  return (
    <div className="surla-story" role="group" aria-label={label}>
      {stories.map((story) => {
        const actors = new Map(story.actors.map((actor) => [actor.key, actor.title]));
        return (
          <section
            key={story.name}
            className="surla-story-section"
            data-story-name={story.name}
            aria-label={"Story: " + story.name}
          >
            <h4>{story.name}</h4>
            {story.description ? <p className="surla-story-muted">{story.description}</p> : null}
            {story.flows.length === 0 ? (
              <p>This Story declares no flows.</p>
            ) : (
              <ol className="surla-story-flows">
                {story.flows.map((flow) => (
                  <FlowRow key={flow.key} flow={flow} actors={actors} />
                ))}
              </ol>
            )}
          </section>
        );
      })}
    </div>
  );
}

// storyCounts counts the flows of stories and the drafts among them.
export function storyCounts(stories: StorySummaryItem[]): StoryCounts {
  let flows = 0;
  let drafts = 0;
  for (const story of stories) {
    for (const flow of story.flows) {
      flows += 1;
      if (!flow.complete) {
        drafts += 1;
      }
    }
  }
  return { flows, drafts };
}

// countsPhrase states flow and draft counts in words, such as "3 flows,
// 1 of them a draft that will not be tested".
export function countsPhrase(counts: StoryCounts): string {
  const flows = counts.flows === 1 ? "1 flow" : counts.flows + " flows";
  if (counts.flows === 0) {
    return "no flows";
  }
  if (counts.drafts === 0) {
    return flows + (counts.flows === 1 ? ", complete" : ", all complete");
  }
  if (counts.drafts === 1) {
    return flows + ", 1 of them a draft that will not be tested";
  }
  return flows + ", " + counts.drafts + " of them drafts that will not be tested";
}

// draftReasons lists why a flow is not complete, from its own data: no
// starting step, no outcome, or the open questions on the flow and its steps.
export function draftReasons(flow: StoryFlowItem): string[] {
  const reasons: string[] = [];
  if (!flow.steps.some((step) => step.kind === "start")) {
    reasons.push("No step starts the flow.");
  }
  if (!flow.steps.some((step) => step.kind === "outcome")) {
    reasons.push("No step is an outcome.");
  }
  const open = flow.questions.concat(...flow.steps.map((step) => step.questions));
  if (open.length > 0) {
    reasons.push("Open questions: " + open.map((question) => question.prompt).join(" "));
  }
  if (reasons.length === 0) {
    reasons.push("The server reports this flow as not complete.");
  }
  return reasons;
}

// compareStories compares a draft's Stories with a change's Stories by
// content and names every difference. A change saved without Stories has
// none.
export function compareStories(
  change: StorySummaryItem[],
  draft: StorySummaryItem[]
): StoryDifference {
  const before = flowsByIdentity(change);
  const after = flowsByIdentity(draft);
  const difference: StoryDifference = {
    same: canonicalJSON(change) === canonicalJSON(draft),
    added: [],
    removed: [],
    changed: [],
    stepOrder: [],
    flowOrder: [],
    details: [],
    storyOrder: false,
  };
  for (const [identity, flow] of after) {
    const prior = before.get(identity);
    if (prior === undefined) {
      difference.added.push(flow.label);
    } else if (prior.content !== flow.content) {
      (prior.unordered === flow.unordered ? difference.stepOrder : difference.changed).push(flow.label);
    }
  }
  for (const [identity, flow] of before) {
    if (!after.has(identity)) {
      difference.removed.push(flow.label);
    }
  }
  const beforeStories = new Map(change.map((story) => [story.name, story]));
  const afterStories = new Map(draft.map((story) => [story.name, story]));
  for (const name of new Set([...beforeStories.keys(), ...afterStories.keys()])) {
    const prior = beforeStories.get(name);
    const next = afterStories.get(name);
    if (prior === undefined || next === undefined) {
      if ((prior ?? next)?.flows.length === 0) {
        difference.details.push(name);
      }
      continue;
    }
    if (storyOutline(prior) !== storyOutline(next)) {
      difference.details.push(name);
    }
    const priorKeys = prior.flows.map((flow) => flow.key);
    const nextKeys = next.flows.map((flow) => flow.key);
    if (!sameOrder(priorKeys.filter((key) => nextKeys.includes(key)), nextKeys.filter((key) => priorKeys.includes(key)))) {
      difference.flowOrder.push(name);
    }
  }
  const beforeNames = change.map((story) => story.name);
  const afterNames = draft.map((story) => story.name);
  difference.storyOrder = !sameOrder(
    beforeNames.filter((name) => afterStories.has(name)),
    afterNames.filter((name) => beforeStories.has(name))
  );
  return difference;
}

// DraftBody shows the evaluation's state and, once it succeeded, its Stories
// and how they compare with the open change's Story.
function DraftBody({ card, evaluation }: { card: ChangeCardResult | null; evaluation: StoryRefreshResult }) {
  switch (evaluation.state) {
    case "none":
      return (
        <p id="surla-story-draft-none">
          No Story evaluation has run since the Surla server started. Ask the guide to refresh the
          Story; the result appears here by itself.
        </p>
      );
    case "running":
      return (
        <p id="surla-story-draft-running">
          The guide's Story evaluation is running for snapshot{" "}
          <code>{evaluation.snapshot_digest ?? "unknown"}</code>. The result appears here when it
          finishes.
        </p>
      );
    case "failed":
      return (
        <div id="surla-story-draft-failed" className="shadcn-alert-destructive">
          <p>
            <strong>The Story evaluation failed</strong> for snapshot{" "}
            <code>{evaluation.snapshot_digest ?? "unknown"}</code>. The server reported:
          </p>
          <pre id="surla-story-draft-diagnostic">{evaluation.diagnostic ?? evaluation.message}</pre>
          <p>Ask the guide to fix the Story and refresh it.</p>
        </div>
      );
    case "succeeded":
      return <SucceededDraft card={card} evaluation={evaluation} />;
  }
}

// SucceededDraft draws a succeeded evaluation's Stories. With a change open
// it first says whether they differ from the change's Story and keeps the
// drawing behind a disclosure, since the change's Story is drawn above.
function SucceededDraft({ card, evaluation }: { card: ChangeCardResult | null; evaluation: StoryRefreshResult }) {
  const counts = storyCounts(evaluation.stories);
  const drawing =
    evaluation.stories.length === 0 ? (
      <p id="surla-story-draft-empty">The design declares no Story.</p>
    ) : (
      <StoryDrawing stories={evaluation.stories} label="Current Story draft" />
    );
  const summary =
    "Evaluated snapshot " +
    (evaluation.snapshot_digest ?? "unknown") +
    "." +
    (evaluation.stories.length > 0 ? " The draft has " + countsPhrase(counts) + "." : "");
  if (card === null) {
    return (
      <div id="surla-story-draft-succeeded">
        <p>{summary}</p>
        {drawing}
      </div>
    );
  }
  const difference = compareStories(card.stories ?? [], evaluation.stories);
  return (
    <div id="surla-story-draft-succeeded">
      {difference.same ? (
        <p id="surla-story-comparison" data-same="true">
          This change's Story is current: the latest draft has the same content.
        </p>
      ) : (
        <div id="surla-story-comparison" data-same="false" role="note">
          <p>
            <strong>The current draft differs from this change's Story.</strong>
          </p>
          <StoryDifferenceList difference={difference} subject="the draft" />
          <p id="surla-story-rule">
            The draft is not part of this change until the guide revises the change with it and you
            confirm the new revision.
          </p>
        </div>
      )}
      <details id="surla-story-draft-details" className="surla-details">
        <summary>{"Show the current draft. " + summary}</summary>
        {drawing}
      </details>
    </div>
  );
}

// StoryDifferenceList names every way a Story differs from the change's
// Story, one item per kind of difference; subject names what differs, such
// as "the draft".
export function StoryDifferenceList({ difference, subject }: { difference: StoryDifference; subject: string }) {
  return (
    <ul className="surla-story-differences">
      <DifferenceItem kind="added" label={"Flows added in " + subject} names={difference.added} />
      <DifferenceItem kind="removed" label={"Flows removed in " + subject} names={difference.removed} />
      <DifferenceItem kind="changed" label={"Flows changed in " + subject} names={difference.changed} />
      <DifferenceItem kind="step-order" label="Flows whose steps are in another order" names={difference.stepOrder} />
      <DifferenceItem kind="flow-order" label="Stories whose flows are in another order" names={difference.flowOrder} />
      <DifferenceItem kind="details" label="Stories whose description or roles changed" names={difference.details} />
      {difference.storyOrder ? <li data-difference="story-order">The Stories are in another order.</li> : null}
    </ul>
  );
}

// DifferenceItem lists the names of one kind of difference, or nothing.
function DifferenceItem({ kind, label, names }: { kind: string; label: string; names: string[] }) {
  if (names.length === 0) {
    return null;
  }
  return (
    <li data-difference={kind}>
      {label + ": "}
      {names.map((name, index) => (
        <React.Fragment key={name + String(index)}>
          {index > 0 ? ", " : null}
          <strong>{name}</strong>
        </React.Fragment>
      ))}
    </li>
  );
}

// FlowRow draws one flow: its title and mark, why it is a draft, its steps
// as cards in a row that scrolls inside the stage, and its open questions.
function FlowRow({ flow, actors }: { flow: StoryFlowItem; actors: Map<string, string> }) {
  return (
    <li className="surla-story-flow" data-flow-key={flow.key} data-complete={flow.complete ? "true" : "false"}>
      <div className="surla-story-flow-header">
        <h5>{flow.title}</h5>
        {flow.complete ? (
          <Badge variant="success" className="surla-story-mark">
            will be tested end to end
          </Badge>
        ) : (
          <Badge variant="warning" className="surla-story-mark">
            draft: not tested
          </Badge>
        )}
      </div>
      {flow.description ? <p className="surla-story-muted">{flow.description}</p> : null}
      {!flow.complete ? (
        <ul className="surla-story-reasons" aria-label={"Why " + flow.title + " is a draft"}>
          {draftReasons(flow).map((reason) => (
            <li key={reason}>{reason}</li>
          ))}
        </ul>
      ) : null}
      {flow.steps.length === 0 ? <p>This flow has no steps yet.</p> : <StepRow flow={flow} actors={actors} />}
      <QuestionList questions={flow.questions} label={"Open questions on " + flow.title} />
    </li>
  );
}

// StepRow draws a flow's steps in order as cards in one row that scrolls
// inside the stage. While its cards overflow it, the row is a keyboard stop
// so it can be scrolled without a pointer; otherwise it is not.
function StepRow({ flow, actors }: { flow: StoryFlowItem; actors: Map<string, string> }) {
  const ref = React.useRef<HTMLOListElement>(null);
  const [scrolls, setScrolls] = React.useState(false);
  React.useLayoutEffect(() => {
    const row = ref.current;
    if (row === null) {
      return;
    }
    const measure = () => setScrolls(row.scrollWidth > row.clientWidth);
    measure();
    window.addEventListener("resize", measure);
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(measure);
    observer?.observe(row);
    return () => {
      window.removeEventListener("resize", measure);
      observer?.disconnect();
    };
  }, [flow]);
  return (
    <ol
      ref={ref}
      className="surla-story-steps"
      aria-label={"Steps of " + flow.title}
      tabIndex={scrolls ? 0 : undefined}
    >
      {flow.steps.map((step) => (
        <li key={step.key} className="surla-story-step" data-step-key={step.key} data-kind={step.kind}>
          <span className="surla-story-actor">{actors.get(step.actor_key) ?? step.actor_key}</span>
          <p className="surla-story-summary">{step.summary}</p>
          <span className="surla-story-kind">{STEP_KIND_LABELS[step.kind]}</span>
          {step.description ? <p className="surla-story-muted">{step.description}</p> : null}
          <QuestionList questions={step.questions} label={"Open questions on " + step.summary} />
        </li>
      ))}
    </ol>
  );
}

// QuestionList shows open questions with their options, read-only.
function QuestionList({ questions, label }: { questions: StoryQuestionItem[]; label: string }) {
  if (questions.length === 0) {
    return null;
  }
  return (
    <ul className="surla-story-questions" aria-label={label}>
      {questions.map((question) => (
        <li key={question.key} data-question-key={question.key}>
          <strong>{question.prompt}</strong>
          {question.description ? <p className="surla-story-muted">{question.description}</p> : null}
          {question.options.length > 0 ? (
            <ul className="surla-story-options" aria-label="Suggested answers">
              {question.options.map((option) => (
                <li key={option}>{option}</li>
              ))}
            </ul>
          ) : (
            <p className="surla-story-muted">No answers suggested.</p>
          )}
        </li>
      ))}
    </ul>
  );
}

// FlowIdentity is one flow keyed by its Story's name and its key, with the
// name people read and its content in comparable forms.
interface FlowIdentity {
  // label names the flow for people.
  label: string;
  // content is the flow's content with each step's role resolved.
  content: string;
  // unordered is content with the steps ordered by key, so two flows whose
  // steps differ only in order have the same unordered content.
  unordered: string;
}

// flowsByIdentity indexes every flow of stories by Story name and flow key.
// Each step's content includes its actor's role, so renaming a role changes
// the flows that use it.
function flowsByIdentity(stories: StorySummaryItem[]): Map<string, FlowIdentity> {
  const flows = new Map<string, FlowIdentity>();
  for (const story of stories) {
    const actors = new Map(story.actors.map((actor) => [actor.key, actor.title]));
    for (const flow of story.flows) {
      const steps = flow.steps.map((step) => ({ step, role: actors.get(step.actor_key) ?? null }));
      const byKey = [...steps].sort((a, b) => (a.step.key < b.step.key ? -1 : a.step.key > b.step.key ? 1 : 0));
      flows.set(JSON.stringify([story.name, flow.key]), {
        label: stories.length > 1 ? story.name + ": " + flow.title : flow.title,
        content: canonicalJSON({ ...flow, steps }),
        unordered: canonicalJSON({ ...flow, steps: byKey }),
      });
    }
  }
  return flows;
}

// storyOutline is a Story's content outside its flows: its description and
// its roles.
function storyOutline(story: StorySummaryItem): string {
  return canonicalJSON({ description: story.description ?? null, actors: story.actors });
}

// sameOrder reports whether two lists hold the same values in the same order.
function sameOrder(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((value, index) => value === b[index]);
}

// canonicalJSON encodes value with object keys sorted and absent optional
// fields left out, so equal content encodes equally whatever key order the
// answer had.
function canonicalJSON(value: unknown): string {
  if (Array.isArray(value)) {
    return "[" + value.map(canonicalJSON).join(",") + "]";
  }
  if (isPlainObject(value)) {
    return (
      "{" +
      Object.keys(value)
        .filter((key) => value[key] !== undefined)
        .sort()
        .map((key) => JSON.stringify(key) + ":" + canonicalJSON(value[key]))
        .join(",") +
      "}"
    );
  }
  return JSON.stringify(value);
}
