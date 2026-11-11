package dsl_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goadsl "goa.design/goa/v3/dsl"
	"goa.design/goa/v3/eval"
	goaexpr "goa.design/goa/v3/expr"

	. "github.com/goadesign/surla/story/dsl"
	"github.com/goadesign/surla/story/expr"
)

func TestStoryFinishedAndUnfinishedFlows(t *testing.T) {
	root := runDSL(t, func() {
		Story("Refunds", func() {
			Description("Return money after a refund request.")
			Actor("customer", "Customer", func() { Description("Requests a refund.") })
			Actor("insurance-agent", "Insurance agent")
			Actor("warehouse-worker", "Warehouse worker")
			Flow("refund-over-limit", "Refund above the limit", func() {
				Description("Approve and pay a refund.")
				StartsWith("request-refund", "customer", "Requests a refund", func() {
					Description("The request exceeds the automatic approval limit.")
				})
				Step("approve-refund", "insurance-agent", "Approves the refund")
				Outcome("refund-paid", "customer", "Receives the refund")
				Outcome("stock-returned", "warehouse-worker", "Receives the returned goods")
			})
			Flow("empty", "Still being explored", func() {})
			Flow("no-start", "No start yet", func() {
				Outcome("paid", "customer", "Receives money")
			})
			Flow("no-outcome", "No outcome yet", func() {
				StartsWith("request", "customer", "Requests money")
			})
			Flow("open-flow", "Decision remains", func() {
				StartsWith("request", "customer", "Requests money")
				Outcome("paid", "customer", "Receives money")
				Question("currency", "Which currency?", func() {
					Description("Choose a supported settlement currency.")
					Option("EUR")
					Option("USD")
				})
			})
			Flow("open-step", "Step decision remains", func() {
				StartsWith("request", "customer", "Requests money")
				Outcome("paid", "customer", "Receives money", func() {
					Question("timing", "When is the refund paid?")
				})
			})
		})
	})
	require.Len(t, root.Stories, 1)
	story := root.Stories[0]
	assert.Equal(t, "Return money after a refund request.", story.Description)
	assert.Equal(t, "Requests a refund.", story.Actors[0].Description)
	require.Len(t, story.Flows, 6)
	for i, flow := range story.Flows {
		assert.Equal(t, i == 0, flow.Complete(), flow.Key)
		assert.Same(t, story, flow.Story)
	}
	finished := story.Flows[0]
	assert.Equal(t, "Approve and pay a refund.", finished.Description)
	assert.Equal(t, "The request exceeds the automatic approval limit.", finished.Steps[0].Description)
	assert.Equal(t, []expr.StepKind{expr.Start, expr.Action, expr.Outcome, expr.Outcome}, []expr.StepKind{
		finished.Steps[0].Kind, finished.Steps[1].Kind, finished.Steps[2].Kind, finished.Steps[3].Kind,
	})
	question := story.Flows[4].Questions[0]
	assert.Equal(t, "Choose a supported settlement currency.", question.Description)
	assert.Equal(t, []string{"EUR", "USD"}, question.Options)

	// Completion follows the current expressions, including a resolved question,
	// without another prepare pass or mutable completion flag.
	story.Flows[4].Questions = nil
	assert.True(t, story.Flows[4].Complete())
}

func TestStableKeysSurviveWordingAndOrderEdits(t *testing.T) {
	declare := func(revised bool) func() {
		return func() {
			Story("Refunds", func() {
				Actor("claims-agent", "Claims agent")
				key, title := "refund-a_2", "Refund"
				if revised {
					title = "Refund above the automatic approval limit"
				}
				Flow(key, title, func() {
					StartsWith("request", "claims-agent", "A refund is requested")
					first, second := "approve-refund", "notify-customer"
					if revised {
						first, second = second, first
					}
					Step(first, "claims-agent", title)
					Step(second, "claims-agent", title)
					Outcome("refund-paid", "claims-agent", "Refund is paid")
				})
			})
		}
	}
	original := runDSL(t, declare(false)).Stories[0].Flows[0]
	revised := runDSL(t, declare(true)).Stories[0].Flows[0]
	assert.Equal(t, original.Key, revised.Key)
	assert.NotEqual(t, original.Title, revised.Title)
	assert.Equal(t, original.Steps[1].Key, revised.Steps[2].Key)
	assert.Equal(t, original.Steps[2].Key, revised.Steps[1].Key)
	assert.True(t, revised.Complete())
}

func TestIndependentKeyScopesAndPortableNames(t *testing.T) {
	runDSL(t, func() {
		for _, storyName := range []string{"Refunds", "Returns"} {
			Story(storyName, func() {
				Actor("a", "A")
				Actor("a0", "A0")
				// These keys may collide after evaluator-specific normalization,
				// or name reserved generated symbols. Neither concerns this DSL.
				for _, key := range []string{"a", "refund-paid", "refund_paid", "hooks", "new"} {
					Flow(key, key, func() {
						StartsWith("a", "a", "Start")
						Outcome("done", "a0", "Done")
						Question("a", "What remains?")
					})
				}
			})
		}
	})
}

func TestStoryCoexistsWithGoaAPI(t *testing.T) {
	goaexpr.ResetDSL(t)
	expr.Root = new(expr.RootExpr)
	require.NoError(t, eval.Register(expr.Root))
	require.True(t, eval.Execute(func() {
		goadsl.API("refunds", func() { goadsl.Description("Refund service") })
		goadsl.Service("refunds", func() {
			goadsl.Method("request", func() { goadsl.Result(goadsl.String) })
		})
		Story("Refunds", func() {
			Actor("customer", "Customer")
			Flow("refund", "Refund", func() {
				StartsWith("request", "customer", "Requests a refund")
				Outcome("paid", "customer", "Receives money")
			})
		})
	}, nil), eval.Context.Error())
	require.NoError(t, eval.RunDSL())
	assert.Equal(t, "refunds", goaexpr.Root.API.Name)
	require.Len(t, goaexpr.Root.Services, 1)
	assert.Equal(t, "request", goaexpr.Root.Services[0].Methods[0].Name)
	assert.True(t, expr.Root.Stories[0].Flows[0].Complete())
}

func TestInvalidKeys(t *testing.T) {
	for _, key := range []string{"", "A", "a b", " a", "a ", "a--b", "a__b", "a-", "1a", "a/b"} {
		for _, kind := range []string{"actor", "flow", "step", "question"} {
			t.Run(fmt.Sprintf("%s/%q", kind, key), func(t *testing.T) {
				err := evaluate(t, func() {
					Story("Refunds", func() {
						actorKey, flowKey, stepKey, questionKey := "customer", "refund", "start", "currency"
						switch kind {
						case "actor":
							actorKey = key
						case "flow":
							flowKey = key
						case "step":
							stepKey = key
						case "question":
							questionKey = key
						}
						Actor(actorKey, "Customer")
						Flow(flowKey, "Refund", func() {
							StartsWith(stepKey, actorKey, "Requests refund")
							Question(questionKey, "Which currency?")
						})
					})
				})
				assert.ErrorContains(t, err, "invalid key")
			})
		}
	}
}

func TestInvalidRelationships(t *testing.T) {
	tests := []struct {
		name string
		dsl  func()
		want string
	}{
		{"duplicate actors", func() { Actor("a", "A"); Actor("a", "Again") }, "duplicate key"},
		{"duplicate flows", func() {
			Flow("f", "F", func() {})
			Flow("f", "Again", func() {})
		}, "duplicate key"},
		{"duplicate step kinds", func() {
			Actor("a", "A")
			Flow("f", "F", func() {
				StartsWith("same", "a", "Starts")
				Outcome("same", "a", "Ends")
			})
		}, "duplicate key"},
		{"duplicate attached question", func() {
			Actor("a", "A")
			Flow("f", "F", func() {
				Question("q", "First?")
				Step("s", "a", "Acts", func() { Question("q", "Second?") })
			})
		}, "duplicate key"},
		{"undeclared actor in unfinished flow", func() {
			Flow("f", "F", func() { Step("s", "unknown", "Acts") })
		}, `actor "unknown" is not declared`},
		{"multiple starts", func() {
			Actor("a", "A")
			Flow("f", "F", func() {
				StartsWith("first", "a", "Starts")
				StartsWith("second", "a", "Starts again")
			})
		}, "more than one starting step"},
		{"empty option", func() {
			Flow("f", "F", func() { Question("q", "Choose?", func() { Option(" \t") }) })
		}, "option must not be empty"},
		{"duplicate options", func() {
			Flow("f", "F", func() {
				Question("q", "Choose?", func() { Option("First"); Option("First") })
			})
		}, "duplicate question option"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := evaluate(t, func() { Story("Refunds", test.dsl) })
			assert.ErrorContains(t, err, test.want)
		})
	}
	// Actor references resolve against the owning Story, even when another
	// Story declares the requested key.
	err := evaluate(t, func() {
		Story("First", func() { Actor("a", "A") })
		Story("Second", func() {
			Flow("f", "F", func() { Outcome("done", "a", "Done") })
		})
	})
	assert.ErrorContains(t, err, `actor "a" is not declared in Story "Second"`)
}

func TestWrongDSLContexts(t *testing.T) {
	tests := []struct {
		name string
		dsl  func()
	}{
		{"actor at top", func() { Actor("a", "A") }},
		{"flow at top", func() { Flow("f", "F", func() {}) }},
		{"start at top", func() { StartsWith("s", "a", "Starts") }},
		{"step at top", func() { Step("s", "a", "Acts") }},
		{"outcome at top", func() { Outcome("s", "a", "Ends") }},
		{"question at top", func() { Question("q", "Why?") }},
		{"option at top", func() { Option("Yes") }},
		{"description at top", func() { Description("Details") }},
		{"nested Story", func() { Story("S", func() { Story("Nested", func() {}) }) }},
		{"question in actor", func() {
			Story("S", func() { Actor("a", "A", func() { Question("q", "Why?") }) })
		}},
		{"step in question", func() {
			Story("S", func() {
				Flow("f", "F", func() { Question("q", "Why?", func() { Step("s", "a", "Acts") }) })
			})
		}},
		{"option in flow", func() { Story("S", func() { Flow("f", "F", func() { Option("Yes") }) }) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.ErrorContains(t, evaluate(t, test.dsl), "invalid use")
		})
	}
}

func TestOnlyOneOptionalDSLFunction(t *testing.T) {
	tests := []struct {
		name string
		dsl  func()
	}{
		{"actor", func() { Actor("a", "A", func() {}, func() {}) }},
		{"start", func() {
			Flow("f", "F", func() { StartsWith("s", "a", "Starts", func() {}, func() {}) })
		}},
		{"step", func() {
			Flow("f", "F", func() { Step("s", "a", "Acts", func() {}, func() {}) })
		}},
		{"outcome", func() {
			Flow("f", "F", func() { Outcome("s", "a", "Ends", func() {}, func() {}) })
		}},
		{"question", func() {
			Flow("f", "F", func() { Question("q", "Why?", func() {}, func() {}) })
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.ErrorContains(t, evaluate(t, func() { Story("S", test.dsl) }), "too many arguments")
		})
	}
}

// runDSL executes public declarations through Goa and requires valid results.
func runDSL(t *testing.T, dsl func()) *expr.RootExpr {
	t.Helper()
	require.NoError(t, evaluate(t, dsl))
	return expr.Root
}

// evaluate follows Goa's test reset pattern. Tests intentionally run serially
// because the actual Goa DSL context and registered roots are process globals.
func evaluate(t *testing.T, dsl func()) error {
	t.Helper()
	eval.Reset()
	expr.Root = new(expr.RootExpr)
	require.NoError(t, eval.Register(expr.Root))
	if !eval.Execute(dsl, nil) {
		return eval.Context.Errors
	}
	return eval.RunDSL()
}
