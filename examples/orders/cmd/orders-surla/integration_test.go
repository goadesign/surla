package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	genordersserver "github.com/goadesign/surla/examples/orders/gen/http/orders/server"
	genorders "github.com/goadesign/surla/examples/orders/gen/orders"
	genrefund "github.com/goadesign/surla/examples/orders/gen/orders/agents/refund"
	genrefundorders "github.com/goadesign/surla/examples/orders/gen/orders/agents/refund/orders"
	"github.com/goadesign/surla/examples/orders/internal/assistant"
	"github.com/goadesign/surla/examples/orders/internal/orders"
	gensurlatools "github.com/goadesign/surla/gen/surla/toolsets/surla"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/runtime/agent/model"
	"goa.design/goa-ai/runtime/agent/prompt"
	"goa.design/goa-ai/runtime/agent/runtime"
	storageinmem "goa.design/goa-ai/runtime/agent/storage/inmem"
	mcpruntime "goa.design/goa-ai/runtime/mcp"
	goahttp "goa.design/goa/v3/http"
)

type (
	// targetFixture serves the real orders prompt operation and runs the
	// real refund planner. Its run engine and model provider are in memory;
	// it claims no refund, deployment, or cross-process restart evidence.
	targetFixture struct {
		// server exposes the generated orders handler and JWT authentication.
		server *httptest.Server
		// prompts is the same prompt store the planner reads.
		prompts *prompt.InMemoryStore
		// agent starts turns with the generated refund agent client.
		agent runtime.AgentClient
		// runs creates the session for each test turn.
		runs *storageinmem.Store
		// model records actual requests sent by the planner.
		model *recordingModel
		// updates counts PUT requests received by the target API.
		updates atomic.Int64
	}

	// recordingModel answers a question and records its model request.
	recordingModel struct {
		// requests holds one completed turn's request until its assertion.
		requests chan *model.Request
	}

	// finalReply streams one final answer without calling refund tools.
	finalReply struct {
		// next identifies the next chunk the model client reads.
		next int
	}

	// toolReply decodes the MCP result at the HTTP boundary. The generated
	// Surla codec decodes the structured business result afterwards.
	toolReply struct {
		// IsError says the tool refused or failed its operation.
		IsError bool `json:"isError"`
		// StructuredContent holds the generated tool result JSON.
		StructuredContent json.RawMessage `json:"structuredContent"`
		// Content carries the tool's public error message.
		Content json.RawMessage `json:"content"`
	}
)

// testSigningKey signs disposable JWTs issued only inside this test server.
var testSigningKey = []byte("phase19-disposable-signing-key")

// TestMCPDraftChangesTheNextOrdersTurn exercises both authenticated HTTP
// boundaries, the generated target client, the real target store, and the
// planner's next model request. No fixture updater substitutes for orders.
func TestMCPDraftChangesTheNextOrdersTurn(t *testing.T) {
	target := newTarget(t)
	baseline := target.nextRequest(t)
	assert.Equal(t, []prompt.PromptRef{{ID: genrefund.SystemPromptID, Version: genrefund.PromptSpecSystem().Version}}, baseline.PromptRefs)
	host := newHost(t, target.server.URL, targetToken(t, "prompts:manage"))
	const text = "Help customers with refunds. Keep the answer to one sentence."
	result := callTool(t, host, "draft_override", draftPayload(t, "prompt_template", text))
	require.False(t, result.IsError, "%s", result.Content)
	draft, err := gensurlatools.UnmarshalDraftOverrideResult(result.StructuredContent)
	require.NoError(t, err)
	require.True(t, draft.AppliedInStore)
	require.False(t, draft.RequiresRebuild)
	require.NotNil(t, draft.PromptVersion)
	assert.Equal(t, prompt.VersionFromTemplate(text), *draft.PromptVersion)
	request := target.nextRequest(t)
	assert.Equal(t, []prompt.PromptRef{{ID: genrefund.SystemPromptID, Version: *draft.PromptVersion}}, request.PromptRefs)
	require.NotEmpty(t, request.Messages)
	assert.Equal(t, text, request.Messages[0].Text())
	assert.Equal(t, int64(1), target.updates.Load())
}

// TestMCPAndTargetFailuresNeverSaveAnOverride distinguishes MCP auth from
// target permission and validation errors, and proves all leave the real
// target store unchanged.
func TestMCPAndTargetFailuresNeverSaveAnOverride(t *testing.T) {
	target := newTarget(t)
	for _, tc := range []struct {
		name, scope, text, message string
	}{
		{"missing target permission", "orders:chat refunds:approve", "New text.", "prompts:manage"},
		{"invalid target template", "prompts:manage", "Hello {{ .Name", "invalid prompt template"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newHost(t, target.server.URL, targetToken(t, tc.scope))
			before := target.updates.Load()
			result := callTool(t, host, "draft_override", draftPayload(t, "prompt_template", tc.text))
			require.True(t, result.IsError)
			assert.Contains(t, string(result.Content), tc.message)
			assert.Equal(t, before+1, target.updates.Load(), "the refusal came from the target API")
			overrides, err := target.prompts.List(t.Context())
			require.NoError(t, err)
			assert.Empty(t, overrides)
		})
	}
	host := newHost(t, target.server.URL, targetToken(t, "prompts:manage"))
	before := target.updates.Load()
	req := toolRequest(t, host.URL, "", "draft_override", draftPayload(t, "prompt_template", "No permission."))
	response, err := host.Client().Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	assert.Equal(t, before, target.updates.Load())
	result := callTool(t, host, "draft_override", draftPayload(t, "compiled_rule", "Change the refund limit."))
	require.False(t, result.IsError)
	draft, err := gensurlatools.UnmarshalDraftOverrideResult(result.StructuredContent)
	require.NoError(t, err)
	assert.True(t, draft.RequiresRebuild)
	assert.False(t, draft.AppliedInStore)
	assert.Equal(t, before, target.updates.Load(), "compiled rules never call the prompt API")
	args, err := gensurlatools.MarshalApproveMergePayload(&gensurlatools.ApproveMergePayload{
		ChangeID: "no-promotion-target", RequestRevision: strings.Repeat("c", 64), CandidateCommit: strings.Repeat("a", 40), ReportDigest: strings.Repeat("b", 64),
	})
	require.NoError(t, err)
	result = callTool(t, host, "approve_merge", args)
	require.True(t, result.IsError)
	assert.Contains(t, string(result.Content), "no promotion target")
}

// TestTargetRedirectNeverReceivesAnOperationOrCredential proves that a
// response cannot choose another recipient or replace a PUT with a GET that
// falsely reports success. Both servers use the same host and different
// ports, which Go's default redirect policy otherwise treats as trusted.
func TestTargetRedirectNeverReceivesAnOperationOrCredential(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var received atomic.Int64
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				received.Add(1)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"prompt_id":"orders.refund.system","version":"unapplied-version"}`)
			}))
			defer destination.Close()
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, destination.URL, code)
			}))
			defer target.Close()
			host := newHost(t, target.URL, "disposable-target-token")
			result := callTool(t, host, "draft_override", draftPayload(t, "prompt_template", "No redirect may apply this."))
			require.True(t, result.IsError, "a redirect is not a successful prompt update")
			assert.Equal(t, int64(0), received.Load(), "the destination must receive neither a request nor its bearer credential")
			assert.Empty(t, result.StructuredContent)
		})
	}
}

// Complete is unsupported because the real refund planner streams.
func (*recordingModel) Complete(context.Context, *model.Request) (*model.Response, error) {
	return nil, errors.New("expected a streaming model request")
}

// Stream records one request and returns a final answer. Unexpected extra
// requests fail without blocking the test's server.
func (p *recordingModel) Stream(_ context.Context, request *model.Request) (model.Streamer, error) {
	select {
	case p.requests <- request:
		return &finalReply{}, nil
	default:
		return nil, errors.New("unexpected extra model request")
	}
}

// Recv returns the answer, its stop reason, then the end of the stream.
func (s *finalReply) Recv() (model.Chunk, error) {
	s.next++
	switch s.next {
	case 1:
		return model.TextChunk{Message: model.Message{Role: model.ConversationRoleAssistant, Parts: []model.Part{model.TextPart{Text: "Test answer"}}}}, nil
	case 2:
		return model.StopChunk{Reason: "end_turn"}, nil
	default:
		return nil, io.EOF
	}
}

// Response returns the same answer after its chunks are drained.
func (*finalReply) Response() *model.Response {
	return &model.Response{StopReason: "end_turn", Content: []model.Message{{Role: model.ConversationRoleAssistant, Parts: []model.Part{model.TextPart{Text: "Test answer"}}}}}
}

// Close releases nothing because the reply owns no external resources.
func (*finalReply) Close() error { return nil }

// newTarget builds the real refund planner and prompt operation with one
// prompt store, mounting the generated orders HTTP server and JWTAuth.
func newTarget(t *testing.T) *targetFixture {
	t.Helper()
	store := storageinmem.New()
	prompts := prompt.NewInMemoryStore()
	rt := runtime.New(store, runtime.WithPromptStore(prompts))
	provider := &recordingModel{requests: make(chan *model.Request, 1)}
	client, err := model.NewClient(provider)
	require.NoError(t, err)
	require.NoError(t, rt.RegisterModel("test", client))
	require.NoError(t, genrefund.RegisterRefundAgent(t.Context(), rt, genrefund.RefundAgentConfig{Planner: assistant.NewPlanner("test")}))
	require.NoError(t, genrefund.RegisterUsedToolsets(t.Context(), rt,
		genrefund.WithOrdersExecutor(genrefundorders.NewRefundOrdersExec(genrefundorders.WithClient(&genorders.Client{}))),
	))
	require.NoError(t, rt.Seal(t.Context()))
	target := &targetFixture{prompts: prompts, agent: genrefund.NewClient(rt), runs: store, model: provider}
	service := orders.New(nil, nil, nil, assistant.NewPrompts(rt.PromptRegistry), testSigningKey)
	mux := goahttp.NewMuxer()
	genordersserver.Mount(mux, genordersserver.New(genorders.NewEndpoints(service), mux, goahttp.RequestDecoder, goahttp.ResponseEncoder, nil, nil))
	target.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			target.updates.Add(1)
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(target.server.Close)
	return target
}

// newHost uses the production configuration parser and handler, with two
// distinct credentials and a real target URL.
func newHost(t *testing.T, targetURL, token string) *httptest.Server {
	t.Helper()
	cfg, err := parseConfig([]string{"-principal", "operator", "-orders-url", targetURL, "-cards-dir", t.TempDir()}, func(key string) string {
		return map[string]string{"SURLA_AUTH_TOKEN": "test-mcp-token", "ORDERS_PROMPTS_TOKEN": token}[key]
	}, io.Discard)
	require.NoError(t, err)
	handler, closeCP, err := newHandler(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, closeCP()) })
	host := httptest.NewServer(handler)
	t.Cleanup(host.Close)
	return host
}

// draftPayload uses the published Surla codec for the tool's business input.
func draftPayload(t *testing.T, kind, text string) json.RawMessage {
	t.Helper()
	id := string(genrefund.SystemPromptID)
	args, err := gensurlatools.MarshalDraftOverridePayload(&gensurlatools.DraftOverridePayload{TargetKind: kind, PromptID: &id, NewContent: text})
	require.NoError(t, err)
	return args
}

// TestHostServesTheCurrentCheckpointViewer reads the checkpoint viewer the
// host serves and checks it is the build from the pinned Surla version: it
// describes a saved approval with its digests in expandable details, keeps
// unsaved edits when switching to an incoming card, shows the swarm manifest
// in expandable details, exposes the C4 diagram as a keyboard-focusable
// region, and bundles only the patched Zod 4 runtime.
func TestHostServesTheCurrentCheckpointViewer(t *testing.T) {
	target := newTarget(t)
	host := newHost(t, target.server.URL, targetToken(t, "prompts:manage"))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, host.URL+"/ui/checkpoint", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test-mcp-token")
	response, err := host.Client().Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var html string
	require.NoError(t, json.NewDecoder(response.Body).Decode(&html))
	for _, want := range []string{
		"surla-cp5-approval-details",
		"surla-cp2-swarm-manifest",
		"Switch to Incoming Card (Keep Unsaved Edits)",
		"C4 architecture diagram",
		".surla-graph-wrap:focus-visible, summary:focus-visible",
		"major:4,minor:6,patch:7",
	} {
		assert.Contains(t, html, want)
	}
	for _, stale := range []string{
		"In-memory approval recorded",
		"ZodEffects",
		"ZodParsedType",
		".surla-details summary:focus-visible",
	} {
		assert.NotContains(t, html, stale)
	}
}

// callTool checks the complete JSON-RPC response before decoding the MCP
// tool result; a transport failure cannot look like an expected tool refusal.
func callTool(t *testing.T, host *httptest.Server, name string, args json.RawMessage) toolReply {
	t.Helper()
	response, err := host.Client().Do(toolRequest(t, host.URL, "test-mcp-token", name, args))
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var envelope struct {
		Result toolReply       `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&envelope))
	require.Empty(t, envelope.Error, "JSON-RPC failed: %s", envelope.Error)
	return envelope.Result
}

// toolRequest builds the public MCP HTTP envelope, including the required
// protocol metadata and method/name headers. It never sends target credentials.
func toolRequest(t *testing.T, hostURL, token, name string, args json.RawMessage) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args, "_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    mcpruntime.ProtocolVersion,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			"io.modelcontextprotocol/clientInfo":         map[string]string{"name": "orders-surla-test", "version": "1"},
		}},
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, hostURL+"/mcp", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("MCP-Protocol-Version", mcpruntime.ProtocolVersion)
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", name)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// targetToken issues a disposable target JWT with exactly the given scopes.
func targetToken(t *testing.T, scope string) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "operator", "tenant_id": "t1", "scope": scope, "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(testSigningKey)
	require.NoError(t, err)
	return token
}

// nextRequest runs one actual planner turn and returns its recorded request.
// Waiting for the synchronous run bounds cleanup as well as observation.
func (s *targetFixture) nextRequest(t *testing.T) *model.Request {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	sessionID := rand.Text()
	_, err := s.runs.CreateSession(ctx, sessionID, time.Now().UTC())
	require.NoError(t, err)
	_, err = s.agent.Run(ctx, sessionID, []*model.Message{{Role: model.ConversationRoleUser, Parts: []model.Part{model.TextPart{Text: "Where is my refund?"}}}},
		runtime.WithRunID(rand.Text()), runtime.WithLabels(map[string]string{"tenant_id": "t1", "customer_id": "c1"}))
	require.NoError(t, err)
	select {
	case request := <-s.model.requests:
		return request
	default:
		t.Fatal("the planner completed without making a model request")
		return nil
	}
}
