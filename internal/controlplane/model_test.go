// This file tests runtime model configuration (`configure_model` / `POST /cp1/model`),
// reference-counted model handle retirement across in-flight `ProposeChange`,
// `ReviseChange`, and `Close`, OpenTelemetry span events and statuses, and
// live-target environment propagation (`liveEnv`).

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"go.opentelemetry.io/otel/trace/noop"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
)

type (
	// modelSpanRecorder captures OpenTelemetry spans, events, attributes,
	// recorded errors, and statuses emitted by ConfigureModel and retireModel.
	modelSpanRecorder struct {
		embedded.TracerProvider
		// mu guards spans.
		mu sync.Mutex
		// spans holds every span started through this provider in start order.
		spans []*modelRecordedSpan
	}

	// modelTracer implements trace.Tracer for modelSpanRecorder.
	modelTracer struct {
		embedded.Tracer
		// provider is the owning modelSpanRecorder.
		provider *modelSpanRecorder
		// name is the instrumentation scope name.
		name string
	}

	// modelRecordedSpan captures one OpenTelemetry span's events, errors, and
	// status.
	modelRecordedSpan struct {
		trace.Span
		// mu guards events, errors, statusCode, statusDesc, and ended.
		mu sync.Mutex
		// name is the span operation name.
		name string
		// events holds every event recorded via AddEvent on this span.
		events []modelSpanEvent
		// errors holds every error recorded via RecordError on this span.
		errors []error
		// statusCode is the last status code set on this span.
		statusCode codes.Code
		// statusDesc is the last status description set on this span.
		statusDesc string
		// ended reports whether End was called on this span.
		ended bool
	}

	// modelSpanEvent captures one OpenTelemetry span event name and its
	// attribute key-value map.
	modelSpanEvent struct {
		// name is the event name passed to AddEvent.
		name string
		// attrs maps attribute keys to string representations for assertion.
		attrs map[string]string
	}
)

// TestConfigureModelValidationTelemetryAndLiveEnv verifies ConfigureModel's
// authentication, payload normalization and cross-field validation, capability
// and credential refusal span events ("model_configuration_refused"), actual
// constructor failure and closed-control-plane span statuses, successful
// provider and gateway activation ("model_configured"), retired-model cleanup
// span isolation ("surla.controlplane.retire_model"), liveEnv propagation, and
// CloseAnalyzer cleanup when controlplane.New fails validation.
func TestConfigureModelValidationTelemetryAndLiveEnv(t *testing.T) {
	prevProvider := otel.GetTracerProvider()
	recorder := &modelSpanRecorder{}
	otel.SetTracerProvider(recorder)
	t.Cleanup(func() { otel.SetTracerProvider(prevProvider) })

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
		recorder.reset()
		cp := newControlPlane(t, Options{})
		gw := "127.0.0.1:9090"
		_, err := cp.ConfigureModel(context.Background(), &gensurla.ConfigureModelPayload{GatewayAddr: &gw})
		require.EqualError(t, err, "authenticated principal is required")
		assert.Empty(t, recorder.matching("surla.controlplane.configure_model"))
	})

	t.Run("reconfiguration unavailable records refusal event without error status and preserves fixed analyzer", func(t *testing.T) {
		recorder.reset()
		cp := newControlPlane(t, Options{
			Analyzer: NewModelCP1Analyzer(newStubModelClient(t, summaryJSON("Fixed host analyzer"))),
			LiveEnv:  []string{"SURLA_LIVE_MODEL_PROVIDER=anthropic", "SURLA_LIVE_MODEL_NAME=claude-fixed"},
		})
		gw := "127.0.0.1:9090"
		_, err := cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{GatewayAddr: &gw})
		require.Error(t, err)
		assert.Equal(t, "not_implemented", goaErrorName(err))
		assert.ErrorContains(t, err, msgModelReconfigUnavailable)

		spans := recorder.matching("surla.controlplane.configure_model")
		require.Len(t, spans, 1)
		assert.True(t, spans[0].ended)
		assert.Equal(t, codes.Unset, spans[0].statusCode)
		assert.Empty(t, spans[0].errors)
		assert.Equal(t, []modelSpanEvent{{
			name:  "model_configuration_refused",
			attrs: map[string]string{"surla.model.refusal_reason": "reconfiguration_unavailable"},
		}}, spans[0].events)

		card, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "chg-fixed", Prompt: "Test fixed analyzer"})
		require.NoError(t, err)
		assert.Equal(t, "Fixed host analyzer", card.Summary)
		assert.Equal(t, []string{"SURLA_LIVE_MODEL_PROVIDER=anthropic", "SURLA_LIVE_MODEL_NAME=claude-fixed"}, cp.liveEnv())
	})

	t.Run("payload and missing-credential refusals record refusal events without error status and preserve active model", func(t *testing.T) {
		var buildErr error
		cp := newControlPlane(t, Options{
			Analyzer: NewModelCP1Analyzer(newStubModelClient(t, summaryJSON("Initial model"))),
			LiveEnv:  []string{"SURLA_LIVE_MODEL_PROVIDER=anthropic", "SURLA_LIVE_MODEL_NAME=claude-sonnet-4-6"},
			ModelBuilder: func(p *gensurla.ConfigureModelPayload) (CP1Analyzer, []string, func() error, error) {
				if buildErr != nil {
					return nil, nil, nil, buildErr
				}
				return NewModelCP1Analyzer(newStubModelClient(t, summaryJSON("Unused"))), nil, nil, nil
			},
		})

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
				recorder.reset()
				_, err := cp.ConfigureModel(alice, tc.payload)
				require.Error(t, err)
				assert.Equal(t, "unprocessable", goaErrorName(err))
				assert.ErrorContains(t, err, tc.wantErr)

				spans := recorder.matching("surla.controlplane.configure_model")
				require.Len(t, spans, 1)
				assert.True(t, spans[0].ended)
				assert.Equal(t, codes.Unset, spans[0].statusCode)
				assert.Empty(t, spans[0].errors)
				assert.Equal(t, []modelSpanEvent{{
					name:  "model_configuration_refused",
					attrs: map[string]string{"surla.model.refusal_reason": "invalid_payload"},
				}}, spans[0].events)
			})
		}

		// Missing API key refusal from ModelBuilder (422 unprocessable).
		recorder.reset()
		buildErr = gensurla.MakeUnprocessable(errors.New("provider openai requires OPENAI_API_KEY in surla serve's environment"))
		_, err := cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{
			Provider:  strPtr("openai"),
			ModelName: strPtr("gpt-5.4-mini"),
		})
		require.Error(t, err)
		assert.Equal(t, "unprocessable", goaErrorName(err))
		spans := recorder.matching("surla.controlplane.configure_model")
		require.Len(t, spans, 1)
		assert.Equal(t, codes.Unset, spans[0].statusCode)
		assert.Empty(t, spans[0].errors)
		assert.Equal(t, []modelSpanEvent{{
			name:  "model_configuration_refused",
			attrs: map[string]string{"surla.model.refusal_reason": "missing_api_key"},
		}}, spans[0].events)

		// Actual constructor failure from ModelBuilder records error + codes.Error.
		recorder.reset()
		buildErr = errors.New("dial gateway failed")
		_, err = cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{
			GatewayAddr: strPtr("127.0.0.1:9090"),
		})
		require.EqualError(t, err, "dial gateway failed")
		spans = recorder.matching("surla.controlplane.configure_model")
		require.Len(t, spans, 1)
		assert.Equal(t, codes.Error, spans[0].statusCode)
		require.Len(t, spans[0].errors, 1)
		assert.EqualError(t, spans[0].errors[0], "dial gateway failed")
		assert.Empty(t, spans[0].events)

		// Active model and liveEnv remain untouched after all refusals and failures.
		card, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "chg-still-active", Prompt: "Still active"})
		require.NoError(t, err)
		assert.Equal(t, "Initial model", card.Summary)
		assert.Equal(t, []string{"SURLA_LIVE_MODEL_PROVIDER=anthropic", "SURLA_LIVE_MODEL_NAME=claude-sonnet-4-6"}, cp.liveEnv())
	})

	t.Run("successful provider and gateway transitions update analyzer, liveEnv, MCP result, and retirement span", func(t *testing.T) {
		var closeInitialCalls atomic.Int32
		var closeSecondCalls atomic.Int32
		cp := newControlPlane(t, Options{
			Analyzer: NewModelCP1Analyzer(newStubModelClient(t, summaryJSON("Initial anthropic"))),
			CloseAnalyzer: func() error {
				closeInitialCalls.Add(1)
				return errors.New("retire initial transport warning")
			},
			LiveEnv: []string{"SURLA_LIVE_MODEL_PROVIDER=anthropic", "SURLA_LIVE_MODEL_NAME=claude-sonnet-4-6"},
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

		// 1. Switch from initial anthropic (whose closeFn returns an error) to openai/gpt-5.4-mini.
		recorder.reset()
		prov, modelName := "  openai ", " gpt-5.4-mini  "
		res, err := cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{
			Provider:  &prov,
			ModelName: &modelName,
		})
		require.NoError(t, err)
		require.NotNil(t, res.Provider)
		require.NotNil(t, res.ModelName)
		assert.Equal(t, "openai", *res.Provider)
		assert.Equal(t, "gpt-5.4-mini", *res.ModelName)
		assert.Nil(t, res.GatewayAddr)
		assert.EqualValues(t, 1, closeInitialCalls.Load())
		assert.Equal(t, []string{"SURLA_LIVE_MODEL_PROVIDER=openai", "SURLA_LIVE_MODEL_NAME=gpt-5.4-mini"}, cp.liveEnv())

		cfgSpans := recorder.matching("surla.controlplane.configure_model")
		require.Len(t, cfgSpans, 1)
		assert.True(t, cfgSpans[0].ended)
		assert.Equal(t, codes.Unset, cfgSpans[0].statusCode, "old transport retirement failure must not mark configure_model failed")
		assert.Empty(t, cfgSpans[0].errors)
		assert.Equal(t, []modelSpanEvent{{
			name: "model_configured",
			attrs: map[string]string{
				"surla.model.provider": "openai",
				"surla.model.name":     "gpt-5.4-mini",
			},
		}}, cfgSpans[0].events)

		retireSpans := recorder.matching("surla.controlplane.retire_model")
		require.Len(t, retireSpans, 1)
		assert.True(t, retireSpans[0].ended)
		assert.Equal(t, codes.Error, retireSpans[0].statusCode)
		require.Len(t, retireSpans[0].errors, 1)
		assert.EqualError(t, retireSpans[0].errors[0], "retire initial transport warning")

		card1, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "chg-oai", Prompt: "Use openai"})
		require.NoError(t, err)
		assert.Equal(t, "Provider openai/gpt-5.4-mini", card1.Summary)

		// 2. Switch via MCP tools/call to gateway_addr.
		recorder.reset()
		adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)
		argsJSON, err := json.Marshal(map[string]any{"gateway_addr": " 127.0.0.1:9090 "})
		require.NoError(t, err)
		mcpRes, err := adapter.ToolsCall(alice, &genmcpadapter.ToolsCallPayload{Name: "configure_model", Arguments: argsJSON})
		require.NoError(t, err)
		complete, ok := mcpRes.Outcome.AsComplete()
		require.True(t, ok)
		assert.False(t, complete.IsError != nil && *complete.IsError)
		assert.EqualValues(t, 1, closeSecondCalls.Load())
		assert.Equal(t, []string{"SURLA_LIVE_MODEL_GATEWAY=127.0.0.1:9090"}, cp.liveEnv())

		cfgSpans = recorder.matching("surla.controlplane.configure_model")
		require.Len(t, cfgSpans, 1)
		assert.Equal(t, codes.Unset, cfgSpans[0].statusCode)
		assert.Equal(t, []modelSpanEvent{{
			name:  "model_configured",
			attrs: map[string]string{"surla.model.gateway_addr": "127.0.0.1:9090"},
		}}, cfgSpans[0].events)

		card2, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "chg-gw", Prompt: "Use gateway"})
		require.NoError(t, err)
		assert.Equal(t, "Gateway 127.0.0.1:9090", card2.Summary)
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
// channel barriers (zero sleeps) across:
//  1. In-flight ProposeChange (Analyze) and separately ReviseChange (Revise)
//     when ControlPlane.Close runs with a CloseAnalyzer spy.
//  2. In-flight ProposeChange and separately ReviseChange when ConfigureModel
//     switches to a new model (including when the retired model's closeFn fails).
//  3. Early-exit ProposeChange (duplicate change_id) and ReviseChange (stale
//     request_revision) retiring a superseded model handle via a still-open
//     "surla.controlplane.retire_model" span.
//  4. Overlapping switches (model 1 -> model 2 -> model 3) while calls A and B
//     are held in flight on models 1 and 2.
//  5. ModelBuilder completing after ControlPlane.Close has already closed the
//     control plane.
func TestModelLifecycleBarriersAcrossCloseAndConfigureModel(t *testing.T) {
	prevProvider := otel.GetTracerProvider()
	recorder := &modelSpanRecorder{}
	otel.SetTracerProvider(recorder)
	t.Cleanup(func() { otel.SetTracerProvider(prevProvider) })

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
				recorder.reset()
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

				retireSpans := recorder.matching("surla.controlplane.retire_model")
				require.Len(t, retireSpans, 1)
				assert.True(t, retireSpans[0].ended)
				assert.Equal(t, codes.Error, retireSpans[0].statusCode)
				require.Len(t, retireSpans[0].errors, 1)
				assert.EqualError(t, retireSpans[0].errors[0], "close1 transport error")

				opSpanName := "surla.controlplane.propose_change"
				if op == "revise" {
					opSpanName = "surla.controlplane.revise_change"
				}
				opSpans := recorder.matching(opSpanName)
				require.NotEmpty(t, opSpans)
				assert.Equal(t, codes.Unset, opSpans[0].statusCode, "completed analysis span must not be marked failed by old-model retirement")
			})
		}
	})

	t.Run("early-exit ProposeChange and ReviseChange on a superseded handle record retire_model span", func(t *testing.T) {
		for _, op := range []string{"propose_duplicate", "revise_stale"} {
			t.Run(op, func(t *testing.T) {
				recorder.reset()
				var close1Count atomic.Int32
				cp := newControlPlane(t, Options{
					Analyzer: &heldAnalyzer{started: make(chan struct{}), release: make(chan struct{})},
					CloseAnalyzer: func() error {
						close1Count.Add(1)
						return errors.New("early-exit retire error")
					},
					ModelBuilder: func(*gensurla.ConfigureModelPayload) (CP1Analyzer, []string, func() error, error) {
						return &heldAnalyzer{started: make(chan struct{}), release: make(chan struct{})}, nil, nil, nil
					},
				})
				saveCard(t, cp, nil)

				// Hold c.mu.Lock() so ProposeChange/ReviseChange acquires model 1
				// and blocks at c.mu.RLock() before starting its operation span.
				cp.mu.Lock()
				errCh := make(chan error, 1)
				go func() {
					if op == "propose_duplicate" {
						_, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{
							ChangeID: "chg",
							Prompt:   "Duplicate change_id",
						})
						errCh <- err
						return
					}
					_, err := cp.ReviseChange(alice, &gensurla.ReviseChangePayload{
						ChangeID:        "chg",
						RequestRevision: strings.Repeat("0", 64),
						Prompt:          "Stale revision",
					})
					errCh <- err
				}()

				// Wait until the goroutine has acquired model 1 (refs == 2).
				for {
					cp.modelMu.Lock()
					refs := cp.activeModel.refs
					cp.modelMu.Unlock()
					if refs == 2 {
						break
					}
				}

				// Switch the active model while the call is waiting on c.mu.RLock().
				gw := "127.0.0.1:9090"
				_, err := cp.ConfigureModel(alice, &gensurla.ConfigureModelPayload{GatewayAddr: &gw})
				require.NoError(t, err)
				assert.EqualValues(t, 0, close1Count.Load())

				recorder.reset()
				cp.mu.Unlock()

				require.Error(t, <-errCh)
				assert.EqualValues(t, 1, close1Count.Load())
				retireSpans := recorder.matching("surla.controlplane.retire_model")
				require.Len(t, retireSpans, 1)
				assert.True(t, retireSpans[0].ended)
				assert.Equal(t, codes.Error, retireSpans[0].statusCode)
				require.Len(t, retireSpans[0].errors, 1)
				assert.EqualError(t, retireSpans[0].errors[0], "early-exit retire error")
			})
		}
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
		recorder.reset()
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

		cfgSpans := recorder.matching("surla.controlplane.configure_model")
		require.Len(t, cfgSpans, 1)
		assert.Equal(t, codes.Error, cfgSpans[0].statusCode)
		assert.Empty(t, cfgSpans[0].events)

		retireSpans := recorder.matching("surla.controlplane.retire_model")
		require.Len(t, retireSpans, 2, "both model 1 and model 2 retirement spans are recorded")

		require.NoError(t, cp.Close())
		assert.EqualValues(t, 1, close1.Load())
		assert.EqualValues(t, 1, close2.Load())
	})
}

// Tracer returns a trace.Tracer that records spans into p.
func (p *modelSpanRecorder) Tracer(name string, _ ...trace.TracerOption) trace.Tracer {
	return &modelTracer{provider: p, name: name}
}

// reset clears all recorded spans.
func (p *modelSpanRecorder) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spans = nil
}

// matching returns all recorded spans with the given operation name.
func (p *modelSpanRecorder) matching(name string) []*modelRecordedSpan {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*modelRecordedSpan
	for _, s := range p.spans {
		if s.name == name {
			out = append(out, s)
		}
	}
	return out
}

// Start begins a new modelRecordedSpan and registers it on t.provider.
func (t *modelTracer) Start(
	ctx context.Context,
	name string,
	opts ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	_, noopSpan := noop.NewTracerProvider().Tracer(t.name).Start(ctx, name, opts...)
	sp := &modelRecordedSpan{
		Span: noopSpan,
		name: name,
	}
	t.provider.mu.Lock()
	t.provider.spans = append(t.provider.spans, sp)
	t.provider.mu.Unlock()
	return trace.ContextWithSpan(ctx, sp), sp
}

// AddEvent records an event and its attributes on s.
func (s *modelRecordedSpan) AddEvent(name string, opts ...trace.EventOption) {
	cfg := trace.NewEventConfig(opts...)
	attrs := make(map[string]string, len(cfg.Attributes()))
	for _, kv := range cfg.Attributes() {
		attrs[string(kv.Key)] = attributeValueString(kv.Value)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, modelSpanEvent{
		name:  name,
		attrs: attrs,
	})
}

// RecordError records err on s when non-nil.
func (s *modelRecordedSpan) RecordError(err error, _ ...trace.EventOption) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors = append(s.errors, err)
}

// SetStatus records code and desc on s.
func (s *modelRecordedSpan) SetStatus(code codes.Code, desc string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusCode = code
	s.statusDesc = desc
}

// End marks s as ended.
func (s *modelRecordedSpan) End(_ ...trace.SpanEndOption) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended = true
}

// attributeValueString returns the string form of an OpenTelemetry attribute value.
func attributeValueString(v attribute.Value) string {
	return v.AsString()
}
