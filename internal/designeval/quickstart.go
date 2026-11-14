// This file is compiled twice: as part of package designeval, where its tests
// run, and, with its package clause changed to main, as a source file of the
// generation program that Evaluator.Generate builds in its harness module.
// That module requires Goa, Goa-AI and goa.design/model at the target's
// versions, or the evaluator's when the target does not require one, so this
// file may import only the standard library, Goa and Goa-AI. It uses
// declaredSuites from suite_command.go, compiled beside it.
package designeval

import (
	"fmt"

	"goa.design/goa/v3/codegen"
	"goa.design/goa/v3/eval"
)

// quickstartPath is the root file Goa-AI's agent generator writes for coding
// agents, the only file generation writes outside gen/.
const quickstartPath = "AGENTS_QUICKSTART.md"

// surlaQuickstart is the "surla-quickstart" Goa plugin that runs after
// Goa-AI's generators. It replaces the sections of AGENTS_QUICKSTART.md that
// tell a reader which commands to run (header, liftoff and golden-rules) with
// quickstartSections' text, which names Surla's tools. It also replaces
// evaluations when the design declares no evaluation suite, because that
// section then only explains how to add one with goa gen and goa example;
// when suites exist, Goa-AI's section describing them stays. Every other
// section, and every fact about the design, is Goa-AI's.
//
// A guide without the agents-quickstart-header section comes from a Goa-AI
// release that renders the guide as one section, and is left as Goa-AI wrote
// it. A guide that has that section but lacks another one Surla replaces is
// an error, because Goa-AI's section contract changed.
func surlaQuickstart(_ string, roots []eval.Root, files []*codegen.File) ([]*codegen.File, error) {
	for _, f := range files {
		if f.Path != quickstartPath || len(f.Section("agents-quickstart-header")) == 0 {
			continue
		}
		names := []string{"header", "liftoff", "golden-rules"}
		if !declaresSuites(roots) {
			names = append(names, "evaluations")
		}
		for _, name := range names {
			sections := f.Section("agents-quickstart-" + name)
			if len(sections) != 1 {
				return nil, fmt.Errorf("%s has %d agents-quickstart-%s sections, want 1", quickstartPath, len(sections), name)
			}
			sections[0].Source = quickstartSections[name]
		}
	}
	return files, nil
}

// declaresSuites reports whether the design declares an evaluation suite, the
// fact Goa-AI's evaluations section branches on.
func declaresSuites(roots []eval.Root) bool {
	return len(declaredSuites(roots)) > 0
}
