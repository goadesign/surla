package orders

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"

	genprompts "github.com/goadesign/surla/examples/orders/gen/prompts"
	"goa.design/goa/v3/security"
)

type (
	// PromptService implements the generated prompts service: the operation
	// Surla's draft_override calls on a live copy of this platform that Surla
	// built and started. Only a caller holding the live key Surla passed to
	// this process may change a prompt.
	PromptService struct {
		// prompts changes the refund agent's prompts.
		prompts Prompts
		// liveKey is the key override_prompt requires; when it is empty,
		// override_prompt rejects every call.
		liveKey string
	}

	// Prompts changes the refund agent's prompts while the service runs. The
	// agent wiring in package assistant implements it on top of the Goa-AI
	// prompt registry and the service's own prompt store.
	Prompts interface {
		// Override makes template the text of the registered prompt promptID
		// for every tenant and returns the version of the accepted text. It
		// fails with ErrPromptNotFound when no prompt promptID is registered
		// and with ErrInvalidPrompt when template does not render; the text
		// in use is then unchanged.
		Override(ctx context.Context, promptID, template string) (version string, err error)
	}
)

var (
	// ErrPromptNotFound reports a prompt ID that is not registered.
	ErrPromptNotFound = errors.New("prompt not found")
	// ErrInvalidPrompt reports a prompt template that does not render.
	ErrInvalidPrompt = errors.New("invalid prompt template")
)

// NewPromptService returns the prompts service. liveKey is the key Surla
// minted for this process when it started it as a live copy; when it is
// empty, as for a process Surla did not start, every override is rejected.
func NewPromptService(prompts Prompts, liveKey string) *PromptService {
	return &PromptService{prompts: prompts, liveKey: liveKey}
}

// OverridePrompt replaces the text of a registered prompt for every tenant.
// APIKeyAuth has already checked the live key. The new text lasts until the
// process exits.
func (s *PromptService) OverridePrompt(ctx context.Context, p *genprompts.OverridePromptPayload) (*genprompts.OverridePromptResult, error) {
	version, err := s.prompts.Override(ctx, p.PromptID, p.Template)
	switch {
	case errors.Is(err, ErrPromptNotFound):
		return nil, genprompts.MakeUnknownPrompt(err)
	case errors.Is(err, ErrInvalidPrompt):
		return nil, genprompts.MakeInvalidPrompt(err)
	case err != nil:
		return nil, fmt.Errorf("override prompt %s: %w", p.PromptID, err)
	}
	return &genprompts.OverridePromptResult{PromptID: p.PromptID, Version: version}, nil
}

// APIKeyAuth accepts only the live key the process started with, compared in
// constant time. A missing key, a wrong key, or any key when the process has
// none answers unauthorized.
func (s *PromptService) APIKeyAuth(ctx context.Context, key string, _ *security.APIKeyScheme) (context.Context, error) {
	if s.liveKey == "" || subtle.ConstantTimeCompare([]byte(key), []byte(s.liveKey)) != 1 {
		return ctx, genprompts.Unauthorized("missing or wrong live key")
	}
	return ctx, nil
}
