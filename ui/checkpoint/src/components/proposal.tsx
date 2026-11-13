// proposal.tsx shows the revision the guide proposed beside an agreed
// change: what it would change against the agreed revision (the summary,
// the questions, and the Story's flows), with Adopt as the primary action and
// Decline beside it. The agreed revision stays in force until the user acts.

import React from "react";
import { AmbiguityQuestion, ChangeCardResult, ProposedRevisionItem } from "../types";
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

      {error !== null ? (
        <Alert id="surla-proposal-error" variant="destructive" role="alert" tabIndex={-1}>
          <strong>The proposed revision was not changed. </strong>
          {error}
        </Alert>
      ) : null}
      <div className="surla-field-row">
        <Button
          id="surla-proposal-adopt"
          variant="primary"
          disabled={busy || unanswered.length > 0}
          aria-describedby={unanswered.length > 0 ? "surla-proposal-adopt-hint" : undefined}
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
