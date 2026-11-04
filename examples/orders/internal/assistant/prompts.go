package assistant

import (
	"context"
	"errors"
	"fmt"

	"github.com/goadesign/surla/examples/orders/internal/orders"
	"goa.design/goa-ai/runtime/agent/prompt"
)

// Prompts changes the refund agent's prompts while the service runs. It
// implements orders.Prompts. The overrides live in the runtime's own prompt
// store, the one the registry reads when the planner renders a prompt, so the
// next model request uses the new text.
type Prompts struct {
	// registry holds the built-in prompt specs and writes validated
	// overrides to the runtime's prompt store.
	registry *prompt.Registry
}

// NewPrompts returns prompts that validate and save overrides through registry.
// registry must be configured with the runtime's prompt store.
func NewPrompts(registry *prompt.Registry) *Prompts {
	return &Prompts{registry: registry}
}

// Override makes template the text of the registered prompt promptID for every
// tenant and returns "sha256:" followed by the template's hex SHA-256, the
// version model requests then carry. It validates template against the
// registered spec's functions, sub-templates, and declared argument type via
// registry.SetOverride before saving it, so a template that does not parse or
// accesses undeclared data is rejected with orders.ErrInvalidPrompt and the
// text in use stays the same. An unregistered promptID fails with
// orders.ErrPromptNotFound.
func (p *Prompts) Override(ctx context.Context, promptID, template string) (string, error) {
	if err := p.registry.SetOverride(ctx, prompt.Ident(promptID), prompt.Scope{}, template, nil); err != nil {
		switch {
		case errors.Is(err, prompt.ErrPromptNotFound):
			return "", fmt.Errorf("%w: %s", orders.ErrPromptNotFound, promptID)
		case errors.Is(err, prompt.ErrInvalidPromptSpec),
			errors.Is(err, prompt.ErrTemplateParse),
			errors.Is(err, prompt.ErrTemplateFieldNotFound):
			return "", fmt.Errorf("%w: %w", orders.ErrInvalidPrompt, err)
		default:
			return "", fmt.Errorf("save prompt override: %w", err)
		}
	}
	return prompt.VersionFromTemplate(template), nil
}
