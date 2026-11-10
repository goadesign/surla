# Product Direction

This page records what Surla is meant to be and the product decisions that
follow from it. It describes direction, not shipped behavior:
[`README.md`](../README.md) and [the reference](REFERENCE.md) describe what
Surla does today. Where this page and an older planning document disagree
(for example [`PRESS_RELEASE.md`](../PRESS_RELEASE.md) or
[`PROPOSAL.md`](../PROPOSAL.md)), this page states the current intent.

[Decisions](#decisions) are settled. [Open questions](#open-questions) are
not.

## The product in five lines

1. **User.** A technical builder — a backend or platform engineer, or an
   architect — who is designing a cloud platform and wants to reason about
   its architecture. Product partners are a secondary audience: they take
   part when a decision is needed, when the result can be tried, and when
   the verdict is in.
2. **What they get.** Surla is a **cloud platform factory**. It gives the
   user an architecture they can reason about, built on best practices that
   are part of the product, with a compiler writing the plumbing and an
   independent verifier proving the result.
3. **Rules that do not bend.** They are listed under
   [Decisions](#decisions).
4. **Out of scope.** A catalogue or console of cloud services, deploying
   or applying infrastructure, and an application builder for non-technical
   users.
5. **Delivery.** An MCP App inside the assistant the user already works in,
   with the same page served by `surla serve` for hosts that cannot show it.

## Decisions

### Surla builds platforms, not applications

Many tools turn a prompt into an application. Surla is for the person
building the platform underneath: several services, agents, data stores and
the systems they depend on. The architecture is therefore the main thing
the user looks at, not supporting evidence behind a link.

### The architecture is shown as real C4 diagrams

Surla renders the platform with [Model](https://github.com/goadesign/model)
from the design code, so a diagram is always a true picture of the design.
The user moves between the C4 views: context, containers, components and
deployment.

- **Dependencies and placement live in the diagrams.** A database, a
  workflow engine or an external provider is an element with relationships;
  where things run is the deployment view. Surla adds no separate lists for
  "depends on" or "runs on".

### The model is a guide and thought partner

The model proposes an architecture, gives its reasons, lays out the
trade-offs where a real choice exists, and flags risks. The user asks about
any element and gets an answer about that element.

The user engages as little or as much as they want:

- **A little.** Accept the proposal and decide only the choices Surla
  raises.
- **Some.** Walk through the choices with the guide.
- **A lot.** Reshape the architecture, by saying what to change or by
  editing the diagram, and have Surla check the result.

Between the user's decisions the agent continues on its own. It stops for a
decision only the user can make, and for a verdict.

### Best practices are built in and drive the design

The embedded playbook,
[`GOLDEN_PATH.md`](../internal/goldenpath/GOLDEN_PATH.md), shapes every
proposal. A choice the playbook settles is not offered as a choice.

- **One owner for each piece of durable state.** One service owns each
  table, collection or key; other services call its API. Two services may
  share a physical database host. Whether they do is a deployment choice.
- **A rule can be broken only on the record.** When the user insists on
  something a rule forbids, Surla offers the ways to get the same result
  within the rule. If the user still chooses to break it, Surla allows it,
  records the exception, and marks it on the diagram until it is removed.

### Major work starts from what happens

For a new platform or a major feature, the guide first drafts a timeline of
what happens in the user's domain, before any architecture. The method is
Event Storming. A small change skips this step.

- **Plain labels.** The notes are named in plain words: what happens, who
  or what triggers it, rules, open questions, and other systems. The
  interface does not use the method's own vocabulary.
- **It leads to the architecture.** The open questions on the timeline are
  the decisions only the user can make. Groups of notes become services.
- **It stays.** The timeline remains a view of the platform, linked to the
  diagrams: selecting a note shows the parts that implement it, and
  selecting a part shows its notes.

### Domain-driven design, without the vocabulary

Surla applies the principles of domain-driven design and does not ask the
user to know them. Each appears in plain words, with the established term
available to those who look for it.

- **Groups become services.** A group of related notes on the timeline is
  one service with its own data (a bounded context).
- **One name for one thing.** The words on the timeline are the names in
  the design, the code and the docs (ubiquitous language).
- **A service knows only what another exposes.** That is its API and its
  public messages: typed, versioned, and declared in the design (published
  language). It never sees another service's tables or internal types.
- **Translate at the edge.** Where an outside system models things
  differently, a translator at the boundary keeps that model out
  (anti-corruption layer).

### Events where they fit

Surla does not default to an event-driven architecture and does not avoid
one. The playbook's rule decides: use events when several consumers need
the same fact or the producer must not wait, and never for a step that
must commit or fail atomically. Where that leaves a real choice, the user
decides it. [Pulse](https://github.com/goadesign/pulse) carries events and
is part of Surla's toolset with Goa, Goa-AI and Model.

### Four words the experience is held to

In order: delightful, intuitive, street smart, and faithful to Google's
user experience standards. Established methods are woven in so that their
benefit arrives without the experience feeling like a method.

### No walls of text

A screen guides without asking the user to read paragraphs: one headline, a
few one-line points, and at most one primary action. Detail is one step
away. The interface uses the vocabulary of the user's platform and of
software architecture, not Surla's internal terms.

### The builder never grades its own work

The verifier is independent of the agents that wrote the code, and hidden
scenarios stay hidden from them. This holds for infrastructure code as
well as application code.

### Surla knows no cloud, and never deploys

- **No cloud-specific knowledge.** Surla does not carry a catalogue of
  cloud services or map the platform onto one. The cloud is whatever the
  user says. The coding agent writes the Terraform from the deployment
  view, the same way it writes service code, and reuses the Terraform
  conventions the repository already has.
- **Surla can produce Terraform.** It is written the way service code is,
  and checked independently of the agent that wrote it.
- **Deployment is separate and explicit.** Surla does not apply
  infrastructure or deploy. Recording an approval does not merge, deploy or
  apply anything.

### One workspace

Deciding, trying the result, reading the verdict and looking at evidence
all happen in the architecture workspace, with the affected elements marked
on the diagram, so that a change has no separate sequence of screens.

### Reshape by saying, or on the diagram

The user changes the architecture in either of two ways, and the design
code stays the source of truth for both.

- **By saying.** The user selects an element or says what to change. The
  guide edits the design code and the diagram redraws with the difference
  marked.
- **On the diagram.** The user adds, edits and removes elements and
  relationships directly. Surla reuses Model's diagram editor, or pieces of
  it, where that is practical.

### Every view says what it shows

- **Each element carries a state.** Planned, changing, or checked. "Checked"
  means an independent check passed for a named commit in a named
  environment.
- **The view names its subject.** A proposal, a build in progress, a checked
  commit, or a design read from an existing repository that has not been
  checked. Surla does not show a platform as running, because it does not
  observe that.

### Checking Terraform

Surla validates the Terraform files and compares them with the deployment
view, reporting an element with no resource and a resource missing from the
diagram.

### Delivered as an MCP App

- **In the conversation,** a small card shows one moment — a proposal, a
  decision, a verdict — with a small diagram and one action.
- **Full screen,** the same app is the architecture workspace.
- **State lives on the Surla server.** A view can then be rebuilt at any
  time from what the server holds. The MCP Apps specification dated
  2026-01-26 leaves it to the host how long a view stays mounted and lists
  state persistence as future work, so Surla does not rely on a view
  surviving between turns.
- **Outside an MCP Apps host,** `surla serve` serves the same page.

## Open questions

- **Who runs `terraform plan`.** Planning against a cloud account needs
  that account's credentials. Whether Surla runs it with credentials the
  user supplies, or the user runs it, is not decided.
- **How an edit on the diagram reaches the design code.** Model's editor
  moves elements today; it does not add, rename or remove them. How much of
  it Surla can reuse, and whether a diagram edit is applied by rewriting the
  code directly or by asking the guide to, is under study.
