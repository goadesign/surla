# Story language

Describe business roles and flows as ordinary Go source, before choosing an
architecture. Goa evaluates the declarations into `expr.Root.Stories`.
This nested module depends on Goa and the standard library. It does not load
Surla's control plane or Goa-AI.

```go
package design

import . "github.com/goadesign/surla/story/dsl"

var _ = Story("Refunds", func() {
	Actor("customer", "Customer")
	Actor("manager", "Support manager")

	Flow("refund-over-limit", "Refund above the limit", func() {
		StartsWith("request-refund", "customer", "Requests a refund")
		Step("approve-refund", "manager", "Approves the refund")
		Outcome("refund-paid", "customer", "Receives the refund")
	})

	Flow("refund-without-receipt", "Refund without a receipt", func() {
		StartsWith("request-refund", "customer", "Requests a refund", func() {
			Question("proof-of-purchase", "Which proof of purchase is acceptable?", func() {
				Option("Order number")
				Option("Payment record")
			})
		})
	})
})
```

Keys preserve identity when titles, summaries, or declaration order change.
They must match `^[a-z][a-z0-9]*(?:[-_][a-z0-9]+)*$` and are kept verbatim.
Actor keys and flow keys each have their own scope within a Story. Step keys
and question keys each have their own scope within a flow; question keys are
unique across both flow questions and attached step questions. Step actor
references must name an actor in the same Story. Business roles such as
`warehouse-worker` and `insurance-agent` are valid.

`Story(name, fn)` appears at the top level. Its function accepts
`Actor(key, title, fn...)`, `Flow(key, title, fn)`, and `Description(text)`.
A flow accepts `StartsWith(key, actorKey, summary, fn...)`,
`Step(key, actorKey, summary, fn...)`, `Outcome(key, actorKey, summary, fn...)`,
`Question(key, prompt, fn...)`, and `Description(text)`. A step accepts
questions and descriptions. An actor accepts a description. A question accepts
`Option(label)` and a description. Each optional function permits at most one
function. Option labels must be nonempty and distinct within the question.

After successful `eval.RunDSL()`, `flow.Complete()` reports whether a flow
has one start, at least one outcome, and no open questions, including questions
attached to steps. A flow may have multiple outcomes but at most one start.
Missing starts, missing outcomes, and open questions leave valid unfinished
drafts; they do not invalidate other finished flows. Suggested options do not
answer a question. Refine the Story with the decision and remove its question
declaration when resolved. Duplicate keys, invalid keys, invalid actor
references, and misplaced DSL calls remain evaluation errors.

Actors, flows, steps, and each question list retain declaration order.
Completion is derived from that tree without generated names or cached status. Like Goa's
other DSLs, evaluation uses a process-wide context and must run serially.
The language can coexist with ordinary Goa API declarations and can also be
evaluated on its own, without any API, service, or model declaration.

This module currently supplies language and expressions only. Story code
generation, sandboxed Story refresh, timeline editing, independent test
authoring, and verification integration are described in the
[engineering design](../docs/STORY_TEST_DESIGN.md) and are not implemented here.

Run this module's checks from `story/`:

```sh
CI=true go test -race -vet=all ./... </dev/null
```
