// This file owns the model choice on a running control plane and the model
// provider request settings the surla command hands over: the choice live
// targets started afterwards receive, the settings permitted programs
// receive, and the environment a live target starts with. Settings live in
// memory only, as modelsettings values, and no result, error or span carries
// one.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/modelsettings"
	"github.com/goadesign/surla/internal/verify"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// HandOverModelSettings holds the request settings the surla command read
// from its shell, by provider. Each provider present in p replaces
// everything held for it and must include its key variable; an absent one
// keeps what is held, so a request with neither changes nothing. Every
// provider is checked before anything changes, so a refused request changes
// nothing. When the choice in force is none and a provider is now held,
// live targets use that provider's default model, in table order; any other
// choice stays. The result names the held variables and the choice. A
// provider's settings without its key, or a value containing NUL, is
// unprocessable, naming the provider and the variable only.
func (c *ControlPlane) HandOverModelSettings(
	ctx context.Context,
	p *gensurla.HandOverModelSettingsPayload,
) (*gensurla.ModelSettingsResult, error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}
	_, span := c.tracer("surla/controlplane").Start(ctx, "surla.controlplane.hand_over_model_settings")
	defer span.End()

	// Providers are checked in table order, so a request refused for both
	// always names the same one.
	handed := handedOver(p)
	arrived := map[string]modelsettings.Settings{}
	for _, provider := range modelsettings.Providers() {
		values, present := handed[provider.Name]
		if !present {
			continue
		}
		settings, err := modelsettings.New(provider.Name, values)
		if err != nil {
			span.AddEvent("model_settings_refused", trace.WithAttributes(attribute.String("surla.model.provider", provider.Name)))
			return nil, gensurla.MakeUnprocessable(err)
		}
		arrived[provider.Name] = settings
	}

	c.modelMu.Lock()
	maps.Copy(c.heldSettings, arrived)
	if c.liveModel.Kind() == gensurla.ConnectionKindNone {
		for _, provider := range modelsettings.Providers() {
			if _, held := c.heldSettings[provider.Name]; held {
				c.liveModel = gensurla.NewConnectionDirect(&gensurla.ModelDirect{Provider: provider.Name, ModelName: provider.DefaultModel})
				break
			}
		}
	}
	held := heldNames(c.heldSettings)
	choice := c.liveModel
	c.modelMu.Unlock()

	span.AddEvent("model_settings_held", trace.WithAttributes(
		attribute.StringSlice("surla.model.providers_handed_over", slices.Sorted(maps.Keys(arrived))),
		attribute.StringSlice("surla.model.providers_held", slices.Sorted(maps.Keys(held))),
		attribute.String("surla.model.connection_kind", string(choice.Kind())),
	))
	return &gensurla.ModelSettingsResult{Held: held, Connection: choice}, nil
}

// ConfigureModel switches, without restarting the server, the model choice
// that live targets started afterwards receive in their environment. A live
// target already running keeps the environment it started with. An invalid
// choice is unprocessable. A direct provider whose request settings the
// server does not hold is unprocessable too, with a refusal naming its key
// variable and how to hand it over; the check and the switch happen under
// one lock, so a hand-over never interleaves with them. A refusal leaves the
// current choice in place. The validated payload is converted once to the
// declared choice, which the result and get_model_connection both report.
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
	if err := c.chooseModel(choice); err != nil {
		span.AddEvent(
			"model_configuration_refused",
			trace.WithAttributes(attribute.String("surla.model.refusal_reason", "missing_api_key")),
		)
		return nil, err
	}

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
// receive: none until a hand-over or configure_model chooses one. It returns
// the choice only, never a held setting, so no API key or other setting
// value can reach the caller.
func (c *ControlPlane) GetModelConnection(ctx context.Context) (*gensurla.ModelConnection, error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	return &gensurla.ModelConnection{Connection: c.liveModel}, nil
}

// chooseModel makes choice the one live targets started afterwards receive,
// after checking, under the same lock, that a direct provider's settings are
// held. It returns the unprocessable refusal configure_model reports.
func (c *ControlPlane) chooseModel(choice gensurla.Connection) error {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	if direct, ok := choice.AsDirect(); ok {
		if _, held := c.heldSettings[direct.Provider]; !held {
			provider, _ := modelsettings.Lookup(direct.Provider)
			return gensurla.MakeUnprocessable(fmt.Errorf(
				"provider %s needs %s: export it, with any %s request settings it needs, in a shell and run surla there; the project server keeps running. Or choose a model gateway.",
				provider.Name, provider.Key, provider.Prefix,
			))
		}
	}
	c.liveModel = choice
	return nil
}

// liveLaunch returns, in one modelMu section, what the next CP3 live target
// starts with: the environment of the model choice in force, built from
// Options.Live.Env, and the held request settings of its provider when the
// choice is a direct provider. A gateway or no model gives no settings.
func (c *ControlPlane) liveLaunch() ([]string, []modelsettings.Settings) {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	env := liveTargetEnv(c.liveBaseEnv, c.liveModel)
	if direct, ok := c.liveModel.AsDirect(); ok {
		if settings, held := c.heldSettings[direct.Provider]; held {
			return env, []modelsettings.Settings{settings}
		}
	}
	return env, nil
}

// evaluatorSettings returns, in one modelMu section, the held request
// settings of every handed-over provider, in table order: what an accepted
// verification with the operator's stored evaluator receives, independently
// of the live model choice.
func (c *ControlPlane) evaluatorSettings() []modelsettings.Settings {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	var all []modelsettings.Settings
	for _, provider := range modelsettings.Providers() {
		if settings, held := c.heldSettings[provider.Name]; held {
			all = append(all, settings)
		}
	}
	return all
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
	_, known := modelsettings.Lookup(provider)
	switch {
	case gatewayAddr != "" && (provider != "" || modelName != ""):
		return gensurla.Connection{}, errors.New("gateway_addr cannot be combined with provider or model_name; the gateway chooses the model")
	case gatewayAddr != "":
		return gensurla.NewConnectionGateway(&gensurla.ModelGateway{GatewayAddr: gatewayAddr}), nil
	case provider == "" && modelName == "":
		return gensurla.Connection{}, errors.New("specify both provider and model_name, or specify gateway_addr")
	case provider == "" || modelName == "":
		return gensurla.Connection{}, errors.New("provider and model_name must be set together")
	case !known:
		return gensurla.Connection{}, fmt.Errorf("unsupported provider %q (expected \"anthropic\" or \"openai\")", provider)
	default:
		return gensurla.NewConnectionDirect(&gensurla.ModelDirect{Provider: provider, ModelName: modelName}), nil
	}
}

// handedOver returns the providers present in p, by name. A nil map is an
// absent attribute, which keeps what is held; a present one, even empty, is
// handed over and checked.
func handedOver(p *gensurla.HandOverModelSettingsPayload) map[string]map[string]string {
	present := map[string]map[string]string{}
	if p == nil {
		return present
	}
	for name, values := range map[string]map[string]string{"anthropic": p.Anthropic, "openai": p.Openai} {
		if values != nil {
			present[name] = values
		}
	}
	return present
}

// heldNames returns the sorted variable names held for each provider; it is
// never nil, because the result's held attribute is required.
func heldNames(held map[string]modelsettings.Settings) map[string][]string {
	names := make(map[string][]string, len(held))
	for name, settings := range held {
		names[name] = settings.Names()
	}
	return names
}

// liveTargetEnv returns the environment a live target's executor starts
// with for choice: base without any provider family or inherited
// SURLA_LIVE_MODEL_* variable, then the choice's SURLA_LIVE_MODEL_*
// variables. The chosen provider's settings are added only by the launch,
// from the held snapshot, so the hand-over is their one source.
func liveTargetEnv(base []string, choice gensurla.Connection) []string {
	_, rest := modelsettings.Split(base)
	out := make([]string, 0, len(rest)+2)
	for _, kv := range rest {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case verify.LiveModelGatewayEnv, verify.LiveModelProviderEnv, verify.LiveModelNameEnv:
			continue
		}
		out = append(out, kv)
	}
	if gateway, ok := choice.AsGateway(); ok {
		out = append(out, verify.LiveModelGatewayEnv+"="+gateway.GatewayAddr)
	}
	if direct, ok := choice.AsDirect(); ok {
		out = append(out, verify.LiveModelProviderEnv+"="+direct.Provider, verify.LiveModelNameEnv+"="+direct.ModelName)
	}
	return out
}
