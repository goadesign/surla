// Package components draws a Story read-only: the Story saved with the open
// change's revision and the project owner's current Story evaluation. Each
// flow is a row of step cards by actor role, marked as tested end to end or
// as an untested draft with the reason its own data gives. It also compares
// a draft with the change's Story by content.

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

// StoryDifference names, by flow title, how a draft's Stories differ from a
// change's Stories. A flow is identified by its Story's name and its key.
export interface StoryDifference {
  // same is true when both have the same content.
  same: boolean;
  // added names flows only the draft has.
  added: string[];
  // removed names flows only the change has.
  removed: string[];
  // changed names flows both have whose content differs.
  changed: string[];
  // details names Stories whose description, actors or flow order differ
  // while no flow of theirs was added, removed or changed.
  details: string[];
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
          The guide's latest Story evaluation of the project's design files. The user, the guide
          and the builder all read this same evaluation; the server keeps it in memory only.
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
// content. A change saved without Stories has none.
export function compareStories(
  change: StorySummaryItem[],
  draft: StorySummaryItem[]
): StoryDifference {
  const before = flowsByIdentity(change);
  const after = flowsByIdentity(draft);
  const added: string[] = [];
  const removed: string[] = [];
  const changed: string[] = [];
  const touched = new Set<string>();
  for (const [identity, flow] of after) {
    const prior = before.get(identity);
    if (prior === undefined) {
      added.push(flow.label);
      touched.add(flow.story);
    } else if (prior.content !== flow.content) {
      changed.push(flow.label);
      touched.add(flow.story);
    }
  }
  for (const [identity, flow] of before) {
    if (!after.has(identity)) {
      removed.push(flow.label);
      touched.add(flow.story);
    }
  }
  const beforeStories = new Map(change.map((story) => [story.name, storyOutline(story)]));
  const afterStories = new Map(draft.map((story) => [story.name, storyOutline(story)]));
  const details: string[] = [];
  for (const name of new Set([...beforeStories.keys(), ...afterStories.keys()])) {
    if (!touched.has(name) && beforeStories.get(name) !== afterStories.get(name)) {
      details.push(name);
    }
  }
  const same = canonicalJSON(change) === canonicalJSON(draft);
  return { same, added, removed, changed, details };
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
        <p id="surla-story-draft-running" role="status">
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
          <ul>
            <DifferenceItem kind="added" label="Flows added in the draft" flows={difference.added} />
            <DifferenceItem kind="removed" label="Flows removed in the draft" flows={difference.removed} />
            <DifferenceItem kind="changed" label="Flows changed in the draft" flows={difference.changed} />
            <DifferenceItem kind="details" label="Other Story details changed" flows={difference.details} />
          </ul>
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

// DifferenceItem lists the names of one kind of difference, or nothing.
function DifferenceItem({ kind, label, flows }: { kind: string; label: string; flows: string[] }) {
  if (flows.length === 0) {
    return null;
  }
  return (
    <li data-difference={kind}>
      {label + ": "}
      {flows.map((name, index) => (
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
      {flow.steps.length === 0 ? (
        <p>This flow has no steps yet.</p>
      ) : (
        <ol className="surla-story-steps" aria-label={"Steps of " + flow.title} tabIndex={0}>
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
      )}
      <QuestionList questions={flow.questions} label={"Open questions on " + flow.title} />
    </li>
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
// name people read and its content in a comparable form.
interface FlowIdentity {
  // story is the name of the flow's Story.
  story: string;
  // label names the flow for people.
  label: string;
  // content is the flow's content with each step's actor title resolved.
  content: string;
}

// flowsByIdentity indexes every flow of stories by Story name and flow key.
// A flow's content includes its steps' actor titles, so renaming a role
// changes the flows that use it.
function flowsByIdentity(stories: StorySummaryItem[]): Map<string, FlowIdentity> {
  const flows = new Map<string, FlowIdentity>();
  for (const story of stories) {
    const actors = new Map(story.actors.map((actor) => [actor.key, actor.title]));
    for (const flow of story.flows) {
      const roles = flow.steps.map((step) => actors.get(step.actor_key) ?? null);
      flows.set(JSON.stringify([story.name, flow.key]), {
        story: story.name,
        label: stories.length > 1 ? story.name + ": " + flow.title : flow.title,
        content: canonicalJSON({ flow, roles }),
      });
    }
  }
  return flows;
}

// storyOutline is a Story's content outside its flows' own content: its
// description, its actors and the order of its flows.
function storyOutline(story: StorySummaryItem): string {
  return canonicalJSON({
    description: story.description ?? null,
    actors: story.actors,
    order: story.flows.map((flow) => flow.key),
  });
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
