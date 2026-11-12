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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/features/model/gateway"
	"goa.design/goa-ai/runtime/agent/model"
	goa "goa.design/goa/v3/pkg"
	"google.golang.org/grpc"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/verify"
)

type (
	// scriptedModelProvider is a model.Provider served by a local model
	// gateway in tests, standing in for the model a live target plans with.
	// It answers every Complete call with an empty assistant text.
	scriptedModelProvider struct{}

	// noopTargetUpdater is a TargetPromptUpdater stub used to verify CP3
	// startup status formatting when a host binds TargetUpdater.
	noopTargetUpdater struct{}
)

// TestValidateModelConfigChecksModelFlags verifies the -model-* flag rules
// serve applies at startup: no flags, a gateway alone, or a provider with a
// model name and that provider's API key; key values never appear in errors.
func TestValidateModelConfigChecksModelFlags(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_API_KEY": "test-anthropic-key",
		"OPENAI_API_KEY":    "test-openai-key",
	}
	cases := []struct {
		name    string
		cfg     serveConfig
		env     map[string]string
		wantErr string
	}{
		{name: "no model flags", cfg: serveConfig{checkTimeout: defaultCheckTimeout}, env: env},
		{name: "gateway", cfg: serveConfig{checkTimeout: defaultCheckTimeout, gatewayAddr: "127.0.0.1:9090"}, env: env},
		{name: "anthropic", cfg: serveConfig{checkTimeout: defaultCheckTimeout, provider: "anthropic", modelName: "claude-test"}, env: env},
		{name: "openai", cfg: serveConfig{checkTimeout: defaultCheckTimeout, provider: "openai", modelName: "gpt-test"}, env: env},
		{
			name:    "gateway with model name",
			cfg:     serveConfig{checkTimeout: defaultCheckTimeout, gatewayAddr: "127.0.0.1:9090", modelName: "claude-test"},
			env:     env,
			wantErr: "-model-gateway-addr cannot be combined",
		},
		{
			name:    "gateway with provider",
			cfg:     serveConfig{checkTimeout: defaultCheckTimeout, gatewayAddr: "127.0.0.1:9090", provider: "anthropic"},
			env:     env,
			wantErr: "-model-gateway-addr cannot be combined",
		},
		{name: "provider without model", cfg: serveConfig{checkTimeout: defaultCheckTimeout, provider: "anthropic"}, env: env, wantErr: "must be set together"},
		{name: "model without provider", cfg: serveConfig{checkTimeout: defaultCheckTimeout, modelName: "claude-test"}, env: env, wantErr: "must be set together"},
		{
			name:    "unknown provider",
			cfg:     serveConfig{checkTimeout: defaultCheckTimeout, provider: "Anthropic", modelName: "claude-test"},
			env:     env,
			wantErr: `unsupported -model-provider "Anthropic"`,
		},
		{
			name:    "missing api key",
			cfg:     serveConfig{checkTimeout: defaultCheckTimeout, provider: "openai", modelName: "gpt-test"},
			env:     map[string]string{"ANTHROPIC_API_KEY": "test-anthropic-key"},
			wantErr: "-model-provider openai requires OPENAI_API_KEY",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateModelConfig(tc.cfg, func(k string) string { return tc.env[k] })
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), "test-anthropic-key")
			assert.NotContains(t, err.Error(), "test-openai-key")
		})
	}
}

// TestNewLiveModelEnvBuildsTheLiveTargetEnvironment verifies the callback
// configure_model uses on surla serve: a gateway or a provider whose key is
// in serve's environment yields serve's environment, without its bearer
// token, plus the SURLA_LIVE_MODEL_* choice; a provider whose key is missing
// is refused as unprocessable and names the key, never its value.
func TestNewLiveModelEnvBuildsTheLiveTargetEnvironment(t *testing.T) {
	environ := []string{"PATH=/bin", "SURLA_AUTH_TOKEN=secret", "ANTHROPIC_API_KEY=ant-k"}
	env := map[string]string{"ANTHROPIC_API_KEY": "ant-k"}
	build := newLiveModelEnv(func(k string) string { return env[k] }, environ)
	gw, provider, model, other := "127.0.0.1:9090", "anthropic", "claude-sonnet-4-6", "openai"

	got, err := build(&gensurla.ConfigureModelPayload{GatewayAddr: &gw})
	require.NoError(t, err)
	assert.Equal(t, []string{"PATH=/bin", "ANTHROPIC_API_KEY=ant-k", "SURLA_LIVE_MODEL_GATEWAY=127.0.0.1:9090"}, got)

	got, err = build(&gensurla.ConfigureModelPayload{Provider: &provider, ModelName: &model})
	require.NoError(t, err)
	assert.Equal(t, []string{"PATH=/bin", "ANTHROPIC_API_KEY=ant-k", "SURLA_LIVE_MODEL_PROVIDER=anthropic", "SURLA_LIVE_MODEL_NAME=claude-sonnet-4-6"}, got)

	_, err = build(&gensurla.ConfigureModelPayload{Provider: &other, ModelName: &model})
	var se *goa.ServiceError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, "unprocessable", se.GoaErrorName())
	assert.Contains(t, err.Error(), "provider openai requires OPENAI_API_KEY in surla serve's environment")
	assert.NotContains(t, err.Error(), "ant-k")
}

func TestServeOptionsDerivesLiveTargetsFromVerification(t *testing.T) {
	isolateServeWorkspace(t)
	path := filepath.Join(t.TempDir(), "verification.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"runs_dir":"/r","report_dir":"/p","candidate_repo":"/c","candidate_module":".","suite_module":".","binaries":{"app":"./cmd/app"},`+
		`"suite_repo":"/s","suite_commit":"`+strings.Repeat("a", 40)+`","suite_command":"./cmd/evals","timeout":"20m"}`), 0o600))
	opts, err := serveOptions(serveConfig{checkTimeout: defaultCheckTimeout, verificationPath: path}, func(string) string { return "" },
		[]string{"PATH=/bin", "SURLA_AUTH_TOKEN=secret", "ANTHROPIC_API_KEY=k"})
	require.NoError(t, err)
	defer closeServeOptions(t, opts)
	assert.Nil(t, opts.ResolveVerification, "explicit -verification-config must not set starter ResolveVerification")
	require.NotNil(t, opts.Live)
	assert.Equal(t, opts.Verification.Command, opts.Live.Command)
	assert.Equal(t, 20*time.Minute, opts.Live.StartTimeout)
	assert.Equal(t, []string{"PATH=/bin", "ANTHROPIC_API_KEY=k"}, opts.Live.Env, "serve's own bearer token never reaches a live target")
}

// TestServeOptionsBindsTheCheckHelperOnEveryServePath checks that both ways
// surla serve finds its verification, an explicit -verification-config and
// the workspace's starter, bind the check runner together with the launcher
// of its check helper, `<this executable> check-run`, which each start of
// surla serve already resolves before this change.
func TestServeOptionsBindsTheCheckHelperOnEveryServePath(t *testing.T) {
	self, err := selfCommand()
	require.NoError(t, err)
	verificationPath := filepath.Join(t.TempDir(), "verification.json")
	require.NoError(t, os.WriteFile(verificationPath, []byte(`{"runs_dir":"/r","report_dir":"/p","candidate_repo":"/c","candidate_module":".","suite_module":".","binaries":{"app":"./cmd/app"},`+
		`"suite_repo":"/s","suite_commit":"`+strings.Repeat("a", 40)+`","suite_command":"./cmd/evals","timeout":"20m"}`), 0o600))
	cases := []struct {
		name string
		cfg  serveConfig
	}{
		{name: "verification config", cfg: serveConfig{checkTimeout: defaultCheckTimeout, verificationPath: verificationPath}},
		{name: "workspace starter", cfg: serveConfig{checkTimeout: defaultCheckTimeout}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateServeWorkspace(t)
			opts, err := serveOptions(tc.cfg, func(string) string { return "" }, nil)
			require.NoError(t, err)
			defer closeServeOptions(t, opts)
			require.NotNil(t, opts.Checker)
			require.NotNil(t, opts.CheckHelper)
			cmd := opts.CheckHelper()
			assert.Equal(t, self[0], cmd.Path)
			assert.Equal(t, []string{self[0], "check-run"}, cmd.Args)
		})
	}
}

func TestLiveTargetEnvCarriesServesModelChoice(t *testing.T) {
	environ := []string{"PATH=/bin", "SURLA_AUTH_TOKEN=secret", "ANTHROPIC_API_KEY=k", "SURLA_LIVE_MODEL_GATEWAY=stale:1"}
	cases := []struct {
		name string
		cfg  serveConfig
		want []string
	}{
		{name: "direct provider", cfg: serveConfig{checkTimeout: defaultCheckTimeout, provider: "anthropic", modelName: "claude-x"},
			want: []string{"PATH=/bin", "ANTHROPIC_API_KEY=k", "SURLA_LIVE_MODEL_PROVIDER=anthropic", "SURLA_LIVE_MODEL_NAME=claude-x"}},
		{name: "gateway", cfg: serveConfig{checkTimeout: defaultCheckTimeout, gatewayAddr: "127.0.0.1:9090"},
			want: []string{"PATH=/bin", "ANTHROPIC_API_KEY=k", "SURLA_LIVE_MODEL_GATEWAY=127.0.0.1:9090"}},
		{name: "no model", cfg: serveConfig{checkTimeout: defaultCheckTimeout},
			want: []string{"PATH=/bin", "ANTHROPIC_API_KEY=k"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, liveTargetEnv(environ, tc.cfg))
		})
	}
}

func TestServeOptionsWiresLiveModelEnv(t *testing.T) {
	isolateServeWorkspace(t)
	opts, err := serveOptions(
		serveConfig{checkTimeout: defaultCheckTimeout, provider: "openai", modelName: "gpt-test"},
		func(string) string { return "test-key" },
		nil,
	)
	require.NoError(t, err)
	assert.NotNil(t, opts.LiveModelEnv)
	assert.Nil(t, opts.TargetUpdater)
	require.NotNil(t, opts.Live)
	assert.Equal(t, []string{"SURLA_LIVE_MODEL_PROVIDER=openai", "SURLA_LIVE_MODEL_NAME=gpt-test"}, opts.Live.Env)
	closeServeOptions(t, opts)

	opts, err = serveOptions(serveConfig{checkTimeout: defaultCheckTimeout, gatewayAddr: "127.0.0.1:9090"}, func(string) string { return "" }, nil)
	require.NoError(t, err)
	require.NotNil(t, opts.Live)
	assert.Equal(t, []string{"SURLA_LIVE_MODEL_GATEWAY=127.0.0.1:9090"}, opts.Live.Env)
	closeServeOptions(t, opts)

	opts, err = serveOptions(serveConfig{checkTimeout: defaultCheckTimeout}, func(string) string { return "" }, nil)
	require.NoError(t, err)
	assert.NotNil(t, opts.LiveModelEnv)
	assert.Nil(t, opts.Verification)
	assert.NotNil(t, opts.ResolveVerification)
	require.NotNil(t, opts.Live)
	assert.Len(t, opts.Live.Command, 1)
	assert.Equal(t, defaultProjectVerifyTimeout, opts.Live.StartTimeout)
	assert.Empty(t, opts.Live.Env, "no model flags and no keys pass no model choice to live targets")
	closeServeOptions(t, opts)

}

func TestResolveStartupModelConfigAndServeOptionsAutoDetection(t *testing.T) {
	baseEnviron := []string{"PATH=/bin", "SURLA_AUTH_TOKEN=secret", "ANTHROPIC_API_KEY=ant-k", "OPENAI_API_KEY=oai-k"}
	cases := []struct {
		name        string
		cfg         serveConfig
		env         map[string]string
		wantCfg     serveConfig
		wantLiveEnv []string
	}{
		{
			name: "anthropic key auto-detects claude-sonnet-4-6 without verification config",
			cfg:  serveConfig{checkTimeout: defaultCheckTimeout},
			env:  map[string]string{"ANTHROPIC_API_KEY": "ant-k"},
			wantCfg: serveConfig{
				checkTimeout: defaultCheckTimeout,
				provider:     "anthropic",
				modelName:    defaultAnthropicModel,
			},
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
			cfg:  serveConfig{checkTimeout: defaultCheckTimeout},
			env:  map[string]string{"OPENAI_API_KEY": "oai-k"},
			wantCfg: serveConfig{
				checkTimeout: defaultCheckTimeout,
				provider:     "openai",
				modelName:    defaultOpenAIModel,
			},
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
			cfg:  serveConfig{checkTimeout: defaultCheckTimeout},
			env: map[string]string{
				"ANTHROPIC_API_KEY": "ant-k",
				"OPENAI_API_KEY":    "oai-k",
			},
			wantCfg: serveConfig{
				checkTimeout: defaultCheckTimeout,
				provider:     "anthropic",
				modelName:    defaultAnthropicModel,
			},
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
				checkTimeout: defaultCheckTimeout,
				provider:     "openai",
				modelName:    "gpt-5.4",
			},
			env: map[string]string{
				"ANTHROPIC_API_KEY": "ant-k",
				"OPENAI_API_KEY":    "oai-k",
			},
			wantCfg: serveConfig{
				checkTimeout: defaultCheckTimeout,
				provider:     "openai",
				modelName:    "gpt-5.4",
			},
			wantLiveEnv: []string{
				"PATH=/bin",
				"ANTHROPIC_API_KEY=ant-k",
				"OPENAI_API_KEY=oai-k",
				"SURLA_LIVE_MODEL_PROVIDER=openai",
				"SURLA_LIVE_MODEL_NAME=gpt-5.4",
			},
		},
		{
			name: "whitespace-only keys leave live targets without a model choice",
			cfg:  serveConfig{checkTimeout: defaultCheckTimeout},
			env: map[string]string{
				"ANTHROPIC_API_KEY": "   ",
				"OPENAI_API_KEY":    "\t\n",
			},
			wantCfg: serveConfig{checkTimeout: defaultCheckTimeout},
			wantLiveEnv: []string{
				"PATH=/bin",
				"ANTHROPIC_API_KEY=ant-k",
				"OPENAI_API_KEY=oai-k",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateServeWorkspace(t)
			getenv := func(k string) string { return tc.env[k] }
			assert.Equal(t, tc.wantCfg, resolveStartupModelConfig(tc.cfg, getenv))
			opts, err := serveOptions(tc.cfg, getenv, baseEnviron)
			require.NoError(t, err)
			assert.Nil(t, opts.Verification, "no verification config was supplied and empty workspace has no committed starter")
			assert.NotNil(t, opts.ResolveVerification)
			require.NotNil(t, opts.Live)
			assert.Len(t, opts.Live.Command, 1)
			assert.Equal(t, defaultProjectVerifyTimeout, opts.Live.StartTimeout)
			assert.Equal(t, tc.wantLiveEnv, opts.Live.Env)
			var statusBuf bytes.Buffer
			writeServeStatus(&statusBuf, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8090}, opts, "")
			assert.Contains(t, statusBuf.String(), "surla: CP3 (draft_override, start_live_target): compiled_rule is on; prompt_template is on once start_live_target runs a candidate commit")
			assert.Contains(t, statusBuf.String(), "surla: CP4 (verify_change, get_verification, list_verifications): workspace ")
			cp, err := controlplane.New(opts)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, cp.Close()) })
			_, err = cp.StartLiveTarget(
				controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: "alice@example.com"}),
				&gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)},
			)
			require.Error(t, err)
			var se *goa.ServiceError
			require.ErrorAs(t, err, &se)
			assert.Equal(t, "live_target_unavailable", se.GoaErrorName())
		})
	}
}

// TestServeProposesWithoutAModelAndConfigureModelSwitchesTheLiveTargetModel
// starts serve with no model flags and no provider keys, proposes a change
// over authenticated HTTP with the caller's analysis (no model is needed),
// then switches the live-target model with POST /cp1/model: a provider whose
// key is missing is refused with 422, and a gateway or a provider whose key
// is present is accepted.
func TestServeProposesWithoutAModelAndConfigureModelSwitchesTheLiveTargetModel(t *testing.T) {
	isolateServeWorkspace(t)
	env := map[string]string{}
	opts, err := serveOptions(
		serveConfig{checkTimeout: defaultCheckTimeout, cardsDir: t.TempDir(), cardsDirExplicit: true},
		func(k string) string { return env[k] },
		[]string{"PATH=/bin", "SURLA_AUTH_TOKEN=secret"},
	)
	require.NoError(t, err)

	cp, err := controlplane.New(opts)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	auth := controlplane.NewTokenAuthenticator(map[string]controlplane.Principal{
		"test-token": {ID: "operator@example.com"},
	})
	srv := httptest.NewServer(controlplane.NewHTTPHandler(cp, auth))
	t.Cleanup(srv.Close)
	post := func(path, body string) (int, []byte) {
		req, reqErr := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		require.NoError(t, reqErr)
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		resp, doErr := http.DefaultClient.Do(req)
		require.NoError(t, doErr)
		defer resp.Body.Close()
		var out bytes.Buffer
		_, readErr := out.ReadFrom(resp.Body)
		require.NoError(t, readErr)
		return resp.StatusCode, out.Bytes()
	}

	// 1. propose_change succeeds with the caller's analysis and no model.
	status, body := post("/changes/propose", `{"change_id":"vip-refunds","prompt":"VIP customers get instant store credit up to $150.",`+
		`"analysis":{"summary":"Give VIP customers instant store credit up to $150.","ambiguities":[],`+
		`"examples":[{"scenario":"VIP refund under the limit","input_summary":"VIP customer, $120.00 total",`+
		`"expected_outcome":"Instant store credit, no manager approval","verification_kind":"deterministic"}],`+
		`"preserved_behaviors":["Non-VIP refunds over $50.00 still wait for a manager."]}}`)
	require.Equal(t, http.StatusOK, status, string(body))
	var card struct {
		Status          string  `json:"status"`
		ConfirmedBy     *string `json:"confirmed_by"`
		RequestRevision string  `json:"request_revision"`
		Examples        []struct {
			ID string `json:"id"`
		} `json:"examples"`
	}
	require.NoError(t, json.Unmarshal(body, &card))
	assert.Equal(t, controlplane.StatusConfirmed, card.Status)
	require.NotNil(t, card.ConfirmedBy)
	assert.Equal(t, "operator@example.com", *card.ConfirmedBy)
	require.Len(t, card.Examples, 1)
	assert.Equal(t, "check_01", card.Examples[0].ID)
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), card.RequestRevision)

	// 2. A direct provider whose API key is missing is refused with 422.
	status, body = post("/cp1/model", `{"provider":"openai","model_name":"gpt-5.4-mini"}`)
	var missingKeyBody struct {
		Message string `json:"message"`
		Fault   bool   `json:"fault"`
	}
	require.NoError(t, json.Unmarshal(body, &missingKeyBody))
	assert.Equal(t, http.StatusUnprocessableEntity, status)
	assert.False(t, missingKeyBody.Fault)
	assert.Contains(t, missingKeyBody.Message, "provider openai requires OPENAI_API_KEY in surla serve's environment")

	// 3. A gateway address is accepted.
	status, body = post("/cp1/model", `{"gateway_addr":"127.0.0.1:9090"}`)
	require.Equal(t, http.StatusOK, status, string(body))
	var gwResult struct {
		Provider    *string `json:"provider"`
		ModelName   *string `json:"model_name"`
		GatewayAddr *string `json:"gateway_addr"`
	}
	require.NoError(t, json.Unmarshal(body, &gwResult))
	assert.Nil(t, gwResult.Provider)
	assert.Nil(t, gwResult.ModelName)
	require.NotNil(t, gwResult.GatewayAddr)
	assert.Equal(t, "127.0.0.1:9090", *gwResult.GatewayAddr)

	// 4. A provider and model name are accepted once the key is in serve's environment.
	env["ANTHROPIC_API_KEY"] = "test-anthropic-key"
	status, body = post("/cp1/model", `{"provider":"anthropic","model_name":"claude-sonnet-4-6"}`)
	require.Equal(t, http.StatusOK, status, string(body))
	var provResult struct {
		Provider    *string `json:"provider"`
		ModelName   *string `json:"model_name"`
		GatewayAddr *string `json:"gateway_addr"`
	}
	require.NoError(t, json.Unmarshal(body, &provResult))
	require.NotNil(t, provResult.Provider)
	require.NotNil(t, provResult.ModelName)
	assert.Equal(t, "anthropic", *provResult.Provider)
	assert.Equal(t, "claude-sonnet-4-6", *provResult.ModelName)
	assert.Nil(t, provResult.GatewayAddr)
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
			name: "loopback with only CP1 and CP2 preview on",
			addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 51234},
			opts: controlplane.Options{CardsDir: ".surla/cards"},
			want: `surla: serving on http://127.0.0.1:51234
surla: connect MCP clients to http://127.0.0.1:51234/mcp with the bearer token (architecture guide: surla://golden-path or http://127.0.0.1:51234/golden-path)
surla: saving Change Cards in .surla/cards
surla: CP1 (propose_change, revise_change, confirm_intent, get_change): on
surla: CP2 (preview_architecture): on
surla: CP3 (draft_override, start_live_target): compiled_rule is on; prompt_template is off (set -project or -verification-config, then call start_live_target with a commit; or edit the prompt's source text, commit, and verify the commit)
surla: CP4 (verify_change, get_verification, list_verifications): off (set -project or -verification-config, or run surla verify on the CLI)
surla: CP5 (approve_merge): off (set -promotion-config, or run surla approve on the CLI)
`,
		},
		{
			name: "all IPv4 interfaces",
			addr: &net.TCPAddr{IP: net.IPv4zero, Port: 8090},
			opts: controlplane.Options{CardsDir: ".surla/cards"},
			want: `surla: serving on http://localhost:8090
surla: connect MCP clients to http://localhost:8090/mcp with the bearer token (architecture guide: surla://golden-path or http://localhost:8090/golden-path)
surla: saving Change Cards in .surla/cards
surla: CP1 (propose_change, revise_change, confirm_intent, get_change): on
surla: CP2 (preview_architecture): on
surla: CP3 (draft_override, start_live_target): compiled_rule is on; prompt_template is off (set -project or -verification-config, then call start_live_target with a commit; or edit the prompt's source text, commit, and verify the commit)
surla: CP4 (verify_change, get_verification, list_verifications): off (set -project or -verification-config, or run surla verify on the CLI)
surla: CP5 (approve_merge): off (set -promotion-config, or run surla approve on the CLI)
`,
		},
		{
			name: "loopback without target updater",
			addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8090},
			opts: controlplane.Options{
				CardsDir: ".surla/cards",
			},
			want: `surla: serving on http://127.0.0.1:8090
surla: connect MCP clients to http://127.0.0.1:8090/mcp with the bearer token (architecture guide: surla://golden-path or http://127.0.0.1:8090/golden-path)
surla: saving Change Cards in .surla/cards
surla: CP1 (propose_change, revise_change, confirm_intent, get_change): on
surla: CP2 (preview_architecture): on
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
				TargetUpdater:   noopTargetUpdater{},
				Verification:    &controlplane.VerificationConfig{RunsDir: "/var/lib/surla/runs", Plan: fixedSuite},
				PromotionTarget: &releaseTarget{},
			},
			want: `surla: serving on http://localhost:8090
surla: connect MCP clients to http://localhost:8090/mcp with the bearer token (architecture guide: surla://golden-path or http://localhost:8090/golden-path)
surla: saving Change Cards in /var/lib/surla/cards
surla: CP1 (propose_change, revise_change, confirm_intent, get_change): on
surla: CP2 (preview_architecture): on
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
surla: CP1 (propose_change, revise_change, confirm_intent, get_change): on
surla: CP2 (preview_architecture): on
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
surla: CP1 (propose_change, revise_change, confirm_intent, get_change): on
surla: CP2 (preview_architecture): on
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
surla: CP1 (propose_change, revise_change, confirm_intent, get_change): on
surla: CP2 (preview_architecture): on
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

// Complete returns an empty assistant text.
func (*scriptedModelProvider) Complete(context.Context, *model.Request) (*model.Response, error) {
	return &model.Response{
		Content: []model.Message{{
			Role:  model.ConversationRoleAssistant,
			Parts: []model.Part{model.TextPart{}},
		}},
		StopReason: "stop",
	}, nil
}

// Stream is unused: callers of this provider only call Complete.
func (*scriptedModelProvider) Stream(context.Context, *model.Request) (model.Streamer, error) {
	return nil, errors.New("streaming is not scripted")
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
