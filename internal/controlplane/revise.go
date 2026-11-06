// This file implements Checkpoint 1 (CP1) Change Card revision (`revise_change`)
// and prior-card semantic re-analysis.

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/goldenpath"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"goa.design/goa-ai/runtime/agent/model"
	goahttp "goa.design/goa/v3/http"
	goa "goa.design/goa/v3/pkg"
)

type (
	// unclassifiedCP1Error wraps a non-rejection CP1 analyzer error (including a
	// multi-cause join that contains a nested Goa ServiceError alongside an
	// independent failure) so the generated HTTP error encoder's
	// errors.As(v, &GoaErrorNamer) check matches this outer unnamed error first
	// and falls through to the HTTP 500 fault encoder, while preserving the
	// complete underlying error tree via Unwrap and keeping MCP tool-error
	// classification unchanged.
	unclassifiedCP1Error struct {
		// cause is the underlying non-rejection error returned by CP1Analyzer.
		cause error
	}

	// CP1PriorDecision describes one ambiguity question and its confirmed answer
	// (when resolved) from the Change Card revision being revised.
	CP1PriorDecision struct {
		// Question is the plain-English ambiguity question.
		Question string `json:"question"`
		// Options lists the candidate answers offered on the prior card.
		Options []string `json:"options"`
		// ConfirmedAnswer is the operator's confirmed choice, or empty when unanswered.
		ConfirmedAnswer string `json:"confirmed_answer"`
	}

	// CP1PriorCard is the semantic snapshot of an existing Change Card passed to
	// CP1Analyzer.Revise when the operator revises requirements on that card.
	CP1PriorCard struct {
		// Prompt is the prior revision's feature request.
		Prompt string `json:"prompt"`
		// Summary is the prior revision's plain-English summary.
		Summary string `json:"summary"`
		// Status is the prior revision's checkpoint status ("needs_confirmation" or "confirmed").
		Status string `json:"status"`
		// Decisions lists the prior revision's ambiguity questions paired with any confirmed answers.
		Decisions []CP1PriorDecision `json:"decisions"`
		// Examples lists the prior revision's concrete examples (including operator-confirmed edits).
		Examples []CP1ModelExample `json:"examples"`
		// PreservedBehaviors lists the prior revision's contract invariants.
		PreservedBehaviors []string `json:"preserved_behaviors"`
	}
)

// Revise invokes the underlying Goa-AI model.Client with the prior Change Card
// semantic context, the revised operator prompt, and the target Goa design
// source, and strictly decodes the validated CP1SemanticAnalysis response.
func (a *ModelCP1Analyzer) Revise(
	ctx context.Context,
	prior CP1PriorCard,
	promptText, designSource string,
) (CP1SemanticAnalysis, error) {
	userPrompt, err := formatCP1ReviseUserPrompt(prior, promptText, designSource)
	if err != nil {
		return CP1SemanticAnalysis{}, err
	}
	return a.completeAnalysis(ctx, goldenpath.ArchitectReviseSystemPrompt(), userPrompt)
}

// ReviseChange re-evaluates an existing Checkpoint 1 Change Card against a
// revised operator prompt (and optional updated Goa design source) bound to the
// card's current request_revision. It passes the prior card's confirmed
// decisions, open questions, examples, and preserved behaviors to the
// configured CP1Analyzer.Revise method, assigns code-owned ambiguity and
// example IDs, and saves the new revision in needs_confirmation status only if
// the saved card still has the caller's request_revision when the analysis
// finishes.
func (c *ControlPlane) ReviseChange(
	ctx context.Context,
	p *gensurla.ReviseChangePayload,
) (*gensurla.ChangeCardResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	if p == nil ||
		strings.TrimSpace(p.ChangeID) == "" ||
		strings.TrimSpace(p.RequestRevision) == "" ||
		strings.TrimSpace(p.Prompt) == "" {
		return nil, gensurla.MakeUnprocessable(errors.New("change_id, request_revision, and prompt are required"))
	}
	if !utf8.ValidString(p.ChangeID) {
		return nil, gensurla.MakeUnprocessable(errors.New("change_id must be valid UTF-8"))
	}
	if c.analyzer == nil {
		return nil, gensurla.MakeNotImplemented(errors.New("cp1 semantic analyzer is required"))
	}

	c.mu.RLock()
	record, exists, err := c.cards.load(p.ChangeID)
	c.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, gensurla.MakeNotFound(fmt.Errorf("unknown change_id %q", p.ChangeID))
	}
	if record.owner != principal.ID {
		return nil, gensurla.MakeForbidden(fmt.Errorf("principal %q is not the owner of change_id %q", principal.ID, p.ChangeID))
	}
	if record.card.RequestRevision != p.RequestRevision {
		return nil, gensurla.MakeConflict(fmt.Errorf(
			"request_revision mismatch for %q: got %q, want %q",
			p.ChangeID,
			p.RequestRevision,
			record.card.RequestRevision,
		))
	}

	designSrc := record.designSource
	if p.DesignSource != nil {
		designSrc = *p.DesignSource
	}
	prior := buildCP1PriorCard(record.card)

	ctx, span := otel.Tracer("surla/controlplane").Start(
		ctx,
		"surla.controlplane.revise_change",
		trace.WithAttributes(attribute.String("surla.change_id", p.ChangeID)),
	)
	defer span.End()

	if err := reportProgress(ctx, 0, 1, "Revising the Change Card with the CP1 model"); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "report cp1 revision progress")
		return nil, err
	}
	analysis, err := c.analyzer.Revise(ctx, prior, p.Prompt, designSrc)
	if err != nil {
		if se, ok := asAnalysisRejected(err); ok {
			err = newAnalysisRejected(
				fmt.Sprintf(
					"cp1 analysis rejected (%s): this revise_change request did not modify Change Card %q; retry revise_change with request_revision %s (and design_source when updating the Goa design), or call get_change if another request may have updated the card",
					se.Message,
					p.ChangeID,
					p.RequestRevision,
				),
				err,
			)
		} else {
			err = unclassifiedCP1Error{cause: err}
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "revise cp1 change card")
		return nil, err
	}
	if err := reportProgress(ctx, 1, 1, "Validating the revised CP1 analysis"); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "report cp1 validation progress")
		return nil, err
	}

	summary, ambiguities, examples, preserved, err := validateCP1Analysis(p.ChangeID, analysis)
	if err != nil {
		se := newAnalysisRejected(
			fmt.Sprintf(
				"cp1 analysis rejected (%s): this revise_change request did not modify Change Card %q; retry revise_change with request_revision %s (and design_source when updating the Goa design), or call get_change if another request may have updated the card",
				err.Error(),
				p.ChangeID,
				p.RequestRevision,
			),
			err,
		)
		span.RecordError(se)
		span.SetStatus(codes.Error, "validate revised cp1 analysis")
		return nil, se
	}

	// Every revised card enters needs_confirmation so the operator explicitly
	// reviews and confirms the revised summary, decisions, and examples before
	// advancing to downstream checkpoints.
	revised := &gensurla.ChangeCardResult{
		ChangeID:           p.ChangeID,
		Status:             StatusNeedsConfirmation,
		Prompt:             p.Prompt,
		Summary:            summary,
		Ambiguities:        ambiguities,
		Examples:           examples,
		PreservedBehaviors: preserved,
	}
	rev, err := computeCanonicalRevision(revised, principal.ID, designSrc)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "compute revised cp1 card revision")
		return nil, err
	}
	revised.RequestRevision = rev
	if analysis.UIComposition != nil {
		revised.UIComposition = &gensurla.UICompositionItem{
			Program:             *analysis.UIComposition,
			ComposedForRevision: rev,
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	current, exists, err := c.cards.load(p.ChangeID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "load cp1 change card")
		return nil, err
	}
	if !exists {
		return nil, gensurla.MakeNotFound(fmt.Errorf("unknown change_id %q", p.ChangeID))
	}
	if current.owner != principal.ID {
		return nil, gensurla.MakeForbidden(fmt.Errorf("principal %q is not the owner of change_id %q", principal.ID, p.ChangeID))
	}
	if current.card.RequestRevision != p.RequestRevision {
		return nil, gensurla.MakeConflict(fmt.Errorf(
			"request_revision mismatch for %q: got %q, want %q",
			p.ChangeID,
			p.RequestRevision,
			current.card.RequestRevision,
		))
	}
	if err := c.cards.save(storedChangeCard{
		card:         revised,
		owner:        principal.ID,
		designSource: designSrc,
	}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "save revised cp1 change card")
		return nil, err
	}
	return revised, nil
}

// Error returns the underlying analyzer error text.
func (e unclassifiedCP1Error) Error() string {
	return e.cause.Error()
}

// Unwrap returns the underlying analyzer error tree.
func (e unclassifiedCP1Error) Unwrap() error {
	return e.cause
}

// GoaErrorName returns an empty Goa error name so the generated HTTP error
// encoder falls through its declared-error switch without unwrapping a nested
// ServiceError inside a multi-cause join.
func (e unclassifiedCP1Error) GoaErrorName() string {
	return ""
}

// httpErrorFormatter formats HTTP error responses for NewHTTPHandler. When err
// is an unclassifiedCP1Error (such as an analysis_rejected ServiceError joined
// with context cancellation or an independent fault), it formats the response
// as an HTTP 500 server fault; all other errors delegate to
// goahttp.NewErrorResponse.
func httpErrorFormatter(ctx context.Context, err error) goahttp.Statuser {
	if _, isUnclassified := err.(unclassifiedCP1Error); isUnclassified {
		return goahttp.NewErrorResponse(ctx, goa.Fault("%s", err.Error()))
	}
	return goahttp.NewErrorResponse(ctx, err)
}

// buildCP1PriorCard extracts the semantic decisions, examples, and preserved
// behaviors from card without positional IDs so CP1Analyzer.Revise can reason
// over settled requirements, open questions, and confirmed examples.
func buildCP1PriorCard(card *gensurla.ChangeCardResult) CP1PriorCard {
	decisions := make([]CP1PriorDecision, 0, len(card.Ambiguities))
	for _, amb := range card.Ambiguities {
		if amb == nil {
			continue
		}
		var confirmed string
		if card.Answers != nil {
			confirmed = card.Answers[amb.ID]
		}
		decisions = append(decisions, CP1PriorDecision{
			Question:        amb.Question,
			Options:         slices.Clone(amb.Options),
			ConfirmedAnswer: confirmed,
		})
	}
	examples := make([]CP1ModelExample, 0, len(card.Examples))
	for _, ex := range card.Examples {
		if ex == nil {
			continue
		}
		examples = append(examples, CP1ModelExample{
			Scenario:         ex.Scenario,
			InputSummary:     ex.InputSummary,
			ExpectedOutcome:  ex.ExpectedOutcome,
			VerificationKind: ex.VerificationKind,
		})
	}
	return CP1PriorCard{
		Prompt:             card.Prompt,
		Summary:            card.Summary,
		Status:             card.Status,
		Decisions:          decisions,
		Examples:           examples,
		PreservedBehaviors: slices.Clone(card.PreservedBehaviors),
	}
}

// newAnalysisRejected constructs a non-fault analysis_rejected ServiceError
// whose public Message is set to reason while retaining cause for Unwrap.
func newAnalysisRejected(reason string, cause error) *goa.ServiceError {
	se := gensurla.MakeAnalysisRejected(cause)
	se.Message = reason
	return se
}

// asAnalysisRejected reports whether err is (or wraps solely) a single
// non-nil analysis_rejected ServiceError. If err joins an analysis_rejected
// error with an independent failure, asAnalysisRejected returns false so the
// combined failure remains a server fault.
func asAnalysisRejected(err error) (*goa.ServiceError, bool) {
	if err == nil {
		return nil, false
	}
	if se, ok := err.(*goa.ServiceError); ok {
		if se != nil && se.GoaErrorName() == "analysis_rejected" {
			return se, true
		}
		return nil, false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var expected *goa.ServiceError
		for _, child := range joined.Unwrap() {
			actual, valid := asAnalysisRejected(child)
			if !valid {
				return nil, false
			}
			if expected == nil {
				expected = actual
				continue
			}
			if expected != actual {
				return nil, false
			}
		}
		return expected, expected != nil
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return asAnalysisRejected(wrapped.Unwrap())
	}
	return nil, false
}

// exactOutputValidation accepts only an error tree whose every leaf is the
// same non-nil *model.OutputValidationError. Ordinary single-cause wrappers
// and duplicate joins of that same validation error preserve its identity;
// any non-validation leaf (such as context cancellation, a provider error,
// or an observer failure) or distinct validation leaf rejects the
// classification so combined failures remain ordinary server faults.
func exactOutputValidation(err error) (*model.OutputValidationError, bool) {
	if err == nil {
		return nil, false
	}
	if ovErr, ok := err.(*model.OutputValidationError); ok {
		return ovErr, ovErr != nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var expected *model.OutputValidationError
		for _, child := range joined.Unwrap() {
			actual, valid := exactOutputValidation(child)
			if !valid {
				return nil, false
			}
			if expected == nil {
				expected = actual
				continue
			}
			if expected != actual {
				return nil, false
			}
		}
		return expected, expected != nil
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return exactOutputValidation(wrapped.Unwrap())
	}
	return nil, false
}

// formatCP1DesignSection returns designSource unchanged when it contains
// non-whitespace text, or an explicit no-design instruction when omitted or
// whitespace-only so the CP1 model does not assume unstated services or
// fixture IDs.
func formatCP1DesignSection(designSource string) string {
	if strings.TrimSpace(designSource) != "" {
		return designSource
	}
	return "(no Goa design source was supplied — do not assume or invent service splits, endpoints, or fixture IDs that are not stated in the request or prior card)"
}

// formatCP1ReviseUserPrompt formats the prior Change Card context, the revised
// operator request, and the target Goa design source for ModelCP1Analyzer.Revise.
func formatCP1ReviseUserPrompt(prior CP1PriorCard, promptText, designSource string) (string, error) {
	priorJSON, err := json.Marshal(prior)
	if err != nil {
		return "", fmt.Errorf("marshal prior cp1 change card: %w", err)
	}
	return fmt.Sprintf(
		"Prior Change Card:\n%s\n\nRevised Request:\n%s\n\nDesign:\n%s",
		string(priorJSON),
		promptText,
		formatCP1DesignSection(designSource),
	), nil
}

// completeAnalysis runs one structured CP1 model completion with systemPrompt
// and userPrompt and strictly decodes the resulting CP1SemanticAnalysis.
func (a *ModelCP1Analyzer) completeAnalysis(
	ctx context.Context,
	systemPrompt, userPrompt string,
) (CP1SemanticAnalysis, error) {
	if a == nil || a.client == nil {
		return CP1SemanticAnalysis{}, errors.New("cp1 model client is required")
	}
	req := &model.Request{
		Messages: []*model.Message{
			{
				Role: model.ConversationRoleSystem,
				Parts: []model.Part{
					model.TextPart{
						Text: systemPrompt,
					},
				},
			},
			{
				Role: model.ConversationRoleUser,
				Parts: []model.Part{
					model.TextPart{
						Text: userPrompt,
					},
				},
			},
		},
		StructuredOutput: &model.StructuredOutput{
			Name:   "cp1_change_card_analysis",
			Schema: cp1AnalysisJSONSchema,
		},
		MaxTokens: cp1AnalysisMaxOutputTokens,
	}
	resp, err := a.client.Complete(ctx, req)
	if err != nil {
		if ovErr, ok := exactOutputValidation(err); ok {
			return CP1SemanticAnalysis{}, newAnalysisRejected(
				fmt.Sprintf("model output failed %q validation", ovErr.Kind()),
				err,
			)
		}
		return CP1SemanticAnalysis{}, fmt.Errorf("cp1 model completion: %w", err)
	}
	if resp.OutputLimited {
		return CP1SemanticAnalysis{}, newAnalysisRejected(
			"provider stopped at output limit",
			errors.New("cp1 model response reached output limit"),
		)
	}
	raw := extractAssistantText(resp)
	if raw == "" {
		return CP1SemanticAnalysis{}, newAnalysisRejected(
			"model returned empty analysis text",
			errors.New("cp1 model returned empty analysis"),
		)
	}
	var parsed CP1SemanticAnalysis
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&parsed); err != nil {
		return CP1SemanticAnalysis{}, newAnalysisRejected("model returned malformed analysis JSON", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return CP1SemanticAnalysis{}, newAnalysisRejected(
			"model returned malformed analysis JSON",
			errors.New("cp1 model analysis contains trailing JSON tokens"),
		)
	}
	return parsed, nil
}
