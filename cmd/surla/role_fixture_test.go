// These helpers give existing domain/transport fixtures separate credentials for each caller purpose.
// The dedicated project consumer tests exercise the production credential lifecycle.
package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	genmcpclient "github.com/goadesign/surla/gen/jsonrpc/mcp_surla/client"
	genmcp "github.com/goadesign/surla/gen/mcp_surla"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goahttp "goa.design/goa/v3/http"

	"github.com/goadesign/surla/internal/controlplane"
)

// workflowAuthenticator provisions independent test credentials rather than giving one token several roles.
func workflowAuthenticator(owners map[string]controlplane.Principal) controlplane.RequestAuthenticator {
	credentials := make(map[string]controlplane.Principal, len(owners)*3)
	for token, p := range owners {
		p.CallerID = "user"
		p.Kind = controlplane.CallerUser
		credentials[token] = p
		p.CallerID = "guide"
		p.Kind = controlplane.CallerGuide
		credentials[token+"-guide"] = p
		p.CallerID = "builder"
		p.Kind = controlplane.CallerBuilder
		credentials[token+"-builder"] = p
	}
	return fixtureCredentials(credentials)
}

// workflowAuthorization chooses the fixture's role credential for its explicitly selected operation.
func workflowAuthorization(request *http.Request, token string) {
	operation := ""
	switch request.URL.Path {
	case "/changes/propose":
		operation = "propose_change"
	case "/changes/revise":
		operation = "revise_change"
	case "/workspace/story/refresh":
		if request.Method == http.MethodPost {
			operation = "refresh_story"
		}
	case "/workspace/init":
		operation = "init_project"
	case "/workspace/generate":
		operation = "generate_code"
	case "/workspace/check":
		operation = "check_project"
	case "/workspace/dependencies":
		operation = "update_dependencies"
	case "/sandbox/draft":
		operation = "draft_override"
	case "/live-target":
		if request.Method == http.MethodPost {
			operation = "start_live_target"
		}
	case "/mcp":
		// Goa-AI's HTTP transport sets the binding header and clears GetBody after encoding.
		operation = request.Header.Get("Mcp-Name")
		if request.GetBody != nil {
			body, err := request.GetBody()
			if err != nil {
				panic(err)
			}
			raw, readErr := io.ReadAll(body)
			closeErr := body.Close()
			if readErr != nil {
				panic(readErr)
			}
			if closeErr != nil {
				panic(closeErr)
			}
			var call struct {
				Params struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			if err := json.Unmarshal(raw, &call); err == nil {
				operation = call.Params.Name
			}
		}
	default:
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/verifications") {
			operation = "verify_change"
		}
	}
	switch {
	case controlplane.OperationAllowed(controlplane.CallerGuide, operation) && !controlplane.OperationAllowed(controlplane.CallerUser, operation):
		token += "-guide"
	case controlplane.OperationAllowed(controlplane.CallerBuilder, operation) && !controlplane.OperationAllowed(controlplane.CallerUser, operation):
		token += "-builder"
	}
	request.Header.Set("Authorization", "Bearer "+token)
}

// fixtureCredentials resolves explicit test-only bearer identities. Production uses the project owner.
func fixtureCredentials(credentials map[string]controlplane.Principal) controlplane.RequestAuthenticator {
	return func(r *http.Request) (controlplane.Principal, error) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if principal, exists := credentials[token]; ok && exists {
			return principal, nil
		}
		return controlplane.Principal{}, errors.New("invalid fixture credential")
	}
}

// TestWorkflowAuthorizationUsesGeneratedMCPRequests keeps transport fixtures honest about caller roles.
func TestWorkflowAuthorizationUsesGeneratedMCPRequests(t *testing.T) {
	for _, operation := range []string{"init_project", "generate_code", "confirm_intent"} {
		request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", nil)
		require.NoError(t, err)
		_, err = genmcpclient.EncodeToolsCallRequest(goahttp.RequestEncoder)(request, &genmcp.ToolsCallPayload{Name: operation, Arguments: json.RawMessage(`{}`)})
		require.NoError(t, err)
		workflowAuthorization(request, "test-token")
		expected := "Bearer test-token"
		if operation == "init_project" {
			expected += "-guide"
		}
		if operation == "generate_code" {
			expected += "-builder"
		}
		require.Equal(t, expected, request.Header.Get("Authorization"))
	}
}

// TestGeneratedMCPClientRetainsCallerRole reaches the real policy wrapper without invoking a compiler.
func TestGeneratedMCPClientRetainsCallerRole(t *testing.T) {
	cp, err := controlplane.New(controlplane.Options{CardsDir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cp.Close()) })
	auth := workflowAuthenticator(map[string]controlplane.Principal{"test-token": {ID: "owner"}})
	server := httptest.NewServer(controlplane.NewHTTPHandler(cp, func(r *http.Request) (controlplane.Principal, error) {
		principal, err := auth(r)
		assert.NoError(t, err)
		assert.Equal(t, controlplane.CallerGuide, principal.Kind)
		return principal, err
	}))
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	require.NoError(t, err)
	transport := genmcpclient.NewClient(address.Scheme, address.Host, &bearerDoer{client: server.Client(), token: "test-token"}, goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	client := &genmcp.Client{ToolsCallEndpoint: transport.ToolsCall()}
	result, err := client.ToolsCall(t.Context(), &genmcp.ToolsCallPayload{Name: "init_project", Arguments: json.RawMessage(`{"module_path":"example.com/shop"}`)})
	require.NoError(t, err)
	complete, ok := result.Outcome.AsComplete()
	require.True(t, ok)
	raw, err := json.Marshal(complete.Content)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "may not call")
	require.Contains(t, string(raw), "workspace")
}
