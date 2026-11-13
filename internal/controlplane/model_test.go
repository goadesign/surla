// This file tests runtime model configuration (`configure_model` /
// `POST /cp1/model`): payload validation, the refusal of a provider whose
// request settings are not held, across Go, HTTP and MCP with their exported
// OpenTelemetry spans, the environment a later start_live_target receives,
// and the choice `get_model_connection` reports.

package controlplane

import (
	"context"
	"encoding/json"
	"io"
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

// refusalAnthropic and refusalOpenAI are configure_model's refusals of a
// provider whose request settings the server does not hold.
const (
	refusalAnthropic = "provider anthropic needs ANTHROPIC_API_KEY: export it, with any ANTHROPIC_ request settings it needs, in a shell and run surla there; the project server keeps running. Or choose a model gateway."
	refusalOpenAI    = "provider openai needs OPENAI_API_KEY: export it, with any OPENAI_ request settings it needs, in a shell and run surla there; the project server keeps running. Or choose a model gateway."
)

// TestConfigureModelValidationTelemetryAndLiveEnv verifies ConfigureModel's
// authentication, payload normalization and cross-field validation, the
// refusal events ("model_configuration_refused") for an invalid payload and
// for a provider whose settings are not held, which carries the exact
// refusal across Go, HTTP and MCP without an error status, bounded
// "model_configured" event attributes (keeping arbitrarily long accepted
// model_name and gateway_addr values out of telemetry), and that a refusal
// leaves the current choice in place.
func TestConfigureModelValidationTelemetryAndLiveEnv(t *testing.T) {
	prevProvider := otel.GetTracerProvider()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prevProvider)
		require.NoError(t, tp.Shutdown(context.Background()))
	})

	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})

	t.Run("unauthenticated ConfigureModel is rejected before starting a span", func(t *testing.T) {
		sr.Reset()
		cp := newControlPlane(t, Options{})
		gw := "127.0.0.1:9090"
		_, err := cp.ConfigureModel(context.Background(), &gensurla.ConfigureModelPayload{GatewayAddr: &gw})
		require.EqualError(t, err, "authenticated principal is required")
		assert.Empty(t, endedSpansByName(sr, "surla.controlplane.configure_model"))
	})

	t.Run("payload and missing settings refusals across Go HTTP MCP and exported spans", func(t *testing.T) {
		cp := newControlPlane(t, Options{})
		handOverSettings(t, cp, &gensurla.HandOverModelSettingsPayload{Anthropic: map[string]string{"ANTHROPIC_API_KEY": sentinelKey}})
		handler := NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{
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

		// openai's settings are not held: an expected 422 refusal, the same
		// text over Go, HTTP and MCP, with a refusal event and no error status.
		missingKeyEvents := map[string]map[string]string{
			"model_configuration_refused": {"surla.model.refusal_reason": "missing_api_key"},
		}
		reqBodyJSON, err := json.Marshal(map[string]any{"provider": "openai", "model_name": "gpt-5.4-mini"})
		require.NoError(t, err)
		sr.Reset()
		_, goErr := cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{Provider: strPtr("openai"), ModelName: strPtr("gpt-5.4-mini")})
		assertServiceError(t, goErr, "unprocessable")
		assert.EqualError(t, goErr, refusalOpenAI)
		assertConfigureSpan(t, sr, codes.Unset, nil, missingKeyEvents)

		sr.Reset()
		req := httptest.NewRequest(http.MethodPost, "/cp1/model", strings.NewReader(string(reqBodyJSON)))
		req.Header.Set("Authorization", "Bearer alice-token")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		var httpErr struct {
			Name    string `json:"name"`
			Message string `json:"message"`
			Fault   bool   `json:"fault"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &httpErr))
		assert.Equal(t, "unprocessable", httpErr.Name)
		assert.Equal(t, refusalOpenAI, httpErr.Message)
		assert.False(t, httpErr.Fault)
		assert.NotContains(t, rec.Body.String(), sentinelKey)
		assertConfigureSpan(t, sr, codes.Unset, nil, missingKeyEvents)

		sr.Reset()
		mcpRes, mErr := mcpAdapter.ToolsCall(alice, &genmcpadapter.ToolsCallPayload{Name: "configure_model", Arguments: reqBodyJSON})
		require.NoError(t, mErr)
		complete, ok := mcpRes.Outcome.AsComplete()
		require.True(t, ok)
		require.NotNil(t, complete.IsError)
		assert.True(t, *complete.IsError)
		raw, err := json.Marshal(complete)
		require.NoError(t, err)
		assert.Contains(t, string(raw), "provider openai needs OPENAI_API_KEY")
		assert.NotContains(t, string(raw), sentinelKey)
		assertConfigureSpan(t, sr, codes.Unset, nil, missingKeyEvents)

		read, err := cp.GetModelConnection(alice)
		require.NoError(t, err)
		assert.Equal(t, directModel("anthropic", "claude-sonnet-4-6"), read.Connection, "no refusal changes the reported choice")
	})

	t.Run("successful provider and gateway transitions record bounded telemetry and preserve long accepted names", func(t *testing.T) {
		cp := newControlPlane(t, Options{})
		handOverSettings(t, cp, &gensurla.HandOverModelSettingsPayload{Openai: map[string]string{"OPENAI_API_KEY": sentinelKey}})

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
		env, settings := cp.liveLaunch()
		assert.Equal(t, []string{"SURLA_LIVE_MODEL_PROVIDER=openai", "SURLA_LIVE_MODEL_NAME=" + longModelName}, env)
		require.Len(t, settings, 1)
		assert.Equal(t, []string{"OPENAI_API_KEY=" + sentinelKey}, settings[0].Environ())
		read, err := cp.GetModelConnection(alice)
		require.NoError(t, err)
		assert.Equal(t, directModel("openai", longModelName), read.Connection, "the read reports the accepted choice")

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
		assertSpanNeverContainsValue(t, cfgSpans[0], sentinelKey)

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
		env, settings = cp.liveLaunch()
		assert.Equal(t, []string{"SURLA_LIVE_MODEL_GATEWAY=" + longGatewayAddr}, env)
		assert.Empty(t, settings, "a gateway gets no provider settings")
		read, err = cp.GetModelConnection(alice)
		require.NoError(t, err)
		assert.Equal(t, gensurla.NewConnectionGateway(&gensurla.ModelGateway{GatewayAddr: longGatewayAddr}), read.Connection)

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

	t.Run("StartLiveTarget launches child process with updated model and held settings across ConfigureModel transitions", func(t *testing.T) {
		self, err := os.Executable()
		require.NoError(t, err)
		envFile := t.TempDir() + "/live-env.txt"
		cardsDir := t.TempDir()
		// Inherited family values stand in for Herdr's: none may reach a
		// live target, which gets only the handed-over snapshot.
		t.Setenv("ANTHROPIC_API_KEY", "inherited-key-sentinel")
		t.Setenv("ANTHROPIC_ADMIN_KEY", "inherited-admin-sentinel")
		cp, err := New(Options{
			CardsDir:     cardsDir,
			Verification: &VerificationConfig{RunsDir: t.TempDir()},
			Live: &LiveConfig{
				Command: []string{
					"/bin/sh",
					"-c",
					`printf 'PROVIDER=%s\nNAME=%s\nGATEWAY=%s\nANTHROPIC_KEY=%s\nANTHROPIC_HANDED=%s\nANTHROPIC_BASE=%s\nANTHROPIC_ADMIN=%s\nOPENAI_KEY=%s\n' ` +
						`"$SURLA_LIVE_MODEL_PROVIDER" "$SURLA_LIVE_MODEL_NAME" "$SURLA_LIVE_MODEL_GATEWAY" "${ANTHROPIC_API_KEY:+set}" ` +
						`"$([ "$ANTHROPIC_API_KEY" = handed-key-sentinel ] && echo yes)" "${ANTHROPIC_BASE_URL:+set}" "${ANTHROPIC_ADMIN_KEY:+set}" "${OPENAI_API_KEY:+set}" > "$1" && exec "$0" "$2" "$3" "$4"`,
					self,
					envFile,
				},
				Env: append(
					os.Environ(),
					liveTargetModeEnv+"=serve",
					"SURLA_LIVE_MODEL_PROVIDER=stale",
					"SURLA_LIVE_MODEL_NAME=stale",
				),
			},
		})
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, cp.Close()) })

		commit := strings.Repeat("a", 40)
		start := func() string {
			t.Helper()
			_, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: commit})
			require.NoError(t, err)
			got, err := os.ReadFile(envFile)
			require.NoError(t, err)
			return string(got)
		}
		assert.Equal(t, "PROVIDER=\nNAME=\nGATEWAY=\nANTHROPIC_KEY=\nANTHROPIC_HANDED=\nANTHROPIC_BASE=\nANTHROPIC_ADMIN=\nOPENAI_KEY=\n", start(),
			"before a hand-over no model is chosen and no inherited setting reaches the target")

		handOverSettings(t, cp, &gensurla.HandOverModelSettingsPayload{
			Anthropic: map[string]string{"ANTHROPIC_API_KEY": "handed-key-sentinel", "ANTHROPIC_BASE_URL": "https://gateway.invalid"},
			Openai:    map[string]string{"OPENAI_API_KEY": sentinelKey},
		})
		assert.Equal(t, "PROVIDER=anthropic\nNAME=claude-sonnet-4-6\nGATEWAY=\nANTHROPIC_KEY=set\nANTHROPIC_HANDED=yes\nANTHROPIC_BASE=set\nANTHROPIC_ADMIN=\nOPENAI_KEY=\n", start(),
			"the default chosen on arrival gets only its provider's handed-over settings")

		prov, modelName := "openai", "gpt-5.4-mini"
		_, err = cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{Provider: &prov, ModelName: &modelName})
		require.NoError(t, err)
		assert.Equal(t, "PROVIDER=openai\nNAME=gpt-5.4-mini\nGATEWAY=\nANTHROPIC_KEY=\nANTHROPIC_HANDED=\nANTHROPIC_BASE=\nANTHROPIC_ADMIN=\nOPENAI_KEY=set\n", start())

		gw := "127.0.0.1:9090"
		_, err = cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{GatewayAddr: &gw})
		require.NoError(t, err)
		assert.Equal(t, "PROVIDER=\nNAME=\nGATEWAY=127.0.0.1:9090\nANTHROPIC_KEY=\nANTHROPIC_HANDED=\nANTHROPIC_BASE=\nANTHROPIC_ADMIN=\nOPENAI_KEY=\n", start())
	})
}

// TestLiveTargetEnvKeepsOnlyTheChoice builds a live target's environment
// from a base that inherited provider families and stale model variables:
// both are dropped, and only the choice's variables are added.
func TestLiveTargetEnvKeepsOnlyTheChoice(t *testing.T) {
	base := []string{"PATH=/bin", "ANTHROPIC_API_KEY=k", "OPENAI_BASE_URL=u", "ANTHROPIC_ADMIN_KEY=a", "SURLA_LIVE_MODEL_GATEWAY=stale:1", "SURLA_LIVE_MODEL_NAME=stale"}
	for _, tc := range []struct {
		name   string
		choice gensurla.Connection
		want   []string
	}{
		{name: "direct", choice: directModel("anthropic", "claude-x"), want: []string{"PATH=/bin", "SURLA_LIVE_MODEL_PROVIDER=anthropic", "SURLA_LIVE_MODEL_NAME=claude-x"}},
		{name: "gateway", choice: gensurla.NewConnectionGateway(&gensurla.ModelGateway{GatewayAddr: "127.0.0.1:9090"}), want: []string{"PATH=/bin", "SURLA_LIVE_MODEL_GATEWAY=127.0.0.1:9090"}},
		{name: "none", choice: noModel(), want: []string{"PATH=/bin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, liveTargetEnv(base, tc.choice))
		})
	}
}

// TestModelConnectionReportsTheChoiceNeverTheSettings verifies that
// get_model_connection reports none before any hand-over and the default a
// hand-over chose afterwards, that its HTTP answer is the declared choice
// and holds no handed-over value, and that no span the read records holds
// one.
func TestModelConnectionReportsTheChoiceNeverTheSettings(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	cp := newControlPlane(t, Options{TracerProvider: provider, Live: &LiveConfig{Env: []string{"ANTHROPIC_API_KEY=inherited-key-sentinel", "PATH=/private/operator/bin"}}})
	read, err := cp.GetModelConnection(alice)
	require.NoError(t, err)
	assert.Equal(t, noModel(), read.Connection, "an inherited key chooses no model")

	handOverSettings(t, cp, &gensurla.HandOverModelSettingsPayload{Anthropic: map[string]string{"ANTHROPIC_API_KEY": sentinelKey, "ANTHROPIC_BASE_URL": sentinelURL}})
	read, err = cp.GetModelConnection(alice)
	require.NoError(t, err)
	assert.Equal(t, directModel("anthropic", "claude-sonnet-4-6"), read.Connection, "the hand-over's default is reported")

	srv := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{"tok": {ID: "alice@example.com"}})))
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/cp1/model", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `{"connection":{"kind":"direct","provider":"anthropic","model_name":"claude-sonnet-4-6"}}`, string(body))
	assertHoldsNoSentinel(t, string(body))
	spans := exporter.GetSpans().Snapshots()
	require.NotEmpty(t, endedSpans(spans, "surla.request.get_model_connection"), "the read records its request span")
	for _, span := range spans {
		assertSpanHoldsNoSentinel(t, span)
	}
}

// assertConfigureSpan checks the one configure_model span sr recorded.
func assertConfigureSpan(t *testing.T, sr *tracetest.SpanRecorder, wantCode codes.Code, wantExceptions []string, wantEvents map[string]map[string]string) {
	t.Helper()
	spans := endedSpansByName(sr, "surla.controlplane.configure_model")
	require.Len(t, spans, 1)
	assert.Equal(t, wantCode, spans[0].Status().Code)
	assert.Equal(t, wantExceptions, exceptionMessages(spans[0]))
	assert.Equal(t, wantEvents, nonExceptionEvents(spans[0]))
	assertSpanHoldsNoSentinel(t, spans[0])
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
			attrs[string(kv.Key)] = kv.Value.Emit()
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

// assertSpanNeverContainsValue asserts that neither the span's name, status,
// attributes nor any of its event names or attributes contain forbidden.
func assertSpanNeverContainsValue(t *testing.T, s sdktrace.ReadOnlySpan, forbidden string) {
	t.Helper()
	checkAttrs := func(kvs []attribute.KeyValue) {
		for _, kv := range kvs {
			assert.NotContains(t, kv.Value.Emit(), forbidden)
		}
	}
	assert.NotContains(t, s.Name(), forbidden)
	assert.NotContains(t, s.Status().Description, forbidden)
	checkAttrs(s.Attributes())
	for _, ev := range s.Events() {
		assert.NotContains(t, ev.Name, forbidden)
		checkAttrs(ev.Attributes)
	}
}

// noModel returns the declared choice of no model.
func noModel() gensurla.Connection {
	return gensurla.NewConnectionNone(&gensurla.ModelNone{})
}

// directModel returns the declared choice of a direct provider and model.
func directModel(provider, name string) gensurla.Connection {
	return gensurla.NewConnectionDirect(&gensurla.ModelDirect{Provider: provider, ModelName: name})
}

// endedSpans returns the spans named name.
func endedSpans(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.Name() == name {
			out = append(out, span)
		}
	}
	return out
}
