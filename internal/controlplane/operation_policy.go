// This file owns the operation policy shared by HTTP invocation and MCP discovery.
package controlplane

import (
	"context"
	"errors"
	"fmt"

	genmcp "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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
	// CallerTestWriter writes the tests a build requires: it reads its own test
	// assignments and reports them finished.
	CallerTestWriter CallerKind = "test_writer"
	// CallerManagement manages the server and credentials, never application decisions.
	CallerManagement CallerKind = "management"
)

// operationRoles is the only role-to-operation table. Missing operations are refused.
var operationRoles = map[string][]CallerKind{
	"ensure_team": {CallerManagement}, "identity": {CallerManagement}, "stop": {CallerManagement}, "browser_link": {CallerManagement},
	"confirm_intent": {CallerUser}, "approve_merge": {CallerUser}, "configure_model": {CallerUser},
	"propose_change": {CallerGuide}, "revise_change": {CallerGuide}, "refresh_story": {CallerGuide},
	"get_story_refresh": {CallerUser, CallerGuide, CallerBuilder}, "init_project": {CallerGuide},
	"generate_code": {CallerBuilder}, "check_project": {CallerBuilder}, "update_dependencies": {CallerBuilder},
	"start_live_target": {CallerBuilder}, "stop_live_target": {CallerUser, CallerBuilder}, "draft_override": {CallerBuilder},
	"get_live_target": {CallerUser, CallerBuilder}, "get_model_connection": {CallerUser},
	"verify_change":        {CallerBuilder},
	"get_change":           {CallerUser, CallerGuide, CallerBuilder},
	"preview_architecture": {CallerUser, CallerGuide, CallerBuilder},
	"diff":                 {CallerUser, CallerGuide, CallerBuilder},
	"get_verification":     {CallerUser, CallerGuide, CallerBuilder},
	"list_verifications":   {CallerUser, CallerGuide, CallerBuilder},
	"list_traces":          {CallerUser, CallerGuide, CallerBuilder},
	"read_check_output":    {CallerUser, CallerGuide, CallerBuilder},
	"golden_path":          {CallerUser, CallerGuide, CallerBuilder, CallerTestWriter},
	"list_changes":         {CallerUser, CallerGuide, CallerBuilder},
	"wait_for_changes":     {CallerUser},
	"get_team":             {CallerUser}, "show_team_pane": {CallerUser}, "start_team_agents": {CallerUser},
	"start_build": {CallerUser}, "get_build": {CallerUser, CallerGuide, CallerBuilder, CallerTestWriter},
	// get_builder_assignment and get_test_writer_assignment are GET routes that
	// record that the assignment was read. The diagnostics history leaves out
	// the user's GET requests that succeed (diagnosticTracer in traces.go), so
	// allowing either to CallerUser requires changing that rule with it;
	// TestUserGETRoutesOnlyRead fails until then.
	"list_builder_assignments": {CallerBuilder}, "get_builder_assignment": {CallerBuilder},
	"list_test_writer_assignments": {CallerTestWriter}, "get_test_writer_assignment": {CallerTestWriter},
	"report_tests_finished": {CallerTestWriter},
	"decline_revision":      {CallerUser},
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
func authorizeOperation(name string, next goa.Endpoint, recorder trace.Tracer) goa.Endpoint {
	return func(ctx context.Context, req any) (result any, err error) {
		ctx, span := recorder.Start(ctx, "surla.request."+name)
		defer span.End()
		defer func() {
			if err != nil {
				var domain *goa.ServiceError
				if !errors.As(err, &domain) || domain.Fault {
					span.RecordError(err)
					span.SetStatus(codes.Error, "operation failed")
				}
			}
		}()

		principal, ok := PrincipalFromContext(ctx)
		if !ok || !OperationAllowed(principal.Kind, name) {
			return nil, gensurla.MakeForbidden(fmt.Errorf("caller may not call %s", name))
		}
		return next(ctx, req)
	}
}

// applyOperationPolicy wraps the one endpoint set supplied to both generated transports.
func applyOperationPolicy(e *gensurla.Endpoints, recorder trace.Tracer) {
	wrap := func(name string, next goa.Endpoint) goa.Endpoint { return authorizeOperation(name, next, recorder) }
	e.GoldenPath = wrap("golden_path", e.GoldenPath)
	e.ProposeChange = wrap("propose_change", e.ProposeChange)
	e.ReviseChange = wrap("revise_change", e.ReviseChange)
	e.ConfirmIntent = wrap("confirm_intent", e.ConfirmIntent)
	e.GetChange = wrap("get_change", e.GetChange)
	e.PreviewArchitecture = wrap("preview_architecture", e.PreviewArchitecture)
	e.DraftOverride = wrap("draft_override", e.DraftOverride)
	e.StartLiveTarget = wrap("start_live_target", e.StartLiveTarget)
	e.StopLiveTarget = wrap("stop_live_target", e.StopLiveTarget)
	e.GetLiveTarget = wrap("get_live_target", e.GetLiveTarget)
	e.VerifyChange = wrap("verify_change", e.VerifyChange)
	e.GetVerification = wrap("get_verification", e.GetVerification)
	e.ListVerifications = wrap("list_verifications", e.ListVerifications)
	e.ApproveMerge = wrap("approve_merge", e.ApproveMerge)
	e.Diff = wrap("diff", e.Diff)
	e.ConfigureModel = wrap("configure_model", e.ConfigureModel)
	e.GetModelConnection = wrap("get_model_connection", e.GetModelConnection)
	e.InitProject = wrap("init_project", e.InitProject)
	e.GenerateCode = wrap("generate_code", e.GenerateCode)
	e.CheckProject = wrap("check_project", e.CheckProject)
	e.UpdateDependencies = wrap("update_dependencies", e.UpdateDependencies)
	e.ReadCheckOutput = wrap("read_check_output", e.ReadCheckOutput)
	e.ListTraces = wrap("list_traces", e.ListTraces)
	e.RefreshStory = wrap("refresh_story", e.RefreshStory)
	e.GetStoryRefresh = wrap("get_story_refresh", e.GetStoryRefresh)
	e.ListChanges = wrap("list_changes", e.ListChanges)
	e.WaitForChanges = wrap("wait_for_changes", e.WaitForChanges)
	e.GetTeam = wrap("get_team", e.GetTeam)
	e.ShowTeamPane = wrap("show_team_pane", e.ShowTeamPane)
	e.StartTeamAgents = wrap("start_team_agents", e.StartTeamAgents)
	e.StartBuild = wrap("start_build", e.StartBuild)
	e.GetBuild = wrap("get_build", e.GetBuild)
	e.ListBuilderAssignments = wrap("list_builder_assignments", e.ListBuilderAssignments)
	e.GetBuilderAssignment = wrap("get_builder_assignment", e.GetBuilderAssignment)
	e.ListTestWriterAssignments = wrap("list_test_writer_assignments", e.ListTestWriterAssignments)
	e.GetTestWriterAssignment = wrap("get_test_writer_assignment", e.GetTestWriterAssignment)
	e.ReportTestsFinished = wrap("report_tests_finished", e.ReportTestsFinished)
	e.DeclineRevision = wrap("decline_revision", e.DeclineRevision)
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
