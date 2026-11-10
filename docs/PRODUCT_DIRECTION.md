# Product Direction

This page records what Surla is meant to be and the product decisions that
follow from it. It describes direction, not shipped behavior:
[`README.md`](../README.md) and [the reference](REFERENCE.md) describe what
Surla does today. Where this page and an older planning document disagree
(for example [`PRESS_RELEASE.md`](../PRESS_RELEASE.md) or
[`PROPOSAL.md`](../PROPOSAL.md)), this page states the current intent.

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
4. **Out of scope.** A drag-and-drop diagram editor, a catalogue or console
   of cloud services, provisioning, and an application builder for
   non-technical users.
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
- **Each element carries a state.** Planned, changing, or checked. "Checked"
  means an independent check passed for a named commit in a named
  environment. Surla never shows a platform as running: it does not observe
  that.
- **The view always says what it shows.** A proposal, a build in progress, a
  checked commit, or a design read from an existing repository that has not
  been checked.

### The model is a guide and thought partner

The model proposes an architecture, gives its reasons, lays out the
trade-offs where a real choice exists, and flags risks. The user asks about
any element and gets an answer about that element.

The user engages as little or as much as they want:

- **A little.** Accept the proposal and decide only the choices Surla
  raises.
- **Some.** Walk through the choices with the guide.
- **A lot.** Reshape the architecture by saying what to change. The guide
  edits the design code and the diagram redraws with the difference marked.
  The user does not drag boxes.

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

### No walls of text

A screen guides without asking the user to read paragraphs: one headline, a
few one-line points, and at most one primary action. Detail is one step
away. The interface uses the vocabulary of the user's platform and of
software architecture, not Surla's internal terms.

### One workspace

Deciding, trying the result, reading the verdict and looking at evidence
all happen in the architecture workspace, with the affected elements marked
on the diagram. There is no separate sequence of screens for a change.

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
- **Surla checks the Terraform.** It validates the files without cloud
  credentials and checks that they match the deployment view: every element
  has a resource, and no resource is missing from the diagram.
- **Deployment is separate and explicit.** `terraform plan` and
  `terraform apply` need the user's credentials and are the user's to run.
  Recording an approval does not merge, deploy or apply anything.

### Delivered as an MCP App

Surla's interface is an MCP App in the user's assistant.

- **In the conversation,** a small card shows one moment — a proposal, a
  decision, a verdict — with a small diagram and one action.
- **Full screen,** the same app is the architecture workspace.
- **State lives on the Surla server.** An MCP App view belongs to one tool
  call and is not kept between turns, so every view is rebuilt from what
  the server holds.
- **Outside an MCP Apps host,** `surla serve` serves the same page.
