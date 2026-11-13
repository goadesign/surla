// This file holds the standalone generation program that Evaluator.Generate
// compiles and runs inside verify/candidate.Exec's sandbox. It is a separate
// program from the evaluation harness, so evaluation never refers to
// generator.CompleteSources and keeps working with every Goa version it
// supports today.
package designeval

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// generateMainTemplate is the generation program. It evaluates the design,
// asks Goa for the complete gen output with generator.CompleteSources, adds
// the gen/goa.json version file the gen command writes, and saves every path
// and its bytes as JSON to the -sources file. It is run with a literal
// --cmd=<text> argument, which Goa's codegen.CommandLine copies into each
// generated file's header; the flag is registered only so parsing accepts it.
// {{DESIGN_IMPORT}} and {{GEN_IMPORT}} are replaced with the target module's
// design package and generated package import paths, and
// {{ACCEPTANCE_PACKAGE}} with whether the module has an acceptance package
// (HasAcceptancePackage). Before generating, it registers surlaQuickstart and
// then surlaSuiteCommand, which generateProgram compiles alongside it.
const generateMainTemplate = `package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"

	_ {{DESIGN_IMPORT}}
	_ "goa.design/goa-ai/codegen/agent"
	_ "goa.design/goa-ai/codegen/mcp"
	"goa.design/goa/v3/codegen"
	"goa.design/goa/v3/codegen/generator"
	"goa.design/goa/v3/eval"
)

type generatedFile struct {
	Path  string ` + "`json:\"path\"`" + `
	Bytes []byte ` + "`json:\"bytes\"`" + `
}

func main() {
	sourcesPath := flag.String("sources", "", "Output JSON path")
	flag.String("cmd", "", "Command line recorded in generated file headers")
	flag.Parse()

	codegen.RegisterPluginLast("surla-quickstart", "gen", nil, surlaQuickstart)
	codegen.RegisterPluginLast("surla-suite-command", "gen", nil, surlaSuiteCommand({{ACCEPTANCE_PACKAGE}}))
	if err := eval.RunDSL(); err != nil {
		fail(err)
	}
	roots, err := eval.Context.Roots()
	if err != nil {
		fail(err)
	}
	sources, err := generator.CompleteSources({{GEN_IMPORT}}, roots)
	if err != nil {
		fail(err)
	}
	version := codegen.VersionFile()
	if _, ok := sources[version.Path]; ok {
		fail(fmt.Errorf("a generator already writes %s, which the gen command writes its version to", version.Path))
	}
	content, err := version.Source()
	if err != nil {
		fail(err)
	}
	sources[version.Path] = content

	files := make([]generatedFile, 0, len(sources))
	for _, path := range slices.Sorted(maps.Keys(sources)) {
		files = append(files, generatedFile{Path: path, Bytes: sources[path]})
	}
	data, err := json.Marshal(files)
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(*sourcesPath, data, 0o600); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
`

// generateProgram returns the generation program's files: main.go from
// generateMainTemplate; quickstart.go, suite_command.go and test_layout.go
// (the surlaQuickstart and surlaSuiteCommand plugins and the layout rule they
// use, with their package clauses changed to main); and
// quickstart_sections.go, which declares quickstartSections as Go string
// literals, with testProgramSection, because the program cannot read this
// package's embedded files.
func generateProgram(acceptance bool) map[string]string {
	return generateProgramWithStories(nil, acceptance)
}

// generateProgramWithStories returns the generation program's files for a
// module that has an acceptance package or not, adding the Story scenario
// evaluation root and its quoted scenario table when at least one complete
// Story flow is present. When scenarios is empty, the returned program is
// identical to generateProgram.
func generateProgramWithStories(scenarios []storyScenarioSpec, acceptance bool) map[string]string {
	asMain := func(name, source string) string {
		_, body, ok := strings.Cut(source, "\npackage designeval\n")
		if !ok {
			panic(name + " has no package designeval clause")
		}
		return "package main\n" + body
	}
	var sections strings.Builder
	sections.WriteString("package main\n\nvar quickstartSections = map[string]string{\n")
	for _, name := range slices.Sorted(maps.Keys(quickstartSections)) {
		fmt.Fprintf(&sections, "\t%s: %s,\n", strconv.Quote(name), strconv.Quote(quickstartSections[name]))
	}
	sections.WriteString("}\n\n")
	fmt.Fprintf(&sections, "var testProgramSection = %s\n", strconv.Quote(testProgramSection))
	files := map[string]string{
		"main.go":                strings.Replace(generateMainTemplate, "{{ACCEPTANCE_PACKAGE}}", strconv.FormatBool(acceptance), 1),
		"quickstart.go":          asMain("quickstart.go", quickstartSource),
		"suite_command.go":       asMain("suite_command.go", suiteCommandSourceFile),
		"test_layout.go":         asMain("test_layout.go", testLayoutSource),
		"quickstart_sections.go": sections.String(),
	}
	if len(scenarios) == 0 {
		return files
	}

	var data strings.Builder
	data.WriteString("package main\n\nimport (\n\t_ \"goa.design/goa-ai/eval/codegen\"\n\t\"goa.design/goa/v3/eval\"\n)\n\nvar storyScenarios = []storyScenarioSpec{\n")
	for _, sc := range scenarios {
		fmt.Fprintf(&data, "\t{name: %s, description: %s},\n", strconv.Quote(sc.name), strconv.Quote(sc.description))
	}
	data.WriteString("}\n\nfunc init() {\n\tif err := eval.Register(&storyScenarioRoot{scenarios: storyScenarios}); err != nil {\n\t\tpanic(err)\n\t}\n}\n")
	files["story_scenarios.go"] = asMain("story_scenarios.go", storyScenariosSource)
	files["story_scenarios_data.go"] = data.String()
	return files
}
