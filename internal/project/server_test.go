package project

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	genprojectclient "github.com/goadesign/surla/gen/http/project/client"
	genproject "github.com/goadesign/surla/gen/project"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goahttp "goa.design/goa/v3/http"
)

type headerDoer struct {
	client  *http.Client
	token   string
	browser bool
}

func (d headerDoer) Do(r *http.Request) (*http.Response, error) {
	if d.token != "" {
		r.Header.Set("Authorization", "Bearer "+d.token)
	}
	if d.browser {
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	return d.client.Do(r)
}

// TestProjectIdentitySessionsAndRoles exercises real generated HTTP and MCP consumers without starting agents or models.
func TestProjectIdentitySessionsAndRoles(t *testing.T) {
	state := testState(t)
	cp, err := controlplane.New(controlplane.Options{CardsDir: state.CardsDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cp.Close()) })
	server := NewServer(state, func() {})
	api := httptest.NewUnstartedServer(server.Handler(controlplane.NewHTTPHandler(cp, server.Authenticate)))
	require.NoError(t, api.Listener.Close())
	api.Listener, err = net.Listen("tcp", state.Address())
	require.NoError(t, err)
	api.Start()
	t.Cleanup(api.Close)
	management, err := state.ManagementCredential()
	require.NoError(t, err)
	client := testClient(state, api.Client(), management, false)
	identity, err := client.Identity(t.Context())
	require.NoError(t, err)
	assert.Equal(t, state.Record.Owner, identity.Owner)
	guide, err := client.AddAgent(t.Context(), &genproject.AddAgentPayload{ActorID: "guide", Role: "guide"})
	require.NoError(t, err)
	guideRaw, err := os.ReadFile(guide.CredentialFile)
	require.NoError(t, err)
	assert.NotEqual(t, state.ManagementFile(), guide.CredentialFile)
	actorTokens := map[controlplane.CallerKind]string{controlplane.CallerGuide: string(guideRaw)}
	for _, role := range []string{"builder", "test_writer"} {
		actor, err := client.AddAgent(t.Context(), &genproject.AddAgentPayload{ActorID: role, Role: role})
		require.NoError(t, err)
		raw, err := os.ReadFile(actor.CredentialFile)
		require.NoError(t, err)
		actorTokens[controlplane.CallerKind(role)] = string(raw)
	}
	rejectedRole := testRequest(t, api.Client(), http.MethodPost, state.Origin()+"/project/agents", `{"actor_id":"forbidden-manager","role":"management"}`, management, false)
	assert.Equal(t, http.StatusBadRequest, rejectedRole.StatusCode)
	require.NoError(t, rejectedRole.Body.Close())

	link, err := client.BrowserLink(t.Context())
	require.NoError(t, err)
	u, err := url.Parse(link.URL)
	require.NoError(t, err)
	exchange := testClient(state, api.Client(), "", true)
	session, err := exchange.ExchangeBrowserLink(t.Context(), &genproject.ExchangeBrowserLinkPayload{Link: strings.TrimPrefix(u.Fragment, "link=")})
	require.NoError(t, err)
	_, err = exchange.ExchangeBrowserLink(t.Context(), &genproject.ExchangeBrowserLinkPayload{Link: strings.TrimPrefix(u.Fragment, "link=")})
	assert.Error(t, err)
	// The same owner can see a guide draft, but caller attribution records the user's session.
	payload := &gensurla.ProposeChangePayload{ChangeID: "change", Prompt: "Keep refunds bounded", Analysis: &gensurla.CP1Analysis{Summary: "Bound refunds", Ambiguities: []*gensurla.CP1AnalysisAmbiguity{}, Examples: []*gensurla.CP1AnalysisExample{{Scenario: "refund", InputSummary: "refund 10", ExpectedOutcome: "refund 10", VerificationKind: "deterministic"}}, PreservedBehaviors: []string{}}}
	raw, err := gensurla.EncodeProposeChangePayload(payload)
	require.NoError(t, err)
	response := testRequest(t, api.Client(), http.MethodPost, state.Origin()+"/changes/propose", string(raw), string(guideRaw), true)
	require.Equal(t, http.StatusOK, response.StatusCode)
	cardRaw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	card, err := gensurla.DecodeChangeCardResult(cardRaw)
	require.NoError(t, err)
	assert.Equal(t, controlplane.StatusNeedsConfirmation, card.Status)
	assert.Nil(t, card.ConfirmedBy)
	read := testRequest(t, api.Client(), http.MethodGet, state.Origin()+"/changes/change", "", session.Session, true)
	assert.Equal(t, http.StatusOK, read.StatusCode)
	require.NoError(t, read.Body.Close())
	confirm := &gensurla.ConfirmIntentPayload{ChangeID: card.ChangeID, RequestRevision: card.RequestRevision, Answers: map[string]string{}, Examples: card.Examples}
	confirmRaw, err := gensurla.EncodeConfirmIntentPayload(confirm)
	require.NoError(t, err)
	denied := testRequest(t, api.Client(), http.MethodPost, state.Origin()+"/changes/confirm", string(confirmRaw), string(guideRaw), true)
	assert.Equal(t, http.StatusForbidden, denied.StatusCode)
	require.NoError(t, denied.Body.Close())
	denied = testRequest(t, api.Client(), http.MethodPost, state.Origin()+"/changes/confirm", string(confirmRaw), management, false)
	assert.Equal(t, http.StatusForbidden, denied.StatusCode)
	require.NoError(t, denied.Body.Close())
	accepted := testRequest(t, api.Client(), http.MethodPost, state.Origin()+"/changes/confirm", string(confirmRaw), session.Session, true)
	require.Equal(t, http.StatusOK, accepted.StatusCode)
	confirmedRaw, err := io.ReadAll(accepted.Body)
	require.NoError(t, err)
	require.NoError(t, accepted.Body.Close())
	confirmed, err := gensurla.DecodeChangeCardResult(confirmedRaw)
	require.NoError(t, err)
	require.NotNil(t, confirmed.ConfirmedBy)
	assert.Equal(t, state.Record.Owner, *confirmed.ConfirmedBy)
	// Native clients cannot accidentally use browser sessions without browser same-origin metadata.
	unmarked := testRequest(t, api.Client(), http.MethodGet, state.Origin()+"/changes/change", "", session.Session, false)
	assert.Equal(t, http.StatusUnauthorized, unmarked.StatusCode)
	var serviceError map[string]any
	require.NoError(t, json.NewDecoder(unmarked.Body).Decode(&serviceError))
	assert.NotEmpty(t, serviceError["message"])
	require.NoError(t, unmarked.Body.Close())
	req, err := http.NewRequest(http.MethodGet, state.Origin()+"/changes/change", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+session.Session)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	cross, err := api.Client().Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, cross.StatusCode)
	require.NoError(t, cross.Body.Close())
	req, err = http.NewRequest(http.MethodGet, state.Origin()+"/changes/change", nil)
	require.NoError(t, err)
	req.Host = "localhost:" + strconv.Itoa(state.Record.Port)
	req.Header.Set("Authorization", "Bearer "+management)
	wrong, err := api.Client().Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, wrong.StatusCode)
	require.NoError(t, wrong.Body.Close())
	// A wrong Origin and an unmarked link exchange fail before consuming the one-use value.
	wrongOrigin, err := http.NewRequest(http.MethodGet, state.Origin()+"/changes/change", nil)
	require.NoError(t, err)
	wrongOrigin.Header.Set("Authorization", "Bearer "+session.Session)
	wrongOrigin.Header.Set("Sec-Fetch-Site", "same-origin")
	wrongOrigin.Header.Set("Origin", "http://127.0.0.1:1")
	refused, err := api.Client().Do(wrongOrigin)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, refused.StatusCode)
	require.NoError(t, refused.Body.Close())
	unmarkedLink, err := client.BrowserLink(t.Context())
	require.NoError(t, err)
	unmarkedURL, err := url.Parse(unmarkedLink.URL)
	require.NoError(t, err)
	exchangeBody, err := json.Marshal(&genproject.ExchangeBrowserLinkPayload{Link: strings.TrimPrefix(unmarkedURL.Fragment, "link=")})
	require.NoError(t, err)
	refused = testRequest(t, api.Client(), http.MethodPost, state.Origin()+"/project/browser-session", string(exchangeBody), "", false)
	assert.Equal(t, http.StatusForbidden, refused.StatusCode)
	require.NoError(t, refused.Body.Close())
	_, err = exchange.ExchangeBrowserLink(t.Context(), &genproject.ExchangeBrowserLinkPayload{Link: strings.TrimPrefix(unmarkedURL.Fragment, "link=")})
	require.NoError(t, err)
	// Catalogs and direct MCP policy use the same table, including management denial.
	for _, tc := range []struct {
		role  controlplane.CallerKind
		token string
	}{{controlplane.CallerGuide, string(guideRaw)}, {controlplane.CallerBuilder, actorTokens[controlplane.CallerBuilder]}, {controlplane.CallerTestWriter, actorTokens[controlplane.CallerTestWriter]}, {controlplane.CallerUser, session.Session}, {controlplane.CallerManagement, management}} {
		t.Run(string(tc.role), func(t *testing.T) {
			list := testRequest(t, api.Client(), http.MethodPost, state.Origin()+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, tc.token, tc.role == controlplane.CallerUser)
			var body struct {
				Error  any `json:"error"`
				Result *struct {
					ResultType string `json:"resultType"`
					Tools      []struct {
						Name string `json:"name"`
					} `json:"tools"`
				} `json:"result"`
			}
			require.Equal(t, http.StatusOK, list.StatusCode)
			require.NoError(t, json.NewDecoder(list.Body).Decode(&body))
			require.NoError(t, list.Body.Close())
			require.Nil(t, body.Error)
			require.NotNil(t, body.Result)
			assert.Equal(t, "complete", body.Result.ResultType)
			listed := map[string]bool{}
			for _, tool := range body.Result.Tools {
				listed[tool.Name] = true
			}
			// wait_for_changes is the views page's HTTP-only waiting read, so it is not listed as a tool.
			httpOrResourceOnly := map[string]bool{"golden_path": true, "wait_for_changes": true}
			for _, name := range gensurla.MethodNames {
				if !httpOrResourceOnly[name] {
					assert.Equal(t, controlplane.OperationAllowed(tc.role, name), listed[name], name)
				}
			}
			resources := testRequest(t, api.Client(), http.MethodPost, state.Origin()+"/mcp", `{"jsonrpc":"2.0","id":4,"method":"resources/list","params":{}}`, tc.token, tc.role == controlplane.CallerUser)
			require.Equal(t, http.StatusOK, resources.StatusCode)
			var catalog struct {
				Error  any `json:"error"`
				Result *struct {
					Resources []struct {
						Name string `json:"name"`
					} `json:"resources"`
				} `json:"result"`
			}
			require.NoError(t, json.NewDecoder(resources.Body).Decode(&catalog))
			require.NoError(t, resources.Body.Close())
			require.Nil(t, catalog.Error)
			require.NotNil(t, catalog.Result)
			names := []string{}
			for _, resource := range catalog.Result.Resources {
				names = append(names, resource.Name)
			}
			expected := []string{}
			for _, name := range []string{"golden_path"} {
				if controlplane.OperationAllowed(tc.role, name) {
					expected = append(expected, name)
				}
			}
			assert.ElementsMatch(t, expected, names)
			if tc.role != controlplane.CallerManagement {
				for _, managementCall := range []struct{ method, path, body string }{
					{http.MethodGet, "/project/identity", ""},
					{http.MethodPost, "/project/stop", ""},
					{http.MethodPost, "/project/agents", `{"actor_id":"injected","role":"guide"}`},
					{http.MethodDelete, "/project/agents/guide", ""},
					{http.MethodPost, "/project/browser-link", ""},
				} {
					denied := testRequest(t, api.Client(), managementCall.method, state.Origin()+managementCall.path, managementCall.body, tc.token, tc.role == controlplane.CallerUser)
					assert.Equal(t, http.StatusForbidden, denied.StatusCode, managementCall.path)
					require.NoError(t, denied.Body.Close())
				}
				assert.Len(t, state.Record.Agents, 3, "management refusals preserve roster")
			}
			for _, request := range policyHTTPRequests(card, string(raw), string(confirmRaw)) {
				if controlplane.OperationAllowed(tc.role, request.operation) {
					continue
				}
				denied := testRequest(t, api.Client(), request.method, state.Origin()+request.path, request.body, tc.token, tc.role == controlplane.CallerUser)
				failure, err := io.ReadAll(denied.Body)
				require.NoError(t, err)
				require.NoError(t, denied.Body.Close())
				assert.Equal(t, http.StatusForbidden, denied.StatusCode, request.operation+": "+string(failure))
				assert.Contains(t, string(failure), "may not call "+request.operation)
			}
			guidance := testRequest(t, api.Client(), http.MethodGet, state.Origin()+"/golden-path", "", tc.token, tc.role == controlplane.CallerUser)
			if tc.role == controlplane.CallerManagement {
				assert.Equal(t, http.StatusForbidden, guidance.StatusCode)
			} else {
				assert.Equal(t, http.StatusOK, guidance.StatusCode)
			}
			require.NoError(t, guidance.Body.Close())
			read := testRequest(t, api.Client(), http.MethodGet, state.Origin()+"/changes/change", "", tc.token, tc.role == controlplane.CallerUser)
			if tc.role == controlplane.CallerManagement || tc.role == controlplane.CallerTestWriter {
				assert.Equal(t, http.StatusForbidden, read.StatusCode)
			} else {
				assert.Equal(t, http.StatusOK, read.StatusCode)
			}
			require.NoError(t, read.Body.Close())
			if tc.role == controlplane.CallerUser || tc.role == controlplane.CallerGuide || tc.role == controlplane.CallerBuilder {
				called := testRequest(t, api.Client(), http.MethodPost, state.Origin()+"/mcp", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_change","arguments":{"change_id":"change"}}}`, tc.token, tc.role == controlplane.CallerUser)
				require.Equal(t, http.StatusOK, called.StatusCode)
				var envelope struct {
					Error  any `json:"error"`
					Result *struct {
						StructuredContent json.RawMessage `json:"structuredContent"`
						IsError           *bool           `json:"isError"`
					} `json:"result"`
				}
				require.NoError(t, json.NewDecoder(called.Body).Decode(&envelope))
				require.NoError(t, called.Body.Close())
				require.Nil(t, envelope.Error)
				require.NotNil(t, envelope.Result)
				assert.False(t, envelope.Result.IsError != nil && *envelope.Result.IsError)
				saved, err := gensurla.DecodeChangeCardResult(envelope.Result.StructuredContent)
				require.NoError(t, err)
				assert.Equal(t, confirmed.ChangeID, saved.ChangeID)
				assert.Equal(t, confirmed.RequestRevision, saved.RequestRevision)
			}
			if tc.role != controlplane.CallerUser {
				call := testRequest(t, api.Client(), http.MethodPost, state.Origin()+"/mcp", fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"confirm_intent","arguments":%s}}`, confirmRaw), tc.token, false)
				var denied map[string]any
				require.NoError(t, json.NewDecoder(call.Body).Decode(&denied))
				assert.Nil(t, denied["error"])
				result, ok := denied["result"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, true, result["isError"])
				assert.Contains(t, fmt.Sprint(result["content"]), "may not call confirm_intent")
				require.NoError(t, call.Body.Close())
			}
		})
	}
	// Restart discards user sessions while keeping the agent file and role.
	fresh := NewServer(state, func() {})
	oldSession := httptest.NewRequest(http.MethodGet, state.Origin()+"/changes/change", nil)
	oldSession.Header.Set("Authorization", "Bearer "+session.Session)
	oldSession.Header.Set("Sec-Fetch-Site", "same-origin")
	recorder := httptest.NewRecorder()
	fresh.Handler(controlplane.NewHTTPHandler(cp, fresh.Authenticate)).ServeHTTP(recorder, oldSession)
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
	var failure map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &failure))
	assert.NotEmpty(t, failure["name"])
	assert.NotEmpty(t, failure["message"])

	request := httptest.NewRequest(http.MethodGet, state.Origin()+"/changes/change", nil)
	request.Header.Set("Authorization", "Bearer "+session.Session)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	_, err = fresh.Authenticate(request)
	assert.Error(t, err)
	request.Header.Set("Authorization", "Bearer "+string(guideRaw))
	principal, err := fresh.Authenticate(request)
	require.NoError(t, err)
	assert.Equal(t, state.Record.Owner, principal.ID)
	assert.Equal(t, "guide", principal.CallerID)
	require.NoError(t, client.RemoveAgent(t.Context(), &genproject.RemoveAgentPayload{ActorID: "guide"}))
	_, err = fresh.Authenticate(request)
	assert.Error(t, err)
}

// TestBrowserLinkExpiryAndDiscovery proves path aliases and nearest parent project records select one identity.
func TestBrowserLinkExpiryAndDiscovery(t *testing.T) {
	state := testState(t)
	server := NewServer(state, func() {})
	management := controlplane.WithPrincipal(t.Context(), controlplane.Principal{ID: state.Record.Owner, CallerID: "management", Kind: controlplane.CallerManagement})
	now := time.Now()
	server.now = func() time.Time { return now }
	link, err := server.BrowserLink(management)
	require.NoError(t, err)
	now = now.Add(2 * time.Minute)
	u, err := url.Parse(link.URL)
	require.NoError(t, err)
	_, err = server.ExchangeBrowserLink(t.Context(), &genproject.ExchangeBrowserLinkPayload{Link: strings.TrimPrefix(u.Fragment, "link=")})
	assert.Error(t, err)
	child := filepath.Join(state.Record.Directory, "platform", "sub")
	require.NoError(t, os.MkdirAll(child, 0o700))
	found, err := Discover(filepath.Dir(state.Dir), child)
	require.NoError(t, err)
	assert.Equal(t, state.Record.ID, found.Record.ID)
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(state.Record.Directory, alias))
	found, err = Discover(filepath.Dir(state.Dir), alias)
	require.NoError(t, err)
	assert.Equal(t, state.Record.ID, found.Record.ID)
	newer, err := Discover(filepath.Dir(state.Dir), filepath.Join(state.Record.Directory, "platform"))
	require.NoError(t, err)
	assert.Equal(t, state.Record.ID, newer.Record.ID)
}

func testState(t *testing.T) *State {
	t.Helper()
	state, err := Discover(t.TempDir(), t.TempDir())
	require.NoError(t, err)
	lock, err := state.StartLock(context.Background())
	require.NoError(t, err)
	created, err := state.Initialize("uid:test")
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, lock.Close())
	return state
}
func testClient(state *State, client *http.Client, token string, browser bool) *genproject.Client {
	transport := genprojectclient.NewClient("http", state.Address(), headerDoer{client: client, token: token, browser: browser}, goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	return genproject.NewClient(transport.Identity(), transport.Stop(), transport.AddAgent(), transport.RemoveAgent(), transport.BrowserLink(), transport.ExchangeBrowserLink())
}
func testRequest(t *testing.T, client *http.Client, method, target, body, token string, browser bool) *http.Response {
	t.Helper()
	if strings.HasSuffix(target, "/mcp") {
		var envelope map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(body), &envelope))
		var params map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(envelope["params"], &params))
		params["_meta"] = json.RawMessage(`{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`)
		raw, err := json.Marshal(params)
		require.NoError(t, err)
		envelope["params"] = raw
		raw, err = json.Marshal(envelope)
		require.NoError(t, err)
		body = string(raw)
	}
	r, err := http.NewRequest(method, target, strings.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	if strings.HasSuffix(target, "/mcp") {
		r.Header.Set("MCP-Protocol-Version", "2026-07-28")
		var call struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &call))
		r.Header.Set("Mcp-Method", call.Method)
		if call.Params.Name != "" {
			r.Header.Set("Mcp-Name", call.Params.Name)
		}
	}
	response, err := (headerDoer{client: client, token: token, browser: browser}).Do(r)
	require.NoError(t, err)
	return response
}

// policyHTTPRequest supplies a schema-valid request so refusal proves the policy boundary ran.
type policyHTTPRequest struct{ operation, method, path, body string }

// policyHTTPRequests covers every generated control-plane route without executing forbidden work.
func policyHTTPRequests(card *gensurla.ChangeCardResult, propose, confirm string) []policyHTTPRequest {
	commit := strings.Repeat("a", 40)
	revision := card.RequestRevision
	revise := fmt.Sprintf(`{"change_id":"change","request_revision":%q,"prompt":"Keep refunds bounded","analysis":{"summary":"Bound refunds","ambiguities":[],"examples":[{"prior_id":"check_01","scenario":"refund","input_summary":"refund 10","expected_outcome":"refund 10","verification_kind":"deterministic"}],"removed_example_ids":[],"preserved_behaviors":[]}}`, revision)
	return []policyHTTPRequest{
		{"golden_path", "GET", "/golden-path", ""},
		{"propose_change", "POST", "/changes/propose", propose},
		{"revise_change", "POST", "/changes/revise", revise},
		{"confirm_intent", "POST", "/changes/confirm", confirm},
		{"get_change", "GET", "/changes/change", ""},
		{"preview_architecture", "POST", "/changes/preview", fmt.Sprintf(`{"change_id":"change","request_revision":%q}`, revision)},
		{"draft_override", "POST", "/sandbox/draft", `{"target_kind":"compiled_rule","new_content":"bounded"}`},
		{"start_live_target", "POST", "/live-target", `{"candidate_commit":"` + commit + `"}`},
		{"stop_live_target", "DELETE", "/live-target", ""},
		{"verify_change", "POST", "/changes/change/verifications", fmt.Sprintf(`{"request_revision":%q,"candidate_commit":%q}`, revision, commit)},
		{"get_verification", "GET", "/verifications/run", ""},
		{"list_verifications", "GET", "/changes/change/verifications", ""},
		{"approve_merge", "POST", "/changes/approve", fmt.Sprintf(`{"change_id":"change","request_revision":%q,"candidate_commit":%q,"report_digest":%q}`, revision, commit, strings.Repeat("b", 64))},
		{"diff", "POST", "/diff", `{"old":{"files":{"go.mod":"module example.com/shop","go.sum":"","design/design.go":"package design"}},"new":{"files":{"go.mod":"module example.com/shop","go.sum":"","design/design.go":"package design"}}}`},
		{"configure_model", "POST", "/cp1/model", `{"gateway_addr":"127.0.0.1:1234"}`},
		{"init_project", "POST", "/workspace/init", `{"module_path":"example.com/shop"}`},
		{"generate_code", "POST", "/workspace/generate", `{}`},
		{"check_project", "POST", "/workspace/check", ""},
		{"update_dependencies", "POST", "/workspace/dependencies", ""},
		{"read_check_output", "POST", "/workspace/check/output", `{"op_id":"op"}`},
		{"list_traces", "GET", "/traces", ""},
		{"refresh_story", "POST", "/workspace/story/refresh", ""},
		{"get_story_refresh", "GET", "/workspace/story/refresh?op_id=op", ""},
		{"list_changes", "GET", "/changes", ""},
		{"wait_for_changes", "GET", "/change-updates?since=version", ""},
	}
}
