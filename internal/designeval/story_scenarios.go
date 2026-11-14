// This file is compiled twice: as part of package designeval, where its unit
// tests run, and, after its package clause, as the body of every test
// module's Story table (StoryTests), compiled in the module's design package
// with the module's own Goa and Goa-AI. It therefore imports only Goa and
// Goa-AI packages, and declares nothing a design package could already
// declare beyond its two type names.
package designeval

import (
	evalexpr "goa.design/goa-ai/eval/expr"
	"goa.design/goa/v3/eval"
)

type (
	// storyScenarioSpec holds the scenario name and description of one
	// finished Story flow.
	storyScenarioSpec struct {
		// name is the lower_snake_case scenario identifier added to the suite.
		name string
		// description is the flow's title, or its key when the title is blank.
		description string
	}

	// storyScenarioRoot is the Goa evaluation root the Story table registers.
	// It runs after evalexpr.Root, appends the Story's scenarios to the
	// single declared suite during Prepare before validation runs, and
	// rejects designs declaring zero or several suites during Validate.
	storyScenarioRoot struct {
		// scenarios lists the finished Story flows, in declaration order.
		scenarios []storyScenarioSpec
	}
)

// EvalName implements eval.Expression. Surla's generator recognizes the Story
// table's root by this name.
func (*storyScenarioRoot) EvalName() string {
	return "Story table (design/story_tests.go)"
}

// DependsOn orders storyScenarioRoot after Goa-AI's evaluation suites root so
// Suite DSL blocks have executed before Prepare attaches Story scenarios.
func (*storyScenarioRoot) DependsOn() []eval.Root {
	return []eval.Root{evalexpr.Root}
}

// Packages implements eval.Root.
func (*storyScenarioRoot) Packages() []string {
	return nil
}

// WalkSets implements eval.Root. SuiteExpr walks the attached scenarios itself.
func (*storyScenarioRoot) WalkSets(eval.SetWalker) {}

// Prepare attaches each finished Story flow to the single evaluation suite
// before SuiteExpr.Validate and ScenarioExpr.Validate run.
func (r *storyScenarioRoot) Prepare() {
	if evalexpr.Root == nil || len(evalexpr.Root.Suites) != 1 {
		return
	}
	suite := evalexpr.Root.Suites[0]
	for _, sc := range r.scenarios {
		suite.Scenarios = append(suite.Scenarios, &evalexpr.ScenarioExpr{
			Name:        sc.name,
			Description: sc.description,
			Suite:       suite,
		})
	}
}

// Validate requires the test module's design to declare exactly one
// evaluation suite, the one the Story's scenarios join.
func (r *storyScenarioRoot) Validate() error {
	var suites int
	if evalexpr.Root != nil {
		suites = len(evalexpr.Root.Suites)
	}
	if suites == 1 {
		return nil
	}
	verr := new(eval.ValidationErrors)
	verr.Add(
		r,
		"the test module must declare exactly one evaluation suite, but its design declares %d: keep exactly one Suite(...) in design/",
		suites,
	)
	return verr
}
