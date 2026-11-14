// This file is compiled twice: as part of package designeval, against the
// Story module version Surla's go.mod requires, where its unit tests run it,
// and, with its package clause changed to main, as story.go of the evaluation
// program Evaluator.Evaluate builds when the target design loads the portable
// Story DSL module, against the Story version the target selects. It imports
// only the Story module and the standard library.

package designeval

import (
	"slices"

	storyexpr "github.com/goadesign/surla/story/expr"
)

type (
	// storySummary is one evaluated Story as the evaluation program prints it;
	// the host decodes it as a StorySummary.
	storySummary struct {
		// Name is the Story's human-facing title.
		Name string `json:"name"`
		// Description is the Story's optional longer explanation.
		Description string `json:"description,omitempty"`
		// Actors lists the Story's roles in declaration order.
		Actors []storyActor `json:"actors,omitempty"`
		// Flows lists the Story's timeline paths in declaration order.
		Flows []storyFlow `json:"flows,omitempty"`
	}

	// storyActor is one role of a Story; the host decodes it as a StoryActor.
	storyActor struct {
		// Key is the Story-scoped actor key.
		Key string `json:"key"`
		// Title is the actor's human-facing label.
		Title string `json:"title"`
		// Description is the actor's optional longer explanation.
		Description string `json:"description,omitempty"`
	}

	// storyFlow is one timeline path of a Story; the host decodes it as a
	// StoryFlow.
	storyFlow struct {
		// Key is the Story-scoped flow key.
		Key string `json:"key"`
		// Title is the flow's human-facing title.
		Title string `json:"title"`
		// Description is the flow's optional longer explanation.
		Description string `json:"description,omitempty"`
		// Complete is the Story module's own verdict: a start, an outcome and
		// no open question.
		Complete bool `json:"complete"`
		// Steps lists the flow's steps in declaration order.
		Steps []storyStep `json:"steps,omitempty"`
		// Questions lists the flow's open questions in declaration order.
		Questions []storyQuestion `json:"questions,omitempty"`
	}

	// storyStep is one step of a flow; the host decodes it as a StoryStep.
	storyStep struct {
		// Key is the flow-scoped step key.
		Key string `json:"key"`
		// ActorKey is the key of the actor performing or observing the step.
		ActorKey string `json:"actor_key"`
		// Summary is the step's timeline sentence.
		Summary string `json:"summary"`
		// Description is the step's optional longer explanation.
		Description string `json:"description,omitempty"`
		// Kind is "start", "step" or "outcome".
		Kind string `json:"kind"`
		// Questions lists the step's open questions in declaration order.
		Questions []storyQuestion `json:"questions,omitempty"`
	}

	// storyQuestion is one open question of a flow or step; the host decodes
	// it as a StoryQuestion.
	storyQuestion struct {
		// Key is the flow-scoped question key.
		Key string `json:"key"`
		// Prompt is the question's human-facing text.
		Prompt string `json:"prompt"`
		// Description is the question's optional longer context.
		Description string `json:"description,omitempty"`
		// Options lists the suggested choices in declaration order.
		Options []string `json:"options,omitempty"`
	}
)

// storySummaries returns the Stories the evaluated design declared, in
// declaration order, or none when the Story module recorded no root. The
// evaluation program calls it after the design's DSL ran.
func storySummaries() []storySummary {
	if storyexpr.Root == nil {
		return nil
	}
	var stories []storySummary
	for _, st := range storyexpr.Root.Stories {
		if st == nil {
			continue
		}
		summary := storySummary{
			Name:        st.Name,
			Description: st.Description,
		}
		for _, actor := range st.Actors {
			if actor == nil {
				continue
			}
			summary.Actors = append(summary.Actors, storyActor{
				Key:         actor.Key,
				Title:       actor.Title,
				Description: actor.Description,
			})
		}
		for _, flow := range st.Flows {
			if flow == nil {
				continue
			}
			flowSummary := storyFlow{
				Key:         flow.Key,
				Title:       flow.Title,
				Description: flow.Description,
				Complete:    flow.Complete(),
				Questions:   describeStoryQuestions(flow.Questions),
			}
			for _, step := range flow.Steps {
				if step == nil {
					continue
				}
				flowSummary.Steps = append(flowSummary.Steps, storyStep{
					Key:         step.Key,
					ActorKey:    step.ActorKey,
					Summary:     step.Summary,
					Description: step.Description,
					Kind:        string(step.Kind),
					Questions:   describeStoryQuestions(step.Questions),
				})
			}
			summary.Flows = append(summary.Flows, flowSummary)
		}
		stories = append(stories, summary)
	}
	return stories
}

// describeStoryQuestions copies the open questions of a flow or a step.
func describeStoryQuestions(questions []*storyexpr.QuestionExpr) []storyQuestion {
	var out []storyQuestion
	for _, q := range questions {
		if q == nil {
			continue
		}
		out = append(out, storyQuestion{
			Key:         q.Key,
			Prompt:      q.Prompt,
			Description: q.Description,
			Options:     slices.Clone(q.Options),
		})
	}
	return out
}
