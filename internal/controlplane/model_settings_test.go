// This file tests hand_over_model_settings: replace or keep by provider, the
// default chosen on arrival, refusals that name variables only, the
// generated validation against the provider table, the management-only
// policy, its order with configure_model under concurrency, and that no
// result, error, span, diagnostics file or formatting of the held state
// reveals a handed-over value.
package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/modelsettings"
	"github.com/goadesign/surla/internal/verifyrun"
)

// Sentinels stand for the values a shell would hand over; none is a real
// credential, and tests only ever compare or search for them.
const (
	sentinelKey       = "sentinel-key-c41d"
	sentinelToken     = "sentinel-token-8be0"
	sentinelSecondKey = "sentinel-second-key-5a77"
	sentinelOpenAIKey = "sentinel-openai-key-0f3e"
	sentinelAdmin     = "sentinel-admin-key-92c6"
	sentinelURLCred   = "sentinel-url-credential-e5b1"
	sentinelURL       = "https://gateway.invalid/" + sentinelURLCred + "/"
)

// allSentinels lists every sentinel value a test may hand over or inherit.
var allSentinels = []string{sentinelKey, sentinelToken, sentinelSecondKey, sentinelOpenAIKey, sentinelAdmin, sentinelURLCred, sentinelURL}

// managementCaller is the surla command's management principal.
var managementCaller = WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "management", Kind: CallerManagement})

// TestHandOverModelSettingsReplacesOrKeepsByProvider hands settings over in
// the order a user's shells would: each present provider replaces what is
// held for it as a whole, an absent one keeps it, and the next launch's
// environment carries exactly what is held then.
func TestHandOverModelSettingsReplacesOrKeepsByProvider(t *testing.T) {
	cp := newControlPlane(t, Options{})
	for _, step := range []struct {
		name     string
		payload  *gensurla.HandOverModelSettingsPayload
		held     map[string][]string
		launched []string
	}{
		{
			name:     "first start: key and base URL",
			payload:  &gensurla.HandOverModelSettingsPayload{Anthropic: map[string]string{"ANTHROPIC_API_KEY": sentinelKey, "ANTHROPIC_BASE_URL": sentinelURL}},
			held:     map[string][]string{"anthropic": {"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"}},
			launched: []string{"ANTHROPIC_API_KEY=" + sentinelKey, "ANTHROPIC_BASE_URL=" + sentinelURL},
		},
		{
			name:     "another tab without a key keeps everything",
			payload:  &gensurla.HandOverModelSettingsPayload{},
			held:     map[string][]string{"anthropic": {"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"}},
			launched: []string{"ANTHROPIC_API_KEY=" + sentinelKey, "ANTHROPIC_BASE_URL=" + sentinelURL},
		},
		{
			name:     "base URL unset, key kept: replaced as a whole without it",
			payload:  &gensurla.HandOverModelSettingsPayload{Anthropic: map[string]string{"ANTHROPIC_API_KEY": sentinelSecondKey}},
			held:     map[string][]string{"anthropic": {"ANTHROPIC_API_KEY"}},
			launched: []string{"ANTHROPIC_API_KEY=" + sentinelSecondKey},
		},
		{
			name:     "another provider arrives; the first is kept",
			payload:  &gensurla.HandOverModelSettingsPayload{Openai: map[string]string{"OPENAI_API_KEY": sentinelOpenAIKey, "OPENAI_ORG_ID": "org-visible"}},
			held:     map[string][]string{"anthropic": {"ANTHROPIC_API_KEY"}, "openai": {"OPENAI_API_KEY", "OPENAI_ORG_ID"}},
			launched: []string{"ANTHROPIC_API_KEY=" + sentinelSecondKey},
		},
	} {
		t.Run(step.name, func(t *testing.T) {
			res := handOverSettings(t, cp, step.payload)
			assert.Equal(t, step.held, res.Held)
			_, settings := cp.liveLaunch()
			require.Len(t, settings, 1, "the default anthropic choice launches with its provider's settings")
			assert.Equal(t, step.launched, settings[0].Environ())
		})
	}
	all := cp.evaluatorSettings()
	require.Len(t, all, 2)
	assert.Equal(t, []string{"ANTHROPIC_API_KEY=" + sentinelSecondKey}, all[0].Environ())
	assert.Equal(t, []string{"OPENAI_API_KEY=" + sentinelOpenAIKey, "OPENAI_ORG_ID=org-visible"}, all[1].Environ())
}

// TestHandOverModelSettingsChoosesTheDefaultOnArrival covers each choice in
// force when settings arrive: none takes the arriving provider's default,
// anthropic first, and an explicit choice stays. The result's connection
// matches get_model_connection each time.
func TestHandOverModelSettingsChoosesTheDefaultOnArrival(t *testing.T) {
	user := asCaller("alice@example.com", CallerUser)
	anthropic := map[string]string{"ANTHROPIC_API_KEY": sentinelKey}
	openai := map[string]string{"OPENAI_API_KEY": sentinelOpenAIKey}
	gateway := gensurla.NewConnectionGateway(&gensurla.ModelGateway{GatewayAddr: "127.0.0.1:9090"})
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, *ControlPlane)
		payload *gensurla.HandOverModelSettingsPayload
		want    gensurla.Connection
	}{
		{name: "none + anthropic", payload: &gensurla.HandOverModelSettingsPayload{Anthropic: anthropic}, want: directModel("anthropic", "claude-sonnet-4-6")},
		{name: "none + openai", payload: &gensurla.HandOverModelSettingsPayload{Openai: openai}, want: directModel("openai", "gpt-5.4-mini")},
		{name: "none + both", payload: &gensurla.HandOverModelSettingsPayload{Anthropic: anthropic, Openai: openai}, want: directModel("anthropic", "claude-sonnet-4-6")},
		{name: "none + nothing", payload: &gensurla.HandOverModelSettingsPayload{}, want: noModel()},
		{
			name: "gateway + settings",
			prepare: func(t *testing.T, cp *ControlPlane) {
				addr := "127.0.0.1:9090"
				_, err := cp.ConfigureModel(user, &gensurla.ConfigureModelPayload{GatewayAddr: &addr})
				require.NoError(t, err)
			},
			payload: &gensurla.HandOverModelSettingsPayload{Anthropic: anthropic, Openai: openai},
			want:    gateway,
		},
		{
			name: "openai + anthropic settings",
			prepare: func(t *testing.T, cp *ControlPlane) {
				handOverSettings(t, cp, &gensurla.HandOverModelSettingsPayload{Openai: openai})
			},
			payload: &gensurla.HandOverModelSettingsPayload{Anthropic: anthropic},
			want:    directModel("openai", "gpt-5.4-mini"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := newControlPlane(t, Options{})
			if tc.prepare != nil {
				tc.prepare(t, cp)
			}
			res := handOverSettings(t, cp, tc.payload)
			assert.Equal(t, tc.want, res.Connection)
			read, err := cp.GetModelConnection(user)
			require.NoError(t, err)
			assert.Equal(t, res.Connection, read.Connection)
		})
	}
}

// TestModelChoiceTextsSayTheDefaultFollowsOnlyWithoutAChoice reads the
// published contract and the user documents: each says a hand-over selects
// a provider's default model only while no model is chosen, which is what
// TestHandOverModelSettingsChoosesTheDefaultOnArrival proves ("gateway +
// settings" keeps the gateway), and none says it does so unconditionally.
func TestModelChoiceTextsSayTheDefaultFollowsOnlyWithoutAChoice(t *testing.T) {
	openAPI, err := os.ReadFile("../../gen/http/openapi3.json")
	require.NoError(t, err)
	reference, err := os.ReadFile("../../docs/REFERENCE.md")
	require.NoError(t, err)
	readme, err := os.ReadFile("../../README.md")
	require.NoError(t, err)
	assert.Contains(t, string(openAPI), "While no model is chosen, a hand_over_model_settings call that holds a provider's settings selects that provider's default model")
	assert.Contains(t, string(reference), "While no model is chosen, a hand-over that holds a provider's settings selects that provider's default model")
	assert.Contains(t, string(readme), "while no model is chosen, live targets then use")
	for _, stale := range []string{
		"the first hand_over_model_settings that holds a provider's settings selects",
		"until `surla` hands over a provider's settings, which selects",
		"the next `surla` hands the settings over again, which selects",
		"until `surla stop` and starts live targets with",
	} {
		for name, text := range map[string][]byte{"OpenAPI": openAPI, "REFERENCE.md": reference, "README.md": readme} {
			assert.NotContains(t, string(text), stale, "%s says a hand-over always selects the default", name)
		}
	}
}

// TestHandOverModelSettingsRefusesWithNamesOnly sends each refused request,
// carrying sentinels, as the management caller over HTTP: the 400 and 422
// bodies, the spans and the diagnostics history hold no value, a 422 names
// the provider and the variable, and nothing held changes.
func TestHandOverModelSettingsRefusesWithNamesOnly(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	cardsDir := t.TempDir()
	cp := newControlPlane(t, Options{CardsDir: cardsDir, TracerProvider: provider})
	handOverSettings(t, cp, &gensurla.HandOverModelSettingsPayload{Openai: map[string]string{"OPENAI_API_KEY": sentinelOpenAIKey}})
	srv := httptest.NewServer(NewHTTPHandler(cp, kindAuthenticator()))
	t.Cleanup(srv.Close)

	oversize := strings.Repeat("s", 4097) + sentinelKey
	for _, tc := range []struct {
		name, body string
		status     int
		errName    string
		message    string
	}{
		{name: "oversize value", body: `{"anthropic":{"ANTHROPIC_API_KEY":"` + oversize + `"}}`, status: http.StatusBadRequest, errName: "invalid_length", message: "must be at most 4096"},
		{name: "unknown provider", body: `{"gemini":{"GEMINI_API_KEY":"` + sentinelKey + `"}}`, status: http.StatusBadRequest, errName: "decode_payload", message: `unknown field "gemini"`},
		{name: "unlisted name", body: `{"anthropic":{"ANTHROPIC_API_KEY":"` + sentinelKey + `","ANTHROPIC_ADMIN_KEY":"` + sentinelAdmin + `"}}`, status: http.StatusBadRequest, errName: "invalid_enum_value", message: `"ANTHROPIC_ADMIN_KEY"`},
		{name: "the other provider's name", body: `{"anthropic":{"ANTHROPIC_API_KEY":"` + sentinelKey + `","OPENAI_ORG_ID":"` + sentinelToken + `"}}`, status: http.StatusBadRequest, errName: "invalid_enum_value", message: `"OPENAI_ORG_ID"`},
		{name: "missing key variable", body: `{"anthropic":{"ANTHROPIC_BASE_URL":"` + sentinelURL + `"}}`, status: http.StatusUnprocessableEntity, errName: "unprocessable", message: "provider anthropic settings must include ANTHROPIC_API_KEY"},
		{name: "present but empty", body: `{"anthropic":{}}`, status: http.StatusUnprocessableEntity, errName: "unprocessable", message: "provider anthropic settings must include ANTHROPIC_API_KEY"},
		{name: "unknown field", body: `{"anthropic":{"ANTHROPIC_API_KEY":"` + sentinelKey + `"},"admin":"` + sentinelAdmin + `"}`, status: http.StatusBadRequest, errName: "decode_payload", message: `unknown field "admin"`},
		{name: "NUL", body: `{"anthropic":{"ANTHROPIC_API_KEY":"` + sentinelKey + `\u0000` + sentinelToken + `"}}`, status: http.StatusUnprocessableEntity, errName: "unprocessable", message: "provider anthropic setting ANTHROPIC_API_KEY contains a NUL character"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder.Reset()
			status, body := postSettings(t, srv.URL, "management", tc.body)
			assert.Equal(t, tc.status, status, body)
			var refused struct {
				Name    string `json:"name"`
				Message string `json:"message"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &refused), body)
			assert.Equal(t, tc.errName, refused.Name)
			assert.Contains(t, refused.Message, tc.message)
			assertHoldsNoSentinel(t, body)
			for _, span := range recorder.Ended() {
				assertSpanHoldsNoSentinel(t, span)
			}
			held := cp.evaluatorSettings()
			require.Len(t, held, 1, "a refused request changes nothing held")
			assert.Equal(t, []string{"OPENAI_API_KEY=" + sentinelOpenAIKey}, held[0].Environ())
		})
	}

	accepted, body := postSettings(t, srv.URL, "management", `{"anthropic":{"ANTHROPIC_API_KEY":"`+sentinelKey+`","ANTHROPIC_BASE_URL":"`+sentinelURL+`"}}`)
	require.Equal(t, http.StatusOK, accepted, body)
	assert.JSONEq(t, `{"held":{"anthropic":["ANTHROPIC_API_KEY","ANTHROPIC_BASE_URL"],"openai":["OPENAI_API_KEY"]},"connection":{"kind":"direct","provider":"openai","model_name":"gpt-5.4-mini"}}`, body)
	for _, span := range recorder.Ended() {
		assertSpanHoldsNoSentinel(t, span)
	}
	history, err := os.ReadFile(filepath.Join(cardsDir, "traces.jsonl"))
	require.NoError(t, err)
	assert.Contains(t, string(history), "hand_over_model_settings", "the diagnostics history records the operation")
	assertHoldsNoSentinel(t, string(history))
}

// TestHandOverModelSettingsValidationMatchesTheTable sends every request
// setting of each provider, then each provider's unlisted family members and
// the other provider's names: the generated validation accepts exactly the
// table's crossing list for each provider.
func TestHandOverModelSettingsValidationMatchesTheTable(t *testing.T) {
	cp := newControlPlane(t, Options{})
	srv := httptest.NewServer(NewHTTPHandler(cp, kindAuthenticator()))
	t.Cleanup(srv.Close)
	providers := modelsettings.Providers()
	for i, p := range providers {
		other := providers[(i+1)%len(providers)]
		t.Run(p.Name, func(t *testing.T) {
			values := map[string]string{}
			for _, name := range p.Settings() {
				values[name] = "v-" + name
			}
			status, body := postSettings(t, srv.URL, "management", settingsBody(t, p.Name, values))
			require.Equal(t, http.StatusOK, status, body)
			var res struct {
				Held map[string][]string `json:"held"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &res))
			assert.ElementsMatch(t, p.Settings(), res.Held[p.Name], "every listed setting crosses")

			refused := append([]string{p.Prefix + "ADMIN_KEY", p.Prefix + "WEBHOOK_SECRET", p.Prefix + "PROFILE", "PATH"}, other.Settings()...)
			for _, name := range refused {
				status, body := postSettings(t, srv.URL, "management", settingsBody(t, p.Name, map[string]string{p.Key: "k", name: "v"}))
				assert.Equal(t, http.StatusBadRequest, status, "%s must be refused for %s: %s", name, p.Name, body)
				assert.Contains(t, body, "invalid_enum_value")
			}
		})
	}
}

// TestHandOverModelSettingsIsManagementOnly refuses every caller kind but
// management with 403 over HTTP, and keeps the operation out of every MCP
// catalog and tool call.
func TestHandOverModelSettingsIsManagementOnly(t *testing.T) {
	cp := newControlPlane(t, Options{})
	srv := httptest.NewServer(NewHTTPHandler(cp, kindAuthenticator()))
	t.Cleanup(srv.Close)
	body := `{"anthropic":{"ANTHROPIC_API_KEY":"` + sentinelKey + `"}}`
	for _, kind := range []CallerKind{CallerUser, CallerGuide, CallerBuilder, CallerTestWriter} {
		t.Run(string(kind), func(t *testing.T) {
			status, answer := postSettings(t, srv.URL, string(kind), body)
			assert.Equal(t, http.StatusForbidden, status, answer)
			assertHoldsNoSentinel(t, answer)
			assert.Empty(t, cp.evaluatorSettings(), "a refused caller hands nothing over")

			adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)
			tools, err := adapter.ToolsList(asCaller("alice@example.com", kind), &genmcpadapter.ToolsListPayload{})
			require.NoError(t, err)
			for _, tool := range tools.Tools {
				assert.NotEqual(t, "hand_over_model_settings", tool.Name)
			}
			_, err = adapter.ToolsCall(asCaller("alice@example.com", kind), &genmcpadapter.ToolsCallPayload{Name: "hand_over_model_settings", Arguments: json.RawMessage(body)})
			assert.Error(t, err, "no MCP tool hands settings over")
		})
	}
	status, answer := postSettings(t, srv.URL, "management", body)
	assert.Equal(t, http.StatusOK, status, answer)
	assert.False(t, OperationAllowed(CallerManagement, "configure_model"), "management never chooses the model")
}

// TestConfigureModelAndHandOverInEitherOrder checks both orders, then runs
// them concurrently under the race detector: configure_model never accepts
// a provider whose settings are not held, and a hand-over never overwrites
// an explicit choice.
func TestConfigureModelAndHandOverInEitherOrder(t *testing.T) {
	user := asCaller("alice@example.com", CallerUser)
	provider, custom := "anthropic", "claude-custom"
	choose := func(cp *ControlPlane) error {
		_, err := cp.ConfigureModel(user, &gensurla.ConfigureModelPayload{Provider: &provider, ModelName: &custom})
		return err
	}
	anthropic := &gensurla.HandOverModelSettingsPayload{Anthropic: map[string]string{"ANTHROPIC_API_KEY": sentinelKey}}

	t.Run("configure_model first is refused, then accepted after the hand-over", func(t *testing.T) {
		cp := newControlPlane(t, Options{})
		err := choose(cp)
		assertServiceError(t, err, "unprocessable")
		assert.EqualError(t, err, refusalAnthropic)
		handOverSettings(t, cp, anthropic)
		require.NoError(t, choose(cp))
		res := handOverSettings(t, cp, anthropic)
		assert.Equal(t, directModel("anthropic", custom), res.Connection, "a later hand-over keeps the explicit choice")
	})
	t.Run("hand-over first, then configure_model", func(t *testing.T) {
		cp := newControlPlane(t, Options{})
		res := handOverSettings(t, cp, anthropic)
		assert.Equal(t, directModel("anthropic", "claude-sonnet-4-6"), res.Connection)
		require.NoError(t, choose(cp))
		openai, model := "openai", "gpt-5.4-mini"
		_, err := cp.ConfigureModel(user, &gensurla.ConfigureModelPayload{Provider: &openai, ModelName: &model})
		assert.EqualError(t, err, refusalOpenAI, "a provider never handed over stays refused")
	})
	t.Run("concurrently", func(t *testing.T) {
		for range 20 {
			cp := newControlPlane(t, Options{})
			var group sync.WaitGroup
			var accepted int
			group.Go(func() {
				for range 20 {
					if choose(cp) == nil {
						accepted++
					}
				}
			})
			group.Go(func() {
				for range 20 {
					_, err := cp.HandOverModelSettings(managementCaller, anthropic)
					assert.NoError(t, err)
				}
			})
			group.Wait()
			read, err := cp.GetModelConnection(user)
			require.NoError(t, err)
			if accepted > 0 {
				assert.Equal(t, directModel("anthropic", custom), read.Connection, "an accepted choice is never overwritten")
			} else {
				assert.Equal(t, directModel("anthropic", "claude-sonnet-4-6"), read.Connection)
			}
		}
	})
}

// TestModelResultsHoldNoHandedOverValue reads every setting and live-target
// operation body over HTTP after a hand-over carrying sentinels: the
// hand-over itself, configure_model accepted and refused, get_model_connection
// and get_live_target.
func TestModelResultsHoldNoHandedOverValue(t *testing.T) {
	cp := newControlPlane(t, Options{})
	srv := httptest.NewServer(NewHTTPHandler(cp, kindAuthenticator()))
	t.Cleanup(srv.Close)
	status, body := postSettings(t, srv.URL, "management", `{"anthropic":{"ANTHROPIC_API_KEY":"`+sentinelKey+`","ANTHROPIC_AUTH_TOKEN":"`+sentinelToken+`","ANTHROPIC_BASE_URL":"`+sentinelURL+`"}}`)
	require.Equal(t, http.StatusOK, status, body)
	bodies := []string{body}
	for _, call := range []struct{ method, path, body string }{
		{http.MethodPost, "/cp1/model", `{"provider":"anthropic","model_name":"claude-sonnet-4-6"}`},
		{http.MethodPost, "/cp1/model", `{"provider":"openai","model_name":"gpt-5.4-mini"}`},
		{http.MethodGet, "/cp1/model", ""},
		{http.MethodGet, "/live-target", ""},
	} {
		_, answer := sendAs(t, srv.URL, "user", call.method, call.path, call.body)
		bodies = append(bodies, answer)
	}
	for _, b := range bodies {
		assertHoldsNoSentinel(t, b)
	}
}

// TestHeldSettingsRevealNothingWhenFormatted prints and serializes the held
// settings and the model state with every verb, slog handler and json.
func TestHeldSettingsRevealNothingWhenFormatted(t *testing.T) {
	cp := newControlPlane(t, Options{})
	handOverSettings(t, cp, &gensurla.HandOverModelSettingsPayload{
		Anthropic: map[string]string{"ANTHROPIC_API_KEY": sentinelKey, "ANTHROPIC_AUTH_TOKEN": sentinelToken, "ANTHROPIC_BASE_URL": sentinelURL},
		Openai:    map[string]string{"OPENAI_API_KEY": sentinelOpenAIKey},
	})
	cp.modelMu.Lock()
	state := struct {
		choice gensurla.Connection
		held   map[string]modelsettings.Settings
	}{cp.liveModel, cp.heldSettings}
	cp.modelMu.Unlock()
	for name, operand := range map[string]any{"held settings": state.held, "model state": state, "pointer to model state": &state} {
		t.Run(name, func(t *testing.T) {
			var outputs []string
			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "% x", "%#x", "%d", "%p"} {
				outputs = append(outputs, fmt.Sprintf(verb, operand))
			}
			for _, handler := range []func(*bytes.Buffer) slog.Handler{
				func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
				func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
			} {
				var buf bytes.Buffer
				slog.New(handler(&buf)).Info("model", "state", operand)
				outputs = append(outputs, buf.String())
			}
			raw, err := json.Marshal(operand)
			require.NoError(t, err)
			outputs = append(outputs, string(raw))
			for _, out := range outputs {
				assertHoldsNoEncodedSentinel(t, out)
			}
		})
	}
}

// TestStoredEvaluatorRunsReceiveEveryHeldProviderSnapshot accepts two
// verifications with the operator's stored evaluator, a hand-over between
// them: each run's supervisor stand-in reports, by names and booleans only,
// that it received every handed-over provider's routing settings and no
// unlisted variable, and that the first run kept its accepted snapshot while
// the second got the new one.
func TestStoredEvaluatorRunsReceiveEveryHeldProviderSnapshot(t *testing.T) {
	isolateProviderFamilies(t)
	report := t.TempDir()
	cfg := verificationConfig(t)
	cfg.Command = []string{"/bin/sh", "-c", presenceScript, filepath.Join(report, "run")}
	t.Setenv("SURLA_TEST_FIRST_KEY", sentinelKey)
	cp := newControlPlane(t, Options{Verification: cfg})
	alice := asCaller("alice@example.com", CallerUser)
	card, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", Analysis: &gensurla.CP1Analysis{
		Summary: "Refunds up to $50 are instant.", Ambiguities: []*gensurla.CP1AnalysisAmbiguity{}, PreservedBehaviors: []string{},
		Examples: []*gensurla.CP1AnalysisExample{{Scenario: "Refund at the limit", InputSummary: "Order total $50.00", ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic"}},
	}})
	require.NoError(t, err)
	card = mustConfirmNoop(t, cp, alice, card)

	handOverSettings(t, cp, &gensurla.HandOverModelSettingsPayload{
		Anthropic: map[string]string{"ANTHROPIC_API_KEY": sentinelKey, "ANTHROPIC_BASE_URL": sentinelURL},
		Openai:    map[string]string{"OPENAI_API_KEY": sentinelOpenAIKey, "OPENAI_ORG_ID": "org-visible"},
	})
	verifyOnce := func() string {
		t.Helper()
		run, err := cp.VerifyChange(alice, &gensurla.VerifyChangePayload{ChangeID: "chg", RequestRevision: card.RequestRevision, CandidateCommit: strings.Repeat("a", 40)})
		require.NoError(t, err)
		return waitForPresence(t, filepath.Join(report, "run"), run.RunID)
	}
	first := verifyOnce()
	handOverSettings(t, cp, &gensurla.HandOverModelSettingsPayload{Anthropic: map[string]string{"ANTHROPIC_API_KEY": sentinelSecondKey}})
	second := verifyOnce()

	assert.Equal(t, "ANTHROPIC_API_KEY=set\nFIRST_KEY=yes\nANTHROPIC_BASE_URL=set\nANTHROPIC_ADMIN_KEY=\nOPENAI_API_KEY=set\nOPENAI_ORG_ID=set\nOPENAI_ADMIN_KEY=\n", first)
	assert.Equal(t, "ANTHROPIC_API_KEY=set\nFIRST_KEY=\nANTHROPIC_BASE_URL=\nANTHROPIC_ADMIN_KEY=\nOPENAI_API_KEY=set\nOPENAI_ORG_ID=set\nOPENAI_ADMIN_KEY=\n", second,
		"the later hand-over reaches the next run, replacing anthropic's settings as a whole")
	assertTreeHoldsNoSentinel(t, cfg.RunsDir)
}

// TestWrittenTestRunsReceiveNoProviderSettings verifies a change with the
// tests agents wrote after a hand-over: the run's supervisor stand-in
// reports that no provider setting reached it.
func TestWrittenTestRunsReceiveNoProviderSettings(t *testing.T) {
	isolateProviderFamilies(t)
	root := realTempDir(t)
	repo := newLayoutRepo(t, filepath.Join(root, "repo"))
	runsDir := filepath.Join(root, "runs")
	report := t.TempDir()
	t.Setenv("SURLA_TEST_FIRST_KEY", sentinelKey)
	cp := newBuildControlPlane(t, Options{
		WorkspaceDir: repo.workspace,
		RunsDir:      runsDir,
		SurlaVersion: testSurlaVersion,
		WrittenTestRuns: &WrittenTestRuns{ReportDir: realTempDir(t), Timeout: time.Hour,
			Command: []string{"/bin/sh", "-c", presenceScript, filepath.Join(report, "run")}},
	})
	handOverSettings(t, cp, &gensurla.HandOverModelSettingsPayload{
		Anthropic: map[string]string{"ANTHROPIC_API_KEY": sentinelKey, "ANTHROPIC_BASE_URL": sentinelURL},
		Openai:    map[string]string{"OPENAI_API_KEY": sentinelOpenAIKey},
	})
	card := saveStoryCard(t, cp, "chg", StatusConfirmed, refundStory("Refund over the limit waits"))
	_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	acceptTests(t, cp, "chg", repo.tests)
	run, err := cp.VerifyChange(asCaller("alice@example.com", CallerBuilder), &gensurla.VerifyChangePayload{
		ChangeID: "chg", RequestRevision: card.RequestRevision, CandidateCommit: repo.candidate,
	})
	require.NoError(t, err)
	assert.Equal(t, "ANTHROPIC_API_KEY=\nFIRST_KEY=\nANTHROPIC_BASE_URL=\nANTHROPIC_ADMIN_KEY=\nOPENAI_API_KEY=\nOPENAI_ORG_ID=\nOPENAI_ADMIN_KEY=\n",
		waitForPresence(t, filepath.Join(report, "run"), run.RunID))
}

// TestLiveStartFailureExportsProtectedDetail hands settings over, then
// starts a live target whose suite prints them and quotes its key in its
// first line before failing: the refusal, every span and the diagnostics
// history hold no value, and the exported executor_log_tail event shows the
// redaction and the base URL's scheme and host only.
func TestLiveStartFailureExportsProtectedDetail(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	cardsDir := t.TempDir()
	cp := newControlPlane(t, Options{
		CardsDir: cardsDir, TracerProvider: provider,
		Verification: &VerificationConfig{RunsDir: t.TempDir()},
		Live:         &LiveConfig{Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=leak"), StartTimeout: time.Minute},
	})
	handOverSettings(t, cp, &gensurla.HandOverModelSettingsPayload{Anthropic: map[string]string{
		"ANTHROPIC_API_KEY": sentinelKey, "ANTHROPIC_AUTH_TOKEN": sentinelToken, "ANTHROPIC_BASE_URL": sentinelURL,
	}})

	_, err = cp.StartLiveTarget(asCaller("alice@example.com", CallerBuilder), &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	assertServiceError(t, err, "live_target_failed")
	assertHoldsNoSentinel(t, err.Error())
	var tail string
	for _, span := range recorder.Ended() {
		assertSpanHoldsNoSentinel(t, span)
		for _, event := range span.Events() {
			for _, kv := range event.Attributes {
				if kv.Key == "executor_log_tail" {
					tail = kv.Value.AsString()
				}
			}
		}
	}
	assert.Contains(t, tail, `json: unknown field "`+modelsettings.Redacted+`"`)
	assert.Contains(t, tail, "ANTHROPIC_AUTH_TOKEN="+modelsettings.Redacted)
	assert.Contains(t, tail, "ANTHROPIC_BASE_URL=https://gateway.invalid\n")
	history, err := os.ReadFile(filepath.Join(cardsDir, "traces.jsonl"))
	require.NoError(t, err)
	assertHoldsNoSentinel(t, string(history))
	dirs, err := verifyrunLeftovers(cardsDir)
	require.NoError(t, err)
	assert.Empty(t, dirs, "the failed start's folder, which held the suite's output, is removed once its group is empty")
}

// TestLiveStartFailureExportsNoKeyPieceAtTheCut hands over a key shaped
// like a real one, then starts a live target whose executor stand-in
// prints it followed by enough output that the exported tail's cut falls
// 13 bytes into it: the executor_log_tail event, every span and the
// diagnostics history hold no piece of the key.
func TestLiveStartFailureExportsNoKeyPieceAtTheCut(t *testing.T) {
	isolateProviderFamilies(t)
	const key = "sk-ant-api03-sentinelRANDOMPART0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZq7w3e9r1t5uZZAA"
	const exported = 8 << 10
	filler := exported - (len(key) - 13)
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	cardsDir := t.TempDir()
	// The stand-in is the executor: its standard error is the log, and it
	// ends without a ready line.
	script := `printf '%s' "$ANTHROPIC_API_KEY" >&2; head -c ` + strconv.Itoa(filler) + ` /dev/zero | tr '\000' y >&2; exit 1`
	cp := newControlPlane(t, Options{
		CardsDir: cardsDir, TracerProvider: provider,
		Verification: &VerificationConfig{RunsDir: t.TempDir()},
		Live:         &LiveConfig{Command: []string{"/bin/sh", "-c", script, "executor"}, Env: os.Environ(), StartTimeout: time.Minute},
	})
	handOverSettings(t, cp, &gensurla.HandOverModelSettingsPayload{Anthropic: map[string]string{"ANTHROPIC_API_KEY": key}})

	_, err := cp.StartLiveTarget(asCaller("alice@example.com", CallerBuilder), &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	assertServiceError(t, err, "live_target_failed")
	var tail string
	for _, span := range recorder.Ended() {
		for _, event := range span.Events() {
			for _, kv := range event.Attributes {
				if kv.Key == "executor_log_tail" {
					tail = kv.Value.AsString()
				}
			}
		}
		for i := 8; i+8 <= len(key); i++ {
			assertSpanNeverContainsValue(t, span, key[i:i+8])
		}
	}
	assert.Equal(t, strings.Repeat("y", filler), tail, "only the output after the key is exported")
	history, err := os.ReadFile(filepath.Join(cardsDir, "traces.jsonl"))
	require.NoError(t, err)
	assert.NotContains(t, string(history), "sentinelRANDOMPART")
}

// TestSavedFilesHoldNoHandedOverValue hands settings over and runs a live
// target with a quiet suite: no file in the state the control plane keeps
// holds a value, and stopping the target removes its folder.
func TestSavedFilesHoldNoHandedOverValue(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	cardsDir := t.TempDir()
	runsDir := t.TempDir()
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	cp := newControlPlane(t, Options{
		CardsDir: cardsDir, TracerProvider: provider,
		Verification: &VerificationConfig{RunsDir: runsDir},
		Live:         &LiveConfig{Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=serve"), StartTimeout: time.Minute},
	})
	handOverSettings(t, cp, &gensurla.HandOverModelSettingsPayload{
		Anthropic: map[string]string{"ANTHROPIC_API_KEY": sentinelKey, "ANTHROPIC_AUTH_TOKEN": sentinelToken, "ANTHROPIC_BASE_URL": sentinelURL},
		Openai:    map[string]string{"OPENAI_API_KEY": sentinelOpenAIKey},
	})
	builder := asCaller("alice@example.com", CallerBuilder)
	started, err := cp.StartLiveTarget(builder, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	assertHoldsNoSentinel(t, started.Message)
	dirs, err := verifyrunLeftovers(cardsDir)
	require.NoError(t, err)
	require.Len(t, dirs, 1, "the running target has its folder")
	assertTreeHoldsNoSentinel(t, cardsDir)
	assertTreeHoldsNoSentinel(t, runsDir)

	_, err = cp.StopLiveTarget(builder)
	require.NoError(t, err)
	dirs, err = verifyrunLeftovers(cardsDir)
	require.NoError(t, err)
	assert.Empty(t, dirs, "Stop removes the live folder once its group is empty")
	assertTreeHoldsNoSentinel(t, cardsDir)
}

// TestChangedSettingsReachTheNextLaunchOnly hands settings over, starts a
// live target, then hands changed settings over: the running target is
// neither restarted nor unbound and keeps the settings it started with,
// while the next launch receives the changed ones.
func TestChangedSettingsReachTheNextLaunchOnly(t *testing.T) {
	isolateProviderFamilies(t)
	self, err := os.Executable()
	require.NoError(t, err)
	reports := t.TempDir()
	t.Setenv("SURLA_TEST_FIRST_KEY", sentinelKey)
	t.Setenv("SURLA_TEST_PRESENCE_DIR", reports)
	// The executor stand-in counts its launches and reports, by booleans
	// only, whether its anthropic key is the first one handed over.
	launch := `n=$(ls "$SURLA_TEST_PRESENCE_DIR" | wc -l | tr -d ' ')
[ "$ANTHROPIC_API_KEY" = "$SURLA_TEST_FIRST_KEY" ] && first=yes || first=no
printf 'FIRST_KEY=%s\nKEY_SET=%s\n' "$first" "${ANTHROPIC_API_KEY:+yes}" > "$SURLA_TEST_PRESENCE_DIR/launch-$n"
exec "$0" "$1" "$2" "$3"`
	cp := newControlPlane(t, Options{
		Verification: &VerificationConfig{RunsDir: t.TempDir()},
		Live: &LiveConfig{Command: []string{"/bin/sh", "-c", launch, self}, Env: append(os.Environ(), liveTargetModeEnv+"=serve"),
			StartTimeout: time.Minute},
	})
	builder := asCaller("alice@example.com", CallerBuilder)
	commit := strings.Repeat("a", 40)
	handOverSettings(t, cp, &gensurla.HandOverModelSettingsPayload{Anthropic: map[string]string{"ANTHROPIC_API_KEY": sentinelKey}})
	started, err := cp.StartLiveTarget(builder, &gensurla.StartLiveTargetPayload{CandidateCommit: commit})
	require.NoError(t, err)

	handOverSettings(t, cp, &gensurla.HandOverModelSettingsPayload{Anthropic: map[string]string{"ANTHROPIC_API_KEY": sentinelSecondKey}})
	status, err := cp.GetLiveTarget(builder)
	require.NoError(t, err)
	running, ok := status.LiveTarget.AsRunning()
	require.True(t, ok, "the hand-over leaves the running target running")
	assert.Equal(t, started.URL, running.URL)
	drafted, err := draft(builder, cp, fakePromptID, "Answer in French.")
	require.NoError(t, err)
	assert.True(t, drafted.AppliedInStore, "the running target still answers")
	entries, err := os.ReadDir(reports)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the hand-over launched nothing")
	raw, err := os.ReadFile(filepath.Join(reports, "launch-0"))
	require.NoError(t, err)
	assert.Equal(t, "FIRST_KEY=yes\nKEY_SET=yes\n", string(raw), "the running target started with the first settings")

	_, err = cp.StartLiveTarget(builder, &gensurla.StartLiveTargetPayload{CandidateCommit: commit})
	require.NoError(t, err)
	raw, err = os.ReadFile(filepath.Join(reports, "launch-1"))
	require.NoError(t, err)
	assert.Equal(t, "FIRST_KEY=no\nKEY_SET=yes\n", string(raw), "the next launch received the changed settings")
}

// verifyrunLeftovers lists the live target folders under cardsDir.
func verifyrunLeftovers(cardsDir string) ([]string, error) {
	return verifyrun.LeftoverLiveDirs(LiveRoot(cardsDir))
}

// presenceScript is a supervisor stand-in: it writes, to the file named by
// $0 followed by the run directory's name, whether each provider variable is
// set and whether the anthropic key equals SURLA_TEST_FIRST_KEY, then exits.
// It writes names and booleans only.
const presenceScript = `out="$0-$(basename "$3")"; {
printf 'ANTHROPIC_API_KEY=%s\n' "${ANTHROPIC_API_KEY:+set}"
printf 'FIRST_KEY=%s\n' "$([ -n "$ANTHROPIC_API_KEY" ] && [ "$ANTHROPIC_API_KEY" = "$SURLA_TEST_FIRST_KEY" ] && echo yes)"
printf 'ANTHROPIC_BASE_URL=%s\n' "${ANTHROPIC_BASE_URL:+set}"
printf 'ANTHROPIC_ADMIN_KEY=%s\n' "${ANTHROPIC_ADMIN_KEY:+set}"
printf 'OPENAI_API_KEY=%s\n' "${OPENAI_API_KEY:+set}"
printf 'OPENAI_ORG_ID=%s\n' "${OPENAI_ORG_ID:+set}"
printf 'OPENAI_ADMIN_KEY=%s\n' "${OPENAI_ADMIN_KEY:+set}"
} > "$out.tmp" && mv "$out.tmp" "$out"`

// waitForPresence returns what presenceScript wrote for runID under prefix,
// waiting up to ten seconds for the detached supervisor stand-in.
func waitForPresence(t *testing.T, prefix, runID string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile(prefix + "-" + runID)
		if err == nil {
			return string(raw)
		}
		require.ErrorIs(t, err, os.ErrNotExist)
		require.True(t, time.Now().Before(deadline), "the supervisor stand-in did not report")
		time.Sleep(20 * time.Millisecond)
	}
}

// handOverSettings hands p over as the management caller and returns the
// result, or stops the test.
func handOverSettings(t *testing.T, cp *ControlPlane, p *gensurla.HandOverModelSettingsPayload) *gensurla.ModelSettingsResult {
	t.Helper()
	res, err := cp.HandOverModelSettings(managementCaller, p)
	require.NoError(t, err)
	return res
}

// kindAuthenticator authenticates one bearer token per caller kind, named
// after it, for the owner alice@example.com.
func kindAuthenticator() RequestAuthenticator {
	return func(r *http.Request) (Principal, error) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		for _, kind := range []CallerKind{CallerUser, CallerGuide, CallerBuilder, CallerTestWriter, CallerManagement} {
			if token == string(kind) {
				return Principal{ID: "alice@example.com", CallerID: string(kind), Kind: kind}, nil
			}
		}
		return Principal{}, errors.New("unknown token")
	}
}

// postSettings posts body to the hand-over route with token's credential.
func postSettings(t *testing.T, base, token, body string) (int, string) {
	t.Helper()
	return sendAs(t, base, token, http.MethodPost, "/cp1/model/settings", body)
}

// sendAs sends one request with token's credential and returns its status
// and body.
func sendAs(t *testing.T, base, token, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out bytes.Buffer
	_, err = out.ReadFrom(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, out.String()
}

// settingsBody encodes one provider's values as a hand-over request body.
func settingsBody(t *testing.T, provider string, values map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]map[string]string{provider: values})
	require.NoError(t, err)
	return string(raw)
}

// assertHoldsNoSentinel fails for every sentinel text holds.
func assertHoldsNoSentinel(t *testing.T, text string) {
	t.Helper()
	for _, sentinel := range allSentinels {
		assert.NotContains(t, text, sentinel)
	}
}

// assertSpanHoldsNoSentinel checks a span's name, status, attributes and
// events, including recorded errors, for every sentinel.
func assertSpanHoldsNoSentinel(t *testing.T, span sdktrace.ReadOnlySpan) {
	t.Helper()
	for _, sentinel := range allSentinels {
		assertSpanNeverContainsValue(t, span, sentinel)
	}
}

// assertTreeHoldsNoSentinel reads every regular file under root and fails
// for each sentinel one holds.
func assertTreeHoldsNoSentinel(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, sentinel := range allSentinels {
			assert.NotContains(t, string(raw), sentinel, path)
		}
		return nil
	}))
}

// isolateProviderFamilies hides the machine's own provider variables from a
// test and restores each when it ends: this process's environment is what a
// verification run inherits, and no test may depend on a real key.
func isolateProviderFamilies(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if modelsettings.InFamily(name) {
			t.Setenv(name, value)
			require.NoError(t, os.Unsetenv(name))
		}
	}
}

// assertHoldsNoEncodedSentinel fails for every sentinel text holds as is,
// quoted or JSON-escaped, in hexadecimal (either case, with or without
// spaces) or in any base64 alphabet, the forms a formatter or marshaller
// could reveal it in.
func assertHoldsNoEncodedSentinel(t *testing.T, text string) {
	t.Helper()
	for _, sentinel := range allSentinels {
		quoted := strconv.Quote(sentinel)
		escaped, err := json.Marshal(sentinel)
		require.NoError(t, err)
		for _, form := range []string{
			sentinel, quoted[1 : len(quoted)-1], string(escaped[1 : len(escaped)-1]),
			hex.EncodeToString([]byte(sentinel)), strings.ToUpper(hex.EncodeToString([]byte(sentinel))),
			fmt.Sprintf("% x", sentinel), fmt.Sprintf("% X", sentinel),
			base64.StdEncoding.EncodeToString([]byte(sentinel)), base64.URLEncoding.EncodeToString([]byte(sentinel)),
			base64.RawStdEncoding.EncodeToString([]byte(sentinel)), base64.RawURLEncoding.EncodeToString([]byte(sentinel)),
		} {
			assert.NotContains(t, text, form)
		}
	}
}
