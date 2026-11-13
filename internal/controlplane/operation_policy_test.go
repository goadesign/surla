package controlplane

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	goa "goa.design/goa/v3/pkg"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/assert"
)

// TestOperationPolicyCoversEveryTool proves every invocation permission, including denial before mutation.
func TestOperationPolicyCoversEveryTool(t *testing.T) {
	roles := []CallerKind{CallerUser, CallerGuide, CallerBuilder, CallerTestWriter, CallerManagement, ""}
	common := []CallerKind{CallerUser, CallerGuide, CallerBuilder}
	expected := map[string][]CallerKind{
		"golden_path":    {CallerUser, CallerGuide, CallerBuilder, CallerTestWriter},
		"confirm_intent": {CallerUser}, "approve_merge": {CallerUser}, "configure_model": {CallerUser},
		"propose_change": {CallerGuide}, "revise_change": {CallerGuide}, "refresh_story": {CallerGuide}, "init_project": {CallerGuide},
		"generate_code": {CallerBuilder}, "check_project": {CallerBuilder}, "update_dependencies": {CallerBuilder}, "start_live_target": {CallerBuilder}, "draft_override": {CallerBuilder}, "verify_change": {CallerBuilder},
		"stop_live_target": {CallerUser, CallerBuilder},
		"get_change":       common, "preview_architecture": common, "diff": common, "get_verification": common, "list_verifications": common, "list_traces": common, "read_check_output": common, "get_story_refresh": common,
		"list_changes": common, "wait_for_changes": {CallerUser},
		"start_build": {CallerUser}, "get_build": {CallerUser, CallerGuide, CallerBuilder, CallerTestWriter},
		"list_builder_assignments": {CallerBuilder}, "get_builder_assignment": {CallerBuilder},
		"list_test_writer_assignments": {CallerTestWriter}, "get_test_writer_assignment": {CallerTestWriter},
		"report_tests_finished": {CallerTestWriter},
	}
	assert.Len(t, expected, len(gensurla.MethodNames))
	for _, name := range gensurla.MethodNames {
		for _, role := range roles {
			t.Run(name+"/"+string(role), func(t *testing.T) {
				permitted := false
				for _, allowed := range expected[name] {
					permitted = permitted || role == allowed
				}
				exporter := tracetest.NewInMemoryExporter()
				provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
				t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
				recorder := provider.Tracer("test")
				mutated := false
				marker := errors.New("called domain operation")
				// Fill every generated field with a unique sentinel, then apply the real wrapper set.
				endpoints := &gensurla.Endpoints{}
				value := reflect.ValueOf(endpoints).Elem()
				for index := 0; index < value.NumField(); index++ {
					if value.Field(index).Type() == reflect.TypeOf(goa.Endpoint(nil)) {
						field := value.Type().Field(index).Name
						value.Field(index).Set(reflect.ValueOf(goa.Endpoint(func(context.Context, any) (any, error) {
							mutated = field == endpointField(name)
							return nil, marker
						})))
					}
				}
				applyOperationPolicy(endpoints, recorder)
				endpoint := value.FieldByName(endpointField(name)).Interface().(goa.Endpoint)
				_, err := endpoint(WithPrincipal(t.Context(), Principal{ID: "owner", CallerID: "caller", Kind: role}), nil)
				assert.Equal(t, permitted, mutated)
				spans := exporter.GetSpans()
				if assert.Len(t, spans, 1) {
					assert.Equal(t, "surla.request."+name, spans[0].Name)
				}
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

// endpointField maps the published operation spelling to its generated Go endpoint field.
func endpointField(operation string) string {
	words := strings.Split(operation, "_")
	for index, word := range words {
		words[index] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, "")
}
