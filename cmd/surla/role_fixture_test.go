// These helpers give existing domain/transport fixtures separate credentials for each caller purpose.
// The dedicated project consumer tests exercise the production credential lifecycle.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

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
	return controlplane.NewTokenAuthenticator(credentials)
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
