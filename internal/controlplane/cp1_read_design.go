// This file owns the Checkpoint 1 Goa design value type (CP1Design), the Phase 1
// read_design tool definition and bounded inspection loop, workspace design
// formatting for Phase 2 structured Change Card completion, and code-authored
// CP1 span diagnostic summaries.
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

	// cp1FaultStage classifies the internal failure stage for a non-rejection
	// CP1 propose_change or revise_change span summary.
	cp1FaultStage uint8
)

const (
	// cp1FaultProgress indicates MCP progress reporting failed before saving.
	cp1FaultProgress cp1FaultStage = iota + 1
	// cp1FaultProvider indicates model inference or transport failed before saving.
	cp1FaultProvider
	// cp1FaultRevision indicates canonical revision hashing failed before saving.
	cp1FaultRevision
	// cp1FaultStorageRead indicates loading Change Card storage failed before saving.
	cp1FaultStorageRead
	// cp1FaultStorageSave indicates saving or syncing Change Card storage failed;
	// because directory sync can fail after atomic file rename, whether the new
	// card version was persisted is unconfirmed until get_change is checked.
	cp1FaultStorageSave

	// cp1InspectionMaxOutputTokens bounds the output tokens for one Phase 1
	// read_design inspection tool turn.
	cp1InspectionMaxOutputTokens = 2048

	// maxReadDesignTurnsCap caps the maximum number of tool inspection turns
	// allowed in Phase 1 read_design inspection.
	maxReadDesignTurnsCap = 8
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

// Error returns the underlying error text.
func (r *cp1Rejection) Error() string {
	return r.err.Error()
}

// Unwrap returns the underlying error.
func (r *cp1Rejection) Unwrap() error {
	return r.err
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
// for a rejected CP1 propose or revise span. It uses only compile-time string
// constants and never interpolates caller prompts, model-authored text, or
// external analyzer error messages.
func cp1SafeRejectionSummary(revise bool, rejectionKind string, hasKind bool) string {
	outcome := "no Change Card was saved"
	prefix := "CP1 analysis rejected"
	retryCall := "propose_change"
	if revise {
		outcome = "the saved Change Card was unchanged"
		prefix = "CP1 revision rejected"
		retryCall = "revise_change"
	}
	if !hasKind {
		return fmt.Sprintf("%s (%s); retry %s.", prefix, outcome, retryCall)
	}
	switch rejectionKind {
	case string(model.OutputValidationOutputLimited):
		return fmt.Sprintf("%s: provider stopped at output limit (%s); shorten the request and retry %s.", prefix, outcome, retryCall)
	case "inspection_budget_exhausted":
		return fmt.Sprintf("%s: design inspection exhausted turn budget with unresolved file errors (%s); retry %s.", prefix, outcome, retryCall)
	case "semantic_validation":
		return fmt.Sprintf("%s: model output failed Change Card semantic validation (%s); retry %s.", prefix, outcome, retryCall)
	default:
		return fmt.Sprintf("%s: model output failed structured validation (%s); retry %s.", prefix, outcome, retryCall)
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

// cp1SafeFaultSummary returns a strictly code-authored diagnostic summary for a
// non-rejection CP1 propose or revise fault span. Because MCP masks internal
// server faults from tool callers, each summary states truthfully whether card
// persistence was untouched or unconfirmed and names the concrete operator or
// retry action without exposing prompts, model text, credentials, or host paths.
func cp1SafeFaultSummary(revise bool, stage cp1FaultStage) string {
	outcome := "no Change Card was saved"
	op := "CP1 analysis"
	if revise {
		outcome = "the saved Change Card was unchanged"
		op = "CP1 revision"
	}
	switch stage {
	case cp1FaultProgress:
		return fmt.Sprintf("%s failed while reporting progress (%s); retry or ask the server operator to check server diagnostics.", op, outcome)
	case cp1FaultProvider:
		return fmt.Sprintf("%s failed due to a model provider or transport fault (%s); ask the server operator to check server diagnostics and model configuration before retrying.", op, outcome)
	case cp1FaultRevision:
		return fmt.Sprintf("%s failed while encoding the card revision (%s); ask the server operator to check server diagnostics.", op, outcome)
	case cp1FaultStorageRead:
		return fmt.Sprintf("%s could not read Change Card storage (%s); preserve your request and ask the server operator to check private card storage.", op, outcome)
	case cp1FaultStorageSave:
		if revise {
			return "CP1 revision could not confirm Change Card save; preserve your request, call get_change to check the saved card revision, and ask the server operator to check private card storage before retrying."
		}
		return "CP1 analysis could not confirm Change Card save; preserve your request, call get_change to check whether the card was saved, and ask the server operator to check private card storage before retrying."
	default:
		panic(fmt.Sprintf("unknown cp1FaultStage %d", stage))
	}
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
func buildPhase1SystemPrompt(
	baseSystemPrompt string,
	designFiles map[string]string,
	maxTurns int,
) string {
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

// formatWorkspacePhase2Design formats the sorted workspace design/*.go manifest
// and the contents of the manifest files read during Phase 1 inspection in
// deterministic path order for Phase 2 structured Change Card completion.
func formatWorkspacePhase2Design(
	designFiles map[string]string,
	readPaths []string,
) string {
	manifestPaths := slices.Sorted(maps.Keys(designFiles))
	var sb strings.Builder
	sb.WriteString("Workspace design manifest:\n")
	for _, p := range manifestPaths {
		fmt.Fprintf(&sb, "- %s (%d bytes)\n", p, len(designFiles[p]))
	}
	if len(readPaths) == 0 {
		sb.WriteString("\n(no design files were read during inspection)")
		return sb.String()
	}
	for _, p := range readPaths {
		fmt.Fprintf(&sb, "\n--- %s ---\n%s", p, designFiles[p])
		if !strings.HasSuffix(designFiles[p], "\n") {
			sb.WriteByte('\n')
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// cloneResponseMessages converts a value slice of provider response messages
// into deeply owned *model.Message pointers via model.CloneMessages so later
// Phase 1 turns cannot alias or mutate earlier request transcripts.
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
// providers when a Phase 1 validation recovery follows an existing user turn.
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

// inspectAndComplete executes either the single-turn structured completion (for
// supplied or empty-workspace designs) or the Phase 1 read_design inspection
// loop followed by single-turn structured completion over the manifest and read
// files (for non-empty workspace designs). Phase 2 never replays Phase 1 tool
// blocks into completion.Complete, keeping one toolless completion contract
// across all direct and gateway model providers.
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
	designSection := formatCP1DesignSection(design.supplied)
	if design.isWorkspace() && len(design.files) > 0 {
		readPaths, err := a.runPhase1Inspection(ctx, baseSystemPrompt, baseUserPrompt, design.files)
		if err != nil {
			return zero, err
		}
		designSection = formatWorkspacePhase2Design(design.files, readPaths)
	}
	return completeStructuredAnalysis(
		ctx,
		a.client,
		spec,
		baseSystemPrompt,
		baseUserPrompt+"\n\nDesign:\n"+designSection,
	)
}

// isWorkspace reports whether d originated from a bound workspace snapshot.
func (d CP1Design) isWorkspace() bool {
	return d.origin == sourceOriginWorkspace
}

// runPhase1Inspection executes the bounded read_design tool inspection loop and
// returns the sorted manifest paths read during inspection. Phase 1 finishes as
// soon as the model returns zero tool calls, all manifest files have been read
// without error, or the final allowed inspection turn completes its requested
// file reads without tool errors; it fails with inspection_budget_exhausted
// only when the final allowed turn ends with an unresolved tool error.
func (a *ModelCP1Analyzer) runPhase1Inspection(
	ctx context.Context,
	baseSystemPrompt, initialUserPrompt string,
	designFiles map[string]string,
) ([]string, error) {
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
		if len(toolCalls) == 0 {
			return slices.Sorted(maps.Keys(readFiles)), nil
		}
		clonedRespMsgs, err := cloneResponseMessages(resp.Content)
		if err != nil {
			return nil, fmt.Errorf("clone cp1 inspection response: %w", err)
		}
		messages = append(messages, clonedRespMsgs...)

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

		if !turnHadToolError && (len(readFiles) == len(designFiles) || turn == maxInspectionTurns-1) {
			return slices.Sorted(maps.Keys(readFiles)), nil
		}

		messages = append(messages, &model.Message{
			Role:  model.ConversationRoleUser,
			Parts: resultParts,
		})
	}

	return nil, newCP1Rejection(
		"inspection_budget_exhausted",
		"cp1 design inspection exhausted turn budget before completing file inspection",
		errors.New("cp1 read_design loop exceeded maxInspectionTurns with unresolved tool error"),
	)
}
