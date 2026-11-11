When emitting `ui_composition` for a Surla CP1 Change Card, write a valid `openui-lang` program starting with `root = DecisionWorkspace(...)` using ONLY the components below.

## Syntax Rules

1. Each statement is on its own line: `identifier = Expression`
2. `root` is the entry point — every program must define `root = DecisionWorkspace(...)`
3. Expressions are: strings ("..."), numbers, booleans (true/false), null, arrays ([...]), objects ({...}), or component calls TypeName(arg1, arg2, ...)
4. Use references for readability: define `name = ...` on one line, then use `name` later
5. EVERY variable (except root) MUST be referenced by at least one other variable. Unreferenced variables are silently dropped and will NOT render. Always include defined variables in their parent's children/items array.
6. Arguments are POSITIONAL (order matters, not names). Write `SomeComp([children], "row", "l")` NOT `SomeComp([children], direction: "row", gap: "l")` — colon syntax is NOT supported and silently breaks
7. Optional arguments can be omitted from the end
- Strings use double quotes with backslash escaping

## Component Signatures

Arguments marked with ? are optional. Sub-components can be inline or referenced; prefer references for better streaming.

### CP1 Decision Instruments
DecisionWorkspace(title: string, children: (PolicySimulatorTable | AmbiguityChoiceCard | WorkflowTimeline | ImpactCallout)[]) — Root container for a Surla CP1 Decision Workspace combining policy simulators, decision cards, workflow timelines, and impact callouts.
PolicySimulatorTable(title: string, ambiguityIndex: number, rows: PolicySimulatorRow[]) — Interactive policy comparison table bound to a 1-based ambiguityIndex (1..N in ambiguities). Toggling an option updates the canonical CP1 answer draft and highlights the active outcome column.
PolicySimulatorRow(scenario: string, inputSummary: string, outcomes: string[], exampleIndex?: number) — A scenario row in PolicySimulatorTable. outcomes must provide one non-empty outcome string per option of the parent ambiguity in exact option order; exampleIndex is an optional 1-based index into examples.
AmbiguityChoiceCard(ambiguityIndex: number, rationale?: string) — Focused decision card bound to a 1-based ambiguityIndex (1..N in ambiguities) with optional domain trade-off rationale.
WorkflowTimeline(title: string, steps: WorkflowStep[]) — Ordered workflow sequence illustrating how actors and policy gates interact.
WorkflowStep(step: string, actor: string, detail: string, gate?: string) — A single workflow step showing the step name, actor, detail, and optional policy gate.
ImpactCallout(variant: "info" | "warning", title: string, summary: string) — Callout banner highlighting a preserved contract invariant or architectural trade-off.
Always start with root = DecisionWorkspace(title, children).
ambiguityIndex is a 1-based integer index (1..N) into the ambiguities array of the analysis you send.
exampleIndex is an optional 1-based integer index (1..M) into the examples array of the analysis you send.
In PolicySimulatorRow, outcomes must be a string array with the exact same length and order as the referenced ambiguity's options array.
Never emit Query(), Mutation(), $variables, or @Action steps.

## Hoisting & Streaming (CRITICAL)

openui-lang supports hoisting: a reference can be used BEFORE it is defined. The parser resolves all references after the full input is parsed.

During streaming, the output is re-parsed on every chunk. Undefined references are temporarily unresolved and appear once their definitions stream in. This creates a progressive top-down reveal — structure first, then data fills in.

**Recommended statement order for optimal streaming:**
1. `root = DecisionWorkspace(...)` — UI shell appears immediately
2. Component definitions — fill in as they stream
3. Data values — leaf content last

Always write the root = DecisionWorkspace(...) statement first so the UI shell appears immediately, even before child data has streamed in.

## Examples

Example — Refund threshold ambiguity with 2 options and 2 scenarios:
root = DecisionWorkspace("Refund Policy Decision Studio", [sim, callout])
sim = PolicySimulatorTable("Threshold Tax Comparison", 1, [row1, row2])
row1 = PolicySimulatorRow("Order with $48 subtotal and $4 tax", "$48 subtotal + $4 tax ($52 total)", ["Auto-approved (< $50 subtotal)", "Manager approval required (>= $50 total)"], 1)
row2 = PolicySimulatorRow("Order with $55 subtotal", "$55 subtotal + $5 tax ($60 total)", ["Manager approval required", "Manager approval required"], 2)
callout = ImpactCallout("info", "Tenant Isolation Invariant", "Cross-tenant refund lookups continue to return 404 Not Found.")

## Important Rules
- Choose components that best represent the content (tables for comparisons, charts for trends, forms for input, etc.)

## Final Verification
Before finishing, walk your output and verify:
1. root = DecisionWorkspace(...) is the FIRST line (for optimal streaming).
2. Every referenced name is defined. Every defined name (other than root) is reachable from root.

- Refer to ambiguities ONLY by 1-based `ambiguityIndex` (1..N matching the `ambiguities` array order in the analysis you send). Never invent `<change_id>-ambiguity-NN` IDs.
- Refer to confirmation examples ONLY by 1-based `exampleIndex` (1..M matching the `examples` array order in the analysis you send).
- In `PolicySimulatorRow`, `outcomes` MUST be a string array with one non-empty outcome string per option in the exact order of `ambiguities[ambiguityIndex - 1].options`.
- Never emit `Query(...)`, `Mutation(...)`, `$variables`, or `@Action` steps.
