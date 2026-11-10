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
- **The diagrams are held to the same standard as the rest.** How a
  diagram looks is part of the experience, not a given of the tool that
  draws it. Where Model's rendering falls short, Model itself can be
  changed and released.

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
domain events — what happens in the user's domain — before proposing
architecture for it. The method is Event Storming. A small change skips
this step.

- **Plain labels.** The timeline's notes are named in plain words — for
  example "what happens" rather than "domain event" — and the interface
  does not use the method's own vocabulary.
- **The user edits it first.** Before Surla proposes architecture, the
  user can add, change and remove notes on the timeline. They do it on the
  timeline itself or by saying it to the guide, including from a chat
  client that talks to Surla over MCP. Both ways change the same timeline,
  and the architecture Surla proposes next follows the edited one.
- **Its flows become end-to-end tests.** A flow on the timeline is a path
  from what starts it to an outcome — for example, a customer asks for a
  refund over the limit, a manager approves it, and the refund is paid.
  Each flow results in an end-to-end test of the built platform. A
  different agent from the builder writes the tests. Generated scaffolds
  help that agent implement the interactions and assertions; scaffolding
  alone is not a working test.
- **It leads to the architecture.** Open questions on the timeline become
  the choices Surla raises, unless the playbook already settles them. How
  the notes group suggests where boundaries could go; the user and the
  guide settle them.
- **It stays.** The timeline stays current as a view of the platform after
  the architecture exists, and is meant to be linked to the diagrams.

The timeline, called a Story, is written in a Go domain-specific language
(DSL) and versioned with the repository. A Goa plugin generates test
scaffolds and pictures from that source. The plugin starts in Surla and
must remain easy to move if it later needs its own home.

When the user edits the Story through the timeline or chat, the guide
changes its source code. Evaluation runs asynchronously and redraws the
result. Drafts and incomplete flows must evaluate cleanly so the user can
refine them in place. Steps have explicit stable keys, and actors name
roles such as a customer or manager, rather than architecture elements
such as an API or worker.

### Domain-driven design, without the vocabulary

Surla applies the principles of domain-driven design and does not ask the
user to know them. Each appears in plain words, with the established term
available to those who look for it.

- **Boundaries follow meaning.** A boundary is drawn where a set of words
  and rules stays consistent (a bounded context). The groups on the
  timeline inform where boundaries go; a group is not automatically one
  service.
- **Within a boundary, one name for one thing.** Inside each boundary, the
  same words name each thing on the timeline and in its design, code and
  docs (ubiquitous language). Two boundaries may use the same word
  differently.
- **A service knows only what another exposes.** That is its API and its
  public messages: typed and declared in the design (published language).
  It never sees another service's tables or internal types.
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

The experience is held to four words: delightful, intuitive, street smart,
and faithful to Google's user experience standards. Established methods
are woven in so that their benefit arrives without the experience feeling
like a method.

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
  relationships through diagram gestures. A gesture gives the guide an
  instruction to edit the design code; Surla evaluates that code and
  redraws the diagram. Moving an element to improve its layout remains a
  direct layout edit. Surla reuses Model's diagram editor, or pieces of it,
  where that is practical.

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
- **Story vocabulary and links.** The Story's Go DSL vocabulary, what
  marks the start and end of a flow, and how a step stays linked to the
  design elements and tests that implement it are not decided.
- **How confirmed flows reach independent test authors.** Tests must use
  the confirmed Story revision, rather than a copy the builder can change.
  How Surla pins that revision, accepts the independently authored tests,
  and follows later Story edits without using stale evidence is not
  decided. The plugin generates scaffolds; the exact test-writing
  workflow remains to be designed.
- **How editing instructions reach the guide.** The guide edits source
  for Story and architecture changes. Which diagram gestures Surla offers
  and how the UI or an MCP client delivers those instructions are not
  decided.
