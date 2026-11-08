package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/goldenpath"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"goa.design/goa-ai/runtime/agent/model"
	goa "goa.design/goa/v3/pkg"
)

type (
	// CP1Analyzer performs semantic Checkpoint 1 analysis of an operator feature
	// request against the current Goa design source.
	CP1Analyzer interface {
		// Analyze inspects the prompt and design source and returns structured
		// summary, open domain questions, concrete examples, and preserved
		// behaviors.
		Analyze(ctx context.Context, promptText, designSource string) (CP1SemanticAnalysis, error)
		// Revise re-analyzes a revised prompt and design source against the
		// prior Change Card's confirmed decisions, open questions, examples,
		// and preserved behaviors.
		Revise(ctx context.Context, prior CP1PriorCard, promptText, designSource string) (CP1RevisionSemanticAnalysis, error)
	}

	// CP1ModelAmbiguity is an open domain question returned by a CP1Analyzer
	// model call before Surla assigns its mechanical question ID.
	CP1ModelAmbiguity struct {
		// Question is the plain-English ambiguity question.
		Question string `json:"question"`
		// Options lists the candidate answers (at least 2 distinct options).
		Options []string `json:"options"`
	}

	// CP1ModelExample is a concrete input/outcome example returned by a
	// CP1Analyzer model call before Surla assigns its mechanical check ID.
	CP1ModelExample struct {
		// Scenario is the plain-English scenario description.
		Scenario string `json:"scenario"`
		// InputSummary describes the representative user request and fixture state.
		InputSummary string `json:"input_summary"`
		// ExpectedOutcome describes the expected side effect and response behavior.
		ExpectedOutcome string `json:"expected_outcome"`
		// VerificationKind classifies the check ("deterministic" or "model_judged").
		VerificationKind string `json:"verification_kind"`
	}

	// CP1SemanticAnalysis is the structured semantic output returned by a
	// CP1Analyzer model call.
	CP1SemanticAnalysis struct {
		// Summary is the plain-English description of the requested behavior.
		Summary string `json:"summary"`
		// Ambiguities lists genuine domain questions that require human confirmation.
		Ambiguities []CP1ModelAmbiguity `json:"ambiguities"`
		// Examples lists concrete input/outcome examples for the Change Card.
		Examples []CP1ModelExample `json:"examples"`
		// PreservedBehaviors lists existing contract invariants preserved by the change.
		PreservedBehaviors []string `json:"preserved_behaviors"`
		// UIComposition is an optional OpenUI Lang decision view composed from
		// the Surla decision catalog. Nil means the model composed none. Any
		// string the model returns, including an empty or invalid program, is
		// carried to the card unchanged: it is presentation only, so a bad
		// program never costs the operator the authoritative analysis.
		UIComposition *string `json:"ui_composition,omitempty"`
	}

	// ModelCP1Analyzer implements CP1Analyzer using a Goa-AI model.Client with
	// schema-validated structured JSON output.
	ModelCP1Analyzer struct {
		// client is the underlying Goa-AI model.Client.
		client model.Client
	}

	// ModelBuilder constructs a CP1Analyzer, its derived CP3 live-target
	// environment, and its transport cleanup function for a validated
	// ConfigureModelPayload. Callers wire ModelBuilder in Options so the
	// composition root (cmd/surla) owns provider and gateway client creation.
	ModelBuilder func(
		p *gensurla.ConfigureModelPayload,
	) (analyzer CP1Analyzer, liveEnv []string, closeFn func() error, err error)

	// modelHandle holds an immutable CP1Analyzer and liveEnv snapshot alongside
	// its reference count. Every read and write of refs and closeFn is guarded
	// exclusively by ControlPlane.modelMu.
	modelHandle struct {
		// analyzer is the CP1 semantic analyzer for this model configuration,
		// or nil when no model is configured.
		analyzer CP1Analyzer
		// liveEnv is the environment slice passed to verifyrun.LaunchLive when
		// starting a CP3 live target under this model configuration.
		liveEnv []string
		// closeFn releases the underlying transport (such as a gRPC gateway
		// connection) when refs reaches 0, or nil when no transport cleanup is
		// needed. Guarded by ControlPlane.modelMu.
		closeFn func() error
		// refs counts active holders: 1 while installed as ControlPlane.activeModel
		// plus 1 for each in-flight ProposeChange or ReviseChange call. Guarded
		// by ControlPlane.modelMu.
		refs int
	}
)

const (
	// msgCP1NotConfiguredAdjustable explains how an operator or coding agent can
	// enable CP1 semantic analysis when ModelBuilder is wired on the control
	// plane (such as on surla serve).
	msgCP1NotConfiguredAdjustable = "cp1 semantic analyzer is required: call configure_model with gateway_addr to attach a running credentialed model gateway, call configure_model with provider and model_name if ANTHROPIC_API_KEY or OPENAI_API_KEY is already set in surla serve's environment, or restart surla serve with ANTHROPIC_API_KEY or OPENAI_API_KEY exported"
	// msgCP1NotConfiguredFixedHost explains how a host embedding the control
	// plane without ModelBuilder can enable CP1 semantic analysis without
	// directing callers to configure_model when configure_model would return
	// not_implemented.
	msgCP1NotConfiguredFixedHost = "cp1 semantic analyzer is required: supply Options.Analyzer or Options.ModelBuilder when constructing the control plane"
	// msgModelReconfigUnavailable explains why configure_model is unavailable
	// when the embedding host did not wire Options.ModelBuilder.
	msgModelReconfigUnavailable = "runtime model reconfiguration is not configured on this control plane: supply Options.ModelBuilder when constructing the control plane"
)

// NewModelCP1Analyzer constructs a CP1Analyzer backed by a Goa-AI model.Client.
func NewModelCP1Analyzer(client model.Client) *ModelCP1Analyzer {
	return &ModelCP1Analyzer{client: client}
}

// Analyze invokes the underlying Goa-AI model.Client with the generated CP1
// completion contract and returns its decoded semantic analysis.
func (a *ModelCP1Analyzer) Analyze(
	ctx context.Context,
	promptText, designSource string,
) (CP1SemanticAnalysis, error) {
	return a.completeAnalysis(
		ctx,
		goldenpath.ArchitectSystemPrompt(),
		fmt.Sprintf("Request:\n%s\n\nDesign:\n%s", promptText, formatCP1DesignSection(designSource)),
	)
}

// ConfigureModel switches the control plane's active CP1 semantic analyzer and
// CP3 live-target model environment without restarting the server. In-flight
// ProposeChange and ReviseChange calls finish on the model handle they
// acquired at entry and close its transport when the last reference drops.
func (c *ControlPlane) ConfigureModel(
	ctx context.Context,
	p *gensurla.ConfigureModelPayload,
) (*gensurla.ModelConfigResult, error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}

	ctx, span := otel.Tracer("surla/controlplane").Start(ctx, "surla.controlplane.configure_model")
	defer span.End()

	normalized, provider, modelName, gatewayAddr, err := validateConfigureModelPayload(p)
	if err != nil {
		span.AddEvent(
			"model_configuration_refused",
			trace.WithAttributes(attribute.String("surla.model.refusal_reason", "invalid_payload")),
		)
		return nil, gensurla.MakeUnprocessable(err)
	}
	if c.modelBuilder == nil {
		span.AddEvent(
			"model_configuration_refused",
			trace.WithAttributes(attribute.String("surla.model.refusal_reason", "reconfiguration_unavailable")),
		)
		return nil, gensurla.MakeNotImplemented(errors.New(msgModelReconfigUnavailable))
	}

	c.modelMu.Lock()
	closed := c.modelClosed
	c.modelMu.Unlock()
	if closed {
		span.RecordError(errCardsClosed)
		span.SetStatus(codes.Error, "control plane closed")
		return nil, errCardsClosed
	}

	analyzer, liveEnv, closeFn, err := c.modelBuilder(normalized)
	if err != nil {
		var se *goa.ServiceError
		if errors.As(err, &se) && se.Name == "unprocessable" {
			span.AddEvent(
				"model_configuration_refused",
				trace.WithAttributes(attribute.String("surla.model.refusal_reason", "missing_api_key")),
			)
			return nil, err
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "build model client failed")
		return nil, err
	}

	next := &modelHandle{
		analyzer: analyzer,
		liveEnv:  liveEnv,
		closeFn:  closeFn,
		refs:     1,
	}
	c.modelMu.Lock()
	if c.modelClosed {
		c.modelMu.Unlock()
		span.RecordError(errCardsClosed)
		span.SetStatus(codes.Error, "control plane closed")
		closeErr := c.retireModel(ctx, next)
		return nil, errors.Join(errCardsClosed, closeErr)
	}
	prev := c.activeModel
	c.activeModel = next
	c.modelMu.Unlock()

	c.releaseModel(ctx, prev)

	if gatewayAddr != "" {
		span.AddEvent(
			"model_configured",
			trace.WithAttributes(attribute.String("surla.model.gateway_addr", gatewayAddr)),
		)
		return &gensurla.ModelConfigResult{GatewayAddr: &gatewayAddr}, nil
	}
	span.AddEvent(
		"model_configured",
		trace.WithAttributes(
			attribute.String("surla.model.provider", provider),
			attribute.String("surla.model.name", modelName),
		),
	)
	return &gensurla.ModelConfigResult{
		Provider:  &provider,
		ModelName: &modelName,
	}, nil
}

// validateConfigureModelPayload normalizes and validates the mutually exclusive
// direct-provider and gateway fields of p at the ConfigureModel boundary.
func validateConfigureModelPayload(
	p *gensurla.ConfigureModelPayload,
) (*gensurla.ConfigureModelPayload, string, string, string, error) {
	if p == nil {
		return nil, "", "", "", errors.New("specify both provider and model_name, or specify gateway_addr")
	}
	var provider, modelName, gatewayAddr string
	if p.Provider != nil {
		provider = strings.TrimSpace(*p.Provider)
	}
	if p.ModelName != nil {
		modelName = strings.TrimSpace(*p.ModelName)
	}
	if p.GatewayAddr != nil {
		gatewayAddr = strings.TrimSpace(*p.GatewayAddr)
	}
	switch {
	case gatewayAddr != "" && (provider != "" || modelName != ""):
		return nil, "", "", "", errors.New("gateway_addr cannot be combined with provider or model_name; the gateway chooses the model")
	case gatewayAddr != "":
		return &gensurla.ConfigureModelPayload{GatewayAddr: &gatewayAddr}, "", "", gatewayAddr, nil
	case provider == "" && modelName == "":
		return nil, "", "", "", errors.New("specify both provider and model_name, or specify gateway_addr")
	case provider == "" || modelName == "":
		return nil, "", "", "", errors.New("provider and model_name must be set together")
	case provider != "anthropic" && provider != "openai":
		return nil, "", "", "", fmt.Errorf("unsupported provider %q (expected \"anthropic\" or \"openai\")", provider)
	default:
		return &gensurla.ConfigureModelPayload{
			Provider:  &provider,
			ModelName: &modelName,
		}, provider, modelName, "", nil
	}
}

// closeInitialAnalyzer runs closeFn when controlplane.New fails validation
// before constructing a ControlPlane.
func closeInitialAnalyzer(closeFn func() error) error {
	if closeFn == nil {
		return nil
	}
	return closeFn()
}

// extractAssistantText concatenates TextPart payloads from a model.Response.
func extractAssistantText(resp *model.Response) string {
	if resp == nil {
		return ""
	}
	var b strings.Builder
	for _, msg := range resp.Content {
		for _, part := range msg.Parts {
			if tp, ok := part.(model.TextPart); ok {
				b.WriteString(tp.Text)
			}
		}
	}
	return b.String()
}

// acquireModel increments the active model handle's reference count under
// c.modelMu and returns the handle for an in-flight ProposeChange or
// ReviseChange call. It returns (nil, nil) when no analyzer is configured and
// errCardsClosed after Close.
func (c *ControlPlane) acquireModel() (*modelHandle, error) {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	if c.modelClosed {
		return nil, errCardsClosed
	}
	h := c.activeModel
	if h == nil || h.analyzer == nil {
		return nil, nil
	}
	h.refs++
	return h, nil
}

// notConfiguredAnalyzerError returns the not_implemented error when no CP1
// semantic analyzer is active, naming configure_model only when
// Options.ModelBuilder is wired on this control plane.
func (c *ControlPlane) notConfiguredAnalyzerError() error {
	if c.modelBuilder != nil {
		return gensurla.MakeNotImplemented(errors.New(msgCP1NotConfiguredAdjustable))
	}
	return gensurla.MakeNotImplemented(errors.New(msgCP1NotConfiguredFixedHost))
}

// retireModel decrements h's reference count under c.modelMu. When the last
// reference drops and h has a non-nil closeFn, retireModel runs closeFn inside
// a dedicated still-open "surla.controlplane.retire_model" span and records
// any cleanup failure with both span.RecordError and codes.Error status before
// ending the span.
func (c *ControlPlane) retireModel(ctx context.Context, h *modelHandle) error {
	if h == nil {
		return nil
	}
	c.modelMu.Lock()
	h.refs--
	if h.refs > 0 {
		c.modelMu.Unlock()
		return nil
	}
	closeFn := h.closeFn
	h.closeFn = nil
	c.modelMu.Unlock()
	if closeFn == nil {
		return nil
	}
	_, span := otel.Tracer("surla/controlplane").Start(ctx, "surla.controlplane.retire_model")
	defer span.End()
	if err := closeFn(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "retire model failed")
		return err
	}
	return nil
}

// releaseModel retires a reference to h after ConfigureModel or an in-flight
// ProposeChange/ReviseChange call completes. Any closeFn failure is recorded
// on the "surla.controlplane.retire_model" span inside retireModel without
// falsifying the completed activation or analysis result.
func (c *ControlPlane) releaseModel(ctx context.Context, h *modelHandle) {
	if err := c.retireModel(ctx, h); err != nil {
		return
	}
}

// liveEnv returns the active model configuration's CP3 live-target environment
// slice under c.modelMu.
func (c *ControlPlane) liveEnv() []string {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	if c.activeModel == nil {
		return nil
	}
	return c.activeModel.liveEnv
}

// closeModel marks the control plane's model state closed and retires its
// active reference. If an in-flight ProposeChange or ReviseChange still holds
// a reference to prev, prev's closeFn runs when that call's deferred
// releaseModel drops the final reference.
func (c *ControlPlane) closeModel() error {
	c.modelMu.Lock()
	c.modelClosed = true
	prev := c.activeModel
	c.activeModel = nil
	c.modelMu.Unlock()
	return c.retireModel(context.Background(), prev)
}
