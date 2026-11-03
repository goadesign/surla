// Package cp1draft provides the single-owner React Context for the active
// Checkpoint 1 Change Card editor draft (answers and confirmation examples).
// Both the authored shadcn CP1 inspector and all OpenUI decision components
// read and mutate this single draft store by canonical item ID.

import React, { createContext, useContext } from "react";
import { ChangeCardResult, ExampleItem } from "../types";

export interface PreservedDraft {
  fromRevision: string;
  lines: string[];
}

export interface CP1DraftContextValue {
  card: ChangeCardResult;
  draftAnswers: Record<string, string>;
  draftExamples: ExampleItem[];
  rpcInFlight: boolean;
  localExamplesEdited: boolean;
  setAnswer: (ambiguityId: string, option: string) => void;
}

export const CP1DraftContext = createContext<CP1DraftContextValue | null>(null);

// useCP1Draft returns the active CP1DraftContextValue or null if rendered
// outside an active CP1 editor (such as during a standalone partial preview).
export function useCP1Draft(): CP1DraftContextValue | null {
  return useContext(CP1DraftContext);
}

// areExamplesModified reports whether the current draft examples differ from
// the saved card's examples in any field.
export function areExamplesModified(saved: ExampleItem[], draft: ExampleItem[]): boolean {
  if (saved.length !== draft.length) {
    return true;
  }
  for (let i = 0; i < saved.length; i++) {
    const s = saved[i];
    const d = draft[i];
    if (
      s.id !== d.id ||
      s.scenario !== d.scenario ||
      s.input_summary !== d.input_summary ||
      s.expected_outcome !== d.expected_outcome ||
      s.verification_kind !== d.verification_kind
    ) {
      return true;
    }
  }
  return false;
}

// isDraftDirty reports whether the operator has modified any answer or example
// relative to the saved Change Card.
export function isDraftDirty(
  card: ChangeCardResult,
  draftAnswers: Record<string, string>,
  draftExamples: ExampleItem[]
): boolean {
  const initialAnswers = card.answers || {};
  const ambiguities = card.ambiguities || [];
  for (let i = 0; i < ambiguities.length; i++) {
    const id = ambiguities[i].id;
    const draftVal = Object.prototype.hasOwnProperty.call(draftAnswers, id)
      ? draftAnswers[id]
      : "";
    const savedVal = Object.prototype.hasOwnProperty.call(initialAnswers, id)
      ? initialAnswers[id]
      : "";
    if (draftVal !== savedVal) {
      return true;
    }
  }
  return areExamplesModified(card.examples, draftExamples);
}
