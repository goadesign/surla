// Package controlplane provides the core business logic and state management
// for the Surla control plane.
//
// This file owns the Checkpoint 1 Goa design value type (CP1Design), the Phase 1
// read_design tool definition and inspection loop, and the Phase 2 structured
// Change Card completion execution.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"goa.design/goa-ai/runtime/agent/completion"
	"goa.design/goa-ai/runtime/agent/model"
	"goa.design/goa-ai/runtime/agent/rawjson"
)

type (
	// CP1Design represents the Checkpoint 1 Goa design input to semantic analysis,
	// originating either from a caller-supplied inline string or from a snapshot of
	// workspace `design/*.go` files.
	CP1Design struct {
		// origin records the design provenance ("supplied" or "workspace").
		origin string
		// supplied holds the caller-supplied inline design source string.
		supplied string
		// files holds the snapshot of workspace design/*.go file contents keyed by
		// relative path.
		files map[string]string
	}

	// readDesignPayload models the JSON argument payload for the read_design tool.
	readDesignPayload struct {
		// Path is an optional relative path to a specific design file in the manifest.
		// When omitted or empty, read_design reads all files in the manifest.
		Path *string `json:"path,omitempty"`
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

// WorkspaceCP1Design constructs a CP1Design from a snapshot of workspace
// design files. The files map is cloned defensively so subsequent mutations
// cannot affect the design.
func WorkspaceCP1Design(files map[string]string) CP1Design {
	var cloned map[string]string
	if len(files) > 0 {
		cloned = make(map[string]string, len(files))
		for k, v := range files {
			cloned[k] = v
		}
	}
	return CP1Design{
		origin: sourceOriginWorkspace,
		files:  cloned,
	}
}

// CanonicalSource returns the canonical design source representation. For
// caller-supplied designs, it returns the supplied string. For workspace
// designs with no files, it returns ("", nil). Otherwise, it encodes the files
// map as JSON with SetEscapeHTML(false) and trims the trailing newline,
// matching the exact representation persisted on stored Change Cards.
func (d CP1Design) CanonicalSource() (string, error) {
	if d.origin != sourceOriginWorkspace {
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

// Kind returns the typed rejection kind string.
func (r *cp1Rejection) Kind() string {
	return r.kind
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

// cp1RejectionKind returns the typed rejection kind from err, falling back to
// the OutputValidationError kind if present or "semantic_validation".
func cp1RejectionKind(err error) string {
	var r *cp1Rejection
	if errors.As(err, &r) && r.kind != "" {
		return r.kind
	}
	var ovErr *model.OutputValidationError
	if errors.As(err, &ovErr) && ovErr.Kind() != "" {
		return string(ovErr.Kind())
	}
	return "semantic_validation"
}

// decodeReadDesignPayload decodes the raw JSON payload for a read_design tool
// call, disallowing unknown fields and treating empty/{} as omitting path.
func decodeReadDesignPayload(raw rawjson.Message) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "{}" {
		return "", nil
	}
	var p readDesignPayload
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return "", err
	}
	if dec.More() {
		return "", errors.New("unexpected trailing data after JSON payload")
	}
	if p.Path == nil {
		return "", nil
	}
	return *p.Path, nil
}

// buildPhase1SystemPrompt composes the base system prompt with the sorted
// workspace design manifest and instructions for calling read_design.
func buildPhase1SystemPrompt(baseSystemPrompt string, designFiles map[string]string) string {
	paths := make([]string, 0, len(designFiles))
	for p := range designFiles {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var sb strings.Builder
	sb.WriteString(baseSystemPrompt)
	sb.WriteString("\n\nWorkspace design manifest:\n")
	for _, p := range paths {
		fmt.Fprintf(&sb, "- %s (%d bytes)\n", p, len(designFiles[p]))
	}
	sb.WriteString("\nYou may call read_design (omitting path to read all files, or passing an exact manifest path) to inspect the design files relevant to the request. When you are ready to produce the Change Card analysis, stop calling tools.\n")
	return sb.String()
}

// completeStructuredMessages runs a typed structured completion using the
// provided spec and messages slice, mapping output limit and validation
// failures into typed cp1Rejection errors.
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
	req := &model.Request{
		Messages:  messages,
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

// isWorkspace reports whether d originated from a bound workspace snapshot.
func (d CP1Design) isWorkspace() bool {
	return d.origin == sourceOriginWorkspace
}

// runPhase1Inspection executes the multi-turn read_design tool inspection loop
// until the model chooses to stop, all manifest files are read, or the turn
// budget is exhausted.
func (a *ModelCP1Analyzer) runPhase1Inspection(
	ctx context.Context,
	baseSystemPrompt string,
	initialUserPrompt string,
	designFiles map[string]string,
) ([]*model.Message, error) {
	messages := []*model.Message{
		{
			Role: model.ConversationRoleSystem,
			Parts: []model.Part{
				model.TextPart{Text: buildPhase1SystemPrompt(baseSystemPrompt, designFiles)},
			},
		},
		{
			Role: model.ConversationRoleUser,
			Parts: []model.Part{
				model.TextPart{Text: initialUserPrompt},
			},
		},
	}
	maxInspectionTurns := min(len(designFiles)+2, maxReadDesignTurnsCap)
	readFiles := make(map[string]bool, len(designFiles))

	for turn := 0; turn < maxInspectionTurns; turn++ {
		phase1Req := &model.Request{
			Messages:  messages,
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
				if ovErr.RecoveryCorrection() != "" {
					if turn+1 < maxInspectionTurns {
						messages = append(messages, &model.Message{
							Role: model.ConversationRoleUser,
							Parts: []model.Part{
								model.TextPart{Text: ovErr.RecoveryCorrection()},
							},
						})
						continue
					}
					return nil, newCP1Rejection(
						string(ovErr.Kind()),
						fmt.Sprintf("model output failed %q validation", ovErr.Kind()),
						err,
					)
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
			for i := range resp.Content {
				messages = append(messages, &resp.Content[i])
			}
			return messages, nil
		}

		for i := range resp.Content {
			messages = append(messages, &resp.Content[i])
		}

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
				for p := range designFiles {
					readFiles[p] = true
				}
				paths := make([]string, 0, len(designFiles))
				for p := range designFiles {
					paths = append(paths, p)
				}
				sort.Strings(paths)
				allFiles := make([]any, 0, len(paths))
				for _, p := range paths {
					allFiles = append(allFiles, map[string]any{
						"path":    p,
						"content": designFiles[p],
					})
				}
				resultParts = append(resultParts, model.ToolResultPart{
					ToolUseID: call.ID,
					Content:   allFiles,
				})
				continue
			}

			content, ok := designFiles[path]
			if !ok {
				turnHadToolError = true
				available := make([]string, 0, len(designFiles))
				for p := range designFiles {
					available = append(available, p)
				}
				sort.Strings(available)
				resultParts = append(resultParts, model.ToolResultPart{
					ToolUseID: call.ID,
					IsError:   true,
					Content:   fmt.Sprintf("unknown design file %q; available files: %s", path, strings.Join(available, ", ")),
				})
				continue
			}

			readFiles[path] = true
			resultParts = append(resultParts, model.ToolResultPart{
				ToolUseID: call.ID,
				Content: map[string]any{
					"path":    path,
					"content": content,
				},
			})
		}

		messages = append(messages, &model.Message{
			Role:  model.ConversationRoleUser,
			Parts: resultParts,
		})

		if !turnHadToolError && len(readFiles) == len(designFiles) {
			return messages, nil
		}

		if turn == maxInspectionTurns-1 {
			return nil, newCP1Rejection(
				"inspection_budget_exhausted",
				"cp1 design inspection exhausted turn budget before completing file inspection",
				errors.New("cp1 read_design loop exceeded maxInspectionTurns"),
			)
		}
	}

	return nil, newCP1Rejection(
		"inspection_budget_exhausted",
		"cp1 design inspection exhausted turn budget before completing file inspection",
		errors.New("cp1 read_design loop exceeded maxInspectionTurns"),
	)
}

// inspectAndComplete executes either the single-turn structured completion (for
// supplied or empty-workspace designs) or the two-phase inspection loop followed
// by structured completion (for non-empty workspace designs).
func (a *ModelCP1Analyzer) inspectAndComplete[T any](
	ctx context.Context,
	baseSystemPrompt string,
	singleTurnUserPrompt string,
	design CP1Design,
	spec completion.Spec[T],
	initialUserPrompt string,
) (T, error) {
	var zero T
	if a == nil {
		return zero, errors.New("cp1 model client is required")
	}

	if !design.isWorkspace() || len(design.files) == 0 {
		return completeStructuredAnalysis(ctx, a.client, spec, baseSystemPrompt, singleTurnUserPrompt)
	}

	messages, err := a.runPhase1Inspection(ctx, baseSystemPrompt, initialUserPrompt, design.files)
	if err != nil {
		return zero, err
	}

	messages = append(messages, &model.Message{
		Role: model.ConversationRoleUser,
		Parts: []model.Part{
			model.TextPart{Text: "Produce the Change Card analysis JSON matching the schema."},
		},
	})

	return completeStructuredMessages(ctx, a.client, spec, messages)
}
