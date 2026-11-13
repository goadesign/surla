// This file loads the quickstart section texts for package designeval. The
// generation program instead receives them as Go string literals from
// generateProgram, because it is compiled in the harness module, where this
// package's embedded files are not available.
package designeval

import (
	"embed"
	"path"
	"strings"
)

var (
	// quickstartFS holds Surla's replacement for each AGENTS_QUICKSTART.md
	// section surlaQuickstart may replace, one Markdown file per section name.
	//
	//go:embed quickstart/*.md
	quickstartFS embed.FS

	// quickstartSections maps a Goa-AI quickstart section name, without its
	// agents-quickstart- prefix, to Surla's replacement text.
	quickstartSections = mustQuickstartSections()

	// quickstartSource is quickstart.go itself, compiled again inside the
	// generation program.
	//
	//go:embed quickstart.go
	quickstartSource string

	// storyScenariosSource is story_scenarios.go itself, compiled inside the
	// generation program when complete Story flows are present.
	//
	//go:embed story_scenarios.go
	storyScenariosSource string

	// testProgramSection is the section surlaSuiteCommand adds to a Surla
	// test module's AGENTS_QUICKSTART.md: a Go template, executed with
	// suiteCommandData, naming what the author's acceptance package must
	// provide. Unlike quickstartSections it replaces no Goa-AI section.
	//
	//go:embed test_program.md
	testProgramSection string

	// testLayoutSource and suiteCommandSourceFile are test_layout.go and
	// suite_command.go themselves, compiled again inside the generation
	// program.
	//
	//go:embed test_layout.go
	testLayoutSource string
	//go:embed suite_command.go
	suiteCommandSourceFile string
)

// mustQuickstartSections reads every embedded section text. The embed
// pattern guarantees the directory exists, so a read error is a build defect.
func mustQuickstartSections() map[string]string {
	entries, err := quickstartFS.ReadDir("quickstart")
	if err != nil {
		panic(err)
	}
	sections := make(map[string]string, len(entries))
	for _, e := range entries {
		b, err := quickstartFS.ReadFile(path.Join("quickstart", e.Name()))
		if err != nil {
			panic(err)
		}
		sections[strings.TrimSuffix(e.Name(), ".md")] = string(b)
	}
	return sections
}
