// This file owns the operation policy shared by HTTP invocation and MCP discovery.
package controlplane

import (
	"context"
	"fmt"

	genmcp "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	goa "goa.design/goa/v3/pkg"
)

// CallerKind is the trusted purpose of a project credential.
type CallerKind string

const (
	// CallerUser is a browser session belonging to the project's owner.
	CallerUser CallerKind = "user"
	// CallerGuide interprets requests and creates the project.
	CallerGuide CallerKind = "guide"
	// CallerBuilder edits and verifies the application.
	CallerBuilder CallerKind = "builder"
	// CallerTestWriter currently reads only the Golden Path resource.
	CallerTestWriter CallerKind = "test_writer"
	// CallerManagement manages the server and credentials, never application decisions.
	CallerManagement CallerKind = "management"
)

// operationRoles is the only role-to-operation table. Missing operations are refused.
var operationRoles = map[string][]CallerKind{
	"confirm_intent": {CallerUser}, "approve_merge": {CallerUser}, "configure_model": {CallerUser},
	"propose_change": {CallerGuide}, "revise_change": {CallerGuide}, "refresh_story": {CallerGuide},
	"get_story_refresh": {CallerUser, CallerGuide, CallerBuilder}, "init_project": {CallerGuide},
	"generate_code": {CallerBuilder}, "check_project": {CallerBuilder}, "update_dependencies": {CallerBuilder},
	"start_live_target": {CallerBuilder}, "stop_live_target": {CallerUser, CallerBuilder}, "draft_override": {CallerBuilder},
	"verify_change":        {CallerBuilder},
	"get_change":           {CallerUser, CallerGuide, CallerBuilder},
	"preview_architecture": {CallerUser, CallerGuide, CallerBuilder},
	"diff":                 {CallerUser, CallerGuide, CallerBuilder},
	"get_verification":     {CallerUser, CallerGuide, CallerBuilder},
	"list_verifications":   {CallerUser, CallerGuide, CallerBuilder},
	"list_traces":          {CallerUser, CallerGuide, CallerBuilder},
	"read_check_output":    {CallerUser, CallerGuide, CallerBuilder},
	"golden_path":          {CallerUser, CallerGuide, CallerBuilder, CallerTestWriter},
	"checkpoint_ui":        {CallerUser},
}

// OperationAllowed reports the same policy used by both transports and tool listing.
func OperationAllowed(kind CallerKind, operation string) bool {
	for _, allowed := range operationRoles[operation] {
		if kind == allowed {
			return true
		}
	}
	return false
}

// authorizeOperation refuses before the operation touches domain state.
func authorizeOperation(name string, next goa.Endpoint) goa.Endpoint {
	return func(ctx context.Context, req any) (any, error) {
		principal, ok := PrincipalFromContext(ctx)
		if !ok || !OperationAllowed(principal.Kind, name) {
			return nil, gensurla.MakeForbidden(fmt.Errorf("caller may not call %s", name))
		}
		return next(ctx, req)
	}
}

// applyOperationPolicy wraps the one endpoint set supplied to both generated transports.
func applyOperationPolicy(e *gensurla.Endpoints) {
	e.CheckpointUI = authorizeOperation("checkpoint_ui", e.CheckpointUI)
	e.GoldenPath = authorizeOperation("golden_path", e.GoldenPath)
	e.ProposeChange = authorizeOperation("propose_change", e.ProposeChange)
	e.ReviseChange = authorizeOperation("revise_change", e.ReviseChange)
	e.ConfirmIntent = authorizeOperation("confirm_intent", e.ConfirmIntent)
	e.GetChange = authorizeOperation("get_change", e.GetChange)
	e.PreviewArchitecture = authorizeOperation("preview_architecture", e.PreviewArchitecture)
	e.DraftOverride = authorizeOperation("draft_override", e.DraftOverride)
	e.StartLiveTarget = authorizeOperation("start_live_target", e.StartLiveTarget)
	e.StopLiveTarget = authorizeOperation("stop_live_target", e.StopLiveTarget)
	e.VerifyChange = authorizeOperation("verify_change", e.VerifyChange)
	e.GetVerification = authorizeOperation("get_verification", e.GetVerification)
	e.ListVerifications = authorizeOperation("list_verifications", e.ListVerifications)
	e.ApproveMerge = authorizeOperation("approve_merge", e.ApproveMerge)
	e.Diff = authorizeOperation("diff", e.Diff)
	e.ConfigureModel = authorizeOperation("configure_model", e.ConfigureModel)
	e.InitProject = authorizeOperation("init_project", e.InitProject)
	e.GenerateCode = authorizeOperation("generate_code", e.GenerateCode)
	e.CheckProject = authorizeOperation("check_project", e.CheckProject)
	e.UpdateDependencies = authorizeOperation("update_dependencies", e.UpdateDependencies)
	e.ReadCheckOutput = authorizeOperation("read_check_output", e.ReadCheckOutput)
	e.ListTraces = authorizeOperation("list_traces", e.ListTraces)
	e.RefreshStory = authorizeOperation("refresh_story", e.RefreshStory)
	e.GetStoryRefresh = authorizeOperation("get_story_refresh", e.GetStoryRefresh)
}

// filterMCPDiscovery derives each caller's catalog from the invocation policy.
func filterMCPDiscovery(e *genmcp.Endpoints) {
	resourceList := e.ResourcesList
	e.ResourcesList = func(ctx context.Context, req any) (any, error) {
		result, err := resourceList(ctx, req)
		if err != nil {
			return nil, err
		}
		catalog := result.(*genmcp.ResourcesListResult)
		principal, _ := PrincipalFromContext(ctx)
		resources := make([]*genmcp.ResourceInfo, 0, len(catalog.Resources))
		for _, resource := range catalog.Resources {
			if OperationAllowed(principal.Kind, resource.Name) {
				resources = append(resources, resource)
			}
		}
		catalog.Resources = resources
		return catalog, nil
	}

	next := e.ToolsList
	e.ToolsList = func(ctx context.Context, req any) (any, error) {
		result, err := next(ctx, req)
		if err != nil {
			return nil, err
		}
		catalog := result.(*genmcp.ToolsListResult)
		principal, _ := PrincipalFromContext(ctx)
		tools := make([]*genmcp.ToolInfo, 0, len(catalog.Tools))
		for _, tool := range catalog.Tools {
			if OperationAllowed(principal.Kind, tool.Name) {
				tools = append(tools, tool)
			}
		}
		catalog.Tools = tools
		return catalog, nil
	}
}
