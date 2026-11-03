// This file holds the standalone generation program that Evaluator.Generate
// compiles and runs inside verify/candidate.Exec's sandbox. It is a separate
// program from the evaluation harness, so evaluation never refers to
// generator.CompleteSources and keeps working with every Goa version it
// supports today.
package designeval

// generateMainTemplate is the generation program. It evaluates the design,
// asks Goa for the complete gen output with generator.CompleteSources, adds
// the gen/goa.json version file the gen command writes, and saves every path
// and its bytes as JSON to the -sources file. It is run with a literal
// --cmd=<text> argument, which Goa's codegen.CommandLine copies into each
// generated file's header; the flag is registered only so parsing accepts it.
// {{DESIGN_IMPORT}} and {{GEN_IMPORT}} are replaced with the target module's
// design package and generated package import paths.
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
