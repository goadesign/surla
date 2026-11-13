# Product Direction

This page records what Surla is meant to be and the product decisions that
follow from it. It describes direction, not shipped behavior:
[`README.md`](../README.md) and [the reference](REFERENCE.md) describe what
Surla does today.

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
5. **Delivery.** The coding agents the user already has, working in
   terminals Surla starts, and a views page the project's Surla server
   serves to the user's browser, where the user agrees changes and approves
   results under their own identity.

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
  Each flow results in an end-to-end test of the built platform. An agent
  that did not write the platform code writes the tests.
- **Finished flows are built; drafts wait.** Surla builds and tests the
  flows that are finished. A flow that is still a draft stays editable, is
  left out of that build, and is clearly shown as untested. A draft does
  not stop the finished flows from being built, and the user never has to
  delete a draft idea to build.
- **Later edits wait until adopted.** Once a change is agreed, a later
  Story edit stays a draft until the user adopts it. The agreed change, its
  answers, confirmation, and test results stay valid for the Story that was
  agreed, and the test writer and verifier read that frozen Story rather
  than mutable workspace files (see the
  [Story test engineering design](STORY_TEST_DESIGN.md#proposed-story-and-build-contract)).
  Adopting the edited Story creates a new revision that needs confirmation.
- **It leads to the architecture.** Open questions on the timeline become
  the choices Surla raises, unless the playbook already settles them. How
  the notes group suggests where boundaries could go; the user and the
  guide settle them.
- **It stays.** The timeline stays current as a view of the platform after
  the architecture exists, and is meant to be linked to the diagrams.

The timeline's source, called the Story, is written in a Go domain-specific
language (DSL) and versioned in the platform's repository. A Goa plugin
generates test scaffolds and pictures from that source. The plugin starts
in Surla and must remain easy to move if it later needs its own home.

When the user edits the Story through the timeline or chat, the guide
changes its source code. Surla runs the changed Story code in the background
and redraws the timeline. Drafts and incomplete flows must still run without
errors so the user can refine them in place. Each step in a flow has an
explicit stable key: a manager approval step can keep `approve-refund` when
its label changes. Actors name roles such as a customer or manager, rather
than architecture elements such as an API or worker.

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
- **Deployment is separate and explicit.** Surla does not run
  `terraform plan`, apply infrastructure or deploy. Planning, applying and
  the credentials they need stay with the user. Recording an approval does
  not merge, deploy or apply anything.
- **A deployed copy can be checked, never created.** The user deploys the
  platform however they choose and gives Surla its address. Surla runs the
  same flow tests there and reports the result under that environment's own
  name, for example "Checked in staging" beside "Checked in simulation".
  Surla does not start or provision that environment.

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
  redraws the diagram. When the user moves an element to improve its layout,
  Surla saves the new position directly, without the guide. Surla reuses
  Model's diagram editor, or pieces of it, where that is practical.

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

### Delivered as a page in the browser

- **Served by the project's Surla server.** Running `surla` opens the page
  with a one-time link; the page keeps its session only while it is open.
- **The page is the architecture workspace,** and the only place where a
  person agrees a change or approves a result. No agent holds the user's
  identity.
- **State lives on the Surla server.** A view can be rebuilt at any time
  from what the server holds, so a reload or a restart loses only the
  session, and the page updates itself when the server's state changes.

## Open questions

- **Testing a deployed copy.** Running the flow tests against an address
  the user supplies is decided; how is not. A test such as a refund could
  have real effects there, outside services are real instead of simulated,
  and Surla needs a way to call the platform's API in that environment.
  None of these is decided.
- **Story vocabulary and links.** The Story's Go DSL vocabulary, what
  marks the start and end of a flow, and how a step stays linked to the
  design elements and tests that implement it are not decided.
- **How editing instructions reach the guide.** The guide edits source
  for Story and architecture changes. Which diagram gestures Surla offers
  and how the UI or an MCP client delivers those instructions are not
  decided.
