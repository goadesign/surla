// This file owns the model choice configure_model sets on a running control
// plane: the environment CP3 live targets start with. The composition root
// reads provider keys and builds that environment; this package only keeps it.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gensurla "github.com/goadesign/surla/gen/surla"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	goa "goa.design/goa/v3/pkg"
)

type (
	// LiveModelEnv returns the environment a CP3 live target starts with once
	// configure_model selects p. ConfigureModel validates p first, so p holds
	// exactly one choice: a gateway address, or a provider with a model name.
	// The composition root (cmd/surla) supplies it because only the root reads
	// the process environment. It returns an unprocessable ServiceError when
	// the chosen provider's API key is missing from that environment; any
	// other error is a server fault.
	LiveModelEnv func(p *gensurla.ConfigureModelPayload) ([]string, error)
)

const (
	// msgModelReconfigUnavailable explains why configure_model is unavailable
	// when the embedding host did not wire Options.LiveModelEnv.
	msgModelReconfigUnavailable = "runtime model reconfiguration is not configured on this control plane: supply Options.LiveModelEnv when constructing the control plane"
)

// ConfigureModel switches, without restarting the server, the model choice
// that live targets started afterwards receive in their environment. A live
// target already running keeps the environment it started with. An invalid
// choice or a missing provider key is unprocessable and leaves the current
// choice in place; a control plane without Options.LiveModelEnv returns
// not_implemented.
func (c *ControlPlane) ConfigureModel(
	ctx context.Context,
	p *gensurla.ConfigureModelPayload,
) (*gensurla.ModelConfigResult, error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}

	_, span := c.tracer("surla/controlplane").Start(ctx, "surla.controlplane.configure_model")
	defer span.End()

	normalized, provider, modelName, gatewayAddr, err := validateConfigureModelPayload(p)
	if err != nil {
		span.AddEvent(
			"model_configuration_refused",
			trace.WithAttributes(attribute.String("surla.model.refusal_reason", "invalid_payload")),
		)
		return nil, gensurla.MakeUnprocessable(err)
	}
	if c.liveModelEnvFor == nil {
		span.AddEvent(
			"model_configuration_refused",
			trace.WithAttributes(attribute.String("surla.model.refusal_reason", "reconfiguration_unavailable")),
		)
		return nil, gensurla.MakeNotImplemented(errors.New(msgModelReconfigUnavailable))
	}

	env, err := c.liveModelEnvFor(normalized)
	if err != nil {
		var se *goa.ServiceError
		if errors.As(err, &se) && se.GoaErrorName() == "unprocessable" {
			span.AddEvent(
				"model_configuration_refused",
				trace.WithAttributes(attribute.String("surla.model.refusal_reason", "missing_api_key")),
			)
			return nil, err
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "build live-target model environment")
		return nil, err
	}
	c.modelMu.Lock()
	c.liveModelEnv = env
	c.modelMu.Unlock()

	if gatewayAddr != "" {
		span.AddEvent(
			"model_configured",
			trace.WithAttributes(attribute.String("surla.model.connection_kind", "gateway")),
		)
		return &gensurla.ModelConfigResult{GatewayAddr: &gatewayAddr}, nil
	}
	span.AddEvent(
		"model_configured",
		trace.WithAttributes(
			attribute.String("surla.model.connection_kind", "provider"),
			attribute.String("surla.model.provider", provider),
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

// liveEnv returns the environment the next CP3 live target starts with:
// Options.Live.Env until configure_model replaces it.
func (c *ControlPlane) liveEnv() []string {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	return c.liveModelEnv
}
