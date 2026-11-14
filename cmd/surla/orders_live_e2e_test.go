//go:build e2e

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/runtime/agent/prompt"
	goahttp "goa.design/goa/v3/http"

	genmcpclient "github.com/goadesign/surla/gen/jsonrpc/mcp_surla/client"
	genmcpsurla "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

// ordersSystemPrompt is the refund agent's system prompt in examples/orders.
const ordersSystemPrompt = "orders.refund.system"

// TestOrdersLiveCopyAppliesADraftThroughTheProjectServer runs the Orders
// example's prompt-draft journey through the project server's composition
// root and authenticated MCP endpoint, against this repository's committed
// HEAD, the way examples/orders/README.md describes it. The operator stores
// verification settings for examples/orders with surla verification set. A
// draft with no live copy is refused with no_live_target. start_live_target
// builds HEAD's orders binaries as verification does and starts them through
// the Orders evaluator suite's --live mode: simulated commerce and payments,
// a Temporal dev server and the evaluator's scripted model. draft_override
// then replaces the refund agent's system prompt through Surla's generated
// prompt client with the key Surla minted, which the copy requires; text that
// does not render and unknown prompts are refused. Stopping the copy removes
// it, and drafts are refused again. It needs git, the temporal CLI and macOS,
// and it tests only what HEAD commits.
func TestOrdersLiveCopyAppliesADraftThroughTheProjectServer(t *testing.T) {
	isolateLocalWork(t)
	for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", verify.LiveModelGatewayEnv, verify.LiveModelProviderEnv, verify.LiveModelNameEnv} {
		t.Setenv(name, "")
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	headOut, err := exec.CommandContext(t.Context(), "git", "-C", repo, "rev-parse", "--verify", "HEAD^{commit}").Output()
	require.NoError(t, err)
	head := strings.TrimSpace(string(headOut))
	workspace := filepath.Join(repo, "examples", "orders")
	settings, err := json.Marshal(map[string]any{
		"candidate_repo":   repo,
		"candidate_module": "examples/orders",
		"binaries":         map[string]string{"orders": "./cmd/orders", "orders-worker": "./cmd/orders-worker"},
		"suite_repo":       repo,
		"suite_commit":     head,
		"suite_module":     "examples/orders-eval",
		"suite_command":    "./cmd/orders_acceptance-evals",
		"timeout":          "30m",
	})
	require.NoError(t, err)
	settingsFile := filepath.Join(t.TempDir(), "verification.json")
	require.NoError(t, os.WriteFile(settingsFile, settings, 0o600))
	t.Chdir(workspace)
	require.NoError(t, runProjectSettings("verification", []string{"set", settingsFile}, io.Discard))

	opts, err := serveOptions(os.Environ(), workspace)
	require.NoError(t, err)
	cp, err := controlplane.New(opts)
	require.NoError(t, err)
	// Registered after the temporary directories, so it runs before they
	// are removed.
	t.Cleanup(func() { closeRetaining(cp, opts.CardsDir, opts.RunsDir) })
	auth := workflowAuthenticator(map[string]controlplane.Principal{"test-token": {ID: "operator@example.com"}})
	srv := httptest.NewServer(controlplane.NewHTTPHandler(cp, auth))
	t.Cleanup(srv.Close)
	serverURL, err := url.Parse(srv.URL)
	require.NoError(t, err)
	transport := genmcpclient.NewClient(serverURL.Scheme, serverURL.Host,
		&bearerDoer{client: srv.Client(), token: "test-token"},
		goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	client := &genmcpsurla.Client{ToolsCallEndpoint: transport.ToolsCall()}
	ctx, cancel := context.WithTimeout(t.Context(), journeyOwnerWait)
	defer cancel()

	refused := ordersDraftRefusal(t, ctx, client, ordersSystemPrompt, "Answer in one sentence.")
	assert.Contains(t, refused, "start_live_target", "with no live copy, the refusal names the step that enables drafts")

	args, err := gensurla.EncodeStartLiveTargetPayload(&gensurla.StartLiveTargetPayload{CandidateCommit: head})
	require.NoError(t, err)
	started, err := gensurla.DecodeLiveTargetResult(trustedSuiteCall(t, ctx, client, "start_live_target", args))
	require.NoError(t, err)
	assert.Equal(t, head, started.CandidateCommit)
	require.NotEmpty(t, started.URL)

	const text = "You help a customer get refunds for their own orders. Answer in one sentence."
	args, err = gensurla.EncodeDraftOverridePayload(&gensurla.DraftOverridePayload{
		TargetKind: controlplane.TargetKindPromptTemplate, PromptID: new(ordersSystemPrompt), NewContent: text,
	})
	require.NoError(t, err)
	drafted, err := gensurla.DecodeDraftOverrideResult(trustedSuiteCall(t, ctx, client, "draft_override", args))
	require.NoError(t, err)
	assert.True(t, drafted.AppliedInStore)
	assert.False(t, drafted.RequiresRebuild)
	require.NotNil(t, drafted.PromptVersion)
	assert.Equal(t, prompt.VersionFromTemplate(text), *drafted.PromptVersion,
		"the Orders copy accepted exactly the drafted text")
	assert.Contains(t, drafted.Message, head)

	assert.Contains(t, ordersDraftRefusal(t, ctx, client, ordersSystemPrompt, "Hello {{ .Name"), "does not render",
		"text that does not render is refused")
	assert.Contains(t, ordersDraftRefusal(t, ctx, client, "orders.refund.other", "Hello."), "no prompt with this ID",
		"an unregistered prompt is refused")
	assert.Equal(t, http.StatusUnauthorized, ordersPromptStatus(t, ctx, started.URL, "not-the-minted-key"),
		"the copy changes prompts only for the key Surla minted")

	stopped, err := gensurla.DecodeStopLiveTargetResult(trustedSuiteCall(t, ctx, client, "stop_live_target", []byte(`{}`)))
	require.NoError(t, err)
	assert.True(t, stopped.Stopped)
	liveDirs, err := verifyrun.LeftoverLiveDirs(controlplane.LiveRoot(opts.CardsDir))
	require.NoError(t, err)
	assert.Empty(t, liveDirs, "stopping the live copy removes its processes and directory")
	refused = ordersDraftRefusal(t, ctx, client, ordersSystemPrompt, text)
	assert.Contains(t, refused, "start_live_target", "after the stop, drafts are refused again")
}

// ordersDraftRefusal calls draft_override for a prompt template, requires the
// tool to refuse it, and returns the builder-safe refusal text.
func ordersDraftRefusal(t *testing.T, ctx context.Context, client *genmcpsurla.Client, promptID, text string) string {
	t.Helper()
	args, err := gensurla.EncodeDraftOverridePayload(&gensurla.DraftOverridePayload{
		TargetKind: controlplane.TargetKindPromptTemplate, PromptID: &promptID, NewContent: text,
	})
	require.NoError(t, err)
	result, err := client.ToolsCall(ctx, &genmcpsurla.ToolsCallPayload{Name: "draft_override", Arguments: args})
	require.NoError(t, err)
	complete, ok := result.Outcome.AsComplete()
	require.True(t, ok, "draft_override completes in the same call")
	require.True(t, complete.IsError != nil && *complete.IsError, "draft_override must refuse %q", text)
	return toolErrorText(complete.Content)
}

// ordersPromptStatus sends a prompt change straight to the live copy at
// origin with key, as only Surla should, and returns the HTTP status.
func ordersPromptStatus(t *testing.T, ctx context.Context, origin, key string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, origin+"/prompts/"+ordersSystemPrompt,
		strings.NewReader(`{"template":"Hijacked."}`))
	require.NoError(t, err)
	req.Header.Set("X-Live-Target-Key", key)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}
