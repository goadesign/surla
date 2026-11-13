// This file declares the model provider request settings the surla command
// hands to the project server. The variable names come from the one provider
// table in internal/modelsettings, so the generated validation accepts
// exactly the names that table lets cross.
package design

import (
	. "goa.design/goa/v3/dsl"

	"github.com/goadesign/surla/internal/modelsettings"
)

// HandOverModelSettingsPayload carries the request settings of each provider
// the surla command found a key for.
var HandOverModelSettingsPayload = Type("HandOverModelSettingsPayload", func() {
	Description("Request settings the surla command read from the shell it runs in, by provider. Each attribute is optional: an absent provider keeps what the project server holds for it.")
	for _, p := range modelsettings.Providers() {
		providerSettings(p)
	}
})

// ModelSettingsResult names what the project server holds after a hand-over.
var ModelSettingsResult = Type("ModelSettingsResult", func() {
	Description("The provider request settings the project server holds, by variable name only, and the model live targets started afterwards use, as get_model_connection reports it. It never carries a value.")
	Extend(ModelConnection)
	Attribute("held", MapOf(String, ArrayOf(String)), "Names of the request settings held for each provider, sorted; a provider holds nothing until its settings are handed over.", func() {
		Key(func() { Enum(providerNames()...) })
	})
	Required("held", "connection")
})

// providerSettings declares the optional attribute of provider p: a map whose
// keys are exactly p's request settings and whose values are bounded.
func providerSettings(p modelsettings.Provider) {
	names := make([]any, 0, len(p.Settings()))
	for _, name := range p.Settings() {
		names = append(names, name)
	}
	// No MinLength on the map: Goa checks a map's length even when the
	// attribute is absent, and absence means keep. An empty map lacks the key
	// variable, which the control plane refuses.
	Attribute(p.Name, MapOf(String, String), "Request settings for "+p.Name+", keyed by variable name, replacing everything held for "+p.Name+". They must include "+p.Key+". Omit the attribute to keep what is held.", func() {
		Key(func() { Enum(names...) })
		Elem(func() {
			MinLength(1)
			// Goa's length error names the variable without echoing the value.
			MaxLength(modelsettings.MaxValueLength)
		})
	})
}

// providerNames lists the providers of the table as enum values.
func providerNames() []any {
	var names []any
	for _, p := range modelsettings.Providers() {
		names = append(names, p.Name)
	}
	return names
}
