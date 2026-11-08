package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/features/model/gateway"
	"goa.design/goa-ai/runtime/agent/model"
	"google.golang.org/grpc"

	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/swarm"
	"github.com/goadesign/surla/internal/verify"
)

type (
	// scriptedCP1Provider is a model.Provider served by a local model gateway in
	// tests. It answers every Complete call with one fixed CP1 analysis and
	// records the output allowance the gateway forwarded.
	scriptedCP1Provider struct {
		// analysis is the JSON CP1 analysis returned as assistant text.
		analysis string
		// maxTokens is the MaxTokens of the last request the gateway delivered.
		maxTokens atomic.Int64
	}

	// noopTargetUpdater is a TargetPromptUpdater stub used to verify CP3
	// startup status formatting when a host binds TargetUpdater.
	noopTargetUpdater struct{}
)

func TestNewCP1AnalyzerSelectsModelFromFlags(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_API_KEY": "test-anthropic-key",
		"OPENAI_API_KEY":    "test-openai-key",
	}
	cases := []struct {
		name         string
		cfg          serveConfig
		env          map[string]string
		wantAnalyzer bool
		wantErr      string
	}{
		{name: "no model flags leaves CP1 unconfigured", cfg: serveConfig{}, env: env},
		{name: "gateway", cfg: serveConfig{gatewayAddr: "127.0.0.1:9090"}, env: env, wantAnalyzer: true},
		{name: "anthropic", cfg: serveConfig{provider: "anthropic", modelName: "claude-test"}, env: env, wantAnalyzer: true},
		{name: "openai", cfg: serveConfig{provider: "openai", modelName: "gpt-test"}, env: env, wantAnalyzer: true},
		{
			name:    "gateway with model name",
			cfg:     serveConfig{gatewayAddr: "127.0.0.1:9090", modelName: "claude-test"},
			env:     env,
			wantErr: "-model-gateway-addr cannot be combined",
		},
		{
			name:    "gateway with provider",
			cfg:     serveConfig{gatewayAddr: "127.0.0.1:9090", provider: "anthropic"},
			env:     env,
			wantErr: "-model-gateway-addr cannot be combined",
		},
		{name: "provider without model", cfg: serveConfig{provider: "anthropic"}, env: env, wantErr: "must be set together"},
		{name: "model without provider", cfg: serveConfig{modelName: "claude-test"}, env: env, wantErr: "must be set together"},
		{
			name:    "unknown provider",
			cfg:     serveConfig{provider: "Anthropic", modelName: "claude-test"},
			env:     env,
			wantErr: `unsupported -model-provider "Anthropic"`,
		},
		{
			name:    "missing api key",
			cfg:     serveConfig{provider: "openai", modelName: "gpt-test"},
			env:     map[string]string{"ANTHROPIC_API_KEY": "test-anthropic-key"},
			wantErr: "-model-provider openai requires OPENAI_API_KEY",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			analyzer, closeModel, err := newCP1Analyzer(tc.cfg, func(k string) string { return tc.env[k] })
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.NotContains(t, err.Error(), "test-anthropic-key")
				assert.NotContains(t, err.Error(), "test-openai-key")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantAnalyzer, analyzer != nil)
			assert.NoError(t, closeModel())
		})
	}
}

func TestLoadSwarmProfilesDecodesStrictly(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []swarm.ProviderProfile
		wantErr string
	}{
		{
			name: "valid profiles",
			content: `[{"id":"claude-a","provider":"anthropic","model":"model-a","family":"claude",` +
				`"authenticated":true,"capabilities":["architect","builder"]}]`,
			want: []swarm.ProviderProfile{{
				ID:            "claude-a",
				Provider:      "anthropic",
				Model:         "model-a",
				Family:        "claude",
				Authenticated: true,
				Capabilities:  []swarm.RoleKind{swarm.RoleArchitect, swarm.RoleBuilder},
			}},
		},
		{name: "unknown field", content: `[{"id":"a","api_key":"x"}]`, wantErr: `unknown field "api_key"`},
		{name: "trailing data", content: `[] []`, wantErr: "trailing data"},
		{name: "empty array", content: `[]`, want: []swarm.ProviderProfile{}},
		{name: "not an array", content: `{"id":"a"}`, wantErr: "decode -swarm-profiles"},
		{name: "null", content: `null`, wantErr: "expected a JSON array, got null"},
		{
			name: "duplicate id",
			content: `[{"id":"a","provider":"openai","model":"m","family":"gpt"},` +
				`{"id":"a","provider":"anthropic","model":"n","family":"claude"}]`,
			wantErr: `duplicate provider profile id "a"`,
		},
		{name: "empty required field", content: `[{"id":"a","provider":"openai","family":"gpt"}]`, wantErr: "requires non-empty"},
		{
			name:    "unknown capability",
			content: `[{"id":"a","provider":"openai","model":"m","family":"gpt","capabilities":["deployer"]}]`,
			wantErr: `unsupported capability "deployer"`,
		},
		{
			name:    "valid profiles without builder capability",
			content: `[{"id":"a","provider":"openai","model":"m","family":"gpt","capabilities":["reviewer"]}]`,
			want: []swarm.ProviderProfile{{
				ID: "a", Provider: "openai", Model: "m", Family: "gpt",
				Capabilities: []swarm.RoleKind{swarm.RoleReviewer},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "profiles.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			got, err := loadSwarmProfiles(path)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestServeOptionsDerivesLiveTargetsFromVerification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verification.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"runs_dir":"/r","report_dir":"/p","candidate_repo":"/c","candidate_module":".","suite_module":".","binaries":{"app":"./cmd/app"},`+
		`"suite_repo":"/s","suite_commit":"`+strings.Repeat("a", 40)+`","suite_command":"./cmd/evals","timeout":"20m"}`), 0o600))
	opts, err := serveOptions(serveConfig{verificationPath: path}, func(string) string { return "" },
		[]string{"PATH=/bin", "SURLA_AUTH_TOKEN=secret", "ANTHROPIC_API_KEY=k"})
	require.NoError(t, err)
	if opts.CloseAnalyzer != nil {
		assert.NoError(t, opts.CloseAnalyzer())
	}
	require.NotNil(t, opts.Live)
	assert.Equal(t, opts.Verification.Command, opts.Live.Command)
	assert.Equal(t, 20*time.Minute, opts.Live.StartTimeout)
	assert.Equal(t, []string{"PATH=/bin", "ANTHROPIC_API_KEY=k"}, opts.Live.Env, "serve's own bearer token never reaches a live target")
	assert.Equal(t, opts.Live.Env, opts.LiveEnv)
}

func TestLiveTargetEnvCarriesServesModelChoice(t *testing.T) {
	environ := []string{"PATH=/bin", "SURLA_AUTH_TOKEN=secret", "ANTHROPIC_API_KEY=k", "SURLA_LIVE_MODEL_GATEWAY=stale:1"}
	cases := []struct {
		name string
		cfg  serveConfig
		want []string
	}{
		{name: "direct provider", cfg: serveConfig{provider: "anthropic", modelName: "claude-x"},
			want: []string{"PATH=/bin", "ANTHROPIC_API_KEY=k", "SURLA_LIVE_MODEL_PROVIDER=anthropic", "SURLA_LIVE_MODEL_NAME=claude-x"}},
		{name: "gateway", cfg: serveConfig{gatewayAddr: "127.0.0.1:9090"},
			want: []string{"PATH=/bin", "ANTHROPIC_API_KEY=k", "SURLA_LIVE_MODEL_GATEWAY=127.0.0.1:9090"}},
		{name: "no model", cfg: serveConfig{},
			want: []string{"PATH=/bin", "ANTHROPIC_API_KEY=k"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, liveTargetEnv(environ, tc.cfg))
		})
	}
}

func TestServeOptionsWiresAnalyzerAndProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	require.NoError(t, os.WriteFile(path, []byte(`[{"id":"a","provider":"openai","model":"m","family":"gpt"}]`), 0o600))

	opts, err := serveOptions(
		serveConfig{provider: "openai", modelName: "gpt-test", swarmProfilesPath: path},
		func(string) string { return "test-key" },
		nil,
	)
	require.NoError(t, err)
	assert.NotNil(t, opts.Analyzer)
	assert.NotNil(t, opts.ModelBuilder)
	assert.Equal(t, []swarm.ProviderProfile{{ID: "a", Provider: "openai", Model: "m", Family: "gpt"}}, opts.SwarmProfiles)
	assert.Nil(t, opts.TargetUpdater)
	require.NotNil(t, opts.CloseAnalyzer)
	assert.NoError(t, opts.CloseAnalyzer())

	opts, err = serveOptions(serveConfig{}, func(string) string { return "" }, nil)
	require.NoError(t, err)
	assert.Nil(t, opts.Analyzer)
	assert.NotNil(t, opts.ModelBuilder)
	assert.Nil(t, opts.SwarmProfiles)
	assert.Nil(t, opts.Live, "live targets need a verification configuration")
	require.NotNil(t, opts.CloseAnalyzer)
	assert.NoError(t, opts.CloseAnalyzer())

	_, err = serveOptions(serveConfig{swarmProfilesPath: filepath.Join(t.TempDir(), "missing.json")}, os.Getenv, nil)
	require.ErrorContains(t, err, "read -swarm-profiles")
}

func TestResolveStartupModelConfigAndServeOptionsAutoDetection(t *testing.T) {
	baseEnviron := []string{"PATH=/bin", "SURLA_AUTH_TOKEN=secret", "ANTHROPIC_API_KEY=ant-k", "OPENAI_API_KEY=oai-k"}
	cases := []struct {
		name         string
		cfg          serveConfig
		env          map[string]string
		wantCfg      serveConfig
		wantAnalyzer bool
		wantLiveEnv  []string
	}{
		{
			name: "anthropic key auto-detects claude-sonnet-4-6 without verification config",
			cfg:  serveConfig{},
			env:  map[string]string{"ANTHROPIC_API_KEY": "ant-k"},
			wantCfg: serveConfig{
				provider:  "anthropic",
				modelName: defaultAnthropicModel,
			},
			wantAnalyzer: true,
			wantLiveEnv: []string{
				"PATH=/bin",
				"ANTHROPIC_API_KEY=ant-k",
				"OPENAI_API_KEY=oai-k",
				"SURLA_LIVE_MODEL_PROVIDER=anthropic",
				"SURLA_LIVE_MODEL_NAME=claude-sonnet-4-6",
			},
		},
		{
			name: "openai key auto-detects gpt-5.4-mini when anthropic key is absent",
			cfg:  serveConfig{},
			env:  map[string]string{"OPENAI_API_KEY": "oai-k"},
			wantCfg: serveConfig{
				provider:  "openai",
				modelName: defaultOpenAIModel,
			},
			wantAnalyzer: true,
			wantLiveEnv: []string{
				"PATH=/bin",
				"ANTHROPIC_API_KEY=ant-k",
				"OPENAI_API_KEY=oai-k",
				"SURLA_LIVE_MODEL_PROVIDER=openai",
				"SURLA_LIVE_MODEL_NAME=gpt-5.4-mini",
			},
		},
		{
			name: "both keys prefer anthropic claude-sonnet-4-6 when model flags are omitted",
			cfg:  serveConfig{},
			env: map[string]string{
				"ANTHROPIC_API_KEY": "ant-k",
				"OPENAI_API_KEY":    "oai-k",
			},
			wantCfg: serveConfig{
				provider:  "anthropic",
				modelName: defaultAnthropicModel,
			},
			wantAnalyzer: true,
			wantLiveEnv: []string{
				"PATH=/bin",
				"ANTHROPIC_API_KEY=ant-k",
				"OPENAI_API_KEY=oai-k",
				"SURLA_LIVE_MODEL_PROVIDER=anthropic",
				"SURLA_LIVE_MODEL_NAME=claude-sonnet-4-6",
			},
		},
		{
			name: "explicit model flags override auto-detection when both keys are present",
			cfg: serveConfig{
				provider:  "openai",
				modelName: "gpt-5.4",
			},
			env: map[string]string{
				"ANTHROPIC_API_KEY": "ant-k",
				"OPENAI_API_KEY":    "oai-k",
			},
			wantCfg: serveConfig{
				provider:  "openai",
				modelName: "gpt-5.4",
			},
			wantAnalyzer: true,
			wantLiveEnv: []string{
				"PATH=/bin",
				"ANTHROPIC_API_KEY=ant-k",
				"OPENAI_API_KEY=oai-k",
				"SURLA_LIVE_MODEL_PROVIDER=openai",
				"SURLA_LIVE_MODEL_NAME=gpt-5.4",
			},
		},
		{
			name: "whitespace-only keys leave CP1 unconfigured",
			cfg:  serveConfig{},
			env: map[string]string{
				"ANTHROPIC_API_KEY": "   ",
				"OPENAI_API_KEY":    "\t\n",
			},
			wantCfg:      serveConfig{},
			wantAnalyzer: false,
			wantLiveEnv: []string{
				"PATH=/bin",
				"ANTHROPIC_API_KEY=ant-k",
				"OPENAI_API_KEY=oai-k",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			assert.Equal(t, tc.wantCfg, resolveStartupModelConfig(tc.cfg, getenv))
			opts, err := serveOptions(tc.cfg, getenv, baseEnviron)
			require.NoError(t, err)
			assert.Equal(t, tc.wantAnalyzer, opts.Analyzer != nil)
			assert.Nil(t, opts.Live, "no verification config was supplied")
			assert.Equal(t, tc.wantLiveEnv, opts.LiveEnv)
			opts.CardsDir = t.TempDir()
			cp, err := controlplane.New(opts)
			require.NoError(t, err)
			require.NoError(t, cp.Close())
		})
	}
}

func TestServeConfigureModelOverHTTPAndMCPSwitchesFromUnconfiguredToGatewayAndProvider(
	t *testing.T,
) {
	analysis, err := json.Marshal(controlplane.CP1SemanticAnalysis{
		Summary:     "Give VIP customers instant store credit up to $150.",
		Ambiguities: []controlplane.CP1ModelAmbiguity{},
		Examples: []controlplane.CP1ModelExample{{
			Scenario:         "VIP refund under the limit",
			InputSummary:     "VIP customer, $120.00 total",
			ExpectedOutcome:  "Instant store credit, no manager approval",
			VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{"Non-VIP refunds over $50.00 still wait for a manager."},
	})
	require.NoError(t, err)
	provider := &scriptedCP1Provider{analysis: string(analysis)}
	gatewayAddr := startModelGateway(t, provider)

	env := map[string]string{}
	opts, err := serveOptions(
		serveConfig{cardsDir: t.TempDir()},
		func(k string) string { return env[k] },
		[]string{"PATH=/bin", "SURLA_AUTH_TOKEN=secret"},
	)
	require.NoError(t, err)
	assert.Nil(t, opts.Analyzer, "starts unconfigured when no model flags or API keys are set")

	cp, err := controlplane.New(opts)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	auth := controlplane.NewTokenAuthenticator(map[string]controlplane.Principal{
		"test-token": {ID: "operator@example.com"},
	})
	srv := httptest.NewServer(controlplane.NewHTTPHandler(cp, auth))
	t.Cleanup(srv.Close)

	// 1. ProposeChange while unconfigured returns 501 naming configure_model.
	proposeReq := func(changeID string) *http.Response {
		req, reqErr := http.NewRequest(http.MethodPost, srv.URL+"/changes/propose",
			strings.NewReader(`{"change_id":"`+changeID+`","prompt":"VIP customers get instant store credit up to $150."}`))
		require.NoError(t, reqErr)
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		resp, doErr := http.DefaultClient.Do(req)
		require.NoError(t, doErr)
		return resp
	}
	unconfResp := proposeReq("vip-unconf")
	var unconfBody struct {
		Message string `json:"message"`
		Fault   bool   `json:"fault"`
	}
	require.NoError(t, json.NewDecoder(unconfResp.Body).Decode(&unconfBody))
	require.NoError(t, unconfResp.Body.Close())
	assert.Equal(t, http.StatusNotImplemented, unconfResp.StatusCode)
	assert.False(t, unconfBody.Fault)
	assert.Contains(t, unconfBody.Message, "call configure_model")

	// 2. Calling POST /cp1/model with a direct provider whose API key is missing returns 422.
	missingKeyReq, err := http.NewRequest(http.MethodPost, srv.URL+"/cp1/model",
		strings.NewReader(`{"provider":"openai","model_name":"gpt-5.4-mini"}`))
	require.NoError(t, err)
	missingKeyReq.Header.Set("Authorization", "Bearer test-token")
	missingKeyReq.Header.Set("Content-Type", "application/json")
	missingKeyResp, err := http.DefaultClient.Do(missingKeyReq)
	require.NoError(t, err)
	var missingKeyBody struct {
		Message string `json:"message"`
		Fault   bool   `json:"fault"`
	}
	require.NoError(t, json.NewDecoder(missingKeyResp.Body).Decode(&missingKeyBody))
	require.NoError(t, missingKeyResp.Body.Close())
	assert.Equal(t, http.StatusUnprocessableEntity, missingKeyResp.StatusCode)
	assert.False(t, missingKeyBody.Fault)
	assert.Contains(t, missingKeyBody.Message, "provider openai requires OPENAI_API_KEY in surla serve's environment")

	// 3. Calling POST /cp1/model with gateway_addr attaches the running gateway and enables propose_change.
	gwReq, err := http.NewRequest(http.MethodPost, srv.URL+"/cp1/model",
		strings.NewReader(`{"gateway_addr":"`+gatewayAddr+`"}`))
	require.NoError(t, err)
	gwReq.Header.Set("Authorization", "Bearer test-token")
	gwReq.Header.Set("Content-Type", "application/json")
	gwResp, err := http.DefaultClient.Do(gwReq)
	require.NoError(t, err)
	var gwResult struct {
		Provider    *string `json:"provider"`
		ModelName   *string `json:"model_name"`
		GatewayAddr *string `json:"gateway_addr"`
	}
	require.NoError(t, json.NewDecoder(gwResp.Body).Decode(&gwResult))
	require.NoError(t, gwResp.Body.Close())
	require.Equal(t, http.StatusOK, gwResp.StatusCode)
	assert.Nil(t, gwResult.Provider)
	assert.Nil(t, gwResult.ModelName)
	require.NotNil(t, gwResult.GatewayAddr)
	assert.Equal(t, gatewayAddr, *gwResult.GatewayAddr)

	okResp := proposeReq("vip-refunds")
	require.NoError(t, okResp.Body.Close())
	assert.Equal(t, http.StatusOK, okResp.StatusCode)

	// 4. Calling configure_model with provider + model_name succeeds when the key is present in getenv.
	env["ANTHROPIC_API_KEY"] = "test-anthropic-key"
	provReq, err := http.NewRequest(http.MethodPost, srv.URL+"/cp1/model",
		strings.NewReader(`{"provider":"anthropic","model_name":"claude-sonnet-4-6"}`))
	require.NoError(t, err)
	provReq.Header.Set("Authorization", "Bearer test-token")
	provReq.Header.Set("Content-Type", "application/json")
	provResp, err := http.DefaultClient.Do(provReq)
	require.NoError(t, err)
	var provResult struct {
		Provider    *string `json:"provider"`
		ModelName   *string `json:"model_name"`
		GatewayAddr *string `json:"gateway_addr"`
	}
	require.NoError(t, json.NewDecoder(provResp.Body).Decode(&provResult))
	require.NoError(t, provResp.Body.Close())
	require.Equal(t, http.StatusOK, provResp.StatusCode)
	require.NotNil(t, provResult.Provider)
	require.NotNil(t, provResult.ModelName)
	assert.Equal(t, "anthropic", *provResult.Provider)
	assert.Equal(t, "claude-sonnet-4-6", *provResult.ModelName)
	assert.Nil(t, provResult.GatewayAddr)
}

// TestServeGatewayModeAnswersProposeChangeOverHTTP drives the path that used
// to fail: serve options built with -model-gateway-addr, a real gRPC model
// gateway, and an authenticated HTTP propose_change call. A clear request
// (no open questions) comes back confirmed with code-owned check IDs and a
// SHA-256 request revision, and the CP1 output allowance reaches the provider.
func TestServeGatewayModeAnswersProposeChangeOverHTTP(t *testing.T) {
	analysis, err := json.Marshal(controlplane.CP1SemanticAnalysis{
		Summary:     "Give VIP customers instant store credit up to $150.",
		Ambiguities: []controlplane.CP1ModelAmbiguity{},
		Examples: []controlplane.CP1ModelExample{{
			Scenario:         "VIP refund under the limit",
			InputSummary:     "VIP customer, $120.00 total",
			ExpectedOutcome:  "Instant store credit, no manager approval",
			VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{"Non-VIP refunds over $50.00 still wait for a manager."},
	})
	require.NoError(t, err)
	provider := &scriptedCP1Provider{analysis: string(analysis)}
	gatewayAddr := startModelGateway(t, provider)

	opts, err := serveOptions(serveConfig{gatewayAddr: gatewayAddr, cardsDir: t.TempDir()}, func(string) string { return "" }, nil)
	require.NoError(t, err)
	cp, err := controlplane.New(opts)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	auth := controlplane.NewTokenAuthenticator(map[string]controlplane.Principal{
		"test-token": {ID: "operator@example.com"},
	})
	srv := httptest.NewServer(controlplane.NewHTTPHandler(cp, auth))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/changes/propose",
		strings.NewReader(`{"change_id":"vip-refunds","prompt":"VIP customers get instant store credit up to $150."}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var card struct {
		Status          string  `json:"status"`
		ConfirmedBy     *string `json:"confirmed_by"`
		RequestRevision string  `json:"request_revision"`
		Examples        []struct {
			ID string `json:"id"`
		} `json:"examples"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&card))
	assert.Equal(t, controlplane.StatusConfirmed, card.Status)
	require.NotNil(t, card.ConfirmedBy)
	assert.Equal(t, "operator@example.com", *card.ConfirmedBy)
	require.Len(t, card.Examples, 1)
	assert.Equal(t, "check_01", card.Examples[0].ID)
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), card.RequestRevision)
	assert.EqualValues(t, 16384, provider.maxTokens.Load(), "CP1 output allowance must reach the provider through the gateway")
}

func TestWriteServeStatusNamesTheBoundAddressAndOffFeatures(t *testing.T) {
	fixedSuite := verify.Plan{SuiteCommit: strings.Repeat("b", 40), ReportDir: "/var/lib/surla/reports"}
	cases := []struct {
		name       string
		addr       *net.TCPAddr
		opts       controlplane.Options
		projectDir string
		want       string
	}{
		{
			name: "loopback without a model",
			addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 51234},
			opts: controlplane.Options{CardsDir: ".surla/cards"},
			want: `surla: serving on http://127.0.0.1:51234
surla: connect MCP clients to http://127.0.0.1:51234/mcp with the bearer token (architecture guide: surla://golden-path or http://127.0.0.1:51234/golden-path)
surla: saving Change Cards in .surla/cards
surla: CP1 (propose_change, revise_change): off — no model is configured to analyze change requests (export ANTHROPIC_API_KEY or OPENAI_API_KEY, set -model-provider and -model-name or -model-gateway-addr, or call configure_model); confirm_intent and get_change are on
surla: CP2 (preview_architecture): design preview is on; swarm roster is off (set -swarm-profiles)
surla: CP3 (draft_override, start_live_target): compiled_rule is on; prompt_template is off (set -project or -verification-config, then call start_live_target with a commit; or edit the prompt's source text, commit, and verify the commit)
surla: CP4 (verify_change, get_verification, list_verifications): off (set -project or -verification-config, or run surla verify on the CLI)
surla: CP5 (approve_merge): off (set -promotion-config, or run surla approve on the CLI)
`,
		},
		{
			name: "all IPv4 interfaces without a model",
			addr: &net.TCPAddr{IP: net.IPv4zero, Port: 8090},
			opts: controlplane.Options{CardsDir: ".surla/cards"},
			want: `surla: serving on http://localhost:8090
surla: connect MCP clients to http://localhost:8090/mcp with the bearer token (architecture guide: surla://golden-path or http://localhost:8090/golden-path)
surla: saving Change Cards in .surla/cards
surla: CP1 (propose_change, revise_change): off — no model is configured to analyze change requests (export ANTHROPIC_API_KEY or OPENAI_API_KEY, set -model-provider and -model-name or -model-gateway-addr, or call configure_model); confirm_intent and get_change are on
surla: CP2 (preview_architecture): design preview is on; swarm roster is off (set -swarm-profiles)
surla: CP3 (draft_override, start_live_target): compiled_rule is on; prompt_template is off (set -project or -verification-config, then call start_live_target with a commit; or edit the prompt's source text, commit, and verify the commit)
surla: CP4 (verify_change, get_verification, list_verifications): off (set -project or -verification-config, or run surla verify on the CLI)
surla: CP5 (approve_merge): off (set -promotion-config, or run surla approve on the CLI)
`,
		},
		{
			name: "loopback with one swarm profile",
			addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8090},
			opts: controlplane.Options{
				CardsDir:      ".surla/cards",
				SwarmProfiles: []swarm.ProviderProfile{{ID: "claude-a"}},
			},
			want: `surla: serving on http://127.0.0.1:8090
surla: connect MCP clients to http://127.0.0.1:8090/mcp with the bearer token (architecture guide: surla://golden-path or http://127.0.0.1:8090/golden-path)
surla: saving Change Cards in .surla/cards
surla: CP1 (propose_change, revise_change): off — no model is configured to analyze change requests (export ANTHROPIC_API_KEY or OPENAI_API_KEY, set -model-provider and -model-name or -model-gateway-addr, or call configure_model); confirm_intent and get_change are on
surla: CP2 (preview_architecture): on (1 swarm profile loaded; pass builders in the request to propose a roster)
surla: CP3 (draft_override, start_live_target): compiled_rule is on; prompt_template is off (set -project or -verification-config, then call start_live_target with a commit; or edit the prompt's source text, commit, and verify the commit)
surla: CP4 (verify_change, get_verification, list_verifications): off (set -project or -verification-config, or run surla verify on the CLI)
surla: CP5 (approve_merge): off (set -promotion-config, or run surla approve on the CLI)
`,
		},
		{
			name: "all interfaces with all checkpoints configured",
			addr: &net.TCPAddr{IP: net.IPv6unspecified, Port: 8090},
			opts: controlplane.Options{
				CardsDir:        "/var/lib/surla/cards",
				Analyzer:        controlplane.NewModelCP1Analyzer(nil),
				SwarmProfiles:   []swarm.ProviderProfile{{ID: "claude-a"}, {ID: "gpt-b"}},
				TargetUpdater:   noopTargetUpdater{},
				Verification:    &controlplane.VerificationConfig{RunsDir: "/var/lib/surla/runs", Plan: fixedSuite},
				PromotionTarget: &releaseTarget{},
			},
			want: `surla: serving on http://localhost:8090
surla: connect MCP clients to http://localhost:8090/mcp with the bearer token (architecture guide: surla://golden-path or http://localhost:8090/golden-path)
surla: saving Change Cards in /var/lib/surla/cards
surla: CP1 (propose_change, revise_change, confirm_intent, get_change): on
surla: CP2 (preview_architecture): on (2 swarm profiles loaded; pass builders in the request to propose a roster)
surla: CP3 (draft_override): on
surla: CP4 (verify_change, get_verification, list_verifications): on (runs in /var/lib/surla/runs, reports in /var/lib/surla/reports, evaluator suite commit ` + fixedSuite.SuiteCommit + `)
surla: CP5 (approve_merge): on
`,
		},
	}
	cardsDir := t.TempDir()
	liveRoot := controlplane.LiveRoot(cardsDir)
	leftover := filepath.Join(liveRoot, "live-123")
	require.NoError(t, os.MkdirAll(leftover, 0o700))
	cases = append(cases, struct {
		name       string
		addr       *net.TCPAddr
		opts       controlplane.Options
		projectDir string
		want       string
	}{
		name: "live targets with a directory an earlier serve left",
		addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8090},
		opts: controlplane.Options{
			CardsDir:     cardsDir,
			Verification: &controlplane.VerificationConfig{RunsDir: "/var/lib/surla/runs", Plan: fixedSuite},
			Live:         &controlplane.LiveConfig{},
		},
		want: `surla: serving on http://127.0.0.1:8090
surla: connect MCP clients to http://127.0.0.1:8090/mcp with the bearer token (architecture guide: surla://golden-path or http://127.0.0.1:8090/golden-path)
surla: saving Change Cards in ` + cardsDir + `
surla: CP1 (propose_change, revise_change): off — no model is configured to analyze change requests (export ANTHROPIC_API_KEY or OPENAI_API_KEY, set -model-provider and -model-name or -model-gateway-addr, or call configure_model); confirm_intent and get_change are on
surla: CP2 (preview_architecture): design preview is on; swarm roster is off (set -swarm-profiles)
surla: CP3 (draft_override, start_live_target): compiled_rule is on; prompt_template is on once start_live_target runs a candidate commit (live targets in ` + liveRoot + `)
surla: CP3: an earlier surla serve using this cards directory left live target directory ` + leftover + `; its files were kept
surla: CP4 (verify_change, get_verification, list_verifications): on (runs in /var/lib/surla/runs, reports in /var/lib/surla/reports, evaluator suite commit ` + fixedSuite.SuiteCommit + `)
surla: CP5 (approve_merge): off (set -promotion-config, or run surla approve on the CLI)
`,
	})
	suite := strings.Repeat("c", 40)
	cases = append(cases, struct {
		name       string
		addr       *net.TCPAddr
		opts       controlplane.Options
		projectDir string
		want       string
	}{
		name: "starter project pins its HEAD as the evaluator suite",
		addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8090},
		opts: controlplane.Options{
			CardsDir: cardsDir,
			Verification: &controlplane.VerificationConfig{
				RunsDir: "/work/shop/.surla/runs",
				Plan:    verify.Plan{SuiteCommit: suite, ReportDir: "/work/shop/reports"},
			},
			Live: &controlplane.LiveConfig{},
		},
		projectDir: ".",
		want: `surla: serving on http://127.0.0.1:8090
surla: connect MCP clients to http://127.0.0.1:8090/mcp with the bearer token (architecture guide: surla://golden-path or http://127.0.0.1:8090/golden-path)
surla: saving Change Cards in ` + cardsDir + `
surla: CP1 (propose_change, revise_change): off — no model is configured to analyze change requests (export ANTHROPIC_API_KEY or OPENAI_API_KEY, set -model-provider and -model-name or -model-gateway-addr, or call configure_model); confirm_intent and get_change are on
surla: CP2 (preview_architecture): design preview is on; swarm roster is off (set -swarm-profiles)
surla: CP3 (draft_override, start_live_target): compiled_rule is on; prompt_template is on once start_live_target runs a candidate commit (live targets in ` + liveRoot + `)
surla: CP3: an earlier surla serve using this cards directory left live target directory ` + leftover + `; its files were kept
surla: CP4 (verify_change, get_verification, list_verifications): on (runs in /work/shop/.surla/runs, reports in /work/shop/reports, evaluator suite commit ` + suite + `)
surla: -project .: the evaluator suite commit is the project's HEAD at startup; commit and restart surla serve to use a later suite
surla: CP5 (approve_merge): off (set -promotion-config, or run surla approve on the CLI)
`,
	})
	cases = append(cases, struct {
		name       string
		addr       *net.TCPAddr
		opts       controlplane.Options
		projectDir string
		want       string
	}{
		name: "explicit configuration keeps its fixed suite commit",
		addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8090},
		opts: controlplane.Options{
			CardsDir: "/srv/cards",
			Verification: &controlplane.VerificationConfig{
				RunsDir: "/srv/runs",
				Plan:    verify.Plan{SuiteCommit: suite, ReportDir: "/srv/reports"},
			},
		},
		want: `surla: serving on http://127.0.0.1:8090
surla: connect MCP clients to http://127.0.0.1:8090/mcp with the bearer token (architecture guide: surla://golden-path or http://127.0.0.1:8090/golden-path)
surla: saving Change Cards in /srv/cards
surla: CP1 (propose_change, revise_change): off — no model is configured to analyze change requests (export ANTHROPIC_API_KEY or OPENAI_API_KEY, set -model-provider and -model-name or -model-gateway-addr, or call configure_model); confirm_intent and get_change are on
surla: CP2 (preview_architecture): design preview is on; swarm roster is off (set -swarm-profiles)
surla: CP3 (draft_override, start_live_target): compiled_rule is on; prompt_template is off (set -project or -verification-config, then call start_live_target with a commit; or edit the prompt's source text, commit, and verify the commit)
surla: CP4 (verify_change, get_verification, list_verifications): on (runs in /srv/runs, reports in /srv/reports, evaluator suite commit ` + suite + `)
surla: CP5 (approve_merge): off (set -promotion-config, or run surla approve on the CLI)
`,
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			writeServeStatus(&out, tc.addr, tc.opts, tc.projectDir)
			assert.Equal(t, tc.want, out.String())
		})
	}
}

// Complete returns the scripted CP1 analysis as assistant text.
func (p *scriptedCP1Provider) Complete(_ context.Context, req *model.Request) (*model.Response, error) {
	p.maxTokens.Store(int64(req.MaxTokens))
	return &model.Response{
		Content: []model.Message{{
			Role:  model.ConversationRoleAssistant,
			Parts: []model.Part{model.TextPart{Text: p.analysis}},
		}},
		StopReason: "stop",
	}, nil
}

// Stream is unused because the CP1 analyzer only calls Complete.
func (p *scriptedCP1Provider) Stream(_ context.Context, _ *model.Request) (model.Streamer, error) {
	return nil, errors.New("streaming not used by CP1 analyzer")
}

// ApplyPromptOverride satisfies controlplane.TargetPromptUpdater for status tests.
func (noopTargetUpdater) ApplyPromptOverride(context.Context, string, string) (string, error) {
	return "v1", nil
}

// startModelGateway serves provider through a Goa-AI model gateway on a
// loopback gRPC listener and returns its address. The server stops when the
// test ends.
func startModelGateway(t *testing.T, provider model.Provider) string {
	t.Helper()
	gw, err := gateway.NewServer(gateway.WithProvider(provider))
	require.NoError(t, err)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcSrv := grpc.NewServer()
	gateway.RegisterGRPCServer(grpcSrv, gw)
	go func() { assert.NoError(t, grpcSrv.Serve(lis)) }()
	t.Cleanup(grpcSrv.Stop)
	return lis.Addr().String()
}
