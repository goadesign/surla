// Package dsl declares domain Stories as ordinary Go source. Goa evaluates
// the declarations into the portable values in story/expr.
package dsl

import (
	"github.com/goadesign/surla/story/expr"
	"goa.design/goa/v3/eval"
)

// Story declares a named business capability at the design's top level.
// Its function declares Actors, Flows, and an optional Description.
func Story(name string, fn func()) *expr.StoryExpr {
	if eval.Current() != eval.Top {
		eval.ReportError("invalid use of Story: must appear at the top level")
		return nil
	}
	story := &expr.StoryExpr{Name: name, DSLFunc: fn}
	expr.Root.Stories = append(expr.Root.Stories, story)
	return story
}

// Actor declares a business role in a Story. Key is its stable identity;
// title is editable wording. The optional function accepts Description.
func Actor(key, title string, fn ...func()) *expr.ActorExpr {
	story, ok := eval.Current().(*expr.StoryExpr)
	if !ok {
		eval.ReportError("invalid use of Actor: must appear in Story")
		return nil
	}
	actor := &expr.ActorExpr{Key: key, Title: title, DSLFunc: optionalDSL(fn)}
	story.Actors = append(story.Actors, actor)
	return actor
}

// Flow declares an ordered business path in a Story. Its function accepts
// StartsWith, Step, Outcome, Question, and Description. Unfinished flows are
// valid drafts and remain visible alongside finished flows.
func Flow(key, title string, fn func()) *expr.FlowExpr {
	story, ok := eval.Current().(*expr.StoryExpr)
	if !ok {
		eval.ReportError("invalid use of Flow: must appear in Story")
		return nil
	}
	flow := &expr.FlowExpr{Key: key, Title: title, Story: story, DSLFunc: fn}
	story.Flows = append(story.Flows, flow)
	return flow
}

// StartsWith declares a flow's single starting step. ActorKey names an Actor
// in the same Story. The optional function accepts Description and Question.
func StartsWith(key, actorKey, summary string, fn ...func()) *expr.StepExpr {
	return step(expr.Start, key, actorKey, summary, fn)
}

// Step declares an intermediate business action in a Flow. Its stable key
// survives summary and order edits. The optional function accepts Description
// and Question; actorKey names an Actor in the same Story.
func Step(key, actorKey, summary string, fn ...func()) *expr.StepExpr {
	return step(expr.Action, key, actorKey, summary, fn)
}

// Outcome declares an observable result in a Flow. A finished flow requires
// at least one outcome. The optional function accepts Description and Question;
// actorKey names an Actor in the same Story.
func Outcome(key, actorKey, summary string, fn ...func()) *expr.StepExpr {
	return step(expr.Outcome, key, actorKey, summary, fn)
}

// Question declares an unresolved decision in a Flow or any of its steps.
// Keys are unique across all questions in that flow. The optional function
// accepts suggested Options and Description. Remove the question after its
// decision has been incorporated into the Story; options do not resolve it.
func Question(key, prompt string, fn ...func()) *expr.QuestionExpr {
	question := &expr.QuestionExpr{Key: key, Prompt: prompt}
	switch current := eval.Current().(type) {
	case *expr.FlowExpr:
		current.Questions = append(current.Questions, question)
	case *expr.StepExpr:
		current.Questions = append(current.Questions, question)
	default:
		eval.ReportError("invalid use of Question: must appear in Flow or a step")
		return nil
	}
	question.DSLFunc = optionalDSL(fn)
	return question
}

// Option adds a distinct, nonempty suggested answer inside Question. Labels
// are preserved verbatim and do not select an answer or finish the flow.
func Option(label string) {
	question, ok := eval.Current().(*expr.QuestionExpr)
	if !ok {
		eval.ReportError("invalid use of Option: must appear in Question")
		return
	}
	question.Options = append(question.Options, label)
}

// Description adds explanatory text inside Story, Actor, Flow, any step, or
// Question. It does not change keys or a flow's completion status.
func Description(text string) {
	switch current := eval.Current().(type) {
	case *expr.StoryExpr:
		current.Description = text
	case *expr.ActorExpr:
		current.Description = text
	case *expr.FlowExpr:
		current.Description = text
	case *expr.StepExpr:
		current.Description = text
	case *expr.QuestionExpr:
		current.Description = text
	default:
		eval.ReportError("invalid use of Description: must appear in a Story declaration")
	}
}

// step preserves declaration order for all three business step kinds.
func step(kind expr.StepKind, key, actorKey, summary string, fn []func()) *expr.StepExpr {
	flow, ok := eval.Current().(*expr.FlowExpr)
	if !ok {
		eval.ReportError("invalid use of %s: must appear in Flow", kind)
		return nil
	}
	step := &expr.StepExpr{
		Key: key, ActorKey: actorKey, Summary: summary, Kind: kind, DSLFunc: optionalDSL(fn),
	}
	flow.Steps = append(flow.Steps, step)
	return step
}

// optionalDSL permits zero or one customization function, as documented by
// declarations with optional nested DSL.
func optionalDSL(fn []func()) eval.DSLFunc {
	if len(fn) > 1 {
		eval.ReportError("too many arguments: expected at most one nested DSL function")
		return nil
	}
	if len(fn) == 0 {
		return nil
	}
	return fn[0]
}
