// This file constructs providers only in a disposable live-target executor.
package main

import (
	"fmt"
	"strings"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	openaisdk "github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
	"goa.design/goa-ai/features/model/anthropic"
	"goa.design/goa-ai/features/model/openai"
	"goa.design/goa-ai/runtime/agent/model"

	"github.com/goadesign/surla/internal/modelsettings"
)

// newLiveProvider builds the chosen provider after preparation. verifyrun has
// applied these held settings to this executor's environment after process
// startup; neither the test program nor its service receives them. The pinned
// Anthropic SDK uses its API key in preference to ANTHROPIC_AUTH_TOKEN and
// ANTHROPIC_CUSTOM_HEADERS, so those two settings are ignored when a key is held.
func newLiveProvider(name, modelName string, settings []modelsettings.Settings) (model.Provider, error) {
	provider, ok := modelsettings.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("unknown live model provider")
	}
	key := ""
	for _, held := range settings {
		if held.Provider() != name {
			continue
		}
		for _, entry := range held.Environ() {
			if variable, value, _ := strings.Cut(entry, "="); variable == provider.Key {
				key = value
			}
		}
	}
	if key == "" {
		return nil, fmt.Errorf("provider %s settings must include %s", name, provider.Key)
	}
	switch name {
	case "anthropic":
		client := anthropicsdk.NewClient(anthropicoption.WithAPIKey(key), anthropicoption.WithoutOpenTelemetry())
		return anthropic.NewProvider(&client.Messages, anthropic.Options{DefaultModel: modelName})
	case "openai":
		client := openaisdk.NewClient(openaioption.WithAPIKey(key))
		return openai.NewProvider(openai.Options{Client: &client.Responses, DefaultModel: modelName})
	default:
		return nil, fmt.Errorf("unknown live model provider")
	}
}
