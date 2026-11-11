// This file tests runtime model configuration (`configure_model` /
// `POST /cp1/model`): payload validation, refusal and fault classification
// across Go, HTTP and MCP with their exported OpenTelemetry spans, and the
// live-target environment (`liveEnv`) that a later start_live_target receives.

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
)

// TestConfigureModelValidationTelemetryAndLiveEnv verifies ConfigureModel's
// authentication, payload normalization and cross-field validation, the
// refusal events ("model_configuration_refused") for an invalid payload, a
// host without Options.LiveModelEnv and a missing provider key, the server
// fault a LiveModelEnv failure is, bounded "model_configured" event
// attributes (keeping arbitrarily long accepted model_name and gateway_addr
// values out of telemetry), and that every refusal or fault leaves the
// current live-target environment in place.
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
	initialEnv := []string{"SURLA_LIVE_MODEL_PROVIDER=anthropic", "SURLA_LIVE_MODEL_NAME=claude-sonnet-4-6"}

	t.Run("unauthenticated ConfigureModel is rejected before starting a span", func(t *testing.T) {
		sr.Reset()
		cp := newControlPlane(t, Options{})
		gw := "127.0.0.1:9090"
		_, err := cp.ConfigureModel(context.Background(), &gensurla.ConfigureModelPayload{GatewayAddr: &gw})
		require.EqualError(t, err, "authenticated principal is required")
		assert.Empty(t, endedSpansByName(sr, "surla.controlplane.configure_model"))
	})

	t.Run("reconfiguration unavailable records refusal event without error status and keeps the live environment", func(t *testing.T) {
		sr.Reset()
		cp := newControlPlane(t, Options{Live: &LiveConfig{Env: initialEnv}})
		gw := "127.0.0.1:9090"
		_, err := cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{GatewayAddr: &gw})
		assertServiceError(t, err, "not_implemented")
		assert.ErrorContains(t, err, msgModelReconfigUnavailable)

		spans := endedSpansByName(sr, "surla.controlplane.configure_model")
		require.Len(t, spans, 1)
		assert.Equal(t, codes.Unset, spans[0].Status().Code)
		assert.Empty(t, exceptionMessages(spans[0]))
		assert.Equal(t, map[string]map[string]string{
			"model_configuration_refused": {"surla.model.refusal_reason": "reconfiguration_unavailable"},
		}, nonExceptionEvents(spans[0]))
		assert.Equal(t, initialEnv, cp.liveEnv())
	})

	t.Run("payload and LiveModelEnv refusal vs fault classification across Go HTTP MCP and exported spans", func(t *testing.T) {
		var envErr error
		cp := newControlPlane(t, Options{
			Live: &LiveConfig{Env: initialEnv},
			LiveModelEnv: func(*gensurla.ConfigureModelPayload) ([]string, error) {
				if envErr != nil {
					return nil, envErr
				}
				return []string{"SURLA_LIVE_MODEL_GATEWAY=unused"}, nil
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
			{name: "nil payload", payload: nil, wantErr: "specify both provider and model_name, or specify gateway_addr"},
			{name: "all empty", payload: &gensurla.ConfigureModelPayload{Provider: strPtr("  "), ModelName: strPtr("\t")}, wantErr: "specify both provider and model_name, or specify gateway_addr"},
			{name: "gateway combined with provider", payload: &gensurla.ConfigureModelPayload{GatewayAddr: strPtr("127.0.0.1:9090"), Provider: strPtr("anthropic")}, wantErr: "gateway_addr cannot be combined with provider or model_name"},
			{name: "gateway combined with model_name", payload: &gensurla.ConfigureModelPayload{GatewayAddr: strPtr("127.0.0.1:9090"), ModelName: strPtr("claude-sonnet-4-6")}, wantErr: "gateway_addr cannot be combined with provider or model_name"},
			{name: "provider without model_name", payload: &gensurla.ConfigureModelPayload{Provider: strPtr("anthropic")}, wantErr: "provider and model_name must be set together"},
			{name: "model_name without provider", payload: &gensurla.ConfigureModelPayload{ModelName: strPtr("claude-sonnet-4-6")}, wantErr: "provider and model_name must be set together"},
			{name: "unsupported provider", payload: &gensurla.ConfigureModelPayload{Provider: strPtr("Anthropic"), ModelName: strPtr("claude-sonnet-4-6")}, wantErr: `unsupported provider "Anthropic"`},
		}
		for _, tc := range invalidCases {
			t.Run(tc.name, func(t *testing.T) {
				sr.Reset()
				_, err := cp.ConfigureModel(alice, tc.payload)
				assertServiceError(t, err, "unprocessable")
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

		reqBodyJSON, err := json.Marshal(map[string]any{"provider": "openai", "model_name": "gpt-5.4-mini"})
		require.NoError(t, err)
		assertSpanOutcome := func(t *testing.T, wantCode codes.Code, wantExceptions []string, wantEvents map[string]map[string]string) {
			t.Helper()
			spans := endedSpansByName(sr, "surla.controlplane.configure_model")
			require.Len(t, spans, 1)
			assert.Equal(t, wantCode, spans[0].Status().Code)
			assert.Equal(t, wantExceptions, exceptionMessages(spans[0]))
			assert.Equal(t, wantEvents, nonExceptionEvents(spans[0]))
		}
		assertHTTPAndMCP := func(t *testing.T, wantHTTPCode int, wantHTTPName string, wantFault bool, wantSpanCode codes.Code, wantExceptions []string, wantEvents map[string]map[string]string) {
			t.Helper()
			sr.Reset()
			req := httptest.NewRequest(http.MethodPost, "/cp1/model", strings.NewReader(string(reqBodyJSON)))
			req.Header.Set("Authorization", "Bearer alice-token")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, wantHTTPCode, rec.Code)
			var httpErr struct {
				Name  string `json:"name"`
				Fault bool   `json:"fault"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &httpErr))
			assert.Equal(t, wantHTTPName, httpErr.Name)
			assert.Equal(t, wantFault, httpErr.Fault)
			assertSpanOutcome(t, wantSpanCode, wantExceptions, wantEvents)

			sr.Reset()
			mcpRes, mErr := mcpAdapter.ToolsCall(alice, &genmcpadapter.ToolsCallPayload{Name: "configure_model", Arguments: reqBodyJSON})
			require.NoError(t, mErr)
			complete, ok := mcpRes.Outcome.AsComplete()
			require.True(t, ok)
			require.NotNil(t, complete.IsError)
			assert.True(t, *complete.IsError)
			assertSpanOutcome(t, wantSpanCode, wantExceptions, wantEvents)
		}

		// A missing provider key is an expected 422 refusal across Go, HTTP,
		// MCP and exported spans, whether returned directly or wrapped.
		missingKeyCause := errors.New("provider openai requires OPENAI_API_KEY in surla serve's environment")
		refusal := gensurla.MakeUnprocessable(missingKeyCause)
		missingKeyEvents := map[string]map[string]string{
			"model_configuration_refused": {"surla.model.refusal_reason": "missing_api_key"},
		}
		for _, tc := range []struct {
			name string
			err  error
		}{
			{name: "direct unprocessable refusal", err: refusal},
			{name: "wrapped unprocessable refusal", err: errors.Join(refusal)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				envErr = tc.err
				sr.Reset()
				_, goErr := cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{Provider: strPtr("openai"), ModelName: strPtr("gpt-5.4-mini")})
				assert.ErrorIs(t, goErr, refusal)
				assert.ErrorIs(t, goErr, missingKeyCause)
				assertSpanOutcome(t, codes.Unset, nil, missingKeyEvents)
				assertHTTPAndMCP(t, http.StatusUnprocessableEntity, "unprocessable", false, codes.Unset, nil, missingKeyEvents)
			})
		}

		// Any other LiveModelEnv failure is a server fault: HTTP 500 with
		// fault true, span RecordError with error status, and no refusal event.
		t.Run("unexpected LiveModelEnv failure", func(t *testing.T) {
			fault := errors.New("read operator environment failed")
			envErr = fault
			sr.Reset()
			_, goErr := cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{Provider: strPtr("openai"), ModelName: strPtr("gpt-5.4-mini")})
			assert.ErrorIs(t, goErr, fault)
			assertSpanOutcome(t, codes.Error, []string{fault.Error()}, nil)
			assertHTTPAndMCP(t, http.StatusInternalServerError, "fault", true, codes.Error, []string{fault.Error()}, nil)
		})

		assert.Equal(t, initialEnv, cp.liveEnv(), "no refusal or fault changes the live-target environment")
	})

	t.Run("successful provider and gateway transitions record bounded telemetry and preserve long accepted names", func(t *testing.T) {
		cp := newControlPlane(t, Options{
			Live: &LiveConfig{Env: initialEnv},
			LiveModelEnv: func(p *gensurla.ConfigureModelPayload) ([]string, error) {
				if p.GatewayAddr != nil {
					return []string{"SURLA_LIVE_MODEL_GATEWAY=" + *p.GatewayAddr}, nil
				}
				return []string{"SURLA_LIVE_MODEL_PROVIDER=" + *p.Provider, "SURLA_LIVE_MODEL_NAME=" + *p.ModelName}, nil
			},
		})

		// 1. Switch to a long openai model_name; surrounding whitespace is trimmed.
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
		assert.Equal(t, []string{"SURLA_LIVE_MODEL_PROVIDER=openai", "SURLA_LIVE_MODEL_NAME=" + longModelName}, cp.liveEnv())

		cfgSpans := endedSpansByName(sr, "surla.controlplane.configure_model")
		require.Len(t, cfgSpans, 1)
		assert.Equal(t, codes.Unset, cfgSpans[0].Status().Code)
		assert.Empty(t, exceptionMessages(cfgSpans[0]))
		assert.Empty(t, cfgSpans[0].Attributes())
		assert.Equal(t, map[string]map[string]string{
			"model_configured": {
				"surla.model.connection_kind": "provider",
				"surla.model.provider":        "openai",
			},
		}, nonExceptionEvents(cfgSpans[0]))
		assertSpanNeverContainsValue(t, cfgSpans[0], longModelName)

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
			LiveModelEnv: func(p *gensurla.ConfigureModelPayload) ([]string, error) {
				base := append(os.Environ(), liveTargetModeEnv+"=serve")
				if p.GatewayAddr != nil {
					return append(base, "SURLA_LIVE_MODEL_GATEWAY="+*p.GatewayAddr), nil
				}
				return append(base, "SURLA_LIVE_MODEL_PROVIDER="+*p.Provider, "SURLA_LIVE_MODEL_NAME="+*p.ModelName), nil
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
