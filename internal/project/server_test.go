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
	assert.True(t, strings.HasPrefix(*confirmed.ConfirmedBy, "user-"))
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
	// Catalogs and direct MCP policy use the same table, including management denial.
	for _, tc := range []struct {
		role  controlplane.CallerKind
		token string
	}{{controlplane.CallerGuide, string(guideRaw)}, {controlplane.CallerUser, session.Session}, {controlplane.CallerManagement, management}} {
		t.Run(string(tc.role), func(t *testing.T) {
			list := testRequest(t, api.Client(), http.MethodPost, state.Origin()+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, tc.token, tc.role == controlplane.CallerUser)
			var body struct {
				Result struct {
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
				} `json:"result"`
			}
			require.NoError(t, json.NewDecoder(list.Body).Decode(&body))
			require.NoError(t, list.Body.Close())
			for _, tool := range body.Result.Tools {
				assert.True(t, controlplane.OperationAllowed(tc.role, tool.Name))
			}
			if tc.role != controlplane.CallerUser {
				call := testRequest(t, api.Client(), http.MethodPost, state.Origin()+"/mcp", fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"confirm_intent","arguments":%s}}`, confirmRaw), tc.token, false)
				var denied map[string]any
				require.NoError(t, json.NewDecoder(call.Body).Decode(&denied))
				assert.NotNil(t, denied["error"])
				require.NoError(t, call.Body.Close())
			}
		})
	}
	// Restart discards user sessions while keeping the agent file and role.
	fresh := NewServer(state, func() {})
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
	r, err := http.NewRequest(method, target, strings.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	response, err := (headerDoer{client: client, token: token, browser: browser}).Do(r)
	require.NoError(t, err)
	return response
}
