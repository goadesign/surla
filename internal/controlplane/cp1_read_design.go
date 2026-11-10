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
	goa "goa.design/goa/v3/pkg"
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
		// "semantic_validation", etc.).
		kind string
		// err is the underlying cause or ServiceError.
		err error
	}

	// cp1FaultStage classifies the internal failure stage for a non-analyzer
	// CP1 propose_change or revise_change span summary.
	cp1FaultStage uint8

	// readDesignArgs decodes a model.Client-validated read_design tool payload.
	readDesignArgs struct {
		// Path names the exact relative manifest path to read.
		Path string `json:"path"`
	}
)

const (
	// cp1FaultProgress indicates MCP progress reporting failed before saving.
	cp1FaultProgress cp1FaultStage = iota + 1
	// cp1FaultRevision indicates canonical revision hashing failed before saving.
	cp1FaultRevision
	// cp1FaultStorageRead indicates loading Change Card storage failed before saving.
	cp1FaultStorageRead
	// cp1FaultStorageSave indicates saving or syncing Change Card storage failed;
	// because directory sync can fail after atomic file rename, whether the new
	// card version was persisted is unconfirmed until get_change is checked.
	cp1FaultStorageSave

	// maxReadDesignTurnsCap caps the maximum number of tool inspection turns
	// allowed in Phase 1 read_design inspection.
	maxReadDesignTurnsCap = 8

	// cp1Phase1InstructionHeader gives Phase 1 read_design turns an inspection-only
	// role so the model selects and reads relevant design files without writing
	// Change Card JSON or UI compositions before Phase 2.
	cp1Phase1InstructionHeader = "Inspect the Goa workspace's design/*.go files to gather the design context relevant to the feature request. Do not write a Change Card, JSON analysis, or UI composition during inspection; a separate structured step produces the Change Card after inspection finishes."
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
// read_design from its static JSON schema requiring an exact manifest path.
func mustBuildReadDesignToolDefinition() *model.ToolDefinition {
	input, err := model.AdvertisedToolInputFromSchema(
		rawjson.Message(`{"type":"object","properties":{"path":{"type":"string","minLength":1}},"required":["path"],"additionalProperties":false}`),
	)
	if err != nil {
		panic(fmt.Sprintf("build read_design tool input schema: %v", err))
	}
	return &model.ToolDefinition{
		Name:        "read_design",
		Description: "Read one Goa design file from the bound workspace design/ directory by exact relative path listed in the workspace design manifest. Multiple read_design calls may be issued in parallel in a single turn.",
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

// isOutputLimitedValidation reports whether ovErr rejected a response that
// stopped at the provider's output token limit, including tool-call rejections
// whose primary Kind is tool_arguments or tool_identity.
func isOutputLimitedValidation(ovErr *model.OutputValidationError) (bool, error) {
	if ovErr.Kind() == model.OutputValidationOutputLimited {
		return true, nil
	}
	rejected, err := ovErr.RejectedResponse()
	if err != nil {
		return false, fmt.Errorf("inspect rejected cp1 response: %w", err)
	}
	return rejected != nil && rejected.OutputLimited, nil
}

// isPureContextError reports whether err is (or wraps solely) target without
// crossing a named Goa ServiceError or joining an independent non-target error.
func isPureContextError(err, target error) bool {
	if err == nil {
		return false
	}
	if err == target {
		return true
	}
	if _, ok := err.(goa.GoaErrorNamer); ok {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !isPureContextError(child, target) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return isPureContextError(wrapped.Unwrap(), target)
	}
	return false
}

// cp1SafeRejectionSummary returns a strictly code-authored, request-scoped
// diagnostic summary for a rejected CP1 propose or revise span. It uses only
// compile-time string constants and never interpolates caller prompts,
// model-authored text, or external analyzer error messages.
func cp1SafeRejectionSummary(revise bool, rejectionKind string, hasKind bool) string {
	outcome := "this request did not save a Change Card"
	prefix := "CP1 analysis rejected"
	retryCall := "propose_change"
	if revise {
		outcome = "this request did not modify the Change Card"
		prefix = "CP1 revision rejected"
		retryCall = "revise_change"
	}
	if !hasKind {
		return fmt.Sprintf("%s (%s); retry %s.", prefix, outcome, retryCall)
	}
	switch model.OutputValidationKind(rejectionKind) {
	case model.OutputValidationOutputLimited:
		return fmt.Sprintf("%s: provider stopped at output limit (%s); shorten the request and retry %s.", prefix, outcome, retryCall)
	case "semantic_validation":
		return fmt.Sprintf("%s: model output failed Change Card semantic validation (%s); retry %s.", prefix, outcome, retryCall)
	case model.OutputValidationStructuredOutput:
		return fmt.Sprintf("%s: model output failed structured validation (%s); retry %s.", prefix, outcome, retryCall)
	case model.OutputValidationToolIdentity,
		model.OutputValidationToolArguments,
		model.OutputValidationToolChoice:
		return fmt.Sprintf("%s: model design-inspection tool call was invalid (%s); retry %s.", prefix, outcome, retryCall)
	case model.OutputValidationResponseShape:
		return fmt.Sprintf("%s: model response failed response-shape validation (%s); retry %s.", prefix, outcome, retryCall)
	case model.OutputValidationOutputBounds:
		return fmt.Sprintf("%s: model response exceeded output size or structure bounds (%s); shorten the request and retry %s.", prefix, outcome, retryCall)
	case model.OutputValidationStreamProtocol:
		return fmt.Sprintf("%s: model response failed stream-protocol validation (%s); retry %s or ask the server operator to check server diagnostics.", prefix, outcome, retryCall)
	case model.OutputValidationUsage:
		return fmt.Sprintf("%s: model response failed token-usage validation (%s); retry %s or ask the server operator to check server diagnostics.", prefix, outcome, retryCall)
	default:
		return fmt.Sprintf("%s (%s); retry %s.", prefix, outcome, retryCall)
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

// cp1SafeAnalyzerErrorSummary returns a strictly code-authored, request-scoped
// diagnostic summary for a non-rejection error returned by CP1Analyzer.Analyze
// or CP1Analyzer.Revise, distinguishing declared unprocessable inputs and pure
// context cancellation/deadlines from server/analyzer faults without claiming
// unproven internal causes.
func cp1SafeAnalyzerErrorSummary(revise bool, err error) string {
	outcome := "this request did not save a Change Card"
	op := "CP1 analysis"
	retryCall := "propose_change"
	if revise {
		outcome = "this request did not modify the Change Card"
		op = "CP1 revision"
		retryCall = "revise_change"
	}
	if _, ok := asUnprocessable(err); ok {
		return fmt.Sprintf("%s rejected the request as unprocessable (%s); revise the request inputs and retry %s.", op, outcome, retryCall)
	}
	if isPureContextError(err, context.DeadlineExceeded) {
		return fmt.Sprintf("%s timed out before completion (%s); retry %s or ask the server operator to check model responsiveness.", op, outcome, retryCall)
	}
	if isPureContextError(err, context.Canceled) {
		return fmt.Sprintf("%s was canceled before completion (%s); retry %s if still needed.", op, outcome, retryCall)
	}
	return fmt.Sprintf("%s failed before completion (%s); ask the server operator to check server diagnostics and model configuration before retrying.", op, outcome)
}

// cp1SafeFaultSummary returns a strictly code-authored, request-scoped
// diagnostic summary for a non-analyzer CP1 propose or revise fault span.
// Because MCP masks internal server faults from tool callers, each summary
// states truthfully whether this request's write ran or remained unconfirmed
// and names the concrete operator or retry action without exposing prompts,
// model text, credentials, or host paths.
func cp1SafeFaultSummary(revise bool, stage cp1FaultStage) string {
	outcome := "this request did not save a Change Card"
	op := "CP1 analysis"
	if revise {
		outcome = "this request did not modify the Change Card"
		op = "CP1 revision"
	}
	switch stage {
	case cp1FaultProgress:
		return fmt.Sprintf("%s failed while reporting progress (%s); retry or ask the server operator to check server diagnostics.", op, outcome)
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

// buildPhase1SystemPrompt composes an inspection-only system prompt containing
// the sorted workspace design manifest, the inspection turn budget, and
// instructions for calling read_design without Change Card output rules.
func buildPhase1SystemPrompt(designFiles map[string]string, maxTurns int) string {
	paths := slices.Sorted(maps.Keys(designFiles))
	var sb strings.Builder
	sb.WriteString(cp1Phase1InstructionHeader)
	sb.WriteString("\n\nWorkspace design manifest:\n")
	for _, p := range paths {
		fmt.Fprintf(&sb, "- %s (%d bytes)\n", p, len(designFiles[p]))
	}
	fmt.Fprintf(
		&sb,
		"\nYou have up to %d inspection turns to call read_design (including multiple parallel calls in a single turn) with exact manifest paths relevant to the request. Each successful read_design result includes remaining_turns. When you have read the relevant design files (or if none are needed), reply briefly with no tool calls to finish inspection.\n",
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
	if len(readPaths) < len(designFiles) {
		sb.WriteString("\nInspected design files (other manifest files were not read during inspection):")
	} else {
		sb.WriteString("\nInspected design files:")
	}
	for _, p := range readPaths {
		fmt.Fprintf(&sb, "\n--- %s ---\n%s", p, designFiles[p])
		if !strings.HasSuffix(designFiles[p], "\n") {
			sb.WriteByte('\n')
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
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
		readPaths, err := a.runPhase1Inspection(ctx, baseUserPrompt, design.files)
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
// returns the sorted manifest paths read during inspection. Goa-AI's
// model.Client owns request cloning, response isolation, and structural
// validation of advertised tool names and payloads before Complete returns;
// Phase 1 checks rejected responses for OutputLimited before attempting
// RecoveryCorrection, resolves exact manifest paths against the in-memory
// snapshot, and proceeds to Phase 2 with the manifest and already-read files
// once all files are read or the inspection turn budget is reached.
func (a *ModelCP1Analyzer) runPhase1Inspection(
	ctx context.Context,
	initialUserPrompt string,
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
				model.TextPart{Text: buildPhase1SystemPrompt(designFiles, maxInspectionTurns)},
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
		phase1Req := &model.Request{
			Messages:  messages,
			Tools:     []*model.ToolDefinition{readDesignToolDef},
			MaxTokens: cp1AnalysisMaxOutputTokens,
		}
		resp, err := a.client.Complete(ctx, phase1Req)
		if err != nil {
			if ovErr, ok := exactOutputValidation(err); ok {
				limited, limErr := isOutputLimitedValidation(ovErr)
				if limErr != nil {
					return nil, fmt.Errorf("cp1 design inspection: %w", limErr)
				}
				if limited {
					return nil, newCP1Rejection(
						string(model.OutputValidationOutputLimited),
						"provider stopped at output limit; shorten the request before retrying",
						err,
					)
				}
				if ovErr.RecoveryCorrection() != "" {
					if turn+1 == maxInspectionTurns {
						return slices.Sorted(maps.Keys(readFiles)), nil
					}
					reminder := "Your previous read_design tool call was rejected before it could run.\n" +
						ovErr.RecoveryCorrection() +
						"\nCall read_design with a valid relative path from the workspace design manifest, or reply with no tool calls when finished."
					messages = appendCP1UserPart(messages, model.TextPart{Text: reminder})
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
		for i := range resp.Content {
			messages = append(messages, &resp.Content[i])
		}

		remainingTurns := maxInspectionTurns - 1 - turn
		resultParts := make([]model.Part, 0, len(toolCalls))
		for _, call := range toolCalls {
			var args readDesignArgs
			if err := json.Unmarshal(call.Payload, &args); err != nil {
				return nil, fmt.Errorf("decode validated read_design payload: %w", err)
			}

			content, ok := designFiles[args.Path]
			if !ok {
				resultParts = append(resultParts, model.ToolResultPart{
					ToolUseID: call.ID,
					IsError:   true,
					Content:   fmt.Sprintf("unknown design file %q; available files: %s", args.Path, strings.Join(sortedManifestPaths, ", ")),
				})
				continue
			}

			readFiles[args.Path] = true
			resultParts = append(resultParts, model.ToolResultPart{
				ToolUseID: call.ID,
				Content: map[string]any{
					"path":            args.Path,
					"content":         content,
					"remaining_turns": remainingTurns,
				},
			})
		}

		if len(readFiles) == len(designFiles) || turn == maxInspectionTurns-1 {
			return slices.Sorted(maps.Keys(readFiles)), nil
		}

		messages = append(messages, &model.Message{
			Role:  model.ConversationRoleUser,
			Parts: resultParts,
		})
	}

	return slices.Sorted(maps.Keys(readFiles)), nil
}
