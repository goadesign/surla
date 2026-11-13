// This file is compiled twice: as part of package designeval, where its tests
// run, and, with its package clause changed to main, as a source file of the
// generation program that Evaluator.Generate builds in its harness module.
// That module requires Goa and Goa-AI at the target's versions, so this file
// may import only the standard library, Goa and Goa-AI, and uses
// test_layout.go, compiled beside it.
package designeval

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	evalexpr "goa.design/goa-ai/eval/expr"
	"goa.design/goa/v3/codegen"
	"goa.design/goa/v3/eval"
)

// suiteCommandEnvironment is the dependency profile every run of a generated
// suite command declares: Surla's test layout runs the program under test
// with a scripted model.
const suiteCommandEnvironment = "scripted-model"

// suiteCommandSource is the body of the generated suite command, after Goa's
// header and imports. It is executed as a Go template with suiteCommandData.
// It builds the module's one evaluation suite from the generated tests
// (genevals) and the author's package (acceptance), calling exactly
// acceptance.Name, acceptance.Hooks, acceptance.New, acceptance.Live and
// acceptance.LiveThroughGateway, and hands it to Surla's suite command
// contract (verify/suite.Main).
const suiteCommandSource = `// environment is the dependency profile every run of this suite declares.
const environment = {{ printf "%q" .Environment }}

func main() {
	suite.Main(suite.Command{
		Environment:    environment,
		Binaries:       []string{acceptance.Name},
		MaxConcurrency: 2,
		Declare: func() (eval.Suite, error) {
			return genevals.New(&acceptance.Hooks{}, genevals.Inputs{})
		},
		Suite: func(pinned map[string]candidate.Artifact) (eval.Suite, error) {
			return genevals.New(acceptance.New(pinned[acceptance.Name]), genevals.Inputs{})
		},
		Live: func(ctx context.Context, pinned map[string]candidate.Artifact, key string) (suite.LiveTarget, error) {
			return live(ctx, pinned[acceptance.Name], key, os.Getenv)
		},
	})
}

// live starts the {{ .Service }} for a live copy with the project server's
// selected model gateway or direct provider and model. Provider keys stay in
// this process.
func live(ctx context.Context, binary candidate.Artifact, key string, getenv func(string) string) (suite.LiveTarget, error) {
	gatewayAddr := getenv("SURLA_LIVE_MODEL_GATEWAY")
	providerName, modelName := getenv("SURLA_LIVE_MODEL_PROVIDER"), getenv("SURLA_LIVE_MODEL_NAME")
	switch {
	case gatewayAddr != "" && (providerName != "" || modelName != ""):
		return suite.LiveTarget{}, errors.New("the live {{ .Service }} got both a model gateway and a model provider; the Surla project server passes only one")
	case gatewayAddr != "":
		return acceptance.LiveThroughGateway(ctx, binary, key, gatewayAddr)
	case providerName == "" && modelName == "":
		return suite.LiveTarget{}, errors.New("no model for the live {{ .Service }}: ask the user to configure a model in Surla")
	case providerName == "" || modelName == "":
		return suite.LiveTarget{}, errors.New("the live {{ .Service }} needs both a model provider and a model name; ask the user to configure a provider and model in Surla")
	}
	provider, err := liveProvider(providerName, modelName, getenv)
	if err != nil {
		return suite.LiveTarget{}, err
	}
	return acceptance.Live(ctx, binary, key, provider)
}

// liveProvider builds the selected provider from its inherited API key.
func liveProvider(providerName, modelName string, getenv func(string) string) (model.Provider, error) {
	switch providerName {
	case "anthropic":
		return anthropic.NewProviderFromAPIKey(getenv("ANTHROPIC_API_KEY"), modelName)
	case "openai":
		return openai.NewProviderFromAPIKey(getenv("OPENAI_API_KEY"), modelName)
	default:
		return nil, fmt.Errorf("unsupported model provider %q for the live {{ .Service }}", providerName)
	}
}
`

// suiteCommandData is the data suiteCommandSource and the quickstart's
// test-program section are executed with.
type suiteCommandData struct {
	// Service is the service the test module tests, such as shop.
	Service string
	// Suite is the name of the module's evaluation suite, such as
	// shop_acceptance.
	Suite string
	// Command is the generated program's package relative to the module,
	// such as ./gen/cmd/shop-evals.
	Command string
	// Environment is suiteCommandEnvironment.
	Environment string
}

// surlaSuiteCommand is the "surla-suite-command" Goa plugin that runs after
// Goa-AI's generators. For a Surla test module (TestModuleService accepts the
// module path, which genpkg, the module's gen import path, ends) whose design
// declares exactly one evaluation suite, it adds the module's program at
// gen/cmd/<service>-evals/main.go (SuiteCommandPackage) and the quickstart's
// test-program section, which names what the author's acceptance package must
// provide: to AGENTS_QUICKSTART.md when Goa-AI wrote it, or as that file's
// only section otherwise. Any other module is left as it is. Goa-AI must have
// planned the suite's package at gen/evals/<suite>/suite.go; if it did not,
// generation fails naming that file rather than writing a program that cannot
// build.
func surlaSuiteCommand(genpkg string, roots []eval.Root, files []*codegen.File) ([]*codegen.File, error) {
	service, ok := TestModuleService(strings.TrimSuffix(genpkg, "/"+codegen.Gendir))
	suites := declaredSuites(roots)
	if !ok || len(suites) != 1 {
		return files, nil
	}
	suiteFile := path.Join(codegen.Gendir, "evals", suites[0].Name, "suite.go")
	if !slices.ContainsFunc(files, func(f *codegen.File) bool { return filepath.ToSlash(f.Path) == suiteFile }) {
		return nil, fmt.Errorf("the evaluation suite %q has no generated package at %s, from which Surla builds the test program", suites[0].Name, suiteFile)
	}
	data := suiteCommandData{
		Service:     service,
		Suite:       suites[0].Name,
		Command:     SuiteCommandPackage(service),
		Environment: suiteCommandEnvironment,
	}
	imports := []*codegen.ImportSpec{
		codegen.SimpleImport("context"),
		codegen.SimpleImport("errors"),
		codegen.SimpleImport("fmt"),
		codegen.SimpleImport("os"),
		codegen.SimpleImport("goa.design/goa-ai/eval"),
		codegen.SimpleImport("goa.design/goa-ai/features/model/anthropic"),
		codegen.SimpleImport("goa.design/goa-ai/features/model/openai"),
		codegen.SimpleImport("goa.design/goa-ai/runtime/agent/model"),
		codegen.SimpleImport("github.com/goadesign/surla/verify/candidate"),
		codegen.SimpleImport("github.com/goadesign/surla/verify/suite"),
		codegen.SimpleImport(path.Join(path.Dir(genpkg), "acceptance")),
		codegen.NewImport("genevals", path.Join(genpkg, "evals", suites[0].Name)),
	}
	files = append(files, &codegen.File{
		Path: path.Join(strings.TrimPrefix(data.Command, "./"), "main.go"),
		SectionTemplates: []*codegen.SectionTemplate{
			codegen.Header(service+"-evals: the test program Surla builds from the generated tests", "main", imports),
			{Name: "surla-suite-command", Source: suiteCommandSource, Data: data},
		},
	})
	section := &codegen.SectionTemplate{Name: "surla-test-program", Source: testProgramSection, Data: data}
	for _, f := range files {
		if f.Path == quickstartPath {
			f.SectionTemplates = append(f.SectionTemplates, section)
			return files, nil
		}
	}
	return append(files, &codegen.File{Path: quickstartPath, SectionTemplates: []*codegen.SectionTemplate{section}}), nil
}

// declaredSuites returns the evaluation suites the design declares, in
// declaration order.
func declaredSuites(roots []eval.Root) []*evalexpr.SuiteExpr {
	for _, r := range roots {
		if root, ok := r.(*evalexpr.RootExpr); ok {
			return root.Suites
		}
	}
	return nil
}
