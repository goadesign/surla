// This file tests runtime model configuration (`configure_model` / `POST /cp1/model`),
// reference-counted model handle retirement across in-flight `ProposeChange`,
// `ReviseChange`, and `Close`, OpenTelemetry SDK exported span events and
// statuses, and live-target environment propagation (`liveEnv`).

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	goa "goa.design/goa/v3/pkg"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
)

// TestConfigureModelValidationTelemetryAndLiveEnv verifies ConfigureModel's
// authentication, payload normalization and cross-field validation, capability
// and credential refusal span events ("model_configuration_refused"), exact
// single/duplicate-refusal vs. mixed/distinct-leaf ModelBuilder error
// classification across Go, HTTP, MCP, and exported OpenTelemetry SDK spans,
// bounded "model_configured" event attributes (preserving arbitrarily long
// accepted model_name and gateway_addr values without leaking them into
// telemetry), retired-model cleanup span isolation ("surla.controlplane.retire_model"),
// liveEnv propagation across StartLiveTarget, and CloseAnalyzer cleanup when
// controlplane.New fails validation.
func TestConfigureModelValidationTelemetryAndLiveEnv(t *testing.T) {
	prevProvider := otel.GetTracerProvider()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prevProvider)
		require.NoError(t, tp.Shutdown(context.Background()))
	})

	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	summaryJSON := func(summary string) string {
		raw, err := json.Marshal(CP1SemanticAnalysis{
			Summary:     summary,
			Ambiguities: []CP1ModelAmbiguity{},
			Examples: []CP1ModelExample{{
				Scenario:         "Refund at limit",
				InputSummary:     "$50",
				ExpectedOutcome:  "Instant credit",
				VerificationKind: "deterministic",
			}},
			PreservedBehaviors: []string{},
		})
		require.NoError(t, err)
		return string(raw)
	}

	t.Run("controlplane.New closes initial analyzer when validation fails", func(t *testing.T) {
		var closed atomic.Int32
		_, err := New(Options{
			CloseAnalyzer: func() error {
				closed.Add(1)
				return nil
			},
		})
		require.ErrorContains(t, err, "cards directory is required")
		assert.EqualValues(t, 1, closed.Load())

		var conflictClosed atomic.Int32
		_, err = New(Options{
			CardsDir:      t.TempDir(),
			TargetUpdater: staticUpdater{},
			Live:          &LiveConfig{},
			CloseAnalyzer: func() error {
				conflictClosed.Add(1)
				return nil
			},
		})
		require.ErrorContains(t, err, "set either TargetUpdater or Live")
		assert.EqualValues(t, 1, conflictClosed.Load())
	})

	t.Run("unauthenticated ConfigureModel is rejected before starting a span", func(t *testing.T) {
		sr.Reset()
		cp := newControlPlane(t, Options{})
		gw := "127.0.0.1:9090"
		_, err := cp.ConfigureModel(context.Background(), &gensurla.ConfigureModelPayload{GatewayAddr: &gw})
		require.EqualError(t, err, "authenticated principal is required")
		assert.Empty(t, endedSpansByName(sr, "surla.controlplane.configure_model"))
	})

	t.Run("reconfiguration unavailable records refusal event without error status and preserves fixed analyzer", func(t *testing.T) {
		sr.Reset()
		cp := newControlPlane(t, Options{
			Analyzer: NewModelCP1Analyzer(newStubModelClient(t, summaryJSON("Fixed host analyzer"))),
			Live:     &LiveConfig{Env: []string{"SURLA_LIVE_MODEL_PROVIDER=anthropic", "SURLA_LIVE_MODEL_NAME=claude-fixed"}},
		})
		gw := "127.0.0.1:9090"
		_, err := cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{GatewayAddr: &gw})
		require.Error(t, err)
		assert.Equal(t, "not_implemented", goaErrorName(err))
		assert.ErrorContains(t, err, msgModelReconfigUnavailable)

		spans := endedSpansByName(sr, "surla.controlplane.configure_model")
		require.Len(t, spans, 1)
		assert.Equal(t, codes.Unset, spans[0].Status().Code)
		assert.Empty(t, exceptionMessages(spans[0]))
		assert.Equal(t, map[string]map[string]string{
			"model_configuration_refused": {"surla.model.refusal_reason": "reconfiguration_unavailable"},
		}, nonExceptionEvents(spans[0]))

		card, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "chg-fixed", Prompt: "Test fixed analyzer"})
		require.NoError(t, err)
		assert.Equal(t, "Fixed host analyzer", card.Summary)
		assert.Equal(t, []string{"SURLA_LIVE_MODEL_PROVIDER=anthropic", "SURLA_LIVE_MODEL_NAME=claude-fixed"}, cp.liveEnv())
	})

	t.Run("payload and ModelBuilder refusal vs fault classification across Go HTTP MCP and exported spans", func(t *testing.T) {
		var buildErr error
		cp := newControlPlane(t, Options{
			Analyzer: NewModelCP1Analyzer(newStubModelClient(t, summaryJSON("Initial model"))),
			Live:     &LiveConfig{Env: []string{"SURLA_LIVE_MODEL_PROVIDER=anthropic", "SURLA_LIVE_MODEL_NAME=claude-sonnet-4-6"}},
			ModelBuilder: func(*gensurla.ConfigureModelPayload) (CP1Analyzer, []string, func() error, error) {
				if buildErr != nil {
					return nil, nil, nil, buildErr
				}
				return NewModelCP1Analyzer(newStubModelClient(t, summaryJSON("Unused"))), nil, nil, nil
			},
		})
		handler := NewHTTPHandler(cp, NewTokenAuthenticator(map[string]Principal{
			"alice-token": {ID: "alice@example.com"},
		}))
		mcpAdapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)

		strPtr := func(s string) *string { return &s }
		invalidCases := []struct {
			name    string
			payload *gensurla.ConfigureModelPayload
			wantErr string
		}{
			{
				name:    "nil payload",
				payload: nil,
				wantErr: "specify both provider and model_name, or specify gateway_addr",
			},
			{
				name:    "all empty",
				payload: &gensurla.ConfigureModelPayload{Provider: strPtr("  "), ModelName: strPtr("\t")},
				wantErr: "specify both provider and model_name, or specify gateway_addr",
			},
			{
				name:    "gateway combined with provider",
				payload: &gensurla.ConfigureModelPayload{GatewayAddr: strPtr("127.0.0.1:9090"), Provider: strPtr("anthropic")},
				wantErr: "gateway_addr cannot be combined with provider or model_name",
			},
			{
				name:    "gateway combined with model_name",
				payload: &gensurla.ConfigureModelPayload{GatewayAddr: strPtr("127.0.0.1:9090"), ModelName: strPtr("claude-sonnet-4-6")},
				wantErr: "gateway_addr cannot be combined with provider or model_name",
			},
			{
				name:    "provider without model_name",
				payload: &gensurla.ConfigureModelPayload{Provider: strPtr("anthropic")},
				wantErr: "provider and model_name must be set together",
			},
			{
				name:    "model_name without provider",
				payload: &gensurla.ConfigureModelPayload{ModelName: strPtr("claude-sonnet-4-6")},
				wantErr: "provider and model_name must be set together",
			},
			{
				name:    "unsupported provider",
				payload: &gensurla.ConfigureModelPayload{Provider: strPtr("Anthropic"), ModelName: strPtr("claude-sonnet-4-6")},
				wantErr: `unsupported provider "Anthropic"`,
			},
		}
		for _, tc := range invalidCases {
			t.Run(tc.name, func(t *testing.T) {
				sr.Reset()
				_, err := cp.ConfigureModel(alice, tc.payload)
				require.Error(t, err)
				assert.Equal(t, "unprocessable", goaErrorName(err))
				assert.ErrorContains(t, err, tc.wantErr)

				spans := endedSpansByName(sr, "surla.controlplane.configure_model")
				require.Len(t, spans, 1)
				assert.Equal(t, codes.Unset, spans[0].Status().Code)
				assert.Empty(t, exceptionMessages(spans[0]))
				assert.Equal(t, map[string]map[string]string{
					"model_configuration_refused": {"surla.model.refusal_reason": "invalid_payload"},
				}, nonExceptionEvents(spans[0]))
			})
		}

		// Pure missing-key refusals (direct, wrapped, and duplicate-joined of the same refusal leaf)
		// remain expected 422 refusals across Go, HTTP, MCP, and exported spans.
		missingKeyCause := errors.New("provider openai requires OPENAI_API_KEY in surla serve's environment")
		sameRefusal := gensurla.MakeUnprocessable(missingKeyCause)
		pureRefusalCases := []struct {
			name string
			err  error
		}{
			{name: "direct unprocessable refusal", err: sameRefusal},
			{name: "wrapped unprocessable refusal", err: fmt.Errorf("check provider credentials: %w", sameRefusal)},
			{name: "duplicate join of same unprocessable refusal", err: errors.Join(sameRefusal, sameRefusal)},
		}
		reqBodyJSON, err := json.Marshal(map[string]any{"provider": "openai", "model_name": "gpt-5.4-mini"})
		require.NoError(t, err)
		for _, tc := range pureRefusalCases {
			t.Run(tc.name, func(t *testing.T) {
				buildErr = tc.err

				sr.Reset()
				_, goErr := cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{
					Provider:  strPtr("openai"),
					ModelName: strPtr("gpt-5.4-mini"),
				})
				require.Error(t, goErr)
				assert.ErrorIs(t, goErr, sameRefusal)
				assert.ErrorIs(t, goErr, missingKeyCause)
				spans := endedSpansByName(sr, "surla.controlplane.configure_model")
				require.Len(t, spans, 1)
				assert.Equal(t, codes.Unset, spans[0].Status().Code)
				assert.Empty(t, exceptionMessages(spans[0]))
				assert.Equal(t, map[string]map[string]string{
					"model_configuration_refused": {"surla.model.refusal_reason": "missing_api_key"},
				}, nonExceptionEvents(spans[0]))

				sr.Reset()
				req := httptest.NewRequest(http.MethodPost, "/cp1/model", strings.NewReader(string(reqBodyJSON)))
				req.Header.Set("Authorization", "Bearer alice-token")
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
				var httpErr struct {
					Name  string `json:"name"`
					Fault bool   `json:"fault"`
				}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &httpErr))
				assert.Equal(t, "unprocessable", httpErr.Name)
				assert.False(t, httpErr.Fault)
				spans = endedSpansByName(sr, "surla.controlplane.configure_model")
				require.Len(t, spans, 1)
				assert.Equal(t, codes.Unset, spans[0].Status().Code)
				assert.Empty(t, exceptionMessages(spans[0]))

				sr.Reset()
				mcpRes, mErr := mcpAdapter.ToolsCall(alice, &genmcpadapter.ToolsCallPayload{
					Name:      "configure_model",
					Arguments: reqBodyJSON,
				})
				require.NoError(t, mErr)
				complete, ok := mcpRes.Outcome.AsComplete()
				require.True(t, ok)
				require.NotNil(t, complete.IsError)
				assert.True(t, *complete.IsError)
				spans = endedSpansByName(sr, "surla.controlplane.configure_model")
				require.Len(t, spans, 1)
				assert.Equal(t, codes.Unset, spans[0].Status().Code)
				assert.Empty(t, exceptionMessages(spans[0]))
			})
		}

		// Mixed or distinct-leaf errors from ModelBuilder are server faults (HTTP 500 fault:true,
		// span RecordError + codes.Error, zero refusal events) while keeping all causes accessible.
		distinctRefusal := gensurla.MakeUnprocessable(errors.New("second distinct unprocessable leaf"))
		independentCleanupErr := errors.New("close partial gateway transport failed")
		unexpectedNotImpl := gensurla.MakeNotImplemented(errors.New("unexpected builder not_implemented"))
		faultCases := []struct {
			name       string
			err        error
			wantCauses []error
		}{
			{
				name:       "plain constructor failure",
				err:        independentCleanupErr,
				wantCauses: []error{independentCleanupErr},
			},
			{
				name:       "unprocessable joined with independent cleanup failure",
				err:        errors.Join(sameRefusal, independentCleanupErr),
				wantCauses: []error{sameRefusal, missingKeyCause, independentCleanupErr},
			},
			{
				name:       "unprocessable joined with context cancellation",
				err:        errors.Join(sameRefusal, context.Canceled),
				wantCauses: []error{sameRefusal, missingKeyCause, context.Canceled},
			},
			{
				name:       "two distinct unprocessable leaves joined",
				err:        errors.Join(sameRefusal, distinctRefusal),
				wantCauses: []error{sameRefusal, distinctRefusal},
			},
			{
				name:       "unexpected not_implemented from ModelBuilder",
				err:        errors.Join(unexpectedNotImpl, independentCleanupErr),
				wantCauses: []error{unexpectedNotImpl, independentCleanupErr},
			},
		}
		for _, tc := range faultCases {
			t.Run(tc.name, func(t *testing.T) {
				buildErr = tc.err

				sr.Reset()
				_, goErr := cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{
					Provider:  strPtr("openai"),
					ModelName: strPtr("gpt-5.4-mini"),
				})
				require.Error(t, goErr)
				for _, cause := range tc.wantCauses {
					assert.ErrorIs(t, goErr, cause)
				}
				var targetSE *goa.ServiceError
				if errors.As(tc.err, &targetSE) {
					var gotSE *goa.ServiceError
					assert.ErrorAs(t, goErr, &gotSE)
				}
				spans := endedSpansByName(sr, "surla.controlplane.configure_model")
				require.Len(t, spans, 1)
				assert.Equal(t, codes.Error, spans[0].Status().Code)
				assert.Equal(t, []string{tc.err.Error()}, exceptionMessages(spans[0]))
				assert.Empty(t, nonExceptionEvents(spans[0]))

				sr.Reset()
				req := httptest.NewRequest(http.MethodPost, "/cp1/model", strings.NewReader(string(reqBodyJSON)))
				req.Header.Set("Authorization", "Bearer alice-token")
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				assert.Equal(t, http.StatusInternalServerError, rec.Code)
				var httpErr struct {
					Name  string `json:"name"`
					Fault bool   `json:"fault"`
				}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &httpErr))
				assert.Equal(t, "fault", httpErr.Name)
				assert.True(t, httpErr.Fault)
				spans = endedSpansByName(sr, "surla.controlplane.configure_model")
				require.Len(t, spans, 1)
				assert.Equal(t, codes.Error, spans[0].Status().Code)
				assert.Equal(t, []string{tc.err.Error()}, exceptionMessages(spans[0]))
				assert.Empty(t, nonExceptionEvents(spans[0]))

				sr.Reset()
				mcpRes, mErr := mcpAdapter.ToolsCall(alice, &genmcpadapter.ToolsCallPayload{
					Name:      "configure_model",
					Arguments: reqBodyJSON,
				})
				require.NoError(t, mErr)
				complete, ok := mcpRes.Outcome.AsComplete()
				require.True(t, ok)
				require.NotNil(t, complete.IsError)
				assert.True(t, *complete.IsError)
				spans = endedSpansByName(sr, "surla.controlplane.configure_model")
				require.Len(t, spans, 1)
				assert.Equal(t, codes.Error, spans[0].Status().Code)
				assert.Equal(t, []string{tc.err.Error()}, exceptionMessages(spans[0]))
				assert.Empty(t, nonExceptionEvents(spans[0]))
			})
		}

		// Active model and liveEnv remain untouched after all refusals and faults.
		card, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "chg-still-active", Prompt: "Still active"})
		require.NoError(t, err)
		assert.Equal(t, "Initial model", card.Summary)
		assert.Equal(t, []string{"SURLA_LIVE_MODEL_PROVIDER=anthropic", "SURLA_LIVE_MODEL_NAME=claude-sonnet-4-6"}, cp.liveEnv())
	})

	t.Run("successful provider and gateway transitions record bounded telemetry and preserve long accepted names", func(t *testing.T) {
		var closeInitialCalls atomic.Int32
		var closeSecondCalls atomic.Int32
		cp := newControlPlane(t, Options{
			Analyzer: NewModelCP1Analyzer(newStubModelClient(t, summaryJSON("Initial anthropic"))),
			CloseAnalyzer: func() error {
				closeInitialCalls.Add(1)
				return errors.New("retire initial transport warning")
			},
			Live: &LiveConfig{Env: []string{"SURLA_LIVE_MODEL_PROVIDER=anthropic", "SURLA_LIVE_MODEL_NAME=claude-sonnet-4-6"}},
			ModelBuilder: func(p *gensurla.ConfigureModelPayload) (CP1Analyzer, []string, func() error, error) {
				if p.GatewayAddr != nil {
					return NewModelCP1Analyzer(newStubModelClient(t, summaryJSON("Gateway "+*p.GatewayAddr))),
						[]string{"SURLA_LIVE_MODEL_GATEWAY=" + *p.GatewayAddr},
						nil,
						nil
				}
				return NewModelCP1Analyzer(newStubModelClient(t, summaryJSON("Provider "+*p.Provider+"/"+*p.ModelName))),
					[]string{"SURLA_LIVE_MODEL_PROVIDER=" + *p.Provider, "SURLA_LIVE_MODEL_NAME=" + *p.ModelName},
					func() error {
						closeSecondCalls.Add(1)
						return nil
					},
					nil
			},
		})

		// 1. Switch from initial anthropic (whose closeFn returns an error) to a long openai model_name.
		sr.Reset()
		longModelName := "gpt-5.4-mini-custom-" + strings.Repeat("m", 4096)
		prov, paddedModelName := "  openai ", " "+longModelName+"  "
		res, err := cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{
			Provider:  &prov,
			ModelName: &paddedModelName,
		})
		require.NoError(t, err)
		require.NotNil(t, res.Provider)
		require.NotNil(t, res.ModelName)
		assert.Equal(t, "openai", *res.Provider)
		assert.Equal(t, longModelName, *res.ModelName)
		assert.Nil(t, res.GatewayAddr)
		assert.EqualValues(t, 1, closeInitialCalls.Load())
		assert.Equal(t, []string{"SURLA_LIVE_MODEL_PROVIDER=openai", "SURLA_LIVE_MODEL_NAME=" + longModelName}, cp.liveEnv())

		cfgSpans := endedSpansByName(sr, "surla.controlplane.configure_model")
		require.Len(t, cfgSpans, 1)
		assert.Equal(t, codes.Unset, cfgSpans[0].Status().Code, "old transport retirement failure must not mark configure_model failed")
		assert.Empty(t, exceptionMessages(cfgSpans[0]))
		assert.Empty(t, cfgSpans[0].Attributes())
		assert.Equal(t, map[string]map[string]string{
			"model_configured": {
				"surla.model.connection_kind": "provider",
				"surla.model.provider":        "openai",
			},
		}, nonExceptionEvents(cfgSpans[0]))
		assertSpanNeverContainsValue(t, cfgSpans[0], longModelName)

		retireSpans := endedSpansByName(sr, "surla.controlplane.retire_model")
		require.Len(t, retireSpans, 1)
		assert.Equal(t, codes.Error, retireSpans[0].Status().Code)
		assert.Equal(t, []string{"retire initial transport warning"}, exceptionMessages(retireSpans[0]))

		card1, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "chg-oai", Prompt: "Use openai"})
		require.NoError(t, err)
		assert.Equal(t, "Provider openai/"+longModelName, card1.Summary)

		// 2. Switch via MCP tools/call to a long gateway_addr.
		sr.Reset()
		longGatewayAddr := "127.0.0.1:9090/" + strings.Repeat("g", 4096)
		adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)
		argsJSON, err := json.Marshal(map[string]any{"gateway_addr": " " + longGatewayAddr + " "})
		require.NoError(t, err)
		mcpRes, err := adapter.ToolsCall(alice, &genmcpadapter.ToolsCallPayload{Name: "configure_model", Arguments: argsJSON})
		require.NoError(t, err)
		complete, ok := mcpRes.Outcome.AsComplete()
		require.True(t, ok)
		assert.False(t, complete.IsError != nil && *complete.IsError)
		assert.EqualValues(t, 1, closeSecondCalls.Load())
		assert.Equal(t, []string{"SURLA_LIVE_MODEL_GATEWAY=" + longGatewayAddr}, cp.liveEnv())

		cfgSpans = endedSpansByName(sr, "surla.controlplane.configure_model")
		require.Len(t, cfgSpans, 1)
		assert.Equal(t, codes.Unset, cfgSpans[0].Status().Code)
		assert.Empty(t, cfgSpans[0].Attributes())
		assert.Equal(t, map[string]map[string]string{
			"model_configured": {
				"surla.model.connection_kind": "gateway",
			},
		}, nonExceptionEvents(cfgSpans[0]))
		assertSpanNeverContainsValue(t, cfgSpans[0], longGatewayAddr)

		card2, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "chg-gw", Prompt: "Use gateway"})
		require.NoError(t, err)
		assert.Equal(t, "Gateway "+longGatewayAddr, card2.Summary)
	})

	t.Run("StartLiveTarget launches child process with updated model liveEnv across ConfigureModel transitions", func(t *testing.T) {
		self, err := os.Executable()
		require.NoError(t, err)
		envFile := t.TempDir() + "/live-env.txt"
		cardsDir := t.TempDir()
		cp, err := New(Options{
			CardsDir:     cardsDir,
			Verification: &VerificationConfig{RunsDir: t.TempDir()},
			Live: &LiveConfig{
				Command: []string{
					"/bin/sh",
					"-c",
					`printf 'PROVIDER=%s\nNAME=%s\nGATEWAY=%s\nTOKEN=%s\n' "$SURLA_LIVE_MODEL_PROVIDER" "$SURLA_LIVE_MODEL_NAME" "$SURLA_LIVE_MODEL_GATEWAY" "$SURLA_AUTH_TOKEN" > "$1" && exec "$0" "$2" "$3" "$4"`,
					self,
					envFile,
				},
				Env: append(
					os.Environ(),
					liveTargetModeEnv+"=serve",
					"SURLA_LIVE_MODEL_PROVIDER=anthropic",
					"SURLA_LIVE_MODEL_NAME=claude-sonnet-4-6",
				),
			},
			ModelBuilder: func(p *gensurla.ConfigureModelPayload) (CP1Analyzer, []string, func() error, error) {
				base := append(os.Environ(), liveTargetModeEnv+"=serve")
				if p.GatewayAddr != nil {
					return NewModelCP1Analyzer(newStubModelClient(t, summaryJSON("gw"))),
						append(base, "SURLA_LIVE_MODEL_GATEWAY="+*p.GatewayAddr),
						nil,
						nil
				}
				return NewModelCP1Analyzer(newStubModelClient(t, summaryJSON("prov"))),
					append(base, "SURLA_LIVE_MODEL_PROVIDER="+*p.Provider, "SURLA_LIVE_MODEL_NAME="+*p.ModelName),
					nil,
					nil
			},
		})
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, cp.Close()) })

		commit := strings.Repeat("a", 40)
		_, err = cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: commit})
		require.NoError(t, err)
		gotEnv, err := os.ReadFile(envFile)
		require.NoError(t, err)
		assert.Equal(t, "PROVIDER=anthropic\nNAME=claude-sonnet-4-6\nGATEWAY=\nTOKEN=\n", string(gotEnv))

		prov, modelName := "openai", "gpt-5.4-mini"
		_, err = cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{Provider: &prov, ModelName: &modelName})
		require.NoError(t, err)
		_, err = cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: commit})
		require.NoError(t, err)
		gotEnv, err = os.ReadFile(envFile)
		require.NoError(t, err)
		assert.Equal(t, "PROVIDER=openai\nNAME=gpt-5.4-mini\nGATEWAY=\nTOKEN=\n", string(gotEnv))

		gw := "127.0.0.1:9090"
		_, err = cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{GatewayAddr: &gw})
		require.NoError(t, err)
		_, err = cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: commit})
		require.NoError(t, err)
		gotEnv, err = os.ReadFile(envFile)
		require.NoError(t, err)
		assert.Equal(t, "PROVIDER=\nNAME=\nGATEWAY=127.0.0.1:9090\nTOKEN=\n", string(gotEnv))
	})
}

// TestModelLifecycleBarriersAcrossCloseAndConfigureModel exercises deterministic
// channel barriers (zero sleeps, zero busy-loops) across:
//  1. In-flight ProposeChange (Analyze) and separately ReviseChange (Revise)
//     when ControlPlane.Close runs with a CloseAnalyzer spy.
//  2. In-flight ProposeChange and separately ReviseChange when ConfigureModel
//     switches to a new model (including when the retired model's closeFn fails).
//  3. Retiring a superseded model handle when releaseModel runs without an
//     enclosing operation span (such as an early pre-span exit).
//  4. Overlapping switches (model 1 -> model 2 -> model 3) while calls A and B
//     are held in flight on models 1 and 2.
//  5. ModelBuilder completing after ControlPlane.Close has already closed the
//     control plane.
func TestModelLifecycleBarriersAcrossCloseAndConfigureModel(t *testing.T) {
	prevProvider := otel.GetTracerProvider()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prevProvider)
		require.NoError(t, tp.Shutdown(context.Background()))
	})

	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})

	t.Run("Close during in-flight ProposeChange waits to call CloseAnalyzer until Analyze finishes", func(t *testing.T) {
		dir := t.TempDir()
		held := &heldAnalyzer{started: make(chan struct{}), release: make(chan struct{})}
		var closeCount atomic.Int32
		cp, err := New(Options{
			CardsDir: dir,
			Analyzer: held,
			CloseAnalyzer: func() error {
				closeCount.Add(1)
				return nil
			},
		})
		require.NoError(t, err)

		proposeErrCh := make(chan error, 1)
		go func() {
			_, pErr := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{
				ChangeID: "chg-inflight-propose",
				Prompt:   "Refunds up to $50 are instant.",
			})
			proposeErrCh <- pErr
		}()
		<-held.started

		require.NoError(t, cp.Close())
		assert.EqualValues(t, 0, closeCount.Load(), "CloseAnalyzer must not be called while Analyze is still in flight")

		close(held.release)
		assert.ErrorIs(t, <-proposeErrCh, errCardsClosed)
		assert.EqualValues(t, 1, closeCount.Load(), "CloseAnalyzer must be called once Analyze returns")
		assert.NoFileExists(t, cp.cards.path("chg-inflight-propose"))

		require.NoError(t, cp.Close())
		assert.EqualValues(t, 1, closeCount.Load(), "second Close must not call CloseAnalyzer again")
	})

	t.Run("Close during in-flight ReviseChange waits to call CloseAnalyzer and preserves saved card", func(t *testing.T) {
		dir := t.TempDir()
		held := &heldAnalyzer{started: make(chan struct{}), release: make(chan struct{})}
		var closeCount atomic.Int32
		cp, err := New(Options{
			CardsDir: dir,
			Analyzer: held,
			CloseAnalyzer: func() error {
				closeCount.Add(1)
				return nil
			},
		})
		require.NoError(t, err)
		saved := saveCard(t, cp, nil)
		beforeBytes, err := os.ReadFile(cp.cards.path("chg"))
		require.NoError(t, err)

		reviseErrCh := make(chan error, 1)
		go func() {
			_, rErr := cp.ReviseChange(alice, &gensurla.ReviseChangePayload{
				ChangeID:        "chg",
				RequestRevision: saved.RequestRevision,
				Prompt:          "Revise while closing.",
			})
			reviseErrCh <- rErr
		}()
		<-held.started

		require.NoError(t, cp.Close())
		assert.EqualValues(t, 0, closeCount.Load(), "CloseAnalyzer must not be called while Revise is still in flight")

		close(held.release)
		assert.ErrorIs(t, <-reviseErrCh, errCardsClosed)
		assert.EqualValues(t, 1, closeCount.Load(), "CloseAnalyzer must be called once Revise returns")
		afterBytes, err := os.ReadFile(cp.cards.path("chg"))
		require.NoError(t, err)
		assert.Equal(t, string(beforeBytes), string(afterBytes), "saved card bytes must remain unchanged")

		require.NoError(t, cp.Close())
		assert.EqualValues(t, 1, closeCount.Load(), "second Close must not call CloseAnalyzer again")
	})

	t.Run("ConfigureModel during in-flight ProposeChange and ReviseChange switches immediately and retires old handle on completion", func(t *testing.T) {
		for _, op := range []string{"propose", "revise"} {
			t.Run(op, func(t *testing.T) {
				sr.Reset()
				held1 := &heldAnalyzer{started: make(chan struct{}), release: make(chan struct{})}
				var close1Count atomic.Int32
				var close2Count atomic.Int32
				fast2 := &stubCP1Analyzer{
					analyzeFn: func(context.Context, string, string) (CP1SemanticAnalysis, error) {
						return CP1SemanticAnalysis{
							Summary: "Fast model 2",
							Examples: []CP1ModelExample{{
								Scenario: "Fast", InputSummary: "$10", ExpectedOutcome: "Instant", VerificationKind: "deterministic",
							}},
						}, nil
					},
				}
				cp := newControlPlane(t, Options{
					Analyzer: held1,
					CloseAnalyzer: func() error {
						close1Count.Add(1)
						return errors.New("close1 transport error")
					},
					ModelBuilder: func(*gensurla.ConfigureModelPayload) (CP1Analyzer, []string, func() error, error) {
						return fast2, nil, func() error {
							close2Count.Add(1)
							return nil
						}, nil
					},
				})
				saved := saveCard(t, cp, nil)

				inflightCh := make(chan error, 1)
				go func() {
					if op == "propose" {
						_, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{
							ChangeID: "chg-held-1",
							Prompt:   "Held on model 1",
						})
						inflightCh <- err
						return
					}
					_, err := cp.ReviseChange(alice, &gensurla.ReviseChangePayload{
						ChangeID:        "chg",
						RequestRevision: saved.RequestRevision,
						Prompt:          "Held revise on model 1",
					})
					inflightCh <- err
				}()
				<-held1.started

				gw := "127.0.0.1:9090"
				_, err := cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{GatewayAddr: &gw})
				require.NoError(t, err)
				assert.EqualValues(t, 0, close1Count.Load(), "model 1 must not close while call 1 is in flight")

				// A new call immediately uses fast2 without waiting for held1.
				card2, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{
					ChangeID: "chg-fast-2",
					Prompt:   "Runs on model 2",
				})
				require.NoError(t, err)
				assert.Equal(t, "Fast model 2", card2.Summary)

				close(held1.release)
				require.NoError(t, <-inflightCh)
				assert.EqualValues(t, 1, close1Count.Load(), "model 1 closes once when the in-flight call finishes")

				retireSpans := endedSpansByName(sr, "surla.controlplane.retire_model")
				require.Len(t, retireSpans, 1)
				assert.Equal(t, codes.Error, retireSpans[0].Status().Code)
				assert.Equal(t, []string{"close1 transport error"}, exceptionMessages(retireSpans[0]))

				opSpanName := "surla.controlplane.propose_change"
				if op == "revise" {
					opSpanName = "surla.controlplane.revise_change"
				}
				opSpans := endedSpansByName(sr, opSpanName)
				require.NotEmpty(t, opSpans)
				assert.Equal(t, codes.Unset, opSpans[0].Status().Code, "completed analysis span must not be marked failed by old-model retirement")
			})
		}
	})

	t.Run("releaseModel without an operation span records a standalone ended retire_model span when superseded", func(t *testing.T) {
		sr.Reset()
		var close1Count atomic.Int32
		cp := newControlPlane(t, Options{
			Analyzer: &heldAnalyzer{started: make(chan struct{}), release: make(chan struct{})},
			CloseAnalyzer: func() error {
				close1Count.Add(1)
				return errors.New("pre-span retire error")
			},
			ModelBuilder: func(*gensurla.ConfigureModelPayload) (CP1Analyzer, []string, func() error, error) {
				return &heldAnalyzer{started: make(chan struct{}), release: make(chan struct{})}, nil, nil, nil
			},
		})

		heldHandle, err := cp.acquireModel()
		require.NoError(t, err)
		require.NotNil(t, heldHandle)

		gw := "127.0.0.1:9090"
		_, err = cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{GatewayAddr: &gw})
		require.NoError(t, err)
		assert.EqualValues(t, 0, close1Count.Load(), "superseded model must remain open while acquired reference is held")

		sr.Reset()
		cp.releaseModel(alice, heldHandle)
		assert.EqualValues(t, 1, close1Count.Load())

		retireSpans := endedSpansByName(sr, "surla.controlplane.retire_model")
		require.Len(t, retireSpans, 1)
		assert.Equal(t, codes.Error, retireSpans[0].Status().Code)
		assert.Equal(t, []string{"pre-span retire error"}, exceptionMessages(retireSpans[0]))
	})

	t.Run("overlapping switches model 1 -> model 2 -> model 3 retire each handle once as its call completes", func(t *testing.T) {
		held1 := &heldAnalyzer{started: make(chan struct{}), release: make(chan struct{})}
		held2 := &heldAnalyzer{started: make(chan struct{}), release: make(chan struct{})}
		fast3 := &stubCP1Analyzer{
			analyzeFn: func(context.Context, string, string) (CP1SemanticAnalysis, error) {
				return CP1SemanticAnalysis{
					Summary: "Model 3",
					Examples: []CP1ModelExample{{
						Scenario: "M3", InputSummary: "$30", ExpectedOutcome: "Instant", VerificationKind: "deterministic",
					}},
				}, nil
			},
		}
		var close1, close2, close3 atomic.Int32
		var step atomic.Int32
		dir := t.TempDir()
		cp, err := New(Options{
			CardsDir: dir,
			Analyzer: held1,
			CloseAnalyzer: func() error {
				close1.Add(1)
				return nil
			},
			ModelBuilder: func(*gensurla.ConfigureModelPayload) (CP1Analyzer, []string, func() error, error) {
				if step.Add(1) == 1 {
					return held2, nil, func() error {
						close2.Add(1)
						return nil
					}, nil
				}
				return fast3, nil, func() error {
					close3.Add(1)
					return nil
				}, nil
			},
		})
		require.NoError(t, err)

		callA := make(chan error, 1)
		go func() {
			_, aErr := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "call-a", Prompt: "On model 1"})
			callA <- aErr
		}()
		<-held1.started

		gw2 := "127.0.0.1:9002"
		_, err = cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{GatewayAddr: &gw2})
		require.NoError(t, err)

		callB := make(chan error, 1)
		go func() {
			_, bErr := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "call-b", Prompt: "On model 2"})
			callB <- bErr
		}()
		<-held2.started

		gw3 := "127.0.0.1:9003"
		_, err = cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{GatewayAddr: &gw3})
		require.NoError(t, err)
		assert.EqualValues(t, 0, close1.Load())
		assert.EqualValues(t, 0, close2.Load())
		assert.EqualValues(t, 0, close3.Load())

		cardC, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "call-c", Prompt: "On model 3"})
		require.NoError(t, err)
		assert.Equal(t, "Model 3", cardC.Summary)

		close(held2.release)
		require.NoError(t, <-callB)
		assert.EqualValues(t, 0, close1.Load())
		assert.EqualValues(t, 1, close2.Load())
		assert.EqualValues(t, 0, close3.Load())

		close(held1.release)
		require.NoError(t, <-callA)
		assert.EqualValues(t, 1, close1.Load())
		assert.EqualValues(t, 1, close2.Load())
		assert.EqualValues(t, 0, close3.Load())

		require.NoError(t, cp.Close())
		assert.EqualValues(t, 1, close1.Load())
		assert.EqualValues(t, 1, close2.Load())
		assert.EqualValues(t, 1, close3.Load())
	})

	t.Run("ModelBuilder finishing after Close retires newly built model immediately and returns errCardsClosed", func(t *testing.T) {
		sr.Reset()
		builderStarted := make(chan struct{})
		builderRelease := make(chan struct{})
		var close1, close2 atomic.Int32
		cp, err := New(Options{
			CardsDir: t.TempDir(),
			Analyzer: &heldAnalyzer{started: make(chan struct{}), release: make(chan struct{})},
			CloseAnalyzer: func() error {
				close1.Add(1)
				return nil
			},
			ModelBuilder: func(*gensurla.ConfigureModelPayload) (CP1Analyzer, []string, func() error, error) {
				close(builderStarted)
				<-builderRelease
				return &heldAnalyzer{started: make(chan struct{}), release: make(chan struct{})}, nil, func() error {
					close2.Add(1)
					return nil
				}, nil
			},
		})
		require.NoError(t, err)

		cfgErrCh := make(chan error, 1)
		go func() {
			gw := "127.0.0.1:9090"
			_, cErr := cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{GatewayAddr: &gw})
			cfgErrCh <- cErr
		}()
		<-builderStarted

		require.NoError(t, cp.Close())
		assert.EqualValues(t, 1, close1.Load(), "initial model 1 closes on Close")
		assert.EqualValues(t, 0, close2.Load())

		close(builderRelease)
		assert.ErrorIs(t, <-cfgErrCh, errCardsClosed)
		assert.EqualValues(t, 1, close2.Load(), "newly built model 2 must be retired immediately when Close raced with ModelBuilder")

		cfgSpans := endedSpansByName(sr, "surla.controlplane.configure_model")
		require.Len(t, cfgSpans, 1)
		assert.Equal(t, codes.Error, cfgSpans[0].Status().Code)
		assert.Empty(t, nonExceptionEvents(cfgSpans[0]))

		retireSpans := endedSpansByName(sr, "surla.controlplane.retire_model")
		require.Len(t, retireSpans, 2, "both model 1 and model 2 retirement spans are recorded")

		require.NoError(t, cp.Close())
		assert.EqualValues(t, 1, close1.Load())
		assert.EqualValues(t, 1, close2.Load())
	})
}

// endedSpansByName returns all ended spans from sr matching the given name.
func endedSpansByName(sr *tracetest.SpanRecorder, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

// nonExceptionEvents extracts non-exception span events as a map from event
// name to string attribute key-value map.
func nonExceptionEvents(s sdktrace.ReadOnlySpan) map[string]map[string]string {
	var out map[string]map[string]string
	for _, ev := range s.Events() {
		if ev.Name == "exception" {
			continue
		}
		if out == nil {
			out = make(map[string]map[string]string)
		}
		attrs := make(map[string]string, len(ev.Attributes))
		for _, kv := range ev.Attributes {
			attrs[string(kv.Key)] = kv.Value.AsString()
		}
		out[ev.Name] = attrs
	}
	return out
}

// exceptionMessages extracts the "exception.message" attribute values from all
// "exception" events recorded via span.RecordError on s.
func exceptionMessages(s sdktrace.ReadOnlySpan) []string {
	var msgs []string
	for _, ev := range s.Events() {
		if ev.Name != "exception" {
			continue
		}
		for _, kv := range ev.Attributes {
			if string(kv.Key) == "exception.message" {
				msgs = append(msgs, kv.Value.AsString())
			}
		}
	}
	return msgs
}

// assertSpanNeverContainsValue asserts that neither the span's attributes nor
// any of its event attributes contain forbidden.
func assertSpanNeverContainsValue(t *testing.T, s sdktrace.ReadOnlySpan, forbidden string) {
	t.Helper()
	checkAttrs := func(kvs []attribute.KeyValue) {
		for _, kv := range kvs {
			assert.NotContains(t, kv.Value.AsString(), forbidden)
		}
	}
	checkAttrs(s.Attributes())
	for _, ev := range s.Events() {
		checkAttrs(ev.Attributes)
	}
}
