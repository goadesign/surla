<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/img/surla-banner-dark.png">
    <img alt="Surla — one compiled contract every coding agent builds to and an independent verifier checks against." src="docs/img/surla-banner.png" width="960">
  </picture>
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/goadesign/surla"><img alt="Go Reference" src="https://pkg.go.dev/badge/github.com/goadesign/surla.svg"></a>
  <a href="https://modelcontextprotocol.io"><img alt="MCP" src="https://img.shields.io/badge/MCP-2026--07--28-6E56CF"></a>
  <a href="https://github.com/goadesign/surla/blob/main/LICENSE"><img alt="MIT License" src="https://img.shields.io/badge/license-MIT-blue.svg"></a>
</p>

# Surla

**An agentic software factory that builds, verifies, and evolves production services at a fraction of the token cost.**

A coding agent left alone writes the code and also the tests that judge it, so
its mistakes pass. Hand-written routing, auth middleware and workflow plumbing
drift with every feature, and each later edit costs more tokens.

Surla organizes the coding agents you already use into a team with separate
jobs, and pairs them with a deterministic compiler. Agents write a compact
service design and the business logic. [Goa](https://github.com/goadesign/goa)
and [Goa-AI](https://github.com/goadesign/goa-ai) generate the transports,
schemas, validation, durable workflows and tool guardrails from that design at
**0 LLM tokens**. One agent writes the code; a different agent writes the
tests; Surla itself judges those tests and runs them against the code, which
it builds and runs in an operating-system sandbox. You agree what is built
and approve the result in your browser. Surla deploys nothing and merges
nothing.

## One change from start to finish

These words appear throughout:

| Term | Meaning |
| --- | --- |
| **The server** | The project's Surla server. `surla` starts one per project folder. It keeps what you agreed, who was asked to do what, and what was accepted, and it runs the checks. |
| **The page** | The server's page in your browser, which the reference calls the views page. Every decision that is yours happens there. |
| **Herdr** | The terminal program the agents run in, one pane per agent. |
| **Chorus** | The messaging service the agents use. Its dialog in Herdr is where you choose which coding agent plays each role. |
| **The guide** | The agent you talk to. It writes the Story and proposes changes. |
| **The builder** | The agent that writes the application code. |
| **The test writer** | The agent that writes the change's tests, in its own working copy of the project. |
| **Story** | A plain description of what the system does, written as Go in the project's design, one flow at a time. |
| **Flow** | One path through a Story: who starts it, the steps by role, and the outcome. For example, "a customer asks the price of tea and gets it". A flow with a start, an outcome and no open question is *finished*; any other flow is a draft. |
| **Change Card** | One change the guide proposes: the request, the Story, open questions, concrete examples and the behavior to preserve. |
| **`request_revision`** | The digest of one saved revision of a Change Card. Previews, verification runs and approvals name the revision they belong to. |

Here is one change, for example a refund assistant whose rule is "refunds up
to $50 get instant store credit; larger ones wait for a manager":

1. **Start.** In your project folder, run `surla`. It starts the server in its
   own Herdr terminal, starts Chorus and the Herdr app if they are not
   running, prepares the team, and opens the page.
2. **Choose agents.** The Chorus dialog opens in Herdr with one row per role.
   At first the only role is the guide. Pick an agent and a model for it. Its
   pane opens on the left.
3. **Describe.** Tell the guide what you want in its pane. It writes the Story
   and proposes a Change Card, which appears on the page by itself. For the
   refund rule, the card might ask: "Does $50 mean the item subtotal, or the
   total with tax and shipping?" and "Does exactly $50.00 get instant credit?"
4. **Agree.** On the page, answer the questions, check the examples and
   confirm. No agent can confirm for you. If the guide later revises the
   change, its proposed revision waits beside the agreed revision until you
   adopt or decline it; the agreed revision stays in force until then.
5. **Build.** Press **Build this change** on the page. Surla saves the agreed
   revision, and the Chorus dialog asks you for agents for two more roles: the
   builder and the test writer. The test writer gets its own working copy of
   the project, in `.surla-tests` on the branch `surla/tests`, one required
   test per finished flow of the Story, and your answers, examples and
   preserved behaviors, which its tests must also hold the service to. Draft
   flows require no test.
6. **Tests.** The test writer writes those tests and reports. Surla does not
   take its word for it: it writes the required tests into the copy again,
   checks that nothing changed outside the test module (the project's `eval/`
   folder), and builds and lists the test program in its sandbox. Problems go
   back to the test writer as findings, which you can also read on the page.
   When the tests pass the check, Surla accepts them at one commit on
   `surla/tests`.
7. **Code and verify.** Meanwhile the builder writes the code, builds and
   tests it, and commits. If the application calls a model, the builder can
   try prompt wording on a *live target*, a running copy of the application
   that the server starts from a commit. The builder then asks Surla to
   verify its commit. Surla builds that commit in its sandbox and runs the
   tests accepted for that exact revision against it. It refuses to verify
   until those tests are accepted. The result appears in the page's **Proof**
   view.
8. **Approve and merge.** On the page, press **Approve this result** for a
   passed run. Surla checks that the run passed and ran the tests accepted
   for the agreed revision, then records the approval with the builder's
   commit, the tests commit and the report. The recorded approval on the page
   shows both commits and tells you to merge the tests commit, which is on
   `surla/tests`. The builder commits in the project folder, so its commit is
   already on the branch checked out there; you merge the tests commit into
   that branch yourself. Surla merges and deploys nothing. Approval needs no
   settings. A project that stores
   [promotion settings](docs/REFERENCE.md#surla-verification-and-surla-promotion)
   (a release folder and the names of its binaries) gets one more check at
   approval: the files in the release folder must be the binaries the run
   built.

Only you confirm a change, start a build and approve a result, and you do it on
the page. Each agent has its own connection to the server with only its own
role's permissions. While the team works, the page's **Team** view shows each
agent's state; an agent that is waiting for you is named above every view,
with an action such as **Show guide pane** that brings its pane forward. When
the builder or the test writer has an assignment it has not read, for example
after Surla accepts the tests, the server sends it one fixed Chorus direct
message asking it to read its assignments. One that stays idle with the
assignment unread for 90 seconds is named as waiting for you, and the page says
what to tell it.

You can go back at any point: adopt a revised change, build again, or the test
writer reports again after findings. Adopting a revised change gives it a new
`request_revision`, which starts without an approval, and verification then
needs a build of that revision: before you adopt, the page says that the
existing build stays on the earlier revision. Building again replaces the
build and requires the tests to be reported, judged and accepted again before
verification.

Two limits hold. All agents run as you on one machine, so they are separated
by job, not walled off from each other. The test writer's test program runs on
your machine outside the sandbox (the code under test runs inside it). The
page states the second limit next to **Build this change**. Each verification
run records where its tests came from. For the test writer's tests, the
**Proof** view shows with the run, and the agents read in its result, the
server's notice that the tests were written by a separate agent on this
machine and are not isolated from the builder.

## Install

Surla needs:

- **macOS or Linux (Landlock ABI v4+, Linux 6.7+).** Surla's candidate sandbox
  uses macOS `sandbox-exec` or Linux Landlock and Seccomp-BPF (when running
  `surla` itself inside a Linux container, pass `--cap-add SYS_PTRACE` or
  `--security-opt seccomp=unconfined`). Where the sandbox is unavailable,
  every step that compiles or runs code in that sandbox is refused, such as
  `surla preview`, `surla gen`, starting a build with a test writer and
  verification.
- **Go 1.27.3** or newer.
- **Git**, with your name and address configured (`git config --global
  user.name` and `user.email`). Surla records accepted tests as commits under
  your identity, and a build is refused until Git has one.
- **Docker Desktop**, or another local Docker engine. The builder builds and
  tests its work in Linux containers.
- **Herdr core** (the `herdr-core` and `hc` commands) and the **Herdr app**
  (`herdr-app`).
- **Chorus v0.11.0** or newer.
- A coding agent that the Chorus dialog can start for each role.

Install Surla, Herdr and Chorus:

```bash
go install github.com/goadesign/surla/cmd/surla@latest
curl -fsSL https://releases.herdr.dev/core/install.sh | sh
curl -fsSL https://releases.herdr.dev/app/install.sh | sh
curl -fsSL https://raw.githubusercontent.com/goadesign/chorus/main/scripts/install.sh | sh
```

Before it creates anything, `surla` checks for Herdr, Chorus and its version,
and the browser opener. A missing Herdr or Chorus program is named with its
install command, or with the folder to add to your `PATH` when it is
installed in `~/.local/bin`; an older Chorus is named with
`chorus update -yes`. The reference says what each prerequisite is for under
[Prerequisites](docs/REFERENCE.md#prerequisites).

Surla itself calls no model, so it needs no model key; each coding agent
uses its own sign-in. An application that calls a model needs a key only when
the builder tries prompts on a live target. Export the provider's key, such as
`ANTHROPIC_API_KEY` or `OPENAI_API_KEY`, with any request settings it needs,
in the shell where you run `surla`: `surla` hands them to the server, which
holds them in memory until `surla stop`, and while no model is chosen, live
targets then use `claude-sonnet-4-6` or `gpt-5.4-mini`. The page's
**Live target** view changes that model to another provider whose settings
were handed over, or to a model gateway; see
[`surla` and `surla stop`](docs/REFERENCE.md#surla-serve).

## Start a project

Start from a starter service, or from an empty folder.

**From a starter.** `surla init` creates a service designed with Goa, Goa-AI
and [Model](https://github.com/goadesign/model) (architecture diagrams as Go
code). The service has a first Story, in `design/story.go`, with one finished
flow, and a test module in `eval/` that already holds the test that flow
requires. `surla init` then prints the commands that make the first commit
and start Surla:

```bash
surla init -dir shop -module example.com/shop
cd shop
git init
git add .gitignore README.md AGENTS_QUICKSTART.md go.mod go.sum design gen internal cmd eval
git commit -m "Start shop from surla init"
surla
```

**From an empty folder.** Run `surla` in an empty folder and ask the guide for
a new service. The guide has the server create the starter project in a
folder inside it and make its first commit, without a restart.

`surla` prints what it did and your next step, for example:

```text
Created a new, empty Surla project. Moving or renaming this folder starts a new project.
Team <name>.
Choose an agent for each role in the open Chorus dialog in Herdr.
Surla is running for <project folder> at <address>
```

The page opens through a single-use link and keeps its session only while it
is open: reloading it, closing it or opening it in another tab needs a new
link, which `surla` gives when you run it again. Running `surla` from a
subfolder finds the same project. `surla stop` shuts the project's server
down. After you install another build of `surla`, run `surla stop`, then
`surla`: `surla` does not use a server that another build started. If the
server stops while starting, `surla` shows its last lines, including its
error. If the team cannot be prepared, `surla` still opens the page, then
names what failed and the step that repairs it; **Start agents** on the page
tries again. See [`surla` and `surla stop`](docs/REFERENCE.md#surla-serve)
for the project's state, settings and recovery.

## Use the tools without a team

These commands need no server, no team and no model key. `surla init`,
`surla preview` and `surla gen` run Goa inside Surla's sandbox with the
module's own versions, so no separately installed `goa` is needed. Run them
from an ordinary terminal, not from inside a coding agent's sandboxed shell:
an outer sandbox can prevent Surla's sandbox from starting.

```bash
# Scaffold a starter service and its test module:
surla init -dir shop -module example.com/shop

# Compile shop/design in the sandbox at 0 LLM tokens and print its
# containers, boundary edges, agent policies and tool contracts:
surla preview -dir shop

# List all commands; "surla help <command>" prints a command's flags:
surla help
```

Or clone this repository and look at the [`examples/orders`](examples/orders/README.md)
reference platform:

```bash
git clone https://github.com/goadesign/surla.git
cd surla

# Preview the Orders platform's architecture and tool guardrails:
surla preview -dir examples/orders

# Compare two Goa design modules structurally:
surla diff --old-dir examples/orders --new-dir examples/orders
```

From a checkout you can also run `go run ./cmd/surla`; `go run ./cmd/surla init`
then needs `-surla-version` with a published version, such as
`-surla-version v0.0.0-20261110163737-d9beac313142`.

After you edit a module's `design/`, `surla gen -dir <module>` regenerates its
`gen/` in the sandbox and replaces it in one step. It needs `goa.design/goa/v3`
`v3.35.1-0.20261103205041-35e789e8f9b1` or later and refuses a design with gRPC
transports; see [`surla gen`](docs/REFERENCE.md#surla-gen). The operator tools
`surla verify` and `surla approve` build a commit, run a test suite from a
Git repository you name against it and check a saved report, outside any
server; see
[`surla verify`](docs/REFERENCE.md#surla-verify).

## Why engineers use Surla

### 1. Agents write the contract; the compiler writes the plumbing

Why pay a model to write thousands of lines of HTTP handlers, gRPC stubs,
OpenAPI specs, MCP schemas and JSON validators? In Surla, agents express your
API, agent tools and security rules in a compact, strongly typed design
(`design/*.go`). Goa generates the transports, OpenAPI specs, MCP schemas and
validators from it with no model inference (**0 LLM tokens**), which removes
whole classes of glue-code bugs and the tokens spent writing them.

### 2. A built-in Golden Path that stays cheap to evolve

AI-built codebases decay when every prompt invents a new folder layout, a new
way to call models or a new auth pattern, and later agents spend tokens
reverse-engineering the repository. Surla serves every agent the same
**Golden Path** rules:

- **Predictable layout and generated agent maps.** Every service follows the
  same structure and ships a generated `AGENTS_QUICKSTART.md`, so an agent can
  extend the platform months later with little context.
- **Production patterns by default.** One HTTP/MCP edge service, internal gRPC
  services, a central model gateway with rate limiting and usage accounting,
  durable Temporal workflows, and tool fields such as `tenant_id` and
  `user_id` that the server injects and the model can never set.

### 3. Independent verification of each commit

The agent that writes the code never decides whether it works. Tests come
from the test writer and count only after Surla's own check accepts them.
Verification builds the builder's exact commit inside the sandbox
(`verify/candidate`) and runs the accepted tests against it, with the code
under test running in the sandbox too.

- **Hidden scenarios.** A project can instead store verification settings
  (`surla verification set <file>`, applied when Surla starts again) that
  name a test suite of your own at one commit of a Git repository you choose;
  its builds then add only the builder. The suite's scenarios tagged
  `holdout` are hidden: [`Report.ForBuilders()`](internal/verify/report.go)
  removes their names, inputs and logs from the report the builder sees.
- **Real crash and budget proof.** The [`examples/orders-eval`](examples/orders-eval/)
  reference suite kills the Temporal worker in the middle of a refund
  (`kill -9`), restarts the API while an approval is paused, and fails any
  scenario whose model token use exceeds its budget
  (`TestSeededPayloadBloatFailsTheBudget` proves it with a seeded payload
  bloat).

### 4. Prompt changes without rebuilding

Prompt wording and compiled business rules are kept apart. The builder starts
a live target from a commit and sends new prompt wording to it with
`draft_override`, without rebuilding Go code. A draft aimed at a compiled
business rule changes nothing and says that the rule needs a rebuild and
verification. A draft lasts as long as the live target: to
keep it, the builder commits the text and verifies that commit. The page's
**Live target** view shows which prompts changed and the model live targets
use. See [`start_live_target`](docs/REFERENCE.md#start_live_target),
[`draft_override`](docs/REFERENCE.md#draft_override) and the
[live target prompt contract](docs/REFERENCE.md#live-target-prompt-contract).

### 5. Your decisions on one page

The page lists the project's changes and shows each one's Story, questions,
build, team, verification runs and approval, and updates itself without
losing what you typed. Its **Traces** drawer shows safe summaries of what the
agents and the server did. See the [views page](docs/REFERENCE.md#views-page)
and [operation diagnostics](docs/REFERENCE.md#operation-diagnostics).

> **Why the name "Surla"?** Named after **Tambdi Surla** in Goa, home to the
> carved basalt Mahadeva temple beside the Surla River. In Surla
> (*SOOR-lah*), `design/*.go` is that enduring stone contract: one compiled
> specification every coding agent builds to, and an independent verifier
> checks against.

## Dive deeper

- **[How Surla works](docs/HOW_IT_WORKS.md)**: the journey step by step, by
  who acts.
- **[Reference](docs/REFERENCE.md)**: commands, MCP tools, builds, the page and
  the server's contracts.
- **[Story language](story/README.md)**: the declarations a Story is written
  with.
- **[Golden Path architecture](docs/ARCHITECTURE_PLAYBOOK.md)**: the rules that
  keep Surla platforms maintainable, and who owns what in Surla.
- **[Story flows and the tests a build requires](docs/STORY_TEST_DESIGN.md)**:
  for contributors, how a Story becomes tests and how Surla judges them.
- **[Orders reference platform](examples/orders/README.md)**: a complete Golden
  Path platform with its test suite.
- **[Product direction](docs/PRODUCT_DIRECTION.md)**: what Surla is meant to be
  and for whom.

## License

Surla is released under the [MIT License](LICENSE).
