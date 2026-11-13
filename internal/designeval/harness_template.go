// Package designeval holds the standalone Goa/Goa-AI/Model/Story evaluator
// source template compiled and executed inside verify/candidate.Exec's
// OS-enforced sandbox.
package designeval

import (
	_ "embed"
	"strings"
)

const (
	// harnessMainTemplate is the standalone evaluator program compiled and
	// executed inside candidate.Exec against the temporary design package.
	harnessMainTemplate = harnessMainHead + harnessMainHelpers + harnessGeneratedHelpers

	// harnessStoryOutputField adds the Story summary slice to the harness
	// output struct when the target module loads the portable Story DSL module.
	harnessStoryOutputField = "\n\t\tStories       []storySummary    `json:\"stories,omitempty\"`"

	// harnessStoryPreEval records whether the target declared a Goa API or
	// Service before eval.RunDSL synthesizes a default API expression.
	harnessStoryPreEval = "\n\thadGoaAPI := goaexpr.Root.API != nil || len(goaexpr.Root.Services) > 0"

	// harnessStoryRecordCall records the design's Stories with storySummaries
	// (story_reader.go) when the target module loads the portable Story DSL
	// module.
	harnessStoryRecordCall = "\n\tout.Stories = storySummaries()"

	// harnessStoryAPIGuard skips Goa's synthesized default API when a design
	// declares Stories without declaring a Goa API or Service.
	harnessStoryAPIGuard = "(hadGoaAPI || len(out.Stories) == 0) && "

	// harnessMainHead contains the package declarations, structured record types,
	// and main evaluation flow of the standalone evaluator program.
	harnessMainHead = `package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	_ {{DESIGN_IMPORT}}
	_ "goa.design/goa-ai/codegen/agent"
	_ "goa.design/goa-ai/codegen/mcp"
	"goa.design/goa/v3/codegen/generator"
	"goa.design/goa-ai/codegen/jsonschema"
	evalexpr "goa.design/goa-ai/eval/expr"
	agentexpr "goa.design/goa-ai/expr/agent"
	"goa.design/goa/v3/eval"
	goaexpr "goa.design/goa/v3/expr"
	modelexpr "goa.design/model/expr"
)

type (
	c4Container struct {
		ID          string
		Name        string
		Kind        string
		Description string
	}

	c4Edge struct {
		FromID   string
		ToID     string
		Protocol string
	}

	agentPolicy struct {
		AgentID                  string
		MaxExecutionSeconds      int
		TimeBudget               string ` + "`json:\"time_budget,omitempty\"`" + `
		PlanTimeout              string ` + "`json:\"plan_timeout,omitempty\"`" + `
		ToolTimeout              string ` + "`json:\"tool_timeout,omitempty\"`" + `
		OnMissingFields          string ` + "`json:\"on_missing_fields,omitempty\"`" + `
		HistoryMode              string ` + "`json:\"history_mode,omitempty\"`" + `
		MaxToolCalls             int
		MaxRecoveryTurns         int
		CompressAtTurns          int
		CompressAtMaxInputTokens int ` + "`json:\"compress_at_max_input_tokens,omitempty\"`" + `
		KeepRecentTurns          int
		KeepMaxInputTokens       int ` + "`json:\"keep_max_input_tokens,omitempty\"`" + `
		CacheAfterSystem         bool
		CacheAfterTools          bool
	}

	// toolContract returns one selected tool and every compiled input choice.
	toolContract struct {
		// ID combines the exact agent and tool identities from the catalog.
		ID                   string
		// Toolset is the compiler's registration route.
		Toolset              string
		// Name is the authored tool name.
		Name                 string
		// Description is the generated model-facing tool description.
		Description          string
		// ArgsSchema is the provider model input before consumer binding.
		ArgsSchema           string
		// TextOnlyArgsSchema is provider UI-free input, absent when unsupported.
		TextOnlyArgsSchema   *string
		// InputBinding contains the consumer's declared input alternatives.
		InputBinding *toolInputBinding
		// ReturnSchema is empty when the tool declares no result.
		ReturnSchema         string
		// InjectedFields are omitted from ordinary input and supplied outside it.
		InjectedFields       []string
		// RequiresConfirmation reports a configured confirmation declaration.
		RequiresConfirmation bool
		// RequiresUI reports a UI-only tool declaration.
		RequiresUI           bool
		// BoundedResult reports the compiler's out-of-band result bounds contract.
		BoundedResult        bool
	}

	output struct {
		SystemName    string            ` + "`json:\"system_name\"`" + `
		Declarations  map[string]string ` + "`json:\"declarations\"`" + `
		Containers    []c4Container     ` + "`json:\"containers\"`" + `
		Edges         []c4Edge          ` + "`json:\"edges\"`" + `
		AgentPolicies []agentPolicy     ` + "`json:\"agent_policies\"`" + `
		Tools         []toolContract    ` + "`json:\"tools\"`" + `{{STORY_OUTPUT_FIELD}}
	}

	schemaDecl struct {
		Schema     json.RawMessage        ` + "`json:\"schema\"`" + `
	}

	securityScopeDecl struct {
		Name        string ` + "`json:\"name\"`" + `
		Description string ` + "`json:\"description,omitempty\"`" + `
	}

	securityFlowDecl struct {
		Kind             string ` + "`json:\"kind\"`" + `
		AuthorizationURL string ` + "`json:\"authorization_url,omitempty\"`" + `
		TokenURL         string ` + "`json:\"token_url,omitempty\"`" + `
		RefreshURL       string ` + "`json:\"refresh_url,omitempty\"`" + `
	}

	securitySchemeDecl struct {
		SchemeName   string              ` + "`json:\"scheme_name\"`" + `
		Kind         string              ` + "`json:\"kind\"`" + `
		Description  string              ` + "`json:\"description,omitempty\"`" + `
		In           string              ` + "`json:\"in,omitempty\"`" + `
		Name         string              ` + "`json:\"name,omitempty\"`" + `
		BearerFormat string              ` + "`json:\"bearer_format,omitempty\"`" + `
		Scopes       []securityScopeDecl ` + "`json:\"scopes,omitempty\"`" + `
		Flows        []securityFlowDecl  ` + "`json:\"flows,omitempty\"`" + `
	}

	securityReqDecl struct {
		Schemes       []string             ` + "`json:\"schemes\"`" + `
		SchemeDetails []securitySchemeDecl ` + "`json:\"scheme_details,omitempty\"`" + `
		Scopes        []string             ` + "`json:\"scopes,omitempty\"`" + `
	}

	errorDecl struct {
		Name        string     ` + "`json:\"name\"`" + `
		Description string     ` + "`json:\"description,omitempty\"`" + `
		Schema      schemaDecl ` + "`json:\"schema\"`" + `
		Temporary   bool       ` + "`json:\"temporary,omitempty\"`" + `
		Timeout     bool       ` + "`json:\"timeout,omitempty\"`" + `
		Fault       bool       ` + "`json:\"fault,omitempty\"`" + `
	}

	apiDecl struct {
		Name        string               ` + "`json:\"name\"`" + `
		Title       string               ` + "`json:\"title,omitempty\"`" + `
		Version     string               ` + "`json:\"version,omitempty\"`" + `
		Description string               ` + "`json:\"description,omitempty\"`" + `
		Schemes     []securitySchemeDecl ` + "`json:\"schemes,omitempty\"`" + `
	}

	typeDecl struct {
		Name        string     ` + "`json:\"name\"`" + `
		Description string     ` + "`json:\"description,omitempty\"`" + `
		Schema      schemaDecl ` + "`json:\"schema\"`" + `
	}

	serviceDecl struct {
		Name          string             ` + "`json:\"name\"`" + `
		Description   string             ` + "`json:\"description,omitempty\"`" + `
		Security      []securityReqDecl  ` + "`json:\"security,omitempty\"`" + `
		Errors        []errorDecl        ` + "`json:\"errors,omitempty\"`" + `
	}

	methodDecl struct {
		Name             string            ` + "`json:\"name\"`" + `
		Description      string            ` + "`json:\"description,omitempty\"`" + `
		Payload          schemaDecl        ` + "`json:\"payload\"`" + `
		StreamingPayload *schemaDecl       ` + "`json:\"streaming_payload,omitempty\"`" + `
		Result           schemaDecl        ` + "`json:\"result\"`" + `
		Security         []securityReqDecl ` + "`json:\"security,omitempty\"`" + `
		Errors           []errorDecl       ` + "`json:\"errors,omitempty\"`" + `
	}

	inputBindingDecl struct {
		Tool   string   ` + "`json:\"tool\"`" + `
		Fields []string ` + "`json:\"fields\"`" + `
	}

	fieldBindingDecl struct {
		Field  string      ` + "`json:\"field\"`" + `
		Lookup *schemaDecl ` + "`json:\"lookup,omitempty\"`" + `
	}

	toolsetDecl struct {
		Name          string             ` + "`json:\"name\"`" + `
		Description   string             ` + "`json:\"description,omitempty\"`" + `
		Deferred      bool               ` + "`json:\"deferred,omitempty\"`" + `
		DeferredTools []string           ` + "`json:\"deferred_tools,omitempty\"`" + `
		InputBindings []inputBindingDecl ` + "`json:\"input_bindings,omitempty\"`" + `
		FieldBindings []fieldBindingDecl ` + "`json:\"field_bindings,omitempty\"`" + `
	}

	agentDecl struct {
		Name        string      ` + "`json:\"name\"`" + `
		Description string      ` + "`json:\"description,omitempty\"`" + `
		Policy      agentPolicy ` + "`json:\"policy\"`" + `
	}

	suiteDecl struct {
		Name        string ` + "`json:\"name\"`" + `
		Description string ` + "`json:\"description,omitempty\"`" + `
		Timeout     string ` + "`json:\"timeout,omitempty\"`" + `
	}

	scenarioDecl struct {
		Name        string     ` + "`json:\"name\"`" + `
		Description string     ` + "`json:\"description,omitempty\"`" + `
		Tags        []string   ` + "`json:\"tags,omitempty\"`" + `
		Input       schemaDecl ` + "`json:\"input\"`" + `
	}

	modelRelDecl struct {
		Destination     string ` + "`json:\"destination\"`" + `
		Description     string ` + "`json:\"description,omitempty\"`" + `
		Technology      string ` + "`json:\"technology,omitempty\"`" + `
		InteractionKind int    ` + "`json:\"interaction_kind,omitempty\"`" + `
		Tags            string ` + "`json:\"tags,omitempty\"`" + `
	}

	modelElementDecl struct {
		Kind          string            ` + "`json:\"kind\"`" + `
		Name          string            ` + "`json:\"name\"`" + `
		Description   string            ` + "`json:\"description,omitempty\"`" + `
		Technology    string            ` + "`json:\"technology,omitempty\"`" + `
		Tags          string            ` + "`json:\"tags,omitempty\"`" + `
		URL           string            ` + "`json:\"url,omitempty\"`" + `
		Properties    map[string]string ` + "`json:\"properties,omitempty\"`" + `
		Relationships []modelRelDecl    ` + "`json:\"relationships,omitempty\"`" + `
	}
)

func main() {
	outPath := flag.String("out", "", "Output JSON path")
	flag.Parse(){{STORY_PRE_EVAL}}

	if err := eval.RunDSL(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	roots, err := eval.Context.Roots()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sources, err := generator.Sources({{GEN_IMPORT}}, roots)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	out := output{
		Declarations:  make(map[string]string),
		Containers:    make([]c4Container, 0),
		Edges:         make([]c4Edge, 0),
		AgentPolicies: make([]agentPolicy, 0),
		Tools:         make([]toolContract, 0),
	}

	seenContainers := make(map[string]bool)
	addContainer := func(c c4Container) {
		if seenContainers[c.ID] {
			return
		}
		seenContainers[c.ID] = true
		out.Containers = append(out.Containers, c)
	}
	seenEdges := make(map[string]bool)
	addEdge := func(e c4Edge) {
		k := e.FromID + "->" + e.ToID + ":" + e.Protocol
		if seenEdges[k] {
			return
		}
		seenEdges[k] = true
		out.Edges = append(out.Edges, e)
	}
	if err := recordGeneratedSources(&out, sources); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}{{STORY_RECORD_CALL}}

	api := goaexpr.Root.API
	for _, sch := range goaexpr.Root.Schemes {
		if sch != nil && sch.SchemeName != "" {
			out.Declarations["SecurityScheme:"+sch.SchemeName] = mustJSON(describeSecurityScheme(sch))
		}
	}
	if {{STORY_API_GUARD}}api != nil && api.Name != "" && api.Name != "api" {
		out.SystemName = api.Name
		decl := apiDecl{
			Name:        api.Name,
			Title:       api.Title,
			Version:     api.Version,
			Description: api.Description,
		}
		for _, sch := range goaexpr.Root.Schemes {
			if sch != nil {
				decl.Schemes = append(decl.Schemes, describeSecurityScheme(sch))
			}
		}
		out.Declarations["API:"+api.Name] = mustJSON(decl)
	}

	for _, ut := range goaexpr.Root.Types {
		if ut == nil {
			continue
		}
		att := ut.Attribute()
		var desc string
		if att != nil {
			desc = att.Description
		}
		out.Declarations["Type:"+ut.Name()] = mustJSON(typeDecl{
			Name:        ut.Name(),
			Description: desc,
			Schema:      describeSchema(api, att, "type."+ut.Name()),
		})
	}

	for _, svc := range goaexpr.Root.Services {
		if svc == nil {
			continue
		}
		if out.SystemName == "" {
			out.SystemName = svc.Name
		}
		svcID := "service:" + svc.Name
		desc := svc.Description
		if desc == "" {
			desc = "Goa service " + svc.Name
		}
		addContainer(c4Container{
			ID:          svcID,
			Name:        svc.Name,
			Kind:        "service",
			Description: desc,
		})

		svcDecl := serviceDecl{
			Name:        svc.Name,
			Description: svc.Description,
			Security:    describeSecurity(svc.Requirements),
			Errors:      describeErrors(api, svc.Errors),
		}
		out.Declarations["Service:"+svc.Name] = mustJSON(svcDecl)

		for _, m := range svc.Methods {
			if m == nil {
				continue
			}
			mDecl := methodDecl{
				Name:        m.Name,
				Description: m.Description,
				Payload:     describeSchema(api, m.Payload, "method.payload."+svc.Name+"."+m.Name),
				Result:      describeSchema(api, m.Result, "method.result."+svc.Name+"."+m.Name),
				Security:    describeSecurity(m.Requirements),
				Errors:      describeErrors(api, m.Errors),
			}
			if m.StreamingPayload != nil && m.StreamingPayload.Type != nil && m.StreamingPayload.Type != goaexpr.Empty {
				sp := describeSchema(api, m.StreamingPayload, "method.stream_payload."+svc.Name+"."+m.Name)
				mDecl.StreamingPayload = &sp
			}
			key := fmt.Sprintf("Service:%s/Method:%s", svc.Name, m.Name)
			out.Declarations[key] = mustJSON(mDecl)
		}

	}

	recordToolset := func(ts *agentexpr.ToolsetExpr, scopePrefix string) string {
		if ts == nil {
			return ""
		}
		origin := ts
		if ts.Origin != nil {
			origin = ts.Origin
		}
		tsID := "toolset:" + origin.Name
		desc := origin.Description
		if desc == "" {
			desc = "Goa-AI toolset " + origin.Name
		}
		addContainer(c4Container{
			ID:          tsID,
			Name:        origin.Name,
			Kind:        "toolset",
			Description: desc,
		})
		tsDeclKey := "Toolset:" + origin.Name
		if scopePrefix != "" {
			tsDeclKey = scopePrefix + "/Toolset:" + origin.Name
		}

		var inputBinds []inputBindingDecl
		for _, ib := range ts.InputBindings {
			if ib != nil {
				fields := slices.Clone(ib.Fields)
				slices.Sort(fields)
				inputBinds = append(inputBinds, inputBindingDecl{
					Tool:   ib.Tool,
					Fields: fields,
				})
			}
		}
		slices.SortFunc(inputBinds, func(a, b inputBindingDecl) int {
			if a.Tool != b.Tool {
				if a.Tool < b.Tool {
					return -1
				}
				return 1
			}
			return slices.Compare(a.Fields, b.Fields)
		})
		var fieldBinds []fieldBindingDecl
		for _, fb := range ts.FieldBindings {
			if fb != nil {
				item := fieldBindingDecl{Field: fb.Field}
				if fb.Lookup != nil {
					lk := describeSchema(api, fb.Lookup, "toolset.lookup."+origin.Name+"."+fb.Field)
					item.Lookup = &lk
				}
				fieldBinds = append(fieldBinds, item)
			}
		}
		slices.SortFunc(fieldBinds, func(a, b fieldBindingDecl) int {
			return strings.Compare(a.Field, b.Field)
		})
		deferredTools := slices.Clone(ts.DeferredTools)
		slices.Sort(deferredTools)

		out.Declarations[tsDeclKey] = mustJSON(toolsetDecl{
			Name:          origin.Name,
			Description:   origin.Description,
			Deferred:      ts.Deferred,
			DeferredTools: deferredTools,
			InputBindings: inputBinds,
			FieldBindings: fieldBinds,
		})

		for _, t := range origin.Tools {
			if t.Method != nil && t.Method.Service != nil {
				addEdge(c4Edge{FromID: tsID, ToID: "service:" + t.Method.Service.Name, Protocol: "Goa Method Binding"})
			}
		}

		return tsID
	}

	if agentexpr.Root != nil {
		for _, ts := range agentexpr.Root.Toolsets {
			recordToolset(ts, "")
		}
		for _, se := range agentexpr.Root.ServiceExports {
			if se == nil || se.Service == nil {
				continue
			}
			for _, ts := range se.Toolsets {
				tsID := recordToolset(ts, "Service:"+se.Service.Name)
				if tsID != "" {
					addEdge(c4Edge{
						FromID:   "service:" + se.Service.Name,
						ToID:     tsID,
						Protocol: "Goa-AI Service Export",
					})
				}
			}
		}
		for _, ag := range agentexpr.Root.Agents {
			if ag == nil || ag.Service == nil {
				continue
			}
			svcID := "service:" + ag.Service.Name
			agID := fmt.Sprintf("agent:%s.%s", ag.Service.Name, ag.Name)
			desc := ag.Description
			if desc == "" {
				desc = "Goa-AI agent " + ag.Name
			}
			addContainer(c4Container{
				ID:          agID,
				Name:        ag.Name,
				Kind:        "agent",
				Description: desc,
			})
			addEdge(c4Edge{
				FromID:   svcID,
				ToID:     agID,
				Protocol: "Goa-AI Agent Workflow",
			})

			pol := buildAgentPolicy(agID, ag.RunPolicy)
			out.AgentPolicies = append(out.AgentPolicies, pol)

			agDeclKey := fmt.Sprintf("Service:%s/Agent:%s", ag.Service.Name, ag.Name)
			out.Declarations[agDeclKey] = mustJSON(agentDecl{
				Name:        ag.Name,
				Description: ag.Description,
				Policy:      pol,
			})

			if ag.Exported != nil {
				for _, ts := range ag.Exported.Toolsets {
					tsID := recordToolset(ts, agDeclKey)
					if tsID != "" {
						addEdge(c4Edge{
							FromID:   tsID,
							ToID:     agID,
							Protocol: "Goa-AI AgentToolset",
						})
					}
				}
			}
			if ag.Used != nil {
				for _, ts := range ag.Used.Toolsets {
					tsID := recordToolset(ts, agDeclKey)
					if tsID != "" {
						addEdge(c4Edge{
							FromID:   agID,
							ToID:     tsID,
							Protocol: "Goa-AI Toolset",
						})
						if ts.Origin != nil && ts.Origin.Agent != nil && ts.Origin.Agent.Service != nil {
							providerAgID := fmt.Sprintf("agent:%s.%s", ts.Origin.Agent.Service.Name, ts.Origin.Agent.Name)
							addEdge(c4Edge{
								FromID:   tsID,
								ToID:     providerAgID,
								Protocol: "Goa-AI AgentToolset",
							})
						}
					}
				}
			}
		}
	}

	recordModelRoot(&out, addContainer, addEdge)

	if evalexpr.Root != nil {
		for _, suite := range evalexpr.Root.Suites {
			if suite == nil {
				continue
			}
			suiteKey := "Suite:" + suite.Name
			out.Declarations[suiteKey] = mustJSON(suiteDecl{
				Name:        suite.Name,
				Description: suite.Description,
				Timeout:     suite.Timeout.String(),
			})
			for _, sc := range suite.Scenarios {
				if sc == nil {
					continue
				}
				tags := slices.Clone(sc.Tags)
				slices.Sort(tags)
				out.Declarations[fmt.Sprintf("%s/Scenario:%s", suiteKey, sc.Name)] = mustJSON(scenarioDecl{
					Name:        sc.Name,
					Description: sc.Description,
					Tags:        tags,
					Input:       describeSchema(api, sc.Input, "scenario.input."+suite.Name+"."+sc.Name),
				})
			}
		}
	}

	rawOut, err := json.Marshal(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	if *outPath != "" {
		if err := os.WriteFile(*outPath, rawOut, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
		return
	}
	os.Stdout.Write(rawOut)
}
`
)

// storyReaderSource is story_reader.go itself, compiled as the evaluation
// program's story.go when the target design loads the portable Story DSL
// module.
//
//go:embed story_reader.go
var storyReaderSource string

// evaluateProgram returns the evaluator harness sources for the target's
// loaded modules, omitting Story extraction when the target does not load the
// portable Story DSL module.
func evaluateProgram(loaded map[string]bool) map[string]string {
	var field, preEval, call, apiGuard string
	if loaded[harnessStoryModule] {
		field = harnessStoryOutputField
		preEval = harnessStoryPreEval
		call = harnessStoryRecordCall
		apiGuard = harnessStoryAPIGuard
	}
	mainSrc := strings.ReplaceAll(harnessMainTemplate, "{{STORY_OUTPUT_FIELD}}", field)
	mainSrc = strings.ReplaceAll(mainSrc, "{{STORY_PRE_EVAL}}", preEval)
	mainSrc = strings.ReplaceAll(mainSrc, "{{STORY_RECORD_CALL}}", call)
	mainSrc = strings.ReplaceAll(mainSrc, "{{STORY_API_GUARD}}", apiGuard)
	files := map[string]string{"main.go": mainSrc}
	if loaded[harnessStoryModule] {
		_, reader, ok := strings.Cut(storyReaderSource, "\npackage designeval\n")
		if !ok {
			panic("story_reader.go has no package designeval clause")
		}
		files["story.go"] = "package main\n" + reader
	}
	return files
}
