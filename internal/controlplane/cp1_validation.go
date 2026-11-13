package controlplane

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// This file holds the Checkpoint 1 checks applied to the caller's analysis and
// to the operator's confirmation: ambiguity questions and examples are checked
// and given code-owned IDs, and answers and confirmed examples are checked
// before a Change Card is confirmed. It also holds the code-authored span
// summaries propose_change and revise_change record when a request fails.

type (
	// cp1FaultStage names the step at which a propose_change or revise_change
	// request failed for a reason other than its input, for the span summary
	// cp1SafeFaultSummary writes.
	cp1FaultStage uint8
)

const (
	// cp1FaultRevision indicates canonical revision hashing failed before saving.
	cp1FaultRevision cp1FaultStage = iota + 1
	// cp1FaultStorageRead indicates loading Change Card storage failed before saving.
	cp1FaultStorageRead
	// cp1FaultStorageSave indicates saving or syncing Change Card storage failed;
	// because directory sync can fail after atomic file rename, whether the new
	// card version was persisted is unconfirmed until get_change is checked.
	cp1FaultStorageSave
)

// ConfirmIntent records the owner's complete answers and example table for a
// Change Card that needs confirmation or is already confirmed, or adopts the
// revision proposed beside a confirmed card. The caller must be the card's
// owner and name a revision the change holds, so an edit based on an older
// revision fails. Every open ambiguity must have an allowed answer.
//
// Naming the proposal's request_revision adopts it: the proposal, with these
// answers and examples, becomes the confirmed revision in force under a new
// request_revision with no approval, and the proposal slot is empty. While a
// proposal waits, naming the confirmed revision is refused: the user adopts
// or declines the proposal first. Otherwise, when a confirmed card's answers
// and examples are unchanged, the card is returned as it is; when they
// changed, the card is confirmed under a new request_revision, so previews
// and verification reports bound to the earlier revision no longer match it.
// The prompt, summary and preserved behaviors never change here: a new
// requirement on an existing card uses revise_change.
//
// An unchanged submission returns the saved card the same way get_change
// does: with its approval in Approval only while that approval still matches
// the promotion target's current inputs, or in UnverifiedApproval when current
// promotion inputs cannot be verified.
func (c *ControlPlane) ConfirmIntent(ctx context.Context, p *gensurla.ConfirmIntentPayload) (*gensurla.ChangeCardResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	if p == nil || strings.TrimSpace(p.ChangeID) == "" || strings.TrimSpace(p.RequestRevision) == "" {
		return nil, gensurla.MakeUnprocessable(errors.New("change_id and request_revision are required"))
	}
	record, changed, err := c.confirm(principal, p)
	if err != nil {
		return nil, err
	}
	if changed {
		return record.card, nil
	}
	// The saved card is checked against the promotion target after c.mu is
	// released, because the target may be slow.
	return c.changeResult(ctx, record)
}

// proposedCardContent turns a proposal's analysis into Change Card content.
// It numbers the ambiguities <changeID>-ambiguity-NN and the examples check_NN
// in list order and returns the preserved behaviors as a new non-nil slice, so
// a card without any hashes them as an empty list. The generated decoders have
// already checked the analysis's structure; the error reports the one rule
// left, an ambiguity that repeats an option.
func proposedCardContent(
	changeID string,
	in *gensurla.CP1Analysis,
) ([]*gensurla.AmbiguityQuestion, []*gensurla.ExampleItem, []string, error) {
	ambiguities, err := validateAmbiguities(changeID, in.Ambiguities)
	if err != nil {
		return nil, nil, nil, err
	}
	examples := make([]*gensurla.ExampleItem, 0, len(in.Examples))
	for i, item := range in.Examples {
		examples = append(examples, &gensurla.ExampleItem{
			ID:               fmt.Sprintf("check_%02d", i+1),
			Scenario:         item.Scenario,
			InputSummary:     item.InputSummary,
			ExpectedOutcome:  item.ExpectedOutcome,
			VerificationKind: item.VerificationKind,
		})
	}
	return ambiguities, examples, append(make([]string, 0, len(in.PreservedBehaviors)), in.PreservedBehaviors...), nil
}

// revisedCardContent turns a revision's analysis into Change Card content
// against priorExamples, the examples of the card revision being revised. It
// requires every prior example ID to appear exactly once, either as an
// examples[].prior_id or in removed_example_ids, and every ID named there to
// be a prior example. Kept or edited examples keep their prior IDs; each new
// example gets the lowest check_NN ID absent from both the prior and the
// resulting tables. Ambiguities are numbered and checked as for a proposal,
// and the preserved behaviors are returned as a new non-nil slice. The error
// names the first broken rule.
func revisedCardContent(
	changeID string,
	priorExamples []*gensurla.ExampleItem,
	in *gensurla.CP1RevisionAnalysis,
) ([]*gensurla.AmbiguityQuestion, []*gensurla.ExampleItem, []string, error) {
	ambiguities, err := validateAmbiguities(changeID, in.Ambiguities)
	if err != nil {
		return nil, nil, nil, err
	}

	priorMap := make(map[string]bool, len(priorExamples))
	orderedPriorIDs := make([]string, 0, len(priorExamples))
	for _, ex := range priorExamples {
		if ex == nil || strings.TrimSpace(ex.ID) == "" {
			continue
		}
		priorMap[ex.ID] = true
		orderedPriorIDs = append(orderedPriorIDs, ex.ID)
	}

	seenRemoved := make(map[string]bool, len(in.RemovedExampleIds))
	for idx, remID := range in.RemovedExampleIds {
		if !priorMap[remID] {
			return nil, nil, nil, fmt.Errorf("removed_example_ids[%d] references unknown prior example ID %q", idx, remID)
		}
		if seenRemoved[remID] {
			return nil, nil, nil, fmt.Errorf("duplicate ID %q in removed_example_ids", remID)
		}
		seenRemoved[remID] = true
	}

	seenPrior := make(map[string]bool, len(in.Examples))
	for i, item := range in.Examples {
		if item.PriorID == nil {
			continue
		}
		priorID := *item.PriorID
		if !priorMap[priorID] {
			return nil, nil, nil, fmt.Errorf("examples[%d].prior_id references unknown prior example ID %q", i, priorID)
		}
		if seenPrior[priorID] {
			return nil, nil, nil, fmt.Errorf("duplicate prior_id %q at examples[%d]", priorID, i)
		}
		if seenRemoved[priorID] {
			return nil, nil, nil, fmt.Errorf("prior example ID %q appears in both examples[%d].prior_id and removed_example_ids", priorID, i)
		}
		seenPrior[priorID] = true
	}

	for _, pID := range orderedPriorIDs {
		if !seenPrior[pID] && !seenRemoved[pID] {
			return nil, nil, nil, fmt.Errorf("prior example ID %q is omitted from both examples and removed_example_ids", pID)
		}
	}

	// Reserve both exact and trimmed forms of every prior example ID so
	// newly allocated check_NN IDs never collide under confirm_intent's
	// validateConfirmedExamples check.
	reserved := make(map[string]bool, len(priorMap)*2+len(in.Examples))
	for id := range priorMap {
		reserved[id] = true
		reserved[strings.TrimSpace(id)] = true
	}

	examples := make([]*gensurla.ExampleItem, 0, len(in.Examples))
	for _, item := range in.Examples {
		var id string
		if item.PriorID != nil {
			id = *item.PriorID
		} else {
			id = allocateExampleID(reserved)
		}
		examples = append(examples, &gensurla.ExampleItem{
			ID:               id,
			Scenario:         item.Scenario,
			InputSummary:     item.InputSummary,
			ExpectedOutcome:  item.ExpectedOutcome,
			VerificationKind: item.VerificationKind,
		})
	}
	return ambiguities, examples, append(make([]string, 0, len(in.PreservedBehaviors)), in.PreservedBehaviors...), nil
}

// allocateExampleID finds the lowest unused fmt.Sprintf("check_%02d", n) (n >= 1)
// that is absent from reserved, marks it reserved, and returns it.
func allocateExampleID(reserved map[string]bool) string {
	for n := 1; ; n++ {
		candidate := fmt.Sprintf("check_%02d", n)
		if !reserved[candidate] {
			reserved[candidate] = true
			return candidate
		}
	}
}

// validateAmbiguities assigns each ambiguity question its code-owned ID
// (`<changeID>-ambiguity-NN`, in list order) and rejects a question that
// repeats an option, comparing options both exactly and with surrounding
// whitespace removed. It returns nil when in is empty, so a card without
// ambiguities hashes them as null, as saved cards always have.
func validateAmbiguities(changeID string, in []*gensurla.CP1AnalysisAmbiguity) ([]*gensurla.AmbiguityQuestion, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]*gensurla.AmbiguityQuestion, 0, len(in))
	for i, q := range in {
		id := fmt.Sprintf("%s-ambiguity-%02d", changeID, i+1)
		seenOpts := make(map[string]bool, len(q.Options)*2)
		opts := make([]string, 0, len(q.Options))
		for j, opt := range q.Options {
			trimmed := strings.TrimSpace(opt)
			if seenOpts[opt] || seenOpts[trimmed] {
				return nil, fmt.Errorf("ambiguity %q (ambiguities[%d]) has duplicate option at options[%d]", id, i, j)
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

// cp1RejectedAnalysisSummary returns the code-authored span summary for a
// propose_change (revise false) or revise_change (revise true) request whose
// analysis broke a rule that spans fields. It never repeats the caller's text;
// the request's error message names the rule.
func cp1RejectedAnalysisSummary(revise bool) string {
	if revise {
		return "Change Card analysis broke a revision rule (this request did not modify the Change Card); correct the analysis and retry revise_change."
	}
	return "Change Card analysis broke a proposal rule (this request did not save a Change Card); correct the analysis and retry propose_change."
}

// cp1SafeFaultSummary returns a strictly code-authored, request-scoped
// diagnostic summary for a propose_change or revise_change fault span.
// Because MCP masks internal server faults from tool callers, each summary
// states truthfully whether this request's write ran or remained unconfirmed
// and names the concrete operator or retry action without exposing prompts,
// analysis text, credentials, or host paths.
func cp1SafeFaultSummary(revise bool, stage cp1FaultStage) string {
	outcome := "this request did not save a Change Card"
	op := "CP1 proposal"
	if revise {
		outcome = "this request did not modify the Change Card"
		op = "CP1 revision"
	}
	switch stage {
	case cp1FaultRevision:
		return fmt.Sprintf("%s failed while encoding the card revision (%s); ask the server operator to check server diagnostics.", op, outcome)
	case cp1FaultStorageRead:
		return fmt.Sprintf("%s could not read Change Card storage (%s); preserve your request and ask the server operator to check private card storage.", op, outcome)
	case cp1FaultStorageSave:
		if revise {
			return "CP1 revision could not confirm Change Card save; preserve your request, call get_change to check the saved card revision, and ask the server operator to check private card storage before retrying."
		}
		return "CP1 proposal could not confirm Change Card save; preserve your request, call get_change to check whether the card was saved, and ask the server operator to check private card storage before retrying."
	default:
		panic(fmt.Sprintf("unknown cp1FaultStage %d", stage))
	}
}

// confirm applies a confirm_intent submission under c.mu. It returns the
// saved change and true when the submission adopted the proposal or changed
// the answers or examples, or the unchanged saved change and false.
func (c *ControlPlane) confirm(principal Principal, p *gensurla.ConfirmIntentPayload) (storedChangeCard, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	record, ok, err := c.cards.load(p.ChangeID)
	if err != nil {
		return storedChangeCard{}, false, err
	}
	if !ok {
		return storedChangeCard{}, false, gensurla.MakeNotFound(fmt.Errorf("unknown change_id %q", p.ChangeID))
	}
	if record.owner != principal.ID {
		return storedChangeCard{}, false, gensurla.MakeForbidden(fmt.Errorf("principal %q is not the owner of change_id %q", principal.ID, p.ChangeID))
	}
	// base is the revision these answers and examples confirm: the proposal
	// when the request adopts it, otherwise the revision in force.
	base := storedRevision{card: record.card, designSource: record.designSource, stories: record.stories}
	adopting := record.proposed != nil && record.proposed.card.RequestRevision == p.RequestRevision
	switch {
	case adopting:
		base = *record.proposed
	case record.proposed != nil && record.card.RequestRevision == p.RequestRevision:
		return storedChangeCard{}, false, gensurla.MakeConflict(fmt.Errorf(
			"change_id %q has a proposed revision %q waiting; Adopt it (confirm it with its request_revision) or Decline it (decline_revision) before refining the confirmed revision",
			p.ChangeID,
			record.proposed.card.RequestRevision,
		))
	case record.card.RequestRevision != p.RequestRevision:
		return storedChangeCard{}, false, gensurla.MakeConflict(fmt.Errorf("request_revision mismatch for %q: got %q, want %q", p.ChangeID, p.RequestRevision, record.latest().card.RequestRevision))
	}
	existing := base.card

	if err := validateAmbiguityAnswers(existing.Ambiguities, p.Answers); err != nil {
		return storedChangeCard{}, false, gensurla.MakeUnprocessable(err)
	}
	examples, err := validateConfirmedExamples(p.Examples)
	if err != nil {
		return storedChangeCard{}, false, gensurla.MakeUnprocessable(fmt.Errorf("invalid confirmed examples: %w", err))
	}
	// confirmed carries no ui_composition: a composition describes only the
	// revision it was composed for, and changed answers or examples produce a
	// new revision. An unchanged submission returns existing, composition
	// included, below.
	confirmedBy := principal.ID
	confirmed := &gensurla.ChangeCardResult{
		ChangeID:           existing.ChangeID,
		Status:             StatusConfirmed,
		Prompt:             existing.Prompt,
		Summary:            existing.Summary,
		Ambiguities:        existing.Ambiguities,
		Answers:            maps.Clone(p.Answers),
		Examples:           examples,
		PreservedBehaviors: slices.Clone(existing.PreservedBehaviors),
		Stories:            storySummaryItemsOrNil(base.stories),
		ConfirmedBy:        &confirmedBy,
	}
	rev, err := computeCanonicalRevision(confirmed, record.owner, base.designSource)
	if err != nil {
		return storedChangeCard{}, false, err
	}
	if rev == existing.RequestRevision && !adopting {
		// Same answers and examples on an already confirmed card.
		return record, false, nil
	}
	confirmed.RequestRevision = rev
	// Adopting empties the proposal slot; the adopted revision carries no
	// approval, since the user has approved no result of it.
	saved := storedChangeCard{
		card:         confirmed,
		owner:        record.owner,
		designSource: base.designSource,
		stories:      cloneStorySummaries(base.stories),
	}
	if err := c.cards.save(saved); err != nil {
		return storedChangeCard{}, false, err
	}
	return saved, true, nil
}
