// This file is compiled twice: as part of package designeval, where its unit
// tests run, and, with its package clause changed to main, as a source file of
// the generation program that Evaluator.GenerateWithStories builds when at
// least one complete Story flow is present. Because the harness module
// compiles against the target's Goa and Goa-AI versions, this file imports
// only Goa and Goa-AI packages.
package designeval

import (
	evalexpr "goa.design/goa-ai/eval/expr"
	"goa.design/goa/v3/eval"
)

type (
	// storyScenarioSpec holds the scenario name and description rendered into
	// the generation harness for one complete Story flow.
	storyScenarioSpec struct {
		// name is the lower_snake_case scenario identifier added to the suite.
		name string
		// description is the flow's title, or its key when the title is blank.
		description string
	}

	// storyScenarioRoot is the Goa evaluation root registered by the
	// generation harness when at least one complete Story flow is present.
	// It runs after evalexpr.Root, appends Story scenarios to the single
	// declared suite during Prepare before validation runs, and rejects
	// designs declaring zero or multiple suites during Validate.
	storyScenarioRoot struct {
		// scenarios lists the complete Story flows mapped on the host.
		scenarios []storyScenarioSpec
	}
)

// EvalName implements eval.Expression.
func (*storyScenarioRoot) EvalName() string {
	return "Story scenario adapter"
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

// Prepare attaches each complete Story flow to the single evaluation suite
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

// Validate requires the evaluator design to declare exactly one evaluation
// suite when complete Story flows are present.
func (r *storyScenarioRoot) Validate() error {
	verr := new(eval.ValidationErrors)
	switch {
	case evalexpr.Root == nil || len(evalexpr.Root.Suites) == 0:
		verr.Add(
			r,
			"Story scenario generation requires one evaluation suite, but the design declares none; declare a single Suite(...) in design/",
		)
		return verr
	case len(evalexpr.Root.Suites) > 1:
		verr.Add(
			r,
			"Story scenario generation requires a single evaluation suite, but the design declares %d suites; keep only one Suite(...) in design/",
			len(evalexpr.Root.Suites),
		)
		return verr
	default:
		return nil
	}
}
