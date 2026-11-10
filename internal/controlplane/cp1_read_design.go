// This file owns the Checkpoint 1 Goa design value type (CP1Design), the Phase 1
// read_design tool definition and bounded inspection loop, the Phase 2
// structured Change Card completion execution, and code-authored CP1 span
// diagnostic summaries.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"goa.design/goa-ai/runtime/agent/completion"
	"goa.design/goa-ai/runtime/agent/model"
	"goa.design/goa-ai/runtime/agent/rawjson"
)

type (
	// CP1Design represents the Checkpoint 1 Goa design input to semantic analysis,
	// originating either from a caller-supplied inline string or from a snapshot of
	// workspace `design/*.go` files. Its fields are unexported so hybrid states
	// cannot be represented.
	CP1Design struct {
		// origin records the design provenance ("supplied" or "workspace").
		origin string
		// supplied holds the caller-supplied inline design source string.
		supplied string
		// files holds the snapshot of workspace design/*.go file contents keyed by
		// relative path.
		files map[string]string
	}

	// cp1Rejection carries a Checkpoint 1 analysis rejection kind alongside its
	// underlying ServiceError or failure.
	cp1Rejection struct {
		// kind classifies the rejection reason ("output_limited", "structured_output",
		// "inspection_budget_exhausted", "semantic_validation", etc.).
		kind string
		// err is the underlying cause or ServiceError.
		err error
	}
)

const (
	// cp1InspectionMaxOutputTokens bounds the output tokens for one Phase 1
	// read_design inspection tool turn.
	cp1InspectionMaxOutputTokens = 2048

	// maxReadDesignTurnsCap caps the maximum number of tool inspection turns
	// allowed in Phase 1 read_design inspection.
	maxReadDesignTurnsCap = 8

	// cp1Phase2UserPrompt instructs the model to emit the final structured
	// Change Card analysis JSON after Phase 1 file inspection completes.
	cp1Phase2UserPrompt = "Produce the Change Card analysis JSON matching the schema."
)

var (
	// readDesignToolDef is the compiled model tool definition for read_design.
	readDesignToolDef = mustBuildReadDesignToolDefinition()
)

// SuppliedCP1Design constructs a CP1Design from a caller-supplied inline Goa
// design source string.
func SuppliedCP1Design(source string) CP1Design {
	return CP1Design{
		origin:   sourceOriginSupplied,
		supplied: source,
	}
}

// WorkspaceCP1Design constructs a CP1Design taking ownership of a freshly built
// workspace design/*.go snapshot map. Callers must not mutate files after
// calling WorkspaceCP1Design.
func WorkspaceCP1Design(files map[string]string) CP1Design {
	if len(files) == 0 {
		files = nil
	}
	return CP1Design{
		origin: sourceOriginWorkspace,
		files:  files,
	}
}

// CanonicalSource returns the canonical design source representation. For
// caller-supplied designs, it returns the supplied string. For workspace
// designs with no files, it returns ("", nil). Otherwise, it encodes the files
// map as JSON with SetEscapeHTML(false) and trims the trailing newline,
// matching the exact representation persisted on stored Change Cards.
func (d CP1Design) CanonicalSource() (string, error) {
	if !d.isWorkspace() {
		return d.supplied, nil
	}
	if len(d.files) == 0 {
		return "", nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d.files); err != nil {
		return "", fmt.Errorf("encode workspace design source: %w", err)
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// mustBuildReadDesignToolDefinition constructs the model.ToolDefinition for
// read_design from its static JSON schema.
func mustBuildReadDesignToolDefinition() *model.ToolDefinition {
	input, err := model.AdvertisedToolInputFromSchema(
		rawjson.Message(`{"type":"object","properties":{"path":{"type":"string","minLength":1}},"additionalProperties":false}`),
	)
	if err != nil {
		panic(fmt.Sprintf("build read_design tool input schema: %v", err))
	}
	return &model.ToolDefinition{
		Name:        "read_design",
		Description: "Read Goa design file(s) from the bound workspace design/ directory. Omit path to read all design/*.go files in the manifest, or pass an exact relative path listed in the workspace design manifest.",
		Input:       input,
	}
}

// newCP1Rejection constructs an analysis_rejected ServiceError wrapped in a
// cp1Rejection so callers and trace spans retain the typed rejection kind.
func newCP1Rejection(kind, reason string, cause error) error {
	se := newAnalysisRejected(reason, cause)
	return &cp1Rejection{
		kind: kind,
		err:  se,
	}
}

// cp1RejectionKind returns the typed rejection kind recorded on err when err
// carries a cp1Rejection or model.OutputValidationError. It never guesses a
// fallback kind when neither is present.
func cp1RejectionKind(err error) (string, bool) {
	var r *cp1Rejection
	if errors.As(err, &r) && r.kind != "" {
		return r.kind, true
	}
	var ovErr *model.OutputValidationError
	if errors.As(err, &ovErr) && ovErr.Kind() != "" {
		return string(ovErr.Kind()), true
	}
	return "", false
}

// cp1SafeRejectionSummary returns a strictly code-authored diagnostic summary
// for a rejected CP1 propose or revise span. It never interpolates caller
// prompts, model-authored text, or external analyzer error messages.
func cp1SafeRejectionSummary(revise bool, rejectionKind string, hasKind bool) string {
	outcome := "no Change Card was saved."
	prefix := "CP1 analysis rejected"
	if revise {
		outcome = "the saved Change Card was unchanged."
		prefix = "CP1 revision rejected"
	}
	if !hasKind {
		return prefix + "; " + outcome
	}
	switch rejectionKind {
	case string(model.OutputValidationOutputLimited):
		return prefix + ": provider stopped at output limit; shorten the request before retrying (" + outcome[:len(outcome)-1] + ")."
	case "inspection_budget_exhausted":
		return prefix + ": design inspection exhausted turn budget with unresolved file errors; " + outcome
	case "semantic_validation":
		return prefix + ": model output failed Change Card semantic validation; " + outcome
	default:
		return fmt.Sprintf("%s: model output failed %q validation; %s", prefix, rejectionKind, outcome)
	}
}

// recordCP1RejectionSpan attaches the typed rejection kind (when known) and a
// strictly code-authored public summary onto span.
func recordCP1RejectionSpan(span trace.Span, revise bool, err error) {
	kind, hasKind := cp1RejectionKind(err)
	attrs := make([]attribute.KeyValue, 0, 2)
	if hasKind {
		attrs = append(attrs, attribute.String("surla.cp1.rejection_kind", kind))
	}
	attrs = append(attrs, attribute.String("surla.public_summary", cp1SafeRejectionSummary(revise, kind, hasKind)))
	span.SetAttributes(attrs...)
}

// decodeSavedWorkspaceCP1Design decodes a stored workspace Change Card's
// designSource JSON back into a WorkspaceCP1Design, rejecting null maps and
// trailing JSON tokens. An empty string represents an empty workspace before
// design files exist.
func decodeSavedWorkspaceCP1Design(designSrc string) (CP1Design, error) {
	if designSrc == "" {
		return WorkspaceCP1Design(nil), nil
	}
	var files map[string]string
	dec := json.NewDecoder(strings.NewReader(designSrc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&files); err != nil {
		return CP1Design{}, fmt.Errorf("decode saved workspace design: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return CP1Design{}, errors.New("decode saved workspace design: unexpected trailing JSON data")
	}
	if files == nil {
		return CP1Design{}, errors.New("decode saved workspace design: expected JSON object, got null")
	}
	return WorkspaceCP1Design(files), nil
}

// decodeReadDesignPayload decodes the raw JSON payload for a read_design tool
// call, requiring a single JSON object `{}` or `{"path": "<non-empty string>"}`
// and rejecting empty/whitespace raw bytes, null, unknown fields, trailing JSON
// tokens, explicit null path values, and empty path strings.
func decodeReadDesignPayload(raw rawjson.Message) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", errors.New("expected JSON object payload, got empty input")
	}
	var fields map[string]rawjson.Message
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&fields); err != nil {
		return "", err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", errors.New("unexpected trailing data after JSON payload")
	}
	if fields == nil {
		return "", errors.New("expected JSON object payload, got null")
	}
	for k := range fields {
		if k != "path" {
			return "", fmt.Errorf("unknown field %q", k)
		}
	}
	rawPath, hasPath := fields["path"]
	if !hasPath {
		return "", nil
	}
	var path *string
	pathDec := json.NewDecoder(bytes.NewReader(rawPath))
	if err := pathDec.Decode(&path); err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	if err := pathDec.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", errors.New("unexpected trailing data in path")
	}
	if path == nil {
		return "", errors.New("path must be a non-empty string when provided, got null")
	}
	if *path == "" {
		return "", errors.New("path must be non-empty when provided; omit path to read all files")
	}
	return *path, nil
}

// buildPhase1SystemPrompt composes the base system prompt with the sorted
// workspace design manifest, the inspection turn budget, and instructions for
// calling read_design.
func buildPhase1SystemPrompt(baseSystemPrompt string, designFiles map[string]string, maxTurns int) string {
	paths := slices.Sorted(maps.Keys(designFiles))
	var sb strings.Builder
	sb.WriteString(baseSystemPrompt)
	sb.WriteString("\n\nWorkspace design manifest:\n")
	for _, p := range paths {
		fmt.Fprintf(&sb, "- %s (%d bytes)\n", p, len(designFiles[p]))
	}
	fmt.Fprintf(
		&sb,
		"\nYou have up to %d inspection turns to call read_design: omit path to read all files in one call, or call read_design (including multiple parallel calls in a single turn) with exact manifest paths relevant to the request. Each successful read_design result includes remaining_turns. When you have the design context you need, stop calling tools.\n",
		maxTurns,
	)
	return sb.String()
}

// cloneResponseMessages converts a value slice of provider response messages
// into deeply owned *model.Message pointers via model.CloneMessages so later
// turns cannot alias or mutate earlier request transcripts.
func cloneResponseMessages(content []model.Message) ([]*model.Message, error) {
	if len(content) == 0 {
		return nil, nil
	}
	ptrs := make([]*model.Message, len(content))
	for i := range content {
		ptrs[i] = &content[i]
	}
	return model.CloneMessages(ptrs)
}

// appendCP1UserPart appends part to the conversation's user turn while
// preserving strict alternating user/assistant message roles required by model
// providers: when the trailing message already has role user (such as after
// ToolResultPart blocks or a Turn 0 validation recovery), part is appended to a
// copy of that message; otherwise a new user message is appended.
func appendCP1UserPart(messages []*model.Message, part model.Part) []*model.Message {
	if n := len(messages); n > 0 && messages[n-1].Role == model.ConversationRoleUser {
		prev := messages[n-1]
		copied := *prev
		copied.Parts = append(slices.Clone(prev.Parts), part)
		out := slices.Clone(messages)
		out[n-1] = &copied
		return out
	}
	return append(slices.Clone(messages), &model.Message{
		Role:  model.ConversationRoleUser,
		Parts: []model.Part{part},
	})
}

// completeStructuredMessages runs a typed structured completion using a cloned
// copy of messages and spec, mapping output limit and validation failures into
// typed cp1Rejection errors.
func completeStructuredMessages[T any](
	ctx context.Context,
	client model.Client,
	spec completion.Spec[T],
	messages []*model.Message,
) (T, error) {
	var zero T
	if client == nil {
		return zero, errors.New("cp1 model client is required")
	}
	cloned, err := model.CloneMessages(messages)
	if err != nil {
		return zero, fmt.Errorf("clone cp1 completion messages: %w", err)
	}
	req := &model.Request{
		Messages:  cloned,
		MaxTokens: cp1AnalysisMaxOutputTokens,
	}
	resp, err := completion.Complete(ctx, client, req, spec)
	if err != nil {
		if ovErr, ok := exactOutputValidation(err); ok {
			if ovErr.Kind() == model.OutputValidationOutputLimited {
				return zero, newCP1Rejection(
					string(model.OutputValidationOutputLimited),
					"provider stopped at output limit; shorten the request before retrying",
					err,
				)
			}
			return zero, newCP1Rejection(
				string(ovErr.Kind()),
				fmt.Sprintf("model output failed %q validation", ovErr.Kind()),
				err,
			)
		}
		return zero, fmt.Errorf("cp1 model completion: %w", err)
	}
	if resp.ModelResponse.OutputLimited {
		return zero, newCP1Rejection(
			string(model.OutputValidationOutputLimited),
			"provider stopped at output limit",
			errors.New("cp1 model response reached output limit"),
		)
	}
	return resp.Value, nil
}

// inspectAndComplete executes either the single-turn structured completion (for
// supplied or empty-workspace designs) or the two-phase inspection loop followed
// by structured completion (for non-empty workspace designs). baseUserPrompt
// contains the formatted request (and prior card for revisions) without an
// inline Design section, which is appended only on the single-turn path.
func inspectAndComplete[T any](
	ctx context.Context,
	a *ModelCP1Analyzer,
	baseSystemPrompt, baseUserPrompt string,
	design CP1Design,
	spec completion.Spec[T],
) (T, error) {
	var zero T
	if a == nil {
		return zero, errors.New("cp1 model client is required")
	}
	if !design.isWorkspace() || len(design.files) == 0 {
		singleTurnPrompt := baseUserPrompt + "\n\nDesign:\n" + formatCP1DesignSection(design.supplied)
		return completeStructuredAnalysis(ctx, a.client, spec, baseSystemPrompt, singleTurnPrompt)
	}
	messages, err := a.runPhase1Inspection(ctx, baseSystemPrompt, baseUserPrompt, design.files)
	if err != nil {
		return zero, err
	}
	messages = appendCP1UserPart(messages, model.TextPart{Text: cp1Phase2UserPrompt})
	return completeStructuredMessages(ctx, a.client, spec, messages)
}

// isWorkspace reports whether d originated from a bound workspace snapshot.
func (d CP1Design) isWorkspace() bool {
	return d.origin == sourceOriginWorkspace
}

// Error returns the underlying error text.
func (r *cp1Rejection) Error() string {
	return r.err.Error()
}

// Unwrap returns the underlying error.
func (r *cp1Rejection) Unwrap() error {
	return r.err
}

// runPhase1Inspection executes the bounded read_design tool inspection loop.
// Phase 1 transitions to Phase 2 as soon as the model returns zero tool calls,
// all manifest files have been read without error, or the final allowed
// inspection turn completes its requested file reads without tool errors; it
// fails with inspection_budget_exhausted only when the final allowed turn ends
// with an unresolved tool error.
func (a *ModelCP1Analyzer) runPhase1Inspection(
	ctx context.Context,
	baseSystemPrompt, initialUserPrompt string,
	designFiles map[string]string,
) ([]*model.Message, error) {
	if a.client == nil {
		return nil, errors.New("cp1 model client is required")
	}
	maxInspectionTurns := min(len(designFiles)+2, maxReadDesignTurnsCap)
	messages := []*model.Message{
		{
			Role: model.ConversationRoleSystem,
			Parts: []model.Part{
				model.TextPart{Text: buildPhase1SystemPrompt(baseSystemPrompt, designFiles, maxInspectionTurns)},
			},
		},
		{
			Role: model.ConversationRoleUser,
			Parts: []model.Part{
				model.TextPart{Text: initialUserPrompt},
			},
		},
	}
	sortedManifestPaths := slices.Sorted(maps.Keys(designFiles))
	readFiles := make(map[string]bool, len(designFiles))

	for turn := range maxInspectionTurns {
		reqMessages, err := model.CloneMessages(messages)
		if err != nil {
			return nil, fmt.Errorf("clone cp1 inspection messages: %w", err)
		}
		phase1Req := &model.Request{
			Messages:  reqMessages,
			Tools:     []*model.ToolDefinition{readDesignToolDef},
			MaxTokens: cp1InspectionMaxOutputTokens,
		}
		resp, err := a.client.Complete(ctx, phase1Req)
		if err != nil {
			if ovErr, ok := exactOutputValidation(err); ok {
				if ovErr.Kind() == model.OutputValidationOutputLimited {
					return nil, newCP1Rejection(
						string(model.OutputValidationOutputLimited),
						"provider stopped at output limit; shorten the request before retrying",
						err,
					)
				}
				if ovErr.RecoveryCorrection() != "" && turn+1 < maxInspectionTurns {
					messages = appendCP1UserPart(messages, model.TextPart{Text: ovErr.RecoveryCorrection()})
					continue
				}
				return nil, newCP1Rejection(
					string(ovErr.Kind()),
					fmt.Sprintf("model output failed %q validation", ovErr.Kind()),
					err,
				)
			}
			return nil, fmt.Errorf("cp1 design inspection: %w", err)
		}

		if resp.OutputLimited {
			return nil, newCP1Rejection(
				string(model.OutputValidationOutputLimited),
				"provider stopped at output limit; shorten the request before retrying",
				errors.New("cp1 design inspection response reached output limit"),
			)
		}

		toolCalls := resp.ToolCalls()
		clonedRespMsgs, err := cloneResponseMessages(resp.Content)
		if err != nil {
			return nil, fmt.Errorf("clone cp1 inspection response: %w", err)
		}
		messages = append(messages, clonedRespMsgs...)
		if len(toolCalls) == 0 {
			return messages, nil
		}

		remainingTurns := maxInspectionTurns - 1 - turn
		turnHadToolError := false
		resultParts := make([]model.Part, 0, len(toolCalls))
		for _, call := range toolCalls {
			if call.Name != "read_design" {
				turnHadToolError = true
				resultParts = append(resultParts, model.ToolResultPart{
					ToolUseID: call.ID,
					IsError:   true,
					Content:   fmt.Sprintf("unknown tool %q; available tools: read_design", call.Name),
				})
				continue
			}

			path, decodeErr := decodeReadDesignPayload(call.Payload)
			if decodeErr != nil {
				turnHadToolError = true
				resultParts = append(resultParts, model.ToolResultPart{
					ToolUseID: call.ID,
					IsError:   true,
					Content:   fmt.Sprintf("invalid read_design payload: %s", decodeErr.Error()),
				})
				continue
			}

			if path == "" {
				allFiles := make([]any, 0, len(sortedManifestPaths))
				for _, p := range sortedManifestPaths {
					readFiles[p] = true
					allFiles = append(allFiles, map[string]any{
						"path":    p,
						"content": designFiles[p],
					})
				}
				resultParts = append(resultParts, model.ToolResultPart{
					ToolUseID: call.ID,
					Content: map[string]any{
						"files":           allFiles,
						"remaining_turns": remainingTurns,
					},
				})
				continue
			}

			content, ok := designFiles[path]
			if !ok {
				turnHadToolError = true
				resultParts = append(resultParts, model.ToolResultPart{
					ToolUseID: call.ID,
					IsError:   true,
					Content:   fmt.Sprintf("unknown design file %q; available files: %s", path, strings.Join(sortedManifestPaths, ", ")),
				})
				continue
			}

			readFiles[path] = true
			resultParts = append(resultParts, model.ToolResultPart{
				ToolUseID: call.ID,
				Content: map[string]any{
					"path":            path,
					"content":         content,
					"remaining_turns": remainingTurns,
				},
			})
		}

		messages = append(messages, &model.Message{
			Role:  model.ConversationRoleUser,
			Parts: resultParts,
		})

		if !turnHadToolError && (len(readFiles) == len(designFiles) || turn == maxInspectionTurns-1) {
			return messages, nil
		}
	}

	return nil, newCP1Rejection(
		"inspection_budget_exhausted",
		"cp1 design inspection exhausted turn budget before completing file inspection",
		errors.New("cp1 read_design loop exceeded maxInspectionTurns with unresolved tool error"),
	)
}
