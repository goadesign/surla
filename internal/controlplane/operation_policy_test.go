package controlplane

import (
	"context"
	"errors"
	"go.opentelemetry.io/otel/trace/noop"
	"testing"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/assert"
)

// TestOperationPolicyCoversEveryTool proves every invocation permission, including denial before mutation.
func TestOperationPolicyCoversEveryTool(t *testing.T) {
	roles := []CallerKind{CallerUser, CallerGuide, CallerBuilder, CallerTestWriter, CallerManagement, ""}
	common := []CallerKind{CallerUser, CallerGuide, CallerBuilder}
	expected := map[string][]CallerKind{
		"checkpoint_ui": {CallerUser}, "golden_path": {CallerUser, CallerGuide, CallerBuilder, CallerTestWriter},
		"confirm_intent": {CallerUser}, "approve_merge": {CallerUser}, "configure_model": {CallerUser},
		"propose_change": {CallerGuide}, "revise_change": {CallerGuide}, "refresh_story": {CallerGuide}, "init_project": {CallerGuide},
		"generate_code": {CallerBuilder}, "check_project": {CallerBuilder}, "update_dependencies": {CallerBuilder}, "start_live_target": {CallerBuilder}, "draft_override": {CallerBuilder}, "verify_change": {CallerBuilder},
		"stop_live_target": {CallerUser, CallerBuilder},
		"get_change":       common, "preview_architecture": common, "diff": common, "get_verification": common, "list_verifications": common, "list_traces": common, "read_check_output": common, "get_story_refresh": common,
	}
	assert.Len(t, expected, len(gensurla.MethodNames))
	for _, name := range gensurla.MethodNames {
		for _, role := range roles {
			t.Run(name+"/"+string(role), func(t *testing.T) {
				permitted := false
				for _, allowed := range expected[name] {
					permitted = permitted || role == allowed
				}
				recorder := noop.NewTracerProvider().Tracer("test")
				mutated := false
				marker := errors.New("called domain operation")
				endpoint := authorizeOperation(name, func(context.Context, any) (any, error) { mutated = true; return nil, marker }, recorder)
				_, err := endpoint(WithPrincipal(t.Context(), Principal{ID: "owner", CallerID: "caller", Kind: role}), nil)
				assert.Equal(t, permitted, mutated)
				if permitted {
					assert.ErrorIs(t, err, marker)
				} else {
					assert.NotErrorIs(t, err, marker)
					assert.ErrorContains(t, err, "may not call")
				}
			})
		}
	}
}
