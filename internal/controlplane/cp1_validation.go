package controlplane

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// This file holds the Checkpoint 1 checks applied to model output and to the
// operator's confirmation: ambiguity questions and examples are validated and
// given code-owned IDs, and answers and confirmed examples are checked before
// a Change Card is confirmed.

// validateCP1Analysis validates all model-returned CP1 fields strictly without
// silent coercion and assigns code-owned mechanical ambiguity and check IDs.
func validateCP1Analysis(
	changeID string,
	in CP1SemanticAnalysis,
) (string, []*gensurla.AmbiguityQuestion, []*gensurla.ExampleItem, []string, error) {
	if strings.TrimSpace(in.Summary) == "" {
		return "", nil, nil, nil, errors.New("summary is required")
	}
	ambiguities, err := validateAmbiguities(changeID, in.Ambiguities)
	if err != nil {
		return "", nil, nil, nil, err
	}
	if len(in.Examples) == 0 {
		return "", nil, nil, nil, errors.New("at least one example is required")
	}
	examples := make([]*gensurla.ExampleItem, 0, len(in.Examples))
	for i, item := range in.Examples {
		if strings.TrimSpace(item.Scenario) == "" ||
			strings.TrimSpace(item.InputSummary) == "" ||
			strings.TrimSpace(item.ExpectedOutcome) == "" {
			return "", nil, nil, nil, fmt.Errorf("examples[%d] requires non-empty scenario, input_summary, and expected_outcome", i)
		}
		if item.VerificationKind != "deterministic" && item.VerificationKind != "model_judged" {
			return "", nil, nil, nil, fmt.Errorf("examples[%d] has invalid verification_kind %q", i, item.VerificationKind)
		}
		examples = append(examples, &gensurla.ExampleItem{
			ID:               fmt.Sprintf("%s-check-%02d", changeID, i+1),
			Scenario:         item.Scenario,
			InputSummary:     item.InputSummary,
			ExpectedOutcome:  item.ExpectedOutcome,
			VerificationKind: item.VerificationKind,
		})
	}
	preserved := make([]string, 0, len(in.PreservedBehaviors))
	for i, item := range in.PreservedBehaviors {
		if strings.TrimSpace(item) == "" {
			return "", nil, nil, nil, fmt.Errorf("preserved_behaviors[%d] is empty", i)
		}
		preserved = append(preserved, item)
	}
	return in.Summary, ambiguities, examples, preserved, nil
}

// validateAmbiguities validates that every ambiguity question has a non-empty
// question and at least two distinct non-empty options, and assigns a
// code-owned question ID (`<changeID>-ambiguity-NN`).
func validateAmbiguities(changeID string, in []CP1ModelAmbiguity) ([]*gensurla.AmbiguityQuestion, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]*gensurla.AmbiguityQuestion, 0, len(in))
	for i, q := range in {
		id := fmt.Sprintf("%s-ambiguity-%02d", changeID, i+1)
		if strings.TrimSpace(q.Question) == "" {
			return nil, fmt.Errorf("ambiguities[%d] requires non-empty question", i)
		}
		if len(q.Options) < 2 {
			return nil, fmt.Errorf("ambiguity %q requires at least 2 options", id)
		}
		seenOpts := make(map[string]bool, len(q.Options)*2)
		opts := make([]string, 0, len(q.Options))
		for _, opt := range q.Options {
			trimmed := strings.TrimSpace(opt)
			if trimmed == "" {
				return nil, fmt.Errorf("ambiguity %q has an empty option", id)
			}
			if seenOpts[opt] || seenOpts[trimmed] {
				return nil, fmt.Errorf("ambiguity %q has duplicate option %q", id, opt)
			}
			seenOpts[opt] = true
			seenOpts[trimmed] = true
			opts = append(opts, opt)
		}
		out = append(out, &gensurla.AmbiguityQuestion{
			ID:       id,
			Question: q.Question,
			Options:  opts,
		})
	}
	return out, nil
}

// validateAmbiguityAnswers verifies that answers resolves every open ambiguity
// question with exactly one of its declared options and names no other
// question. It reports every problem in one error, so a caller fixes all of
// them in one retry: missing answers and answers that are not an option, in
// question order and each with its options quoted, then answers for unknown
// question IDs in sorted order. When an answer is missing or is not an
// option, the error ends by pointing to revise_change for a decision that
// none of the options expresses.
func validateAmbiguityAnswers(questions []*gensurla.AmbiguityQuestion, answers map[string]string) error {
	known := make(map[string]bool, len(questions))
	var problems []string
	for _, q := range questions {
		known[q.ID] = true
		ans, has := answers[q.ID]
		switch {
		case !has || strings.TrimSpace(ans) == "":
			problems = append(problems, fmt.Sprintf("missing confirmation answer for ambiguity %q (options: %q)", q.ID, q.Options))
		case !slices.Contains(q.Options, ans):
			problems = append(problems, fmt.Sprintf("answer %q for ambiguity %q is not one of its options: %q", ans, q.ID, q.Options))
		}
	}
	unresolved := len(problems) > 0
	var unknown []string
	for id := range answers {
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	slices.Sort(unknown)
	for _, id := range unknown {
		problems = append(problems, fmt.Sprintf("unexpected confirmation answer for unknown ambiguity %q", id))
	}
	if len(problems) == 0 {
		return nil
	}
	msg := strings.Join(problems, "; ")
	if unresolved {
		msg += "; each answer must be exactly one of its question's options, and a decision that no option expresses goes in revise_change's prompt"
	}
	return errors.New(msg)
}

// validateConfirmedExamples validates that every confirmed ExampleItem has a
// unique non-empty ID and valid fields.
func validateConfirmedExamples(in []*gensurla.ExampleItem) ([]*gensurla.ExampleItem, error) {
	if len(in) == 0 {
		return nil, errors.New("at least one example is required")
	}
	seenIDs := make(map[string]bool, len(in)*2)
	out := make([]*gensurla.ExampleItem, 0, len(in))
	for i, item := range in {
		if item == nil {
			return nil, fmt.Errorf("examples[%d] is nil", i)
		}
		trimmedID := strings.TrimSpace(item.ID)
		if trimmedID == "" ||
			strings.TrimSpace(item.Scenario) == "" ||
			strings.TrimSpace(item.InputSummary) == "" ||
			strings.TrimSpace(item.ExpectedOutcome) == "" {
			return nil, fmt.Errorf("examples[%d] requires non-empty id, scenario, input_summary, and expected_outcome", i)
		}
		if item.VerificationKind != "deterministic" && item.VerificationKind != "model_judged" {
			return nil, fmt.Errorf("examples[%d] has invalid verification_kind %q", i, item.VerificationKind)
		}
		if seenIDs[item.ID] || seenIDs[trimmedID] {
			return nil, fmt.Errorf("duplicate example id %q", item.ID)
		}
		seenIDs[item.ID] = true
		seenIDs[trimmedID] = true
		out = append(out, &gensurla.ExampleItem{
			ID:               item.ID,
			Scenario:         item.Scenario,
			InputSummary:     item.InputSummary,
			ExpectedOutcome:  item.ExpectedOutcome,
			VerificationKind: item.VerificationKind,
		})
	}
	return out, nil
}
