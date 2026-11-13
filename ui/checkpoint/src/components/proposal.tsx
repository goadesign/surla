// proposal.tsx shows the revision the guide proposed beside an agreed
// change: every part adopting it would confirm, against the agreed revision
// (the request, summary, questions, examples, preserved behaviors, the
// Story's flows and the design files it was made from), with Adopt as the
// primary action and Decline beside it. The agreed revision stays in force
// until the user acts.

import React from "react";
import { AmbiguityQuestion, ChangeCardResult, ExampleItem, ProposedRevisionItem } from "../types";
import { Alert, Badge, Button, Card } from "./shadcn";
import { compareStories, StoryDifferenceList } from "./story";

// ProposedRevisionViewProps supplies the agreed change, its waiting proposal,
// the user's answers to the proposal's questions, and the actions.
export interface ProposedRevisionViewProps {
  // card is the agreed revision in force.
  card: ChangeCardResult;
  // proposal is the revision the guide proposed beside card.
  proposal: ProposedRevisionItem;
  // answers holds the user's chosen option per proposal question ID.
  answers: Record<string, string>;
  // busy reflects an outstanding Change Card request.
  busy: boolean;
  // error describes the last refused adopt or decline, or null.
  error: string | null;
  // buildRevisionNotice describes adoption's effect on the agreed build.
  buildRevisionNotice: string | null;
  // onAnswer records the user's choice for one proposal question.
  onAnswer: (questionId: string, option: string) => void;
  // onAdopt confirms the proposal with the chosen answers.
  onAdopt: () => void;
  // onDecline removes the proposal, keeping the agreed revision.
  onDecline: () => void;
}

// ProposedRevisionView renders the waiting proposal against the agreed
// revision. Adopt is enabled once every proposal question has an answer.
export function ProposedRevisionView({
  card,
  proposal,
  answers,
  busy,
  error,
  buildRevisionNotice,
  onAnswer,
  onAdopt,
  onDecline,
}: ProposedRevisionViewProps) {
  const questions = proposal.ambiguities ?? [];
  const agreedQuestions = new Set((card.ambiguities ?? []).map((question) => question.question));
  const proposedQuestions = new Set(questions.map((question) => question.question));
  const dropped = (card.ambiguities ?? []).filter((question) => !proposedQuestions.has(question.question));
  const unanswered = questions.filter((question) => !answers[question.id]);
  const story = compareStories(card.stories ?? [], proposal.stories ?? []);
  return (
    <Card
      id="surla-proposal"
      className="surla-card surla-proposal"
      role="region"
      aria-labelledby="surla-proposal-title"
      aria-busy={busy ? "true" : undefined}
      data-request-revision={proposal.request_revision}
    >
      <div className="surla-region-header">
        <div className="surla-region-title-group">
          <h2 id="surla-proposal-title">Proposed revision</h2>
          <Badge variant="warning">Waiting for you</Badge>
        </div>
      </div>
      <p>
        The guide proposed a new revision of this change. The agreed revision stays in force, and
        architecture, verification and approval keep working on it, until you adopt or decline
        this one. Adopting confirms it with your answers and makes it the agreed revision; an
        approval of the agreed revision does not carry over.
      </p>

      <h3>Request</h3>
      {proposal.prompt === card.prompt ? (
        <p id="surla-proposal-prompt" data-changed="false">
          Unchanged: {proposal.prompt}
        </p>
      ) : (
        <div id="surla-proposal-prompt" data-changed="true">
          <p>
            <strong>Proposed: </strong>
            {proposal.prompt}
          </p>
          <p>
            <strong>Agreed: </strong>
            {card.prompt}
          </p>
        </div>
      )}

      <h3>Summary</h3>
      {proposal.summary === card.summary ? (
        <p id="surla-proposal-summary" data-changed="false">
          Unchanged: {proposal.summary}
        </p>
      ) : (
        <div id="surla-proposal-summary" data-changed="true">
          <p>
            <strong>Proposed: </strong>
            {proposal.summary}
          </p>
          <p>
            <strong>Agreed: </strong>
            {card.summary}
          </p>
        </div>
      )}

      <h3>{"Questions to answer (" + questions.length + ")"}</h3>
      {questions.length === 0 ? <p>This revision asks no questions.</p> : null}
      <ul className="surla-proposal-questions">
        {questions.map((question) => (
          <ProposalQuestion
            key={question.id}
            question={question}
            isNew={!agreedQuestions.has(question.question)}
            selected={answers[question.id] ?? ""}
            busy={busy}
            onAnswer={onAnswer}
          />
        ))}
      </ul>
      {dropped.length > 0 ? (
        <p id="surla-proposal-dropped-questions">
          {"No longer asked: " + dropped.map((question) => question.question).join(" ")}
        </p>
      ) : null}

      <h3>{"Examples (" + proposal.examples.length + ")"}</h3>
      <ul id="surla-proposal-examples">
        {exampleChanges(card.examples, proposal.examples).map(({ example, prior, status }) => (
          <li key={status + example.id} data-example-id={example.id} data-status={status}>
            <code>{example.id}</code>{" "}
            <Badge variant={status === "Kept" ? "default" : status === "Removed" ? "destructive" : "accent"}>
              {status}
            </Badge>{" "}
            {prior !== undefined ? (
              <>
                <div className="surla-proposal-example-agreed">
                  {"Agreed: "}
                  <ExampleText example={prior} />
                </div>
                <div className="surla-proposal-example-proposed">
                  {"Proposed: "}
                  <ExampleText example={example} />
                </div>
              </>
            ) : (
              <ExampleText example={example} />
            )}
          </li>
        ))}
      </ul>

      <h3>{"Preserved behaviors (" + proposal.preserved_behaviors.length + ")"}</h3>
      {proposal.preserved_behaviors.length === 0 ? <p>None declared.</p> : null}
      <ul id="surla-proposal-preserved">
        {proposal.preserved_behaviors.map((behavior, index) => (
          <li key={String(index)} data-new={card.preserved_behaviors.includes(behavior) ? "false" : "true"}>
            {behavior} {card.preserved_behaviors.includes(behavior) ? null : <Badge variant="accent">New</Badge>}
          </li>
        ))}
      </ul>
      {card.preserved_behaviors.some((behavior) => !proposal.preserved_behaviors.includes(behavior)) ? (
        <p id="surla-proposal-dropped-preserved">
          {"No longer preserved: " +
            card.preserved_behaviors
              .filter((behavior) => !proposal.preserved_behaviors.includes(behavior))
              .join(" ")}
        </p>
      ) : null}

      <h3>Story</h3>
      {story.same ? (
        <p id="surla-proposal-story" data-same="true">
          The Story is unchanged.
        </p>
      ) : (
        <div id="surla-proposal-story" data-same="false">
          <StoryDifferenceList difference={story} subject="this revision" />
        </div>
      )}

      <h3>Design files</h3>
      {proposal.changed_design_files.length === 0 ? (
        <p id="surla-proposal-design" data-changed="false">
          Made from the same design files as the agreed revision.
        </p>
      ) : (
        <div id="surla-proposal-design" data-changed="true">
          <p>Made from different design files than the agreed revision. Changed:</p>
          <ul>
            {proposal.changed_design_files.map((path) => (
              <li key={path}>
                <code>{path}</code>
              </li>
            ))}
          </ul>
        </div>
      )}

      {error !== null ? (
        <Alert id="surla-proposal-error" variant="destructive" role="alert" tabIndex={-1}>
          <strong>The proposed revision was not changed. </strong>
          {error}
        </Alert>
      ) : null}
      {buildRevisionNotice !== null ? (
        <p id="surla-proposal-build-consequence">{buildRevisionNotice}</p>
      ) : null}
      <div className="surla-field-row">
        <Button
          id="surla-proposal-adopt"
          variant="primary"
          disabled={busy || unanswered.length > 0}
          aria-describedby={[
            unanswered.length > 0 ? "surla-proposal-adopt-hint" : "",
            buildRevisionNotice !== null ? "surla-proposal-build-consequence" : "",
          ].filter(Boolean).join(" ") || undefined}
          onClick={onAdopt}
        >
          Adopt this revision
        </Button>
        <Button id="surla-proposal-decline" disabled={busy} onClick={onDecline}>
          Decline
        </Button>
      </div>
      {unanswered.length > 0 ? (
        <p id="surla-proposal-adopt-hint">Answer every question to adopt this revision.</p>
      ) : null}
    </Card>
  );
}

// ProposalQuestion offers one proposal question's options as pressable
// choices, marking a question the agreed revision did not ask.
function ProposalQuestion({
  question,
  isNew,
  selected,
  busy,
  onAnswer,
}: {
  question: AmbiguityQuestion;
  isNew: boolean;
  selected: string;
  busy: boolean;
  onAnswer: (questionId: string, option: string) => void;
}) {
  return (
    <li data-question-id={question.id} data-new={isNew ? "true" : "false"}>
      <div>
        <strong>{question.question}</strong> {isNew ? <Badge variant="accent">New</Badge> : null}
      </div>
      <div role="group" aria-label={question.question}>
        {question.options.map((option) => (
          <Button
            key={option}
            variant="pill"
            disabled={busy}
            data-option={option}
            aria-pressed={selected === option ? "true" : "false"}
            onClick={() => onAnswer(question.id, option)}
          >
            {option}
          </Button>
        ))}
      </div>
    </li>
  );
}

// ExampleChange is one example as the proposal view lists it, with how it
// differs from the agreed revision.
interface ExampleChange {
  // example is the proposal's example, or the agreed one it removes.
  example: ExampleItem;
  // prior is the agreed example an Edited example would replace.
  prior?: ExampleItem;
  // status says how the example differs from the agreed revision.
  status: "New" | "Edited" | "Kept" | "Removed";
}

// exampleChanges lists the proposal's examples, each marked New, Edited or
// Kept against the agreed example with the same ID, followed by the agreed
// examples the proposal removes.
function exampleChanges(agreed: ExampleItem[], proposed: ExampleItem[]): ExampleChange[] {
  const byID = new Map(agreed.map((example) => [example.id, example]));
  const kept = new Set(proposed.map((example) => example.id));
  const changes: ExampleChange[] = proposed.map((example): ExampleChange => {
    const prior = byID.get(example.id);
    if (prior === undefined) {
      return { example, status: "New" };
    }
    const same =
      prior.scenario === example.scenario &&
      prior.input_summary === example.input_summary &&
      prior.expected_outcome === example.expected_outcome &&
      prior.verification_kind === example.verification_kind;
    return same ? { example, status: "Kept" } : { example, prior, status: "Edited" };
  });
  for (const example of agreed) {
    if (!kept.has(example.id)) {
      changes.push({ example, status: "Removed" });
    }
  }
  return changes;
}

// ExampleText shows an example's scenario, input, expected outcome and kind
// on one line, as the previous-revision panel shows them.
function ExampleText({ example }: { example: ExampleItem }) {
  return (
    <>
      <strong>{example.scenario}</strong>
      {" — Input: " +
        example.input_summary +
        " -> Expected: " +
        example.expected_outcome +
        " [" +
        example.verification_kind +
        "]"}
    </>
  );
}
