// Package expr defines domain Stories evaluated by Goa's DSL engine. It has
// no dependency on an architecture, a test generator, or the Surla runtime.
package expr

import (
	"fmt"
	"regexp"
	"strings"

	"goa.design/goa/v3/eval"
)

type (
	// RootExpr contains the Stories loaded by a Go design package.
	RootExpr struct {
		// Stories preserves declaration order.
		Stories []*StoryExpr
	}

	// StoryExpr describes a business capability and its participating roles.
	StoryExpr struct {
		eval.DSLFunc
		Name        string
		Description string
		// Actors and Flows preserve declaration order within their own scopes.
		Actors []*ActorExpr
		Flows  []*FlowExpr
	}

	// ActorExpr names a domain role, without prescribing its implementation.
	ActorExpr struct {
		eval.DSLFunc
		Key         string
		Title       string
		Description string
	}

	// FlowExpr describes an ordered path through a Story. Incomplete paths are
	// valid drafts; Complete derives whether the path is ready for testing.
	FlowExpr struct {
		eval.DSLFunc
		Key         string
		Title       string
		Description string
		// Story owns the actor and flow key scopes.
		Story *StoryExpr
		// Steps preserves the declared order of starts, actions, and outcomes.
		Steps []*StepExpr
		// Questions are flow-level decisions, in declaration order.
		Questions []*QuestionExpr
	}

	// StepKind identifies a step's role in its flow, not a runtime operation.
	StepKind string

	// StepExpr describes one actor's action or observed business outcome.
	// Key remains unchanged when Summary or declaration order changes.
	StepExpr struct {
		eval.DSLFunc
		Key         string
		ActorKey    string
		Summary     string
		Description string
		Kind        StepKind
		// Questions share the owning flow's question key scope.
		Questions []*QuestionExpr
	}

	// QuestionExpr records an unresolved decision attached to a flow or step.
	// Options are suggestions, not answers; the question remains open until
	// its declaration is removed when the Story is refined.
	QuestionExpr struct {
		eval.DSLFunc
		Key         string
		Prompt      string
		Description string
		Options     []string
	}
)

const (
	// Start marks the step that initiates a flow.
	Start StepKind = "start"
	// Action marks an intermediate business action.
	Action StepKind = "step"
	// Outcome marks an observable result of the flow.
	Outcome StepKind = "outcome"
)

var (
	// Root is registered with Goa at package initialization. Like Goa's own
	// DSL roots, it belongs to one process-wide, non-concurrent evaluation.
	Root = new(RootExpr)
	// keyPattern accepts raw stable keys without rewriting their spelling.
	keyPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[-_][a-z0-9]+)*$`)
)

func init() {
	if err := eval.Register(Root); err != nil {
		panic(err)
	}
}

// EvalName identifies the portable Story root in Goa diagnostics.
func (*RootExpr) EvalName() string { return "Stories" }

// DependsOn returns no roots: a Story requires no architecture declarations.
func (*RootExpr) DependsOn() []eval.Root { return nil }

// Packages identifies the syntax and expression packages for diagnostics.
func (*RootExpr) Packages() []string {
	return []string{"github.com/goadesign/surla/story/dsl", "github.com/goadesign/surla/story/expr"}
}

// WalkSets exposes parents before children in every Goa evaluation phase.
func (r *RootExpr) WalkSets(walk eval.SetWalker) {
	walk(eval.ToExpressionSet(r.Stories))
	for _, story := range r.Stories {
		walk(eval.ToExpressionSet(story.Actors))
		walk(eval.ToExpressionSet(story.Flows))
		for _, flow := range story.Flows {
			walk(eval.ToExpressionSet(flow.Steps))
			walk(eval.ToExpressionSet(flow.Questions))
			for _, step := range flow.Steps {
				walk(eval.ToExpressionSet(step.Questions))
			}
		}
	}
}

// EvalName identifies a Story in Goa diagnostics.
func (s *StoryExpr) EvalName() string { return fmt.Sprintf("Story %q", s.Name) }

// Validate checks the raw keys owned by the Story, independently of future
// evaluator names or generated Go symbols.
func (s *StoryExpr) Validate() error {
	var errors eval.ValidationErrors
	actors := make(map[string]struct{}, len(s.Actors))
	for _, actor := range s.Actors {
		validateKey(&errors, actor, actor.Key, actors)
	}
	flows := make(map[string]struct{}, len(s.Flows))
	for _, flow := range s.Flows {
		validateKey(&errors, flow, flow.Key, flows)
	}
	return &errors
}

// EvalName identifies an actor without interpreting its domain meaning.
func (a *ActorExpr) EvalName() string { return fmt.Sprintf("actor %q", a.Key) }

// EvalName identifies a flow and its owning Story.
func (f *FlowExpr) EvalName() string {
	return fmt.Sprintf("flow %q in %s", f.Key, f.Story.EvalName())
}

// Complete reports whether the flow has a start, at least one outcome, and
// no open questions, including questions attached to steps. Call it after
// successful Goa evaluation; it does not replace mechanical validation.
func (f *FlowExpr) Complete() bool {
	if len(f.Questions) > 0 {
		return false
	}
	var start, outcome bool
	for _, step := range f.Steps {
		if len(step.Questions) > 0 {
			return false
		}
		start = start || step.Kind == Start
		outcome = outcome || step.Kind == Outcome
	}
	return start && outcome
}

// Validate checks step and question keys, actor references, and the single
// start invariant. Missing starts, missing outcomes, and open questions are
// unfinished content, not validation errors.
func (f *FlowExpr) Validate() error {
	var errors eval.ValidationErrors
	actors := make(map[string]struct{}, len(f.Story.Actors))
	for _, actor := range f.Story.Actors {
		actors[actor.Key] = struct{}{}
	}
	steps := make(map[string]struct{}, len(f.Steps))
	questions := make(map[string]struct{})
	starts := 0
	for _, step := range f.Steps {
		validateKey(&errors, step, step.Key, steps)
		if _, ok := actors[step.ActorKey]; !ok {
			errors.Add(step, "actor %q is not declared in %s", step.ActorKey, f.Story.EvalName())
		}
		if step.Kind == Start {
			starts++
			if starts > 1 {
				errors.Add(step, "flow %q has more than one starting step", f.Key)
			}
		}
		for _, question := range step.Questions {
			validateKey(&errors, question, question.Key, questions)
		}
	}
	for _, question := range f.Questions {
		validateKey(&errors, question, question.Key, questions)
	}
	return &errors
}

// EvalName identifies a step by its preserved domain key.
func (s *StepExpr) EvalName() string { return fmt.Sprintf("%s %q", s.Kind, s.Key) }

// EvalName identifies an unresolved question by its preserved key.
func (q *QuestionExpr) EvalName() string { return fmt.Sprintf("question %q", q.Key) }

// Validate rejects empty and repeated option labels. A question may have no
// suggested options while its author is still exploring the decision.
func (q *QuestionExpr) Validate() error {
	var errors eval.ValidationErrors
	options := make(map[string]struct{}, len(q.Options))
	for _, option := range q.Options {
		if strings.TrimSpace(option) == "" {
			errors.Add(q, "question option must not be empty")
		}
		if _, ok := options[option]; ok {
			errors.Add(q, "duplicate question option %q", option)
		}
		options[option] = struct{}{}
	}
	return &errors
}

// validateKey enforces spelling and uniqueness in one domain-owned scope.
func validateKey(errors *eval.ValidationErrors, expression eval.Expression,
	key string, seen map[string]struct{},
) {
	if !keyPattern.MatchString(key) {
		errors.Add(expression, "invalid key %q: must match %s", key, keyPattern.String())
	}
	if _, ok := seen[key]; ok {
		errors.Add(expression, "duplicate key %q", key)
	}
	seen[key] = struct{}{}
}
