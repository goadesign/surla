# Report template

The tester writes `report.md` using this structure. Every finding cites the
observation and the step it came from, so a reader can check it against the
transcript. Write for a product team that did not watch the session: name the
command, output, or doc passage, and quote the participant exactly.

```markdown
# Surla usability study: <task in a few words>

<date> · Surla build <commit> · Moderator: AI UX tester · Participant: AI agent

## Summary

<3–5 sentences: whether the participant reached the goal, how far it got,
the most important problems, and what to fix first.>

## Study design

- **Research questions:** <the questions this study answers>
- **Method:** Moderated think-aloud usability test with one AI participant per
  attempt. The participant had only the README and Surla's own output; the
  moderator had the user documentation and no source code.
- **Participant profile:** <persona given to the participant>
- **Task:** <the challenge text, verbatim>
- **Environment:** <OS, tool versions, credentials present (not values),
  Surla commit for each attempt>
- **Success criteria:** <the milestones defined before the session>

## Results

| Milestone | Outcome | Elapsed time | Hints (level) | Errors | SEQ (1–7) |
| --- | --- | --- | --- | --- | --- |
| <milestone> (attempt N) | Completed / Completed with help / Failed / Not reached / Not valid (after a scope leak) / Not supported on this machine | <mm:ss> | <n (levels)> | <n> | <score> |

<With more than one attempt, give each milestone one row per attempt, so a
reader sees what changed after each fix. These are the first-pass results,
copied from observations.md before the first mid-build change.>

<One paragraph on overall effectiveness, efficiency, and ease, with totals.>

## Mid-build changes

| Change / sub-milestone | Outcome | Elapsed time | Hints (level) | Errors | SEQ (1–7) | Same card and project? | Work redone |
| --- | --- | --- | --- | --- | --- | --- | --- |
| <C1a …> (attempt N) | Completed / Completed with help / Not completed: <reason> / Not delivered: <reason> / Not supported on this machine / Blocked by <capability or bug> / Not valid (after a scope leak) | <mm:ss> | <n (levels)> | <n> | <score> | <yes / no / n/a> | <short> |

<For C2, the route chosen (runtime tuning, or rebuild). For C3, the suite
commit and scenario names the passing verification reported. Say whether
Surla showed what each change made out of date.>

## Comparison with the baseline

<Only when this study was compared with an earlier one. Name its report and
commit.>

| Milestone | Baseline outcome (with its qualification) | This study's outcome | Hints | Errors | Time | Why it changed |
| --- | --- | --- | --- | --- | --- | --- |
| <milestone> | <outcome, for example "completed for the unmodified starter only"> | <outcome> | <baseline → now> | <baseline → now> | <baseline → now> | <fixed finding Fn (PR) / new capability / environment difference / no change> |

<Write "not available" for a value the baseline never recorded, such as an
SEQ it did not ask, and "not retested" for a milestone this study did not
reach. Never estimate. Then list each baseline finding as fixed, improved,
unchanged, regressed, or not retested, with evidence.>

## Findings

Ordered by severity, highest first. Severity uses Nielsen's scale: 0 not a
problem, 1 cosmetic, 2 minor, 3 major, 4 catastrophe (blocks the task).

### F1. <Problem as the user experiences it, in one line>

- **Severity:** <0–4> · **Type:** Bug / Usability issue / Documentation gap / Missing capability
- **Heuristic:** <violated heuristic, if one fits>
- **Where:** <step, command, output, or README section>
- **What happened:** <observed behavior with exact quotes and commands>
- **Expected:** <what the docs or the user's goal led them to expect>
- **Impact:** <time lost, wrong action taken, hint needed, task blocked>
- **Recommendation:** <a concrete change to the tool, output, or docs>
- **Evidence:** <observation numbers and transcript attempt/step>

## Bugs fixed during the study

| Bug | Blocked at | Fix (PR) | Retested in attempt |
| --- | --- | --- | --- |

## What worked well

<Positive observations worth keeping, with evidence.>

## Recommendations

<Prioritized list: the fix, which findings it addresses, and the expected
effect on users.>

## Limitations

<The participant is an AI agent, not a human: it reads faster, and it may
know Go, Goa, or similar tools from training. One participant per attempt.
Restarts after fixes change the experience. Note any milestone this machine
could not run, and anything the facilitator recorded about its own
influence. With mid-build changes: one example of each kind of change, written
by the tester; differences from the baseline's environment, such as
credentials now set; and any bound that ended the session.>

## Appendix: debrief

<The participant's debrief answers, verbatim.>
```
