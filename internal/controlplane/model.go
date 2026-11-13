// This file owns the model choice on a running control plane: the choice
// live targets started afterwards receive, and the environment it becomes.
// The composition root reads provider keys and builds that environment; this
// package keeps the choice and its environment together and reports the
// choice, never the environment.
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
	// configure_model selects choice, which is never the none case. The
	// composition root (cmd/surla) supplies it because only the root reads
	// the process environment. It returns an unprocessable ServiceError when
	// the chosen provider's API key is missing from that environment, naming
	// the variable and never its value; any other error is a server fault.
	LiveModelEnv func(choice gensurla.Connection) ([]string, error)
)

const (
	// msgModelReconfigUnavailable explains why configure_model is unavailable
	// when the composition root did not wire Options.LiveModelEnv.
	msgModelReconfigUnavailable = "runtime model reconfiguration is not configured on this control plane: supply Options.LiveModelEnv when constructing the control plane"
)

// ConfigureModel switches, without restarting the server, the model choice
// that live targets started afterwards receive in their environment. A live
// target already running keeps the environment it started with. An invalid
// choice or a missing provider key is unprocessable and leaves the current
// choice in place; a control plane without Options.LiveModelEnv returns
// not_implemented. The validated payload is converted once to the declared
// choice, which the environment, the result and get_model_connection all come
// from.
func (c *ControlPlane) ConfigureModel(
	ctx context.Context,
	p *gensurla.ConfigureModelPayload,
) (*gensurla.ModelConfigResult, error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}

	_, span := c.tracer("surla/controlplane").Start(ctx, "surla.controlplane.configure_model")
	defer span.End()

	choice, err := modelChoice(p)
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

	env, err := c.liveModelEnvFor(choice)
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
	c.liveModel = choice
	c.liveModelEnv = env
	c.modelMu.Unlock()

	if gateway, ok := choice.AsGateway(); ok {
		span.AddEvent(
			"model_configured",
			trace.WithAttributes(attribute.String("surla.model.connection_kind", "gateway")),
		)
		return &gensurla.ModelConfigResult{GatewayAddr: &gateway.GatewayAddr}, nil
	}
	direct, _ := choice.AsDirect()
	span.AddEvent(
		"model_configured",
		trace.WithAttributes(
			attribute.String("surla.model.connection_kind", "provider"),
			attribute.String("surla.model.provider", direct.Provider),
		),
	)
	return &gensurla.ModelConfigResult{
		Provider:  &direct.Provider,
		ModelName: &direct.ModelName,
	}, nil
}

// GetModelConnection reports the model choice live targets started afterwards
// receive: the choice the server started with until configure_model replaces
// it. It returns the choice only, never the environment it became, so no API
// key or other environment value can reach the caller.
func (c *ControlPlane) GetModelConnection(ctx context.Context) (*gensurla.ModelConnection, error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	return &gensurla.ModelConnection{Connection: c.liveModel}, nil
}

// modelChoice validates configure_model's mutually exclusive direct-provider
// and gateway fields at the boundary and returns them as the declared choice:
// direct with both a provider and a model name, or gateway with an address.
func modelChoice(p *gensurla.ConfigureModelPayload) (gensurla.Connection, error) {
	if p == nil {
		return gensurla.Connection{}, errors.New("specify both provider and model_name, or specify gateway_addr")
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
		return gensurla.Connection{}, errors.New("gateway_addr cannot be combined with provider or model_name; the gateway chooses the model")
	case gatewayAddr != "":
		return gensurla.NewConnectionGateway(&gensurla.ModelGateway{GatewayAddr: gatewayAddr}), nil
	case provider == "" && modelName == "":
		return gensurla.Connection{}, errors.New("specify both provider and model_name, or specify gateway_addr")
	case provider == "" || modelName == "":
		return gensurla.Connection{}, errors.New("provider and model_name must be set together")
	case provider != "anthropic" && provider != "openai":
		return gensurla.Connection{}, fmt.Errorf("unsupported provider %q (expected \"anthropic\" or \"openai\")", provider)
	default:
		return gensurla.NewConnectionDirect(&gensurla.ModelDirect{Provider: provider, ModelName: modelName}), nil
	}
}

// liveEnv returns the environment the next CP3 live target starts with:
// Options.Live.Env until configure_model replaces it.
func (c *ControlPlane) liveEnv() []string {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	return c.liveModelEnv
}
