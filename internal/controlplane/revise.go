// This file implements Checkpoint 1 (CP1) Change Card revision (`revise_change`)
// and prior-card semantic re-analysis.

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	gensurla "github.com/goadesign/surla/gen/surla"
	gencompletions "github.com/goadesign/surla/gen/surla/completions"
	"github.com/goadesign/surla/internal/goldenpath"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"goa.design/goa-ai/runtime/agent/completion"
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
		// Examples lists the prior revision's concrete examples with their confirmed IDs.
		Examples []CP1PriorExample `json:"examples"`
		// PreservedBehaviors lists the prior revision's contract invariants.
		PreservedBehaviors []string `json:"preserved_behaviors"`
	}
)

// Revise invokes the underlying Goa-AI model.Client with the prior Change Card
// semantic context, the revised operator prompt, and the target Goa design, and
// strictly decodes the validated CP1RevisionSemanticAnalysis response.
func (a *ModelCP1Analyzer) Revise(
	ctx context.Context,
	prior CP1PriorCard,
	promptText string,
	design CP1Design,
) (CP1RevisionSemanticAnalysis, error) {
	baseUserPrompt, err := formatCP1ReviseBasePrompt(prior, promptText)
	if err != nil {
		return CP1RevisionSemanticAnalysis{}, err
	}
	response, err := inspectAndComplete(
		ctx,
		a,
		goldenpath.ArchitectReviseSystemPrompt(),
		baseUserPrompt,
		design,
		gencompletions.SpecCp1ReviseChangeCardAnalysis(),
	)
	if err != nil {
		return CP1RevisionSemanticAnalysis{}, err
	}
	return cp1RevisionAnalysisFromCompletion(response), nil
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
	model, err := c.acquireModel()
	if err != nil {
		return nil, err
	}
	if model == nil {
		return nil, c.notConfiguredAnalyzerError()
	}
	defer func() {
		c.releaseModel(ctx, model)
	}()

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

	var design CP1Design
	var designSrc string
	nextOrigin := record.sourceOrigin
	if nextOrigin == "" {
		nextOrigin = sourceOriginSupplied
	}
	switch {
	case p.DesignSource != nil:
		designSrc = *p.DesignSource
		nextOrigin = sourceOriginSupplied
		design = SuppliedCP1Design(designSrc)
	case nextOrigin == sourceOriginWorkspace:
		wsDir, wsDev, wsIno := c.currentWorkspaceTarget()
		if wsDir != "" {
			design, designSrc, err = readWorkspaceCP1Design(wsDir, wsDev, wsIno)
			if err != nil {
				return nil, err
			}
		} else {
			designSrc = record.designSource
			design, err = decodeSavedWorkspaceCP1Design(designSrc)
			if err != nil {
				return nil, err
			}
		}
	default:
		designSrc = record.designSource
		design = SuppliedCP1Design(designSrc)
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
	analysis, err := model.analyzer.Revise(ctx, prior, p.Prompt, design)
	if err != nil {
		if se, ok := asAnalysisRejected(err); ok {
			recordCP1RejectionSpan(span, true, err)
			err = newAnalysisRejected(
				fmt.Sprintf(
					"cp1 analysis rejected (%s): this revise_change request did not modify Change Card %q; retry revise_change with request_revision %s (and design_source when updating the Goa design), or call get_change if another request may have updated the card",
					se.Message,
					p.ChangeID,
					p.RequestRevision,
				),
				err,
			)
		} else if containsAnalysisRejected(err) {
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

	summary, ambiguities, examples, preserved, err := validateCP1RevisionAnalysis(p.ChangeID, record.card.Examples, analysis)
	if err != nil {
		cause := &cp1Rejection{kind: "semantic_validation", err: err}
		recordCP1RejectionSpan(span, true, cause)
		se := newAnalysisRejected(
			fmt.Sprintf(
				"cp1 analysis rejected (%s): this revise_change request did not modify Change Card %q; retry revise_change with request_revision %s (and design_source when updating the Goa design), or call get_change if another request may have updated the card",
				err.Error(),
				p.ChangeID,
				p.RequestRevision,
			),
			cause,
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
		sourceOrigin: nextOrigin,
	}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "save revised cp1 change card")
		return nil, err
	}
	span.SetAttributes(attribute.String("surla.public_summary", "Revised Change Card (needs_confirmation)."))
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
// behaviors from card, preserving each example's confirmed ID so
// CP1Analyzer.Revise can reason over settled requirements and account for prior
// examples under Decision 1A.
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
	examples := make([]CP1PriorExample, 0, len(card.Examples))
	for _, ex := range card.Examples {
		if ex == nil {
			continue
		}
		examples = append(examples, CP1PriorExample{
			ID:               ex.ID,
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
	if named, ok := err.(goa.GoaErrorNamer); ok && named != nil {
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

// containsAnalysisRejected reports whether err has an analysis_rejected
// ServiceError in its unwrap tree without traversing past an already named Goa
// error owner.
func containsAnalysisRejected(err error) bool {
	if err == nil {
		return false
	}
	if se, ok := err.(*goa.ServiceError); ok {
		return se != nil && se.GoaErrorName() == "analysis_rejected"
	}
	if named, ok := err.(goa.GoaErrorNamer); ok && named != nil {
		return named.GoaErrorName() == "analysis_rejected"
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if containsAnalysisRejected(child) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return containsAnalysisRejected(wrapped.Unwrap())
	}
	return false
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

// formatCP1ReviseBasePrompt formats the prior Change Card JSON and the revised
// operator request once for ModelCP1Analyzer.Revise.
func formatCP1ReviseBasePrompt(prior CP1PriorCard, promptText string) (string, error) {
	priorJSON, err := json.Marshal(prior)
	if err != nil {
		return "", fmt.Errorf("marshal prior cp1 change card: %w", err)
	}
	return fmt.Sprintf(
		"Prior Change Card:\n%s\n\nRevised Request:\n%s",
		string(priorJSON),
		promptText,
	), nil
}

// completeStructuredAnalysis runs one generated CP1 completion and applies the
// CP1 rejection contract: only a sole model validation failure is a caller-visible
// rejection, and even valid JSON is rejected when the provider reached its output limit.
func completeStructuredAnalysis[T any](
	ctx context.Context,
	client model.Client,
	spec completion.Spec[T],
	systemPrompt, userPrompt string,
) (T, error) {
	messages := []*model.Message{
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
	}
	return completeStructuredMessages(ctx, client, spec, messages)
}

// cp1AnalysisFromCompletion adapts the generated final response to the public
// analyzer interface. Generated validation has already checked required values;
// the result owns its collections and keeps the model's optional view unchanged.
func cp1AnalysisFromCompletion(
	response *gencompletions.Cp1ChangeCardAnalysisResult,
) CP1SemanticAnalysis {
	analysis := CP1SemanticAnalysis{
		Summary:            response.Summary,
		Ambiguities:        make([]CP1ModelAmbiguity, len(response.Ambiguities)),
		Examples:           make([]CP1ModelExample, len(response.Examples)),
		PreservedBehaviors: response.PreservedBehaviors,
		UIComposition:      response.UIComposition,
	}
	for i, ambiguity := range response.Ambiguities {
		analysis.Ambiguities[i] = CP1ModelAmbiguity{
			Question: ambiguity.Question,
			Options:  ambiguity.Options,
		}
	}
	for i, example := range response.Examples {
		analysis.Examples[i] = CP1ModelExample{
			Scenario:         example.Scenario,
			InputSummary:     example.InputSummary,
			ExpectedOutcome:  example.ExpectedOutcome,
			VerificationKind: example.VerificationKind,
		}
	}
	return analysis
}

// cp1RevisionAnalysisFromCompletion adapts the generated revision response to the
// public analyzer interface. An absent prior_id becomes the interface's empty
// string for a new example; string slices and the optional view keep their values.
func cp1RevisionAnalysisFromCompletion(
	response *gencompletions.Cp1ReviseChangeCardAnalysisResult,
) CP1RevisionSemanticAnalysis {
	analysis := CP1RevisionSemanticAnalysis{
		Summary:            response.Summary,
		Ambiguities:        make([]CP1ModelAmbiguity, len(response.Ambiguities)),
		Examples:           make([]CP1RevisionModelExample, len(response.Examples)),
		RemovedExampleIDs:  response.RemovedExampleIds,
		PreservedBehaviors: response.PreservedBehaviors,
		UIComposition:      response.UIComposition,
	}
	for i, ambiguity := range response.Ambiguities {
		analysis.Ambiguities[i] = CP1ModelAmbiguity{
			Question: ambiguity.Question,
			Options:  ambiguity.Options,
		}
	}
	for i, example := range response.Examples {
		converted := CP1RevisionModelExample{
			Scenario:         example.Scenario,
			InputSummary:     example.InputSummary,
			ExpectedOutcome:  example.ExpectedOutcome,
			VerificationKind: example.VerificationKind,
		}
		if example.PriorID != nil {
			converted.PriorID = *example.PriorID
		}
		analysis.Examples[i] = converted
	}
	return analysis
}
