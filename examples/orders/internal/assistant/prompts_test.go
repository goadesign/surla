package assistant

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	genordersclient "github.com/goadesign/surla/examples/orders/gen/http/orders/client"
	genordersserver "github.com/goadesign/surla/examples/orders/gen/http/orders/server"
	genorders "github.com/goadesign/surla/examples/orders/gen/orders"
	genrefund "github.com/goadesign/surla/examples/orders/gen/orders/agents/refund"
	genrefundorders "github.com/goadesign/surla/examples/orders/gen/orders/agents/refund/orders"
	"github.com/goadesign/surla/examples/orders/internal/orders"
	"github.com/goadesign/surla/examples/orders/internal/runstore"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/runtime/agent/model"
	"goa.design/goa-ai/runtime/agent/prompt"
	"goa.design/goa-ai/runtime/agent/runtime"
	goahttp "goa.design/goa/v3/http"
)

type (
	// recordingProvider answers every model request with a short final reply
	// and keeps the requests, so a test can read the system prompt and prompt
	// version each turn sent.
	recordingProvider struct {
		// mu guards requests.
		mu sync.Mutex
		// requests are the model requests received, oldest first.
		requests []*model.Request
	}

	// replyStream streams one text reply.
	replyStream struct {
		// chunks are returned by Recv in order.
		chunks []model.Chunk
		// response is the canonical response matching chunks.
		response *model.Response
	}

	// service is one running instance of the refund agent: what a process of
	// cmd/orders builds at startup, with the engine in memory.
	service struct {
		// agent starts refund agent runs.
		agent runtime.AgentClient
		// store creates the sessions runs belong to.
		store *runstore.Store
		// prompts edits the agent's prompts.
		prompts *Prompts
		// model records the agent's model requests.
		model *recordingProvider
	}
)

// TestOverridePromptChangesTheNextModelRequest calls override_prompt through
// the generated HTTP server and client with an operator token, wired to the
// agent's real prompt store, and reads the model request of the next turn.
func TestOverridePromptChangesTheNextModelRequest(t *testing.T) {
	spec := genrefund.PromptSpecSystem()
	svc := startService(t)
	first := svc.turn(t)
	assert.Equal(t, []prompt.PromptRef{{ID: genrefund.SystemPromptID, Version: spec.Version}}, first.PromptRefs)
	assert.Equal(t, spec.Template, systemText(t, first))

	const draft = "You help customers with refunds. Answer in one sentence."
	res, err := svc.overridePrompt(t, string(genrefund.SystemPromptID), draft)
	require.NoError(t, err)
	assert.Equal(t, &genorders.PromptVersion{PromptID: string(genrefund.SystemPromptID), Version: prompt.VersionFromTemplate(draft)}, res)

	second := svc.turn(t)
	assert.Equal(t, []prompt.PromptRef{{ID: genrefund.SystemPromptID, Version: res.Version}}, second.PromptRefs,
		"the next model request names the version override_prompt returned")
	assert.Equal(t, draft, systemText(t, second))

	// A second service built the same way, as a newly started process would
	// be, has its own empty prompt store. This is an in-process stand-in for
	// a restart, not a restart of a running orders process.
	fresh := startService(t)
	third := fresh.turn(t)
	assert.Equal(t, []prompt.PromptRef{{ID: genrefund.SystemPromptID, Version: spec.Version}}, third.PromptRefs,
		"a newly built service starts with the built-in prompt")
	assert.Equal(t, spec.Template, systemText(t, third))
}

// TestOverrideRejectsWithoutChangingThePrompt verifies that Prompts.Override
// validates both the prompt ID and the template AST against the registered
// zero-argument prompt spec before saving, leaving the active prompt unchanged
// when validation fails.
func TestOverrideRejectsWithoutChangingThePrompt(t *testing.T) {
	cases := []struct {
		name     string
		promptID string
		template string
		want     error
	}{
		{"unregistered prompt", "orders.refund.other", "Hello.", orders.ErrPromptNotFound},
		{"template does not parse", string(genrefund.SystemPromptID), "Hello {{ .Name", orders.ErrInvalidPrompt},
		{"template does not execute", string(genrefund.SystemPromptID), `Hello {{ template "missing" }}.`, orders.ErrInvalidPrompt},
		{"template reads data the planner does not pass", string(genrefund.SystemPromptID), "Hello {{ .Customer }}.", orders.ErrInvalidPrompt},
		{"template reads undeclared field inside branch", string(genrefund.SystemPromptID), "Hello {{ if . }}{{ .Customer }}{{ else }}guest{{ end }}.", orders.ErrInvalidPrompt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := prompt.NewInMemoryStore()
			registry := prompt.NewRegistry(store)
			require.NoError(t, genrefund.RegisterPrompts(registry))
			prompts := NewPrompts(registry)
			const kept = "Kept text."
			_, err := prompts.Override(t.Context(), string(genrefund.SystemPromptID), kept)
			require.NoError(t, err)

			_, err = prompts.Override(t.Context(), c.promptID, c.template)
			require.ErrorIs(t, err, c.want)

			active, err := genrefund.RenderSystemPromptWithRegistry(t.Context(), registry, prompt.Scope{})
			require.NoError(t, err)
			assert.Equal(t, kept, active.Text, "a rejected override leaves the text in use unchanged")
			overrides, err := store.List(t.Context())
			require.NoError(t, err)
			assert.Len(t, overrides, 1, "a rejected override is not saved")
		})
	}
}

// Complete is not used by the planner.
func (p *recordingProvider) Complete(context.Context, *model.Request) (*model.Response, error) {
	return nil, errors.New("the planner only streams")
}

// Stream records req and replies with one final text message.
func (p *recordingProvider) Stream(_ context.Context, req *model.Request) (model.Streamer, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	reply := model.Message{Role: model.ConversationRoleAssistant, Parts: []model.Part{model.TextPart{Text: "It is on its way."}}}
	return &replyStream{
		chunks:   []model.Chunk{model.TextChunk{Message: reply}, model.StopChunk{Reason: "end_turn"}},
		response: &model.Response{StopReason: "end_turn", Content: []model.Message{reply}},
	}, nil
}

// Recv returns the next chunk, then io.EOF.
func (s *replyStream) Recv() (model.Chunk, error) {
	if len(s.chunks) == 0 {
		return nil, io.EOF
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, nil
}

// Response returns the canonical response.
func (s *replyStream) Response() *model.Response { return s.response }

// Close releases nothing.
func (s *replyStream) Close() error { return nil }

// startService builds the refund agent the way cmd/orders does, with its own
// runtime store and its own in-memory prompt store.
func startService(t *testing.T) *service {
	t.Helper()
	ctx := t.Context()
	store, err := runstore.Open(ctx, filepath.Join(t.TempDir(), "runtime.db"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close()) })
	promptStore := prompt.NewInMemoryStore()
	rt := runtime.New(store, runtime.WithPromptStore(promptStore))
	recorder := &recordingProvider{}
	client, err := model.NewClient(recorder)
	require.NoError(t, err)
	require.NoError(t, rt.RegisterModel("test", client))
	require.NoError(t, genrefund.RegisterRefundAgent(ctx, rt, genrefund.RefundAgentConfig{Planner: NewPlanner("test")}))
	// The recorded model never calls a tool, so the tools' orders client has
	// no endpoints.
	require.NoError(t, genrefund.RegisterUsedToolsets(ctx, rt,
		genrefund.WithOrdersExecutor(genrefundorders.NewRefundOrdersExec(genrefundorders.WithClient(&genorders.Client{}))),
	))
	require.NoError(t, rt.Seal(ctx))
	return &service{agent: genrefund.NewClient(rt), store: store, prompts: NewPrompts(rt.PromptRegistry), model: recorder}
}

// systemText returns the system message text of req.
func systemText(t *testing.T, req *model.Request) string {
	t.Helper()
	require.NotEmpty(t, req.Messages)
	require.Equal(t, model.ConversationRoleSystem, req.Messages[0].Role)
	return req.Messages[0].Text()
}

// overridePrompt calls override_prompt on an orders service that uses s's
// prompts, through the generated HTTP server and client, with an operator
// token holding the "prompts:manage" scope.
func (s *service) overridePrompt(t *testing.T, promptID, template string) (*genorders.PromptVersion, error) {
	t.Helper()
	signingKey := []byte("test-signing-key")
	api := orders.New(nil, nil, nil, s.prompts, signingKey)
	mux := goahttp.NewMuxer()
	genordersserver.Mount(mux, genordersserver.New(genorders.NewEndpoints(api), mux, goahttp.RequestDecoder, goahttp.ResponseEncoder, nil, nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client := genordersclient.NewClient(u.Scheme, u.Host, srv.Client(), goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "op1", "tenant_id": "t1", "scope": "prompts:manage", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(signingKey)
	require.NoError(t, err)
	res, err := client.OverridePrompt()(t.Context(), &genorders.OverridePromptPayload{Token: token, PromptID: promptID, Template: template})
	if err != nil {
		return nil, err
	}
	return res.(*genorders.PromptVersion), nil
}

// turn runs one customer message through the agent and returns the model
// request the planner sent for it.
func (s *service) turn(t *testing.T) *model.Request {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const sessionID = "t1/c1"
	_, err := s.store.CreateSession(ctx, sessionID, time.Now().UTC())
	require.NoError(t, err)
	before := s.model.count()
	_, err = s.agent.Run(ctx, sessionID, []*model.Message{{
		Role: model.ConversationRoleUser, Parts: []model.Part{model.TextPart{Text: "Where is my refund?"}},
	}}, runtime.WithRunID(newRunID()), runtime.WithLabels(map[string]string{labelTenant: "t1", labelCustomer: "c1"}))
	require.NoError(t, err)
	return s.model.since(t, before)
}

// count returns how many requests were recorded.
func (p *recordingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// since returns the one request recorded after the first n.
func (p *recordingProvider) since(t *testing.T, n int) *model.Request {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Len(t, p.requests, n+1, "one model request per turn")
	return p.requests[n]
}
