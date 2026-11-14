# Surla Reference

**[← Back to README](../README.md)** · **[← How Surla Works](HOW_IT_WORKS.md)** · **[← Golden Path Architecture](ARCHITECTURE_PLAYBOOK.md)**

---

This page is for a user or an agent who needs one exact contract: a command,
an operation, who may call it, what it returns and how it refuses.
[How Surla Works](HOW_IT_WORKS.md) explains the journey step by step.

## Terminology at a Glance

| Term | Meaning |
| :--- | :--- |
| **The user** | The person who runs `surla` in a project folder. Only the user confirms a change, starts a build and approves a result, all on the views page: the page the server serves to the user's browser, which the README and How Surla Works call the page. |
| **The server** | The project's Surla server: one per project folder, started by `surla`. It keeps the Change Cards, builds, verification runs and approvals, and it serves the operations below over HTTP and MCP. |
| **Herdr** | The terminal program the server and the agents run in. Its core service is the Herdr core (the commands `herdr-core` and `hc`); its window is the Herdr app (`herdr-app`). A *Herdr space* is the set of tabs that holds one project's terminals. |
| **Chorus** | The messaging service the agents use to talk to each other. Its dialog in Herdr, **the Chorus dialog**, is where the user picks which installed agent app and model play each role. The list of roles the dialog shows is the *roster*. An agent's *watcher* is its background wait for Chorus messages. |
| **The guide** | The agent the user talks to. It writes the Story and proposes and revises changes. |
| **The builder** | The agent that writes the application's code, tries prompt wording on a live target and asks the server to verify its commit. |
| **The test writer** | The agent that writes one test per finished flow, in its own working copy of the project, and reports when they are finished. |
| **Story** | A description, in the project's Go design, of what the system does, written with the [`story`](../story/README.md) language. |
| **Flow** | One path through a Story: who does what, step by step, ending in an outcome. A flow with a start, an outcome and no open question is *finished*; each finished flow requires one test. |
| **Change Card** | The saved record of one change: the request, the guide's summary, its questions and the user's answers, examples, preserved behaviors, the Story and design it was made from, and any approval. |
| **`request_revision`** | The SHA-256 hex digest of a Change Card's content, including its design source and Story. Previews, builds, verification runs and approvals name the revision they are for, so editing the agreed answers or examples, or adopting another revision, leaves them attached to the earlier revision. |
| **Agreed revision** | The confirmed revision of a change, the one *in force* (`in_force: true` on builds and assignments). It stays in force while the guide's proposed revision waits beside it. |
| **Proposed revision** | A revision the guide saved beside an agreed change. The user adopts or declines it on the views page. |
| **Build** | The user's decision to build one agreed revision. It saves that revision's Story and design, with the answers, examples and preserved behaviors the user confirmed, and gives the builder and the test writer their assignments. |
| **The test writer's working copy** | A linked Git working copy of the project at `.surla-tests`, on the branch `surla/tests`, where the test writer works and where Surla commits accepted tests. |
| **Evaluator** | A test program that follows the [evaluator suite command contract](#evaluator-suite-command-contract): it declares its scenarios, runs them against the application's binaries and prints a report. Surla generates the evaluator of a Surla test module, such as the one the test writer works in, from that module's tests; stored verification settings name an evaluator the user maintains. |
| **Scenario** | One named test in an evaluator, such as `prices_tea`. A report gives each scenario's outcome: passed, failed, not run or a harness error. |
| **Tests commit** | The commit at which Surla accepted a build's tests, or the evaluator commit named in stored verification settings. Verification runs those tests. |
| **Stored verification settings** | An evaluator the user stores with `surla verification set`. With them, every change verifies with that evaluator and builds add no test writer. |
| **Live target** | A running copy of the application, built from a commit by the server, on which the builder tries prompt wording. Surla never changes an application the user runs or deployed. |
| **Hidden scenario** | A scenario whose name, inputs, checks and logs are never shown to the builder. An evaluator marks it with the tag `holdout`; the builder sees it only as `holdout-N`. Tests written by the test writer may not use the tag. |
| **Run directory** | The private folder that holds one verification run: its request, its state, its report digest and its full output (`executor.log`). |
| **`CP1` – `CP5`** | The checkpoint labels used in operation descriptions and the server's status lines: **CP1** agreeing the change, **CP2** previewing the architecture, **CP3** trying prompt wording on a live target, **CP4** verification, and **CP5** approval. |
| **`input_binding`** | For one tool an agent in the application uses: its generated input schemas and the fields the compiler marks as injected. It describes the declared choices, not what the running application installed. |

---

## Prerequisites

| Need | Why | How to get it |
| --- | --- | --- |
| **macOS** | Surla builds agent-written code, and runs the application it builds, only inside the macOS sandbox (`sandbox-exec`). Two kinds of agent-written code run elsewhere: the test program the test writer wrote runs on this machine outside the sandbox, with only `PATH` and `TMPDIR` (see [`verify_change`](#verify_change)), and `check_project` runs the project's tests in Linux containers (see **Docker** below). On another system every sandboxed step refuses with `candidate isolation is unavailable on this platform`, and a build start without stored verification settings refuses with `sandbox_unavailable`. | — |
| **Go 1.27.3 or later** | `surla` is a Go program, and the project's checks use Go. Surla runs Goa inside its sandbox with each module's own versions, so no `goa` binary is needed. | [go.dev/dl](https://go.dev/dl/) |
| **Surla** | The command itself. | `go install github.com/goadesign/surla/cmd/surla@latest` |
| **Herdr core** (`herdr-core` and `hc`) | The server and every agent run in Herdr terminals. | `curl -fsSL https://releases.herdr.dev/core/install.sh \| sh` ([herdr.dev/core](https://herdr.dev/core/)) |
| **Herdr app** (`herdr-app`) | The window that shows the team's panes. | `curl -fsSL https://releases.herdr.dev/app/install.sh \| sh` ([herdr.dev](https://herdr.dev/)) |
| **Chorus v0.11.0 or later** | Agent messaging and the Chorus dialog. | `curl -fsSL https://raw.githubusercontent.com/goadesign/chorus/main/scripts/install.sh \| sh`; update with `chorus update -yes` |
| **A coding agent** that the Chorus dialog can start | An agent app plays each role: the user picks one per role in the Chorus dialog. | — |
| **Git**, with a name and address | Without stored verification settings, builds, live targets and verification need the project in a Git repository with a first commit, and Surla records accepted tests as commits under your Git identity. | `git config --global user.name …` and `user.email …` |
| **Docker** (Docker Desktop, or another local engine on a `unix://` socket) | `check_project` and `update_dependencies` run Go in Linux containers. The first check pulls `golang:1.27.3`. | [docker.com](https://www.docker.com/) |
| **Temporal CLI** | Only for the full-suite tests that run the Orders evaluator: the `examples/orders` acceptance suite and the `e2e` tests of `./internal/verify` and `./cmd/surla`. | `brew install temporal` |

Before it creates any state, `surla` checks for `herdr-core`, `herdr-app` and
`chorus` on the path and names the install command of the one that is
missing. When that program is in `~/.local/bin`, where the installers put it,
it says instead `<program> is installed in <folder>, which is not on PATH;
add <folder> to PATH, then run surla again`. It then checks that Chorus is
v0.11.0 or later (`chorus update -yes` updates it), that `hc` is on the path
or in `~/.local/bin`, and that the browser opener (`open`) is on the path. A
missing opener is named without an install command.

**The sandbox and restricted shells.** When a sandboxed command exits with
status 71, Surla keeps the error and adds: `The sandboxed command exited with
status 71. If Surla is running inside a restricted coding-agent shell, its
sandbox may have been blocked; run Surla from an unrestricted host shell.` This
happens when `surla preview`, `surla gen` or `surla init` run inside an agent's
own sandbox. Run them from your own shell, or let the agent call the matching
operation (`preview_architecture`, `generate_code`, `init_project`) on the
server, which runs in its own Herdr terminal. Surla never retries outside its
sandbox.

---

## Commands

`surla help`, `surla -h` and `surla --help` print the command list to standard
output and exit 0. `surla help <command>` and `surla <command> -h` print that
command's flags to standard error and exit 0. Every command refuses
unexpected positional arguments: `surla stop` with `surla: stop takes no
arguments`, `surla verification` and `surla promotion` with their usage line
(`surla: usage: surla verification set <file> | show | remove`), and the
others with `surla: unexpected arguments [...]`. An unknown command, such as
`surla build`, answers `surla: unknown command "build"; run "surla help" to
list the commands`.

<a id="surla-serve"></a>
### `surla` and `surla stop`

**Input:** `surla`, run in a project folder. **Who acts:** the command starts
the folder's server, or attaches to the one already running, then prepares the
team and opens the views page. **What the user sees**, in order:

```text
Created a new, empty Surla project. Moving or renaming this folder starts a new project.
Team shop-3f9c1a2b4d5e.
Choose an agent for each role in the open Chorus dialog in Herdr.
Surla is running for /path/to/shop at http://127.0.0.1:52011
```

The first line appears only when the folder had no project. The second names
the team, after the folder and the first 12 characters of the project's
identifier. The third is the user's next step, chosen by the server:

| Chorus dialog | Step printed |
| --- | --- |
| Opened for this project | `Choose an agent for each role in the open Chorus dialog in Herdr.` |
| Already open | `If the open Chorus dialog is for this project, click "Reload team", then choose the agents. If it is for another project, close it, then start the agents again (Start agents on the views page, or run surla).` |
| Not needed | `Every listed role is running.` |

Then the browser opens the views page, and the last line prints. When the
browser cannot be opened, `surla` stops with `open project views: …` instead
of the last line; the server and the team keep running. When the team cannot
be prepared, `surla` prints no team lines but still opens the views page,
because confirming and approving there need no agent, prints the last line,
and then fails with `surla: the project team is not ready: ` followed by
what failed and the step that repairs it (see
[`ensure_team`](#ensure_team-management)); **Start agents** on the views
page tries again.

**The team.** Once the server answers, `surla` starts the Chorus service
(`chorus up`) and then the Herdr app when either is not running. The server
owns the team: its stable Chorus name, each role's handle and credential, the
Herdr space, and the roster the Chorus dialog reads. Before any build, the
roster holds only the guide; a build adds the builder and, unless
verification settings are stored, the test writer. When a listed role has no
agent running, or its pane holds only an idle shell, Surla focuses this
project's space and opens the Chorus dialog, where the user picks an agent
app and a model for each role. When every listed role is running, no dialog
opens. The guide's pane is on the left of the team's tab; the builder's pane
is to its right, and the test writer's below the builder's. Starting again
reuses the guide and the saved space. If the space was closed, the server
adopts the one local space that matches its team name and folder, or creates
one; when several match, it refuses with their count, and the user closes the
duplicates. Team preparation needs no Git.

<a id="idle-shell"></a>

**Idle shell detection.** Surla treats a pane's foreground program as an *idle
shell* when both hold:

- Its executable name, with leading dashes removed, is `sh`, `bash`, `zsh`,
  `fish`, `dash`, `ksh`, `csh`, `tcsh`, `nu`, `pwsh`, `xonsh` or `elvish`, or
  the name of the user's own `$SHELL`.
- Neither of the first two arguments after the program is `-c` or a
  single-dash flag group containing `c`, such as `-lic`.

A shell started with such a command flag counts as running. A live pane that
reports no foreground program also counts as running.

**The project and its state folder.** A project is a folder, with or without
Git. Surla resolves symlinks in the path and uses the nearest project record
in that folder or its parents, so running `surla` inside a project the guide
created with `init_project` finds the original project. Moving or renaming the
folder starts a different, empty project. Each project's private state lives
outside the folder, in
`<UserConfigDir>/surla/operator/project-<digest>/`, where `<UserConfigDir>` is
the user's configuration folder (on macOS, `~/Library/Application Support`).
It holds the owner's account identity, one saved `127.0.0.1` port, the Change
Cards and builds (`cards/`), verification runs (`runs/`), reports
(`reports/`), agent credentials (`agents/`), stored settings, and the running
server's record. A new project saves a free port that no other project of
this account saved, running or not, and keeps it. No caller chooses an
identity, address, token or storage path.

`surla` finds that folder from the shell where it runs and starts the server
with it. The server never looks the folder up itself, because Herdr starts it
with the environment of the Herdr core, not the environment of your shell.
For the same reason, a variable the server reads, such as a model provider's
key or a Docker setting, must be in the Herdr core's environment. The server
refuses a state folder that is not private to your account, holds no project,
belongs to another project folder, lies inside the project folder, or is not
named by its physical path.

When `surla`, `surla stop` or a settings command cannot use a project's
record or state folder, it names them and the repair. `<record>` is the
`project.json` in the project's state folder, and the folder that holds
every project's state folder is `<UserConfigDir>/surla/operator`:

| What cannot be used | The error |
| --- | --- |
| The record cannot be read, for example because it is not a private file of this account | `the record of the Surla project in <project folder> cannot be read: <record>: <cause>; make it a regular file of this account that only this account can read and write (chmod 600 <record>, and chown it to this account if another account owns it), then run surla again` |
| The record is damaged | `the record of the Surla project in <project folder> is damaged: <record>: <cause>; restore it from a backup and run surla again, or move the folder <state folder> out of <UserConfigDir>/surla/operator and run surla in <project folder> to start that project folder again as a new, empty Surla project` |
| `<UserConfigDir>/surla/operator` exists but is not a folder of this account that only this account can open; it is checked before any record is read | `prepare operator storage root <folder>: <cause>; make it a folder of this account that only this account can open (chmod 700 <folder>, and chown -R it to this account if another account owns it), then run surla again` |
| A project's state folder is closed to this account or owned by another, or a new project cannot list the folder that holds them | `the Surla state folder <folder> cannot be used: <cause>; make it a folder of this account that only this account can open (chmod 700 <folder>, and chown -R it to this account if another account owns it), then run surla again` |
| The project's state folder cannot be created | `project state folder <folder> could not be created: <cause>; make <UserConfigDir>/surla/operator a folder this account can write, then run surla again` |

**Starting, attaching and failures.** A start lock serializes simultaneous
commands. A running server is attached to only after it proves the project's
identity and reports the build it started from, the SHA-256 of its
executable, which must be that of the `surla` attaching. A server another
build started, such as the one still running after a new `surla` is
installed, is refused with `surla: this project's server was started by
another build of surla; run surla stop, then surla`; `surla stop` stops a
server of any build. A record left by a server that died is stale and is
removed by the next start. The server runs in its own Herdr terminal, labeled
`surla_project=<project id>`, which outlives the shell that ran `surla`. That
terminal shows the server's status lines: the address, the Change Card folder
and, for each checkpoint, whether it is on. The `CP4` line says whether
verification uses the evaluator in stored verification settings or the tests
accepted for each change's build.

Before it starts the server's terminal, `surla` checks that the saved port is
free. When it is taken, `surla` starts nothing and names what holds it: the
running server of another Surla project that saved the same port, with
`surla: project port 52011 is held by the Surla server of the project in
<folder>, which saved the same port, so only one of the two can run at a
time; no server was started; run surla stop in that folder, then run surla
here`, or otherwise another program, with `surla: another program is
listening on 127.0.0.1:52011, the address saved for this project; no server
was started; stop that program, then run surla (lsof -nP -iTCP:52011
-sTCP:LISTEN names it)`. When the server's terminal ends before the server
answers, for example because it cannot read its stored settings, or because
another program took the port after that check, `surla` reads what that
terminal printed, removes the terminal, and shows the last lines (at most 20
lines and 4 KiB, with control characters removed):

```text
surla: the project server stopped while starting, and its last lines were:
  surla: project port 52011 is occupied: listen tcp 127.0.0.1:52011: bind: address already in use; stop the program listening on it, then run surla
```

If those lines cannot be read, the error says so and keeps the original
failure. A server whose terminal still runs but has not answered within the
command's deadline keeps its terminal, and the error names it so you can read
the server's output there. A taken port leaves the saved port and credentials
in place.

**`surla stop`.** Asks the server to shut down in order and waits until it has
released the project, then prints `Surla stopped.`, or `Surla is stopped.`
when no server was running. Before its listener closes, the server revokes its
management credential and withdraws its running record. Interrupt,
termination and hangup signals cause the same shutdown. A killed server is
recovered by the next `surla`.

**Credentials and browser access.** Each server creates a management
credential in memory and publishes it in its private running record after
binding its port; `surla` and `surla stop` read it there for each request,
use the literal local address without a proxy, refuse redirects, and never
print it. Management can check the project's identity, prepare the team,
stop the server and mint a browser link; it cannot confirm, build or approve.
Agent credentials survive restarts and are removed with their agent; agent
connection settings and rosters name only their own credential files.

`surla` opens only the saved views address with a single-use link in the
address fragment. The link expires after two minutes and is exchanged once
for a browser session (see [Views page](#views-page)). Every response
disables MIME sniffing, and every request must use the saved host and, when
present, the server's own origin.

All processes run as the same operating system account. File permissions keep
other accounts out, not other programs of that account: a deliberate program
could read another role's credential or run `surla` to obtain a browser link.
The roles are a separation of duties, not isolation from a malicious agent.

### `surla init`

Creates a starter project in `<dir>`: a Goa and Goa-AI service with an agent
and a `goa.design/model` architecture, and a test module in `<dir>/eval`. It
resolves dependencies, generates each module's code inside Surla's sandbox
with the generator [`surla gen`](#surla-gen) uses, runs `go vet ./...` and
`go test ./...` in both modules, and prints the commands that make the first
commit and start `surla` in the project. It builds the project in protected
temporary storage on the destination's file system and moves it to `<dir>` in
one step only when everything succeeded; a failure or interrupt leaves `<dir>`
absent, and it never replaces an existing entry.

- `-dir <path>`: the folder to create; it must not exist *(required)*.
- `-module <path>`: the service's Go module path, such as `example.com/shop`
  *(required)*. Its last element names the service and must be a lowercase Go
  identifier (`^[a-z][a-z0-9]*$`, not a Go keyword or `main`).
- `-surla-version <version>`: the exact semantic version of
  `github.com/goadesign/surla` that `eval/go.mod` requires. It defaults to the
  version the `surla` binary was built from and is required for a `(devel)` or
  `+dirty` build; `latest` and branch names are refused.

**What the starter holds.** `cmd/<name>` runs a `helper` agent that prices
items through a tool and reaches a model through `-model-gateway <host:port>`;
it registers the prompt `<name>.helper.system`.

`design/story.go` declares the service's Story with one finished flow,
`prices-cookie`. The service's `go.mod` requires the Story language's module,
`github.com/goadesign/surla/story`, at the version Surla itself requires.

`eval/` is the test module. It declares three scenarios, all tagged `repair`
(`prices_tea`, `unknown_item` and `prompt_override_reaches_model`), runs them
against a scripted model, and implements the
[live target prompt contract](#live-target-prompt-contract). Its hand-written
parts are `eval/design` (its own copy of the service's public contract and
the scenarios), `eval/acceptance` (the scenarios' implementation and the
program's entry points) and `eval/scripted` (the scripted model); its
program, `eval/gen/cmd/<name>-evals`, is generated with the rest of
`eval/gen/`. `eval/acceptance` also holds the test the Story's finished flow
requires, the method `PricesCookie`, so the first build of a change needs
only the tests of the flows the guide adds. This test module is the layout
builds and verification rely on.

### `surla verification` and `surla promotion`

Each takes `set <file>`, `show` or `remove` and works with or without a
running server. `set` reads a JSON file outside the project folder and its
Git working tree, refuses unknown fields and trailing data, resolves relative
paths against the file's folder, and stores a private copy; a refused file
leaves the earlier settings in place. `show` prints the stored settings;
`remove` deletes them. The server reads them when it starts, so a change takes
effect after `surla stop` and `surla`.

**Verification settings** name an evaluator Surla runs instead of tests
written by the test writer:

| Field | Meaning |
| --- | --- |
| `candidate_repo`, `candidate_module` | The repository and module folder of the application. |
| `binaries` | Binary name to package path, such as `{"orders": "./cmd/orders"}`. |
| `suite_repo`, `suite_commit`, `suite_module`, `suite_command` | The evaluator's repository, full commit ID, module folder and program package. |
| `timeout` | A positive duration for one run, such as `"30m"`. |

With stored verification settings, every change verifies with that evaluator,
builds add only the builder, and live targets run that evaluator. Without
them, `verify_change` runs the tests Surla accepted for the change's build,
and a live target runs the test module of the commit being tried. Runs and
reports always go to the project's state folder.

**Promotion settings** are optional: approval needs none. When they are
stored, each approval also checks the release binaries the user published:

| Field | Meaning |
| --- | --- |
| `release_dir` | A folder outside the project folder and its Git working tree. Publish each `<release_dir>/<candidate_commit>/` complete, in one rename, and do not edit it afterwards. |
| `binaries` | Distinct plain names of the release binaries in that folder. |

Approval hashes those binaries again in `<release_dir>/<candidate_commit>/`,
through one folder handle. A missing, non-regular or escaping file, or one
that differs from the binary the run verified, prevents approval; the
refusal names no folder path and ends with the step "publish the binaries
the run built into the release folder that the promotion settings name, in a
folder named after the commit, or remove the promotion settings". Without
promotion settings, an approval records no release binaries. A stored file
the server cannot accept, such as one with any other field (`suite_id`),
stops the server while it starts; the shown error ends by naming
`surla promotion show` to see the settings, `surla promotion set <file>` to
store them again and `surla promotion remove` to remove them, then `surla`.
Approval never merges or deploys.

### `surla preview`

Reads a design module into a snapshot (`go.mod`, `go.sum` and non-test `.go`
files, never following symlinks; run `go mod tidy` first if `go.sum` is
missing), evaluates it inside the sandbox, and prints the architecture as
indented JSON to standard output: `system_name`, `declarations`, C4
`containers` and `edges`, `agent_policies`, `tools`, and `stories` when the
design declares Stories. Then it writes one
summary line to standard error, such as
`surla: "orders": 3 container(s), 2 boundary edge(s), 1 agent policy(s), 4 tool contract(s)`.
It needs no server, no Change Card and no model key. Interrupt and termination
signals cancel it; it removes its temporary folders before exiting, or names
one it could not remove.

- `-dir <path>`: the module's root, holding `go.mod`, `go.sum` and `design/`
  (default `.`).

### `surla diff`

Evaluates two design modules, as `surla preview` does, at their own
dependency versions, and prints the structural `DiffResult` as JSON:
declarations added, removed and changed, dependency version changes and a
summary. It gives no compatibility verdict.

- `--old-dir <path>`: the earlier module *(required)*.
- `--new-dir <path>`: the later module *(required)*.

### `surla gen`

Regenerates a design module's `gen/` from its `design/` inside the sandbox and
replaces `gen/` in one atomic rename, so `gen/` is never missing or
half-written. Generated subfolders the design does not produce disappear,
and top-level files in `gen/` that Surla did not generate are kept. On success
it prints `gen/ updated`, the files added, changed and removed, and a reminder
that it did not build the application against the regenerated code; after
the swap, problems are only warnings. Before the swap, any failure or
interrupt leaves `gen/` untouched and exits non-zero. It never changes
`go.mod` or `go.sum`, and says to run `go mod tidy` when generated code
imports a module `go.mod` lacks. Leftover `.surla-gen-*` folders from an
interrupted run are listed, never deleted, because another run's staging
folder looks the same.

- `-dir <path>`: the module's root (default `.`).

| The module | What generation also writes |
| --- | --- |
| Declares agents | `AGENTS_QUICKSTART.md`, Goa-AI's guide to the design's agents and tools. Surla replaces its parts that name commands to run with steps that use Surla's operations, and replaces its evaluation section when the design declares no evaluation suite. |
| Is a Surla test module: its path is `<service module>/eval`, its design declares exactly one evaluation suite, and it has an `acceptance` package | The program `gen/cmd/<service>-evals/main.go`, built from the generated tests and the `acceptance` package's `Name`, `Hooks`, `New`, `Live` and `LiveThroughGateway`. A missing scenario method then fails to compile. Every run declares the environment `scripted-model`. `AGENTS_QUICKSTART.md` gains a section naming those five names. |

Generation needs `goa.design/goa/v3` `v3.35.1-0.20261103205041-35e789e8f9b1`
or later; with an older Goa the build fails and the error names the Goa
version in use. The named-section rewrite of `AGENTS_QUICKSTART.md` needs
`goa.design/goa-ai` `v0.90.1-0.20261110011046-f32bd628f2bf` or later. A design
with gRPC transports is refused, because protoc cannot run in the sandbox;
generate it with the module's own `go run goa.design/goa/v3/cmd/goa gen`.
`generate_code` writes the same files. The test writer never regenerates its
test module: Surla does that with the required tests at each build start and
report.

### `surla verify`

An operator tool that checks a commit without a server. It clones the
committed application and evaluator into a private work folder, builds the
application's binaries and the evaluator's program inside the sandbox, runs
the evaluator, writes the full report to `<report-dir>/<digest>.json`, and
prints the builder view of the report (hidden scenarios shown only as
`holdout-N`) to standard output. Then it writes one summary line to standard
error, such as
`surla: verification report did not pass: 1/3 scenarios passed; failed: 1, not run: 1, harness errors: 0; not passing: "refund_over_limit", "slow_refund" (report 4f2c…)`.
A report that did not pass is written and printed before the command exits
non-zero; a setup or build failure prints no report.

| Flag | Meaning |
| --- | --- |
| `-candidate-repo <path\|url>` *(required)* | Repository holding the application. |
| `-candidate-commit <sha>` *(required)* | Full commit ID to build. |
| `-candidate-module <dir>` | Module folder inside the repository (default `.`). |
| `-binary <name=package>` *(repeatable, at least one)* | Binary name and package path, `.` or `./…`, such as `orders=./cmd/orders`. |
| `-suite-repo <path\|url>` *(required)* | Repository holding the evaluator. |
| `-suite-commit <sha>` *(required)* | Full commit ID of the evaluator. |
| `-suite-module <dir>` | Evaluator module folder (default `.`). |
| `-suite-command <package>` *(required)* | The evaluator program's package, such as `./cmd/orders_acceptance-evals`. |
| `-request-revision <digest>` *(required)* | The `request_revision` recorded in the report. Without a Change Card, any label works if `surla approve` gets the same one. |
| `-work-dir <path>` *(required)* | A missing or empty private folder that agents cannot read. |
| `-report-dir <path>` *(required)* | Where the full report is written; keep it from the builder when scenarios are hidden. |
| `-timeout <duration>` | Limit for the run (default `0`, no limit). A stopped run prints any partial report the evaluator saved and exits non-zero; a partial report can pass. |
| `-builder-view` | Default `true`. `-builder-view=false` prints the full report and names hidden scenarios in the summary. Raw diagnostics of a stopped run go to standard error in either view; do not show them to the builder. |

#### Evaluator suite command contract

An evaluator program implements this contract with
[`suite.Main(suite.Command{...})`](../verify/suite/suite.go). Surla's own test
layout generates it; an evaluator named in stored verification settings
implements it itself:

```go
func main() {
    suite.Main(suite.Command{
        Environment:    "sim",
        Binaries:       []string{"orders", "orders-worker"},
        MaxConcurrency: 4,
        Declare:        declare,
        Suite:          newSuite,
    })
}
```

1. **`--describe`.** The program prints its declaration and exits 0, without
   candidate binaries:
   ```json
   {
     "id": "orders_acceptance",
     "environment": "sim",
     "scenarios": [
       {"id": "store_credit_within_limit", "tags": ["repair"]},
       {"id": "other_customers_order_refused", "tags": ["holdout"]}
     ]
   }
   ```
   A scenario tagged `holdout` is hidden in every builder view.
2. **`--artifact <name>=<path> ...`.** One flag per bound binary. The program
   checks the flags against `Binaries`, pins each binary into a read-only
   store with [`candidate.Pin`](../verify/candidate/candidate.go), runs the
   scenarios, and prints a `goa.design/goa-ai/eval.Report`. Each scenario
   starts the application with
   [`candidate.Exec`](../verify/candidate/candidate.go) inside the sandbox and
   attaches the evidence of every binary it used, such as
   `pinned["orders"].Evidence("orders")`, which `surla verify` compares with the
   digest it measured; missing or different evidence makes the scenario a
   `harness_error`. Exit codes: `0` all passed; `1` the suite ran and a
   scenario failed; `2` the suite could not run or finish cleanly. The program
   also accepts `--scenario <id>`, or `--tag <tag>`, and `--max-concurrency <n>`
   for direct runs.
3. **`--live --artifact <name>=<path> ...`** *(optional)*. Starts the
   application for a live target and is never used by verification. The
   program reads the live key from `SURLA_LIVE_TARGET_KEY`, calls
   `Command.Live`, prints one line `{"url":"http://127.0.0.1:<port>"}`, and
   serves until interrupted or until the target ends. A program without
   `Live` exits 2 with `this evaluator suite does not support --live`.

[`examples/orders-eval/cmd/orders_acceptance-evals/main.go`](../examples/orders-eval/cmd/orders_acceptance-evals/main.go)
and [`examples/orders-eval/acceptance/hooks.go`](../examples/orders-eval/acceptance/hooks.go)
are a complete example.

### `surla approve`

An operator tool that checks a saved full report against explicit promotion
facts and prints a JSON summary (`report_digest`, `binding`, `counts`). It
exits 0 when the report passed and matches every flag, and 1 otherwise. It
records nothing, contacts no server, and merges and deploys nothing.

| Flag *(all required)* | Meaning |
| --- | --- |
| `-report <path>` | A full report written by `surla verify`. |
| `-candidate-commit <sha>` | The commit being promoted. |
| `-request-revision <digest>` | The revision the report must name. |
| `-suite-id <id>`, `-suite-commit <sha>`, `-environment <env>` | The evaluator and environment the report must name. |
| `-artifact <name=sha256>` *(repeatable)* | Each binary's 64-hex-digit SHA-256 digest. |

### `surla version`

Prints the module version the binary was built from, or `(devel)`.

---

<a id="mcp-tools--resources"></a>
## Callers and refusals

The server serves every operation as an HTTP route and, for agents, most of
them as MCP tools at `POST /mcp`, on its `127.0.0.1` address. Each request
carries a credential, and the credential fixes the caller's kind:

| Caller | Credential | What it is trusted with |
| --- | --- | --- |
| The user | A browser session from the views page | Agreeing changes, building, approving, the live target's model, and the team's panes |
| The guide | Its agent credential | Writing the Story and proposing and revising changes, and creating the project |
| The builder | Its agent credential | Code, checks, live targets and prompt drafts, and asking for verification |
| The test writer | Its agent credential | Reading its test assignments and reporting them finished |
| Management | The server's management credential, used by `surla` and `surla stop` | The server and its team, never a change |

One table decides which caller may call each operation. It governs HTTP
calls, MCP calls and MCP discovery, so an agent's tool list shows only what
its role may call. Every resource belongs to the project's owner, the
operating system account that created the project; the user and the agents
all act for that owner.

| Operation | User | Guide | Builder | Test writer | Management | Offered as |
| --- | :---: | :---: | :---: | :---: | :---: | --- |
| [`list_changes`](#list_changes) | ✓ | ✓ | ✓ | | | MCP tool and HTTP |
| [`wait_for_changes`](#wait_for_changes) | ✓ | | | | | HTTP |
| [`get_change`](#get_change) | ✓ | ✓ | ✓ | | | MCP tool and HTTP |
| [`propose_change`](#propose_change) | | ✓ | | | | MCP tool and HTTP |
| [`revise_change`](#revise_change) | | ✓ | | | | MCP tool and HTTP |
| [`confirm_intent`](#confirm_intent) | ✓ | | | | | MCP tool and HTTP |
| [`decline_revision`](#decline_revision) | ✓ | | | | | MCP tool and HTTP |
| [`refresh_story`](#refresh_story) | | ✓ | | | | MCP tool and HTTP |
| [`get_story_refresh`](#get_story_refresh) | ✓ | ✓ | ✓ | | | MCP tool and HTTP |
| [`surla://golden-path`](#mcp-resources) (`golden_path`) | ✓ | ✓ | ✓ | ✓ | | MCP resource and HTTP |
| [`init_project`](#init_project) | | ✓ | | | | MCP tool and HTTP |
| [`generate_code`](#generate_code) | | | ✓ | | | MCP tool and HTTP |
| [`check_project`](#check_project) | | | ✓ | | | MCP tool and HTTP |
| [`update_dependencies`](#update_dependencies) | | | ✓ | | | MCP tool and HTTP |
| [`read_check_output`](#read_check_output) | ✓ | ✓ | ✓ | | | MCP tool and HTTP |
| [`preview_architecture`](#preview_architecture) | ✓ | ✓ | ✓ | | | MCP tool and HTTP |
| [`diff`](#diff) | ✓ | ✓ | ✓ | | | MCP tool and HTTP |
| [`start_build`](#start_build) | ✓ | | | | | HTTP |
| [`get_build`](#get_build) | ✓ | ✓ | ✓ | ✓ | | MCP tool and HTTP |
| [`get_test_findings`](#get_test_findings) | ✓ | | | | | HTTP |
| [`list_builder_assignments`](#list_builder_assignments-and-get_builder_assignment) | | | ✓ | | | MCP tool and HTTP |
| [`get_builder_assignment`](#list_builder_assignments-and-get_builder_assignment) | | | ✓ | | | MCP tool and HTTP |
| [`list_test_writer_assignments`](#list_test_writer_assignments-and-get_test_writer_assignment) | | | | ✓ | | MCP tool and HTTP |
| [`get_test_writer_assignment`](#list_test_writer_assignments-and-get_test_writer_assignment) | | | | ✓ | | MCP tool and HTTP |
| [`report_tests_finished`](#report_tests_finished) | | | | ✓ | | MCP tool and HTTP |
| [`start_live_target`](#start_live_target) | | | ✓ | | | MCP tool and HTTP |
| [`stop_live_target`](#stop_live_target) | ✓ | | ✓ | | | MCP tool and HTTP |
| [`get_live_target`](#get_live_target) | ✓ | | ✓ | | | MCP tool and HTTP |
| [`draft_override`](#draft_override) | | | ✓ | | | MCP tool and HTTP |
| [`configure_model`](#configure_model) | ✓ | | | | | MCP tool and HTTP |
| [`get_model_connection`](#get_model_connection) | ✓ | | | | | HTTP |
| [`verify_change`](#verify_change) | | | ✓ | | | MCP tool and HTTP |
| [`get_verification`](#get_verification) | ✓ | ✓ | ✓ | ✓ | | MCP tool and HTTP |
| [`list_verifications`](#list_verifications) | ✓ | ✓ | ✓ | ✓ | | MCP tool and HTTP |
| [`approve_merge`](#approve_merge) | ✓ | | | | | MCP tool and HTTP |
| [`list_traces`](#list_traces) | ✓ | ✓ | ✓ | | | MCP tool and HTTP |
| [`get_team`](#get_team) | ✓ | | | | | HTTP |
| [`show_team_pane`](#show_team_pane) | ✓ | | | | | HTTP |
| [`start_team_agents`](#start_team_agents) | ✓ | | | | | HTTP |
| `identity` (`GET /project/identity`) | | | | | ✓ | HTTP |
| [`ensure_team`](#ensure_team-management) (`POST /project/team`) | | | | | ✓ | HTTP |
| `stop` (`POST /project/stop`) | | | | | ✓ | HTTP |
| `browser_link` (`POST /project/browser-link`) | | | | | ✓ | HTTP |

The user's browser session sends HTTP only, so the user-only MCP tools
(`confirm_intent`, `decline_revision`, `configure_model`, `approve_merge`)
appear in no agent's tool list. `surla` and `surla stop` call the management
operations. The views page exchanges its single-use link with
`POST /project/browser-session`, which needs no credential because the link
is one.

### How a call is refused

A call refused for its credential, its caller or its fields changes nothing,
and so does a refusal made before the operation's work starts, such as an
unknown `change_id` or a stale `request_revision`. Each operation's entry
says what a later refusal may leave. For example, `start_live_target` stops
the running target only once it has accepted the commit to start, so a start
refused for its commit, repository or folder leaves the running target and its
prompt drafts in place, while one whose build or start then fails has already
stopped it. `update_not_confirmed` means the live target may or may not have
applied the text. Refusals take these shapes:

| Cause | HTTP | MCP |
| --- | --- | --- |
| No credential, or one the server does not hold (for example after a restart) | `401` with the message `open the project views with surla; session or credential is no longer valid` | The same `401` on `POST /mcp` |
| The caller's kind may not call the operation | `403` `forbidden`, message `caller may not call <operation>` | A tool error result (`isError: true`) with the same message |
| A field the route does not declare, at any depth, or a structurally invalid value | `400`, such as `decode_payload` naming the field (`json: unknown field "id"`), `missing_field` or `invalid_pattern` | A tool error starting with `invalid arguments:` that names the field |
| A named domain refusal, listed with each operation | The status the route declares, such as `404` `not_found`, `409` `conflict` or `422` `unprocessable` | A tool error result with the same message |
| An unexpected failure | `500`, with `fault: true` | A JSON-RPC error when the server marks the failure as a fault; otherwise a tool error result |

The messages in the first two rows are those of the operations agents and the
views page call. The management operations answer the same statuses with
messages of their own.

An HTTP error body is Goa's error result:

```json
{"name": "conflict", "id": "…", "message": "…", "temporary": false, "timeout": false, "fault": false}
```

`name` is the refusal's name. `message` says what went wrong. Many messages
end with the step that recovers and who takes it; each operation's entry
below lists its named refusals and when they happen.
Each route's declared refusals are in [`design/`](../design/) and in the
generated OpenAPI description, [`gen/http/openapi3.yaml`](../gen/http/openapi3.yaml).

---

<a id="mcp-tools"></a>
## Operations

Each operation lists its input and output types, what the server does, and
its named refusals. The types and every field's description are in
[`design/`](../design/) and in the MCP input schemas agents receive.

### Changes

#### `list_changes`

`GET /changes`. **Input:** none. **Output:** `ChangeList` (`changes`,
`version`, `approval_available`).

Lists the project's Change Cards, most recently saved first (ties by
`change_id`). Each item has `change_id`, `summary`, `status`
(`needs_confirmation` or `confirmed`), the `request_revision` in force,
`proposed_revision` while a proposed revision waits, `saved_at`, and
`latest_verification`, absent when no run was started. `approval_available` is
`false` when the server runs no verification and has no promotion target, the
release check that stored promotion settings configure. It is also `false`
when resolving that target fails: the list still answers, and the error is
recorded on the read's span
(`surla.controlplane.list_changes`, or `surla.controlplane.wait_for_changes`
on each of its polls). A server started by `surla` always runs verification,
with or without promotion settings, so there it is `true`.
`version` changes whenever a listed value changes, and also when a build, the
team, the Story evaluation or the live target changes. A card, build or run
that cannot be read fails the call instead of being left out.

`latest_verification` describes the change's most recently accepted run:

| Field | Meaning |
| --- | --- |
| `run_id` | The run, as [`get_verification`](#get_verification) reads it. |
| `state` | `running`, `passed`, `failed` or `not_completed`. |
| `inputs_current` | `true` when the run names the agreed revision and the tests a run of that revision would use. Without stored verification settings, the change's build must be of the agreed revision and its accepted tests commit must be the run's; a later report, or the acceptance of another commit, makes the fact `false`. With stored verification settings, the run must have used the evaluator those settings name: the same repository, commit, module and command. |

The latest run stays readable as history whatever `inputs_current` says. The
fact compares the revision and the tests only: it says nothing about working
files, a deployment or an approval.

#### `wait_for_changes`

`GET /change-updates?since=<version>`. **Output:** `ChangeListVersion`
(`version`).

The views page's waiting read. It answers as soon as the change list's
`version` differs from `since`, or after a few seconds with the same version.
It is traced but never saved in the diagnostics history.

#### `get_change`

`GET /changes/{change_id}`. **Output:** `ChangeCardResult`.

Reads one Change Card and changes nothing. The card's fields describe the
agreed revision, or the only revision while none is confirmed; `answers`
holds the user's answers to its questions. A proposed revision waiting beside
an agreed change is returned in `proposed_revision` with its `prompt`,
`summary`, `ambiguities`, `examples`, `preserved_behaviors`, `stories`,
`request_revision`, `ui_composition` and `changed_design_files` (the
`design/*.go` files that differ from the ones the agreed revision was made
from). The server checks each saved revision's digest when it reads the file.
`not_found` (`404`) means no card has this `change_id`.

A saved approval is returned in `approval` only while the tests chosen for
its revision are still at the approval's `suite_commit` and, when promotion
settings are stored, the release binaries still have the approved digests. A
later report of the tests, the acceptance of other tests, a build that does
not accept the same commit again, or changed release binaries hide it; the
card file keeps it. A build accepts the same commit again only while
`surla/tests` still points at it and the regenerated tests are identical. A
build that brings the working copy to a later project commit, such as the
user's merge of the tests, moves `surla/tests` there, so the approval stays
hidden. When the tests still match and promotion settings are stored but the
release binaries cannot be read, `unverified_approval` carries the saved
approval and this `reason`: "Saved approval for candidate
commit <commit> could not be verified because the release binaries could not
be read. Publish the binaries the run built into the release folder that the
promotion settings name, in a folder named after the commit, and press Check
current approval; or remove the promotion settings with surla promotion
remove and restart Surla; or press Approve this result on a newly verified
run." The read records why on its `controlplane.check_approval` span: the
event `approval.hidden_tests_changed` when the tests changed, and the error
when the release binaries cannot be read.

#### `propose_change`

`POST /changes/propose`. **Input:** `change_id`, `prompt`,
`story_refresh_op_id?`, `analysis`. **Output:** `ChangeCardResult`.

Saves the guide's analysis of a request as a Change Card waiting for the
user. Surla runs no model to propose or revise, so these operations need no
model key. The guide reads the project's `design/*.go` and
[`surla://golden-path`](#mcp-resources), then sends `analysis`:

| Field | Content |
| --- | --- |
| `summary` | The change in plain words. |
| `ambiguities` | The questions only the user can answer, each a `question` with at least two distinct `options`; empty when nothing is open. |
| `examples` | At least one concrete example: `scenario`, `input_summary`, `expected_outcome`, and `verification_kind` (`deterministic` or `model_judged`). |
| `preserved_behaviors` | The behaviors the change must keep, possibly none. |
| `ui_composition` *(optional)* | An illustration of the decision for the views page (see [MCP resources](#mcp-resources)). It is never checked, never part of `request_revision`, and is dropped when the user changes the answers or examples. |

Surla numbers the questions `<change_id>-ambiguity-NN` and the examples
`check_01`, `check_02`, …, keeps every text as sent, computes
`request_revision`, and saves the card as `needs_confirmation`, even with no
question. With `story_refresh_op_id`, it adopts the Stories of that succeeded
[`refresh_story`](#refresh_story) operation and the design files they were
evaluated from, after checking that the project's files still match them.
Without it, it records the project's `design/*.go`.

| Refusal | When |
| --- | --- |
| `conflict` (`409`) | `change_id` already has a card. |
| `unprocessable` (`422`) | `change_id` or `prompt` is blank; the analysis breaks a rule that spans fields (message starting `invalid analysis:`, such as a question that repeats an option); `story_refresh_op_id` is unknown, failed, replaced or stale (the message names `refresh_story`); or the design files cannot be read. |

Surla cannot judge whether an analysis is complete or right; the user does
that when confirming.

#### `revise_change`

`POST /changes/revise`. **Input:** `change_id`, `request_revision`, `prompt`,
`story_refresh_op_id?`, `analysis` (as for `propose_change`, with
`examples[].prior_id` and `removed_example_ids`). **Output:**
`ChangeCardResult`.

Saves the next revision of a change. `request_revision` names the latest
revision the guide read: the waiting proposed revision's, when one waits,
otherwise the card's. It is checked when the call starts and again before the
save.

| The change is | The revision |
| --- | --- |
| `needs_confirmation` | Replaces it. |
| `confirmed` | Is saved as the proposed revision beside the agreed one, replacing any earlier proposed revision. The agreed revision stays in force with its answers, approval, Story and design source, and previews, builds, verification and approval keep working on it until the user adopts or declines the proposed revision. |

The guide keeps or edits each current example by naming it in `prior_id`,
lists each dropped one in `removed_example_ids`, and restates the decisions
that still hold, because the revision has no answers of its own. Kept and
edited examples keep their IDs; added ones get the lowest free `check_NN`.
With `story_refresh_op_id`, the revision takes that operation's Story and
design files; without it, a revision with Stories keeps its Story and design,
and one without re-reads `design/*.go`.

| Refusal | When |
| --- | --- |
| `not_found` (`404`) | No card has this `change_id`. |
| `conflict` (`409`) | `request_revision` is not the latest revision, including the agreed revision while a proposed revision waits. |
| `unprocessable` (`422`) | Blank text, an example not accounted for exactly once, a stale `story_refresh_op_id`, or unreadable design files. |

#### `confirm_intent`

`POST /changes/confirm`. **Input:** `change_id`, `request_revision`,
`answers`, `examples`. **Output:** `ChangeCardResult`.

The user's **Confirm Intent & Lock Examples**, **Refine Confirmed Answers &
Examples** and **Adopt this revision** on the views page. It
confirms a waiting card, refines an agreed card, or adopts the proposed
revision named by `request_revision`. `answers` maps every open question's
`id` to exactly one of its `options`, character for character. Adopting makes
the proposed revision the agreed one, records the user in `confirmed_by`, and
carries over no approval. Changed answers or examples give the card another
`request_revision` and clear its approval; a submission that changes nothing
keeps both and returns the approval as [`get_change`](#get_change) does. A
decision no option expresses goes to the guide, which records it
with `revise_change`.

| Refusal | When |
| --- | --- |
| `not_found` (`404`) | No card has this `change_id`. |
| `conflict` (`409`) | A stale `request_revision`, or refining the agreed revision while a proposed revision waits (the message names **Adopt** and **Decline**). |
| `unprocessable` (`422`) | One message naming every missing answer, every answer that is not an option, and every unknown question `id`. |

#### `decline_revision`

`POST /changes/decline`. **Input:** `change_id`, `request_revision` (the
proposed revision's, 64 lowercase hex characters). **Output:**
`ChangeCardResult`.

The user's **Decline**. The proposed revision is removed; the agreed revision
stays exactly as saved.

| Refusal | When |
| --- | --- |
| `400` | `change_id` is empty or `request_revision` is malformed. |
| `not_found` (`404`) | No card has this `change_id`. |
| `conflict` (`409`) | No proposed revision waits, or `request_revision` is not the waiting one. |

### The Story

#### `refresh_story`

`POST /workspace/story/refresh`. **Input:** none. **Output:**
`StoryRefreshResult` (`op_id`, `state`, `snapshot_digest`, `stories`,
`diagnostic`, `message`).

Reads the project's Go module (`go.mod`, `go.sum` and non-test `.go` files),
computes its `snapshot_digest`, and starts, or reuses, a sandboxed evaluation
of its Stories. It answers at once with `state` `running` or `succeeded`.
There is one current evaluation per project, kept in memory only: refreshing
unchanged files returns it, and refreshing changed files, or after a failure,
replaces it. An evaluation has 5 minutes and survives the caller
disconnecting. A design that does not use the Story language succeeds with
`stories: []`.

| Refusal | When |
| --- | --- |
| `unprocessable` (`422`) | The project folder has no `go.mod`, was replaced, or holds a file that is not valid UTF-8. |
| `unavailable` (`503`) | The server is stopping. |

#### `get_story_refresh`

`GET /workspace/story/refresh`. **Output:** `StoryRefreshResult`.

Reads the current evaluation without starting one. `state` is `none` (none
since the server started; call `refresh_story`), `running`, `succeeded` with
`stories` (each Story's `name`, `description`, `actors` and `flows`, with
`complete`, `steps` and open `questions`), or `failed` with a `diagnostic`.
Pass a succeeded `op_id` as `story_refresh_op_id` to
[`propose_change`](#propose_change) or [`revise_change`](#revise_change).

When a design file imports the Story language but the project's `go.mod`
does not require `github.com/goadesign/surla/story`, the `diagnostic` of the
failed evaluation also names the step: in the folder that holds that
`go.mod`, run `go get github.com/goadesign/surla/story@<version>`, with the
version Surla's own `go.mod` requires, commit `go.mod` and `go.sum`, then try
again. A project made by `surla init` or `init_project` already requires the
module.

### Project files and checks

#### `init_project`

`POST /workspace/init`. **Input:** `module_path`, `dir?`. **Output:**
`InitProjectResult` (`dir`, `module_path`, `service_name`, `commit`).

Creates the [`surla init`](#surla-init) starter in a child folder of an empty
project folder, runs `go mod tidy`, `go vet ./...` and `go test ./...` in both
modules, records the first commit `Initial scaffold` with the server's Git
identity, and makes that child folder the server's project folder without a
restart. A restart keeps it. `dir` defaults to the service name and must be a
single folder name (`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`, not `.surla…`). With an
MCP `progressToken`, it reports each phase.

| Refusal | When |
| --- | --- |
| `unprocessable` (`422`) | `module_path` or `dir` is invalid. |
| `conflict` (`409`) | The project folder already holds a project (`go.mod`, `design`, `gen`, `internal`, `cmd` or `eval`), a commit, verification runs or Change Cards made from its design files, or `<dir>` exists. |
| `not_implemented` (`501`) | The `surla` binary was built without an exact module version; install a tagged or pseudo-versioned release. |

#### `generate_code`

`POST /workspace/generate`. **Input:** `target?` (`candidate`, the default, or
`eval`). **Output:** `GenerateCodeResult` (`target`, `files_written`,
`warnings`).

The builder's generation: compiles the edited design of the application, or
of `eval/` with `{"target":"eval"}`, in the sandbox and replaces that
module's `gen/` exactly as [`surla gen`](#surla-gen) does. It compiles only;
call [`check_project`](#check_project) to build and test before committing.
An error means `gen/` was not replaced; a warning means it was.

| Refusal | When |
| --- | --- |
| `unprocessable` (`422`) | The module is absent, replaced or invalid, or its design does not compile; the message states the failed phase. |

#### `check_project`

`POST /workspace/check`. **Input:** none. **Output:** `CheckProjectResult`
(`op_id`, `passed`, `message`).

Runs `go test ./...` in every Go module of the project folder (skipping
`testdata`, `vendor` and folders starting with `.` or `_`) on the current
files, uncommitted edits included, without changing them. The server copies
the files (leaving out `.git` and `.surla*`, never following a link) into a
private folder, then runs each module in two Linux containers from
`golang:1.27.3`: first downloading dependencies with your `GOPROXY`,
`GOSUMDB`, `GOPRIVATE`, `GONOPROXY`, `GONOSUMDB` and `GOINSECURE` and no
credentials, then testing with no network. A helper process, `surla
check-run`, owns the containers and finishes removing them even when the call
is canceled or the server stops. `passed` is true only when every module's
tests pass. `message` (at most 16 KiB) names the first failure; read the full
output with [`read_check_output`](#read_check_output). When the message says
`go.sum` or `go.work.sum` needs entries or `go.mod` needs an update, call
[`update_dependencies`](#update_dependencies).

| Refusal | When |
| --- | --- |
| `unprocessable` (`422`) | The project folder has no Go module. |
| `unavailable` (`503`) | `docker` is missing, the engine does not answer or is not local, or the image cannot be pulled. Start Docker and call again: the server looks Docker up on every call. An engine other than the default needs `DOCKER_HOST` or the Docker context in the server's environment. |
| An error naming a path or a container | The files changed while they were copied, or a container's removal could not be confirmed (`docker rm -f <name>`). The server checks again at every stop and start. |

#### `update_dependencies`

`POST /workspace/dependencies`. **Input:** none. **Output:**
`UpdateDependenciesResult` (`op_id`, `files_written`, `message`).

Runs `go mod tidy` for every module in the same kind of private copy and
containers, with network, then writes back only the `go.mod`, `go.sum` and
`go.work.sum` files whose bytes changed. A file that is a symlink, or that
someone edited during the call, is not written, and `message` says why.
Modules and files are written one at a time, so a stopped call can leave a
module with its updated `go.mod` and its earlier `go.sum`; `message` lists
each file written and not written. Refusals are those of `check_project`.

#### `read_check_output`

`POST /workspace/check/output`. **Input:** `op_id`, `cursor?`. **Output:**
`CheckOutputPage` (`text`, `next_cursor`, `captured_bytes`,
`dropped_bytes`).

Returns one page, at most 16 KiB of valid UTF-8, of the output a
`check_project` or `update_dependencies` operation captured. Omit `cursor`
for the first page and pass `next_cursor` for the next; it is absent at the
end. Each operation keeps at most 64 MiB; `dropped_bytes` counts the rest.

| Refusal | When |
| --- | --- |
| `not_found` (`404`) | No output exists for `op_id`. |
| `unprocessable` (`422`) | The cursor is malformed or belongs to another operation. |

### Architecture

#### `preview_architecture`

`POST /changes/preview`. **Input:** `change_id`, `request_revision`.
**Output:** `ArchitecturePreviewResult`.

For a confirmed change, evaluates the project's design in the sandbox and
returns the C4 `containers` and `edges`, `agent_policies`, and `tools` with
their `input_binding`. It reports what the design declares; it does not
compare it with the change. A tool's `requires_confirmation` shows a design
declares a confirmation gate, not that the application delivers it; see
[`examples/orders`](../examples/orders/README.md) for an implemented one.

| Refusal | When |
| --- | --- |
| `not_found` (`404`) | No card has this `change_id`. |
| `conflict` (`409`) | The card is not confirmed, or `request_revision` is stale. |
| `unprocessable` (`422`) | The design does not evaluate or declares no Goa API or service container. |

#### `diff`

`POST /diff`. **Input:** `old` and `new` (`DesignSnapshot`). **Output:**
`DiffResult`. Compares two design snapshots as [`surla diff`](#surla-diff)
does.

### Live target and model connection

A live target is the one running copy of the application the server builds
from a commit, so the builder can try prompt wording with
[`draft_override`](#draft_override). Its program follows the
[live target prompt contract](#live-target-prompt-contract).

#### `start_live_target`

`POST /live-target`. **Input:** `candidate_commit` (a full commit ID).
**Output:** `LiveTargetResult` (`candidate_commit`, `url`, `message`).

Builds the commit as verification does and runs its program's `--live` mode.
Without stored verification settings, the program is the generated test
program of the test module in that same commit, the builder's own tree: no
build and no accepted tests are needed. With them, it is the evaluator the
settings name. `url` is the application's loopback address. There is one live
target per server. A start refused before the running target is stopped
leaves that target and its prompt drafts in place: the server is stopping,
or, without stored verification settings, the commit is not in the repository
or lacks the test layout, or the project folder is in no Git repository or was
replaced. Once the commit is accepted, the running target is stopped, which
discards every prompt draft applied to it, even if the build then fails.
Prompt text survives only when it is committed. A target runs until
`stop_live_target`, a later start, its own exit or the server's stop; the
build continues if the caller disconnects.

| Refusal | When |
| --- | --- |
| `live_target_failed` (`422`) | A step failed, named in the message: the commit lacks the test layout `surla init` makes or is not in the repository, the project folder is in no Git repository or was replaced after the server started, or the build or start failed. When the earlier target's processes could not be confirmed stopped, no target can start until the user stops and starts Surla. |
| `live_target_unavailable` (`501`) | The server is stopping; start the project again with `surla`. |

#### `stop_live_target`

`DELETE /live-target`. **Output:** `StopLiveTargetResult` (`stopped`,
`message`).

Stops the live target and discards its drafts; `stopped` is `false` when none
ran. A stop is attempted once.

| Refusal | When |
| --- | --- |
| `live_target_stop_failed` (`500`) | Its processes or folder could not be confirmed stopped; stop and start Surla. A later stop reports the same. |

#### `get_live_target`

`GET /live-target`. **Output:** `LiveTargetStatus`.

`live_target.state` is `none`; `starting` (`candidate_commit`, `since`),
which cannot be stopped until the start finishes; `running`
(`candidate_commit`, `url`, `started_at`, and `prompts`: each accepted
draft's `prompt_id`, `version` and `accepted_at`, never its text); or
`not_confirmed_stopped` (`candidate_commit` and a `message` naming the step:
stop and start Surla). It changes and saves nothing.

#### `draft_override`

`POST /sandbox/draft`. **Input:** `target_kind` (`prompt_template` or
`compiled_rule`), `prompt_id?`, `new_content`. **Output:**
`DraftOverrideResult`.

Sends prompt text to the running live target. An accepted `prompt_template`
draft answers `applied_in_store: true` with the `prompt_version` the target
returned. A `compiled_rule` change is never applied and needs no target: it
answers `applied_in_store: false` and `requires_rebuild: true`, with a
`message` saying that the rule needs a Go rebuild and verification.

| Refusal | When |
| --- | --- |
| `no_live_target` (`409`) | No live target runs; call `start_live_target` first. Nothing changed. |
| `prompt_not_found` (`404`), `invalid_prompt` (`422`) | The target refused the text; its prompt is unchanged. |
| `target_rejected_credential` (`502`) | The target refused the server's key; start it again. |
| `live_target_ended` (`409`) | The target stopped during the update, so it may or may not have applied the text; start it again. |
| `update_not_confirmed` (`502`) | The target did not answer, so it may or may not have applied the text; send it again. |

#### `configure_model`

`POST /cp1/model`. **Input:** `provider` (`anthropic` or `openai`) with
`model_name`, or `gateway_addr` alone. **Output:** `ModelConfigResult`.

The user's **Apply** in the Live target view. It chooses, without a restart,
the model that live targets started afterwards receive: a direct provider,
whose key the target reads from the server's environment (`ANTHROPIC_API_KEY`
or `OPENAI_API_KEY`), or a plaintext gRPC `goa-ai` model gateway. A running
target keeps its model. Keys never travel in a request. The choice lasts
until the server stops; verification does not use it.

| Refusal | When |
| --- | --- |
| `400` | An unknown provider or another schema violation. |
| `unprocessable` (`422`) | An invalid combination, or the provider's key is missing from the server's environment (the message names the variable). |

#### `get_model_connection`

`GET /cp1/model`. **Output:** `ModelConnection`, one of `none`, `direct`
(`provider`, `model_name`) or `gateway` (`gateway_addr`).

The choice live targets started afterwards receive. At start the server
picks `anthropic` with `claude-sonnet-4-6` when `ANTHROPIC_API_KEY` is set,
otherwise `openai` with `gpt-5.4-mini` when `OPENAI_API_KEY` is set, otherwise
none. It never returns a key or any other environment value.

### Verification and approval

#### `verify_change`

`POST /changes/{change_id}/verifications`. **Input:** `change_id`,
`request_revision`, `candidate_commit`. **Output:**
`VerificationStatusResult`.

The builder asks the server to verify its commit for the agreed revision. The
server accepts the run into a run directory and answers at once with
`state.type == "running"`. A separate supervised process carries the run out
and outlives the call and server restarts; read it with
[`get_verification`](#get_verification).

Which tests run depends on the project:

| The project has | Tests that run |
| --- | --- |
| No stored verification settings | The tests Surla accepted for the user's build of this revision, at their tests commit (`get_build` shows `tests.state == "accepted"`). The run builds the application's programs from `candidate_commit` and the test program `eval/gen/cmd/<service>-evals` from the tests commit, both read from Git, never from working files. Both commits need the layout `surla init` makes: `go.mod`, `eval/go.mod` for `<module>/eval`, `cmd/<service>` in the candidate and the generated test program in the tests commit. |
| Stored verification settings | The evaluator the settings name, at their `suite_commit`. No build is needed. |

Tests a separate agent wrote run outside Surla's sandbox, which isolates only
the application's builds and processes. Their program therefore starts with
only `PATH` and `TMPDIR` (a folder inside the run directory): no model key
and no other variable of the server. An evaluator named in stored
verification settings runs with the server's environment.

| Refusal (all `409`) | When and the step |
| --- | --- |
| `no_build` | The user has not built the agreed revision, the build is of another revision (named), or the build was started without a test writer while verification settings were stored, and they have since been removed. The user builds again. |
| `tests_not_accepted` | Surla has not accepted the build's tests. The message names their state (assigned, reported, needs changes, could not run) and who acts next. |
| `no_test_layout` | A commit lacks the layout, naming what is missing: the builder fixes its commit, or the test writer reports again for the tests commit. Also an accepted tests commit the repository does not hold (the test writer reports again, or the user builds again), and a project folder in no Git repository (make it one and commit). |
| `conflict` | The card is not confirmed or `request_revision` is stale; `candidate_commit` is not in the repository (commit it and try again); or the project folder was replaced after the server started (stop and start Surla). |

`not_found` (`404`) means no card has this `change_id`.

#### `get_verification`

`GET /verifications/{run_id}`. **Output:** `VerificationStatusResult`.

Reads one run, whatever happened to the card since. It returns
`change_id`, `request_revision`, `candidate_commit`, `suite_commit`,
`accepted_at`, `test_source`, and `state`, whose `type` names the outcome and
whose `value` holds its fields:

| `state.type` | Fields of `state.value` |
| --- | --- |
| `running` | `started_at` |
| `completed` | `suite_id`, `environment` and the builder view of the `report`, in which hidden scenarios appear only as `holdout-N` |
| `not_completed` | `kind` (`execution_failed` or `interrupted`), `stage` (`checkout`, `build`, `describe`, `suite`, `report` or `supervise`) and a `summary`; the full output stays in the run directory's `executor.log` |

`test_source`, recorded when the run started, is `evaluator`, or
`written_tests` with the tests' `commit` and the `notice` `These tests were
written by a separate agent on this machine and are not isolated from the
builder.` The test writer may read only the runs of a change it has an
assignment for; any other run is refused as `forbidden` (`403`). `not_found`
(`404`) means no such run.

#### `list_verifications`

`GET /changes/{change_id}/verifications`. **Output:**
`VerificationListResult` (`change_id`, `runs`).

Every run of the change, newest first by `accepted_at` (ties by `run_id`),
including runs of earlier revisions, each as `get_verification` returns it.
The test writer may list only the runs of a change it has an assignment for;
any other change is refused as `forbidden` (`403`). `not_found` (`404`) means
no card has this `change_id`.

#### `approve_merge`

`POST /changes/approve`. **Input:** `change_id`, `request_revision`,
`candidate_commit`, `report_digest`. **Output:** `ChangeCardResult` with
`approval`.

The user's **Approve this result** for a passed run. The server takes the
tests commit from the plan for that revision: the commit Surla accepted for
the build, or the `suite_commit` of the stored verification settings. It
loads the full report from the project's report store, derives it again from
its evidence, and checks that it passed and ran those tests against
`candidate_commit` for `request_revision`. When promotion settings are
stored, it also measures the release binaries in
`<release_dir>/<candidate_commit>/`; without them, the approval records no
release binaries. Then it saves the approval, after checking that the
revision and its accepted tests did not change meanwhile. The approval names
two commits: `candidate_commit`, the builder's commit, and `suite_commit`,
the tests commit, with `test_source`. The user merges the builder's commit
and, for written tests, the tests commit on the branch `surla/tests`. Surla
merges and deploys nothing.

| Refusal | When and the step |
| --- | --- |
| `not_found` (`404`) | No card has this `change_id`. |
| `conflict` (`409`) | The card is not confirmed, `request_revision` is stale, or the accepted tests changed while approving (approve again). |
| `no_build`, `tests_not_accepted` (`409`) | As for `verify_change`. |
| `unprocessable` (`422`) | The report did not pass, does not follow from its own evidence, was made for another candidate commit or another revision than the ones named, or ran tests at another commit than the tests chosen for the revision. For other tests the message names both commits and ends with the step `the builder verifies the candidate again; then approve that run`. |
| `unprocessable` (`422`) | Promotion settings are stored and a release binary is missing, unreadable or differs from the run's. The message names no folder path and ends with the step `publish the binaries the run built into the release folder that the promotion settings name, in a folder named after the commit, or remove the promotion settings`: that folder is `<release_dir>/<candidate_commit>/`, and removing the settings is `surla promotion remove`, then `surla stop` and `surla`. |
| `not_implemented` (`501`) | The server runs no verification. A server started by `surla` always does. |

### Diagnostics

#### `list_traces`

`GET /traces`. **Input:** `status?` (`all`, `error` or `ok`; default `all`),
`limit?` (1–100, default 20), `cursor?`. **Output:** `TraceList` (`spans`,
`has_more`, `next_cursor`).

Reads the project's saved operation diagnostics, newest first. For example,
`{"status":"error"}` lists recorded failures. Each span has its operation
name, a safe summary, timestamps, duration, status and selected identifiers
and results; raw errors, events, prompts and credentials are left out, and
`shortened: true` marks shortened text. Each call examines at most 256 KiB of
history: an empty page with `has_more: true` means older history remains; pass
`next_cursor` with the same filter. `ok` selects spans without an error
status, including `unset`, which does not prove success. `unprocessable`
(`422`) means the cursor is invalid; read again without it. See
[Operation diagnostics](#operation-diagnostics).

---

## Builds and assignments

**Input:** the user's start button in the Build section of the Decisions &
Approval view, such as **Build this change** for a first build.
**Who acts:** the server saves the agreed revision's Story and design, with
the answers, examples and preserved behaviors the user confirmed for it,
prepares the test writer's working copy, gives the builder and the test
writer their assignments, adds them to the team, and writes the required
tests. **What the user sees:** the Build section shows the build, each
role's assignment and the test writer's progress, and the Chorus dialog asks
for an agent for each role it adds.

Each change has one current build. Building again replaces it and makes both
assignments unread. A proposed revision is built only after the user adopts
it. With stored verification settings, a build adds only the builder.

**The test writer's working copy.** Without stored verification settings, the
project needs the test layout `surla init` makes. The server opens a linked
Git working copy of the project's repository at `.surla-tests` inside the
project folder, on the branch
`surla/tests`, which starts at the project's commit. It adds `.surla-tests/`
to the repository's own `.git/info/exclude` file, never to a tracked file, so
`git status` stays clean. Each later build moves the copy to the project's
commit when it can, and keeps it where it is when the test writer has
uncommitted work or when `surla/tests` holds commits the project's branch
does not contain; the user merges `surla/tests`, then builds again. Accepted
tests are commits on `surla/tests`, so deleting `.surla-tests` loses only
uncommitted work: the next build recreates the folder on the same branch.

**Required tests.** The build writes one required test per finished flow of
the saved Story into the test module's generated code. Flow
`refund-over-limit` requires scenario `refund_over_limit` and the hook method
`RefundOverLimit`. Unfinished flows require none; with no finished flow, the
test program must still build. Surla writes the test module's generated code
at the build start and again at each report, so the test writer never
regenerates it. When the start cannot write it, a cause the test writer can
fix, such as a test module design that does not compile, sets the tests to
`needs_changes` with that finding, at report 0. Any other cause sets
`could_not_run`, with a reason that says what failed (the start's time
limit or a server stop, a download of the module's dependencies that fails
outside the test writer's changes, the generator and its phase, or writing
the generated folder into the copy; for any other cause, the reason points
at the server's trace of the start) and the user's step, which ends with
building the change again.

**Limits and recovery.** Preparation has five minutes and continues if the
page closes. A build start, or Surla's check of a report, holds the project's
turn; another start during it is refused as `working_copy_busy`, not queued.
After the turn is released, bringing the team up has 45 seconds. A team
problem after saving leaves the build and its roles in place: the result says
what to do, and **Start agents** or `surla` finishes it. If the server stops
after saving, the saved build stays; build again to finish preparation.

### `start_build`

`POST /changes/{change_id}/build`. **Input:** `change_id`, `request_revision`
(the agreed revision). **Output:** `StartBuildResult` (`build`, `team`,
`working_copy`).

- `build` is the saved `StartedBuild`, as `get_build` returns it.
- `team.state` is `ready` (the team result with its `step`), `window_busy`,
  `ambiguous_space` (with the `count`) or `unavailable`, each with a `step`.
  A team warning still means the build was saved.
- `working_copy.state` is `not_used` (stored verification settings),
  `at_project_commit` (`commit`), `not_contained` (`commit`, and the step:
  merge `surla/tests` into the project's branch, then build again) or
  `dirty` (`commit`; uncommitted work kept). These describe this start only.

Every refusal below saves no build and registers no role. A refusal after
Surla opened the test writer's working copy, such as a `conflict` because the
agreed revision changed meanwhile, or `unavailable`, may leave that copy; the
next build reuses it.

| Refusal | HTTP | Recovery |
| --- | --- | --- |
| `not_found` | 404 | No card has this `change_id`. |
| `conflict` | 409 | The card is not confirmed, or `request_revision` is not its agreed revision. |
| `unprocessable` | 422 | Two finished flows would have the same test name, or one has an empty name; the guide revises the Story. |
| `not_repository` | 409 | Make the project folder a Git repository and commit its files. |
| `no_commit` | 409 | Commit the project's files. |
| `no_test_layout` | 409 | The project has neither stored verification settings nor the test layout; store verification settings, then build again. |
| `git_identity_missing` | 409 | Configure Git's name and address, then build again. |
| `copy_folder_in_use` | 409 | Move the project's `.surla-tests` entry, which is not Surla's working copy, out of the project. |
| `tests_branch_in_use` | 409 | Another working copy holds `surla/tests`; the message names it and the step. |
| `working_copy_busy` | 409 | Build again when the current start or check has ended. |
| `sandbox_unavailable` | 501 | Use macOS. |
| `not_implemented` | 501 | The server has no team; start the project with `surla`. |
| `unavailable` | 503 | The server is stopping or the start ran out of time; build again. |

### Judging reports

Surla, not the test writer, decides whether the tests count. Each
[`report_tests_finished`](#report_tests_finished) raises the build's report
number and wakes the server's report checker, which judges the working copy
against the build's saved Story without running any of its code outside the
sandbox:

1. It writes the required tests into the copy again from the Story. Like a
   build start, it first removes any `.surla-gen-*` staging folder that a
   write cut short by a server's death left in the test module. An entry with
   such a name that is a link, a file, or a folder holding anything but a
   generated `gen` folder is the test writer's and is left alone.
2. It checks that only files inside the test module changed since the copy's
   base, regenerates the generated folder in a private snapshot, downloads
   the module's dependencies, builds the test program
   `./gen/cmd/<name>-evals` in the build sandbox (10 minutes), and lists its
   scenarios in the application sandbox (60 seconds). The whole judgment has
   15 minutes.
3. Its verdict replaces `reported` and marks the test writer's assignment
   unread:
   - `needs_changes`, with findings, such as a missing hook method, a file
     changed outside the test module, a scenario tagged `holdout`, a module
     that does not declare exactly one evaluation suite, or a build or
     listing that ran out of time;
   - `accepted`: Surla commits the tests on `surla/tests` and marks the
     builder's assignment unread too;
   - `could_not_run`: a cause the test writer cannot fix, with the user's
     step.

Surla never shows the output of the dependency download, which may hold
proxy settings, so it decides whose failure a failed download is from what it
can compare. When the test module's `go.mod` and `go.sum` are those of the
copy's base, or the base's own cannot be downloaded either, the check could
not run, and the step has the user run the download in the terminal `surla`
was started from, where its output can be read. When the base's lists
download and only the test writer's changed ones do not, it is the test
writer's finding, which names the base commit. A module the test writer
added that exists but cannot be reached from where Surla runs reads the same
as one that does not exist, so that finding also says to tell the user when
the download works in the copy. A build start decides the same way when
generation fails while loading the module's dependencies.

Every acceptance carries `not_judged`, always the sentence `Surla checked that
these tests build into the generated test program and declare every required
scenario; it did not judge what they assert.` The user reads the tests before
merging them.

One judgment runs at a time, in the project's turn. A report that arrives
during a judgment makes the server discard that judgment's verdict and judge
the latest report next. Stopping the server ends a judgment without saving
it, and the next server judges every reported build when it starts. A
verdict that could not be saved is judged again a minute later, and when a
build could not be read at all, at a start or before a judgment, the checker
reads the builds again a minute later. Each is tried up to three times in a
row; after that the build stays `reported` until the next report or server
start. Findings contain no absolute path.

### `get_build`

`GET /changes/{change_id}/build`. **Output:** `ChangeBuild` (`change_id`,
`build`).

`build.state` is `not_started` when the change has no usable build, otherwise
`started` with:

| Field | Meaning |
| --- | --- |
| `request_revision`, `started_at` | The revision built and when. |
| `in_force` | `true` while that revision is the agreed one, including while a proposed revision waits; `false` once the user adopts or confirms another revision. The build is not redone by itself. |
| `required_test_count` | Finished flows in the saved Story; `0` means no test was required. It counts entries; it does not judge what the tests assert. |
| `builder.read` | Whether the builder read its assignment. |
| `test_writer` | `read`, `report` (0 until the first report, one more with each) and `tests`. |

`tests.state` is `not_used` (stored verification settings), `assigned`,
`reported`, `needs_changes` (`finding_count`, never the text), `accepted`
(`commit`, `not_judged`) or `could_not_run` (`reason`, `step`); each verdict
carries `checked_at`. A build started with only the builder is usable while
verification settings are stored; once they are removed, it reads as
`not_started` and the user builds again.

### `get_test_findings`

`GET /changes/{change_id}/build/findings`. **Output:** `TestFindings`
(`change_id`, `request_revision`, `started_at`, `report`, `checked_at`,
`findings`).

The text of Surla's findings while the tests are `needs_changes`, for the
views page, with the build and the report they belong to (`report` is 0 when
the problems were found at the build start). `conflict` (`409`) in any other
state, naming it; `not_found` when there is no usable build.

### `list_builder_assignments` and `get_builder_assignment`

`GET /assignments/builder` and `GET /assignments/builder/{change_id}`.
**Output:** `BuilderAssignmentList` (most recently started first) or one
`BuilderAssignment` (`change_id`, `request_revision`, `in_force`, `read`,
`written_tests`).

The builder works in the project folder. `written_tests.state` is
`not_used`, `not_yet_accepted`, or `accepted` with the tests commit and the
verdict's time. Reading one assignment marks it read; listing changes
nothing. `not_found` means there is no usable build of that change.

### `list_test_writer_assignments` and `get_test_writer_assignment`

`GET /assignments/test-writer` and `GET /assignments/test-writer/{change_id}`.
**Output:** the list, or one `TestWriterAssignment`:

| Field | Meaning |
| --- | --- |
| `request_revision`, `in_force`, `progress` | As in `get_build`. |
| `working_folder` | The project's folder inside the test writer's working copy. |
| `design_source`, `stories` | The design and Story as agreed at the built revision; later revisions do not change them. |
| `answers` | Each question of the agreed revision with the option the user confirmed (`id`, `question`, `answer`), in question order; empty when the revision asked none. |
| `examples`, `preserved_behaviors` | The revision's confirmation examples and preserved behaviors, exactly as the user confirmed them. |
| `required_tests` | One entry per finished flow: `story`, `flow_key`, `scenario`, `hook`. |
| `findings` | The text of Surla's findings while `progress` is `needs_changes`; empty otherwise. The list carries only their number. |

The build saves `answers`, `examples` and `preserved_behaviors` when it
starts, so a revision proposed or adopted later does not change them. The
test writer may not read the change with `get_change`, so these fields are
how its tests learn what the user agreed: the tests must hold the service to
them as well as to the Story's flows. For example, when the user answered
"Without tax" to "Does the $50 limit include tax?", a test checks that an
order of $50.00 before tax is refunded at once. Surla's check of a report
confirms the required tests, not what they assert. Reading the assignment of
a build that holds no example, such as one saved by a `surla` without these
fields, fails with `the build of change "<change_id>" holds none of the
agreed answers, examples and preserved behaviors; the user builds this change
again on the views page`.

The test writer aligns the service interface in its copy with the agreed
design, writes the tests in `eval/` of `working_folder`, checks them with
`go build ./...` in `eval/`, which names any required method still missing,
and then reports. It never calls `generate_code` or `surla gen`, which would
drop the required tests; after changing the test module's design it reports,
and Surla generates the module again. Reading an assignment marks it read.

### `report_tests_finished`

`POST /assignments/test-writer/{change_id}/finished`. **Input:** `change_id`,
`request_revision`. **Output:** `TestWriterAssignmentItem`.

Records `reported` from any state, raises `report` by one, clears an earlier
verdict, returns at once and wakes the report checker.

| Refusal | When |
| --- | --- |
| `conflict` (`409`) | Another revision than the assignment's, or the project stores verification settings and uses no test writer. |
| `not_found` (`404`) | No usable test assignment for this change. |

---

## Team supervision

The server rebuilds the team's picture in memory from Herdr and Chorus and
saves none of it. The user's views page reads it and acts on it with
`get_team`, `show_team_pane` and `start_team_agents`; agents and management
cannot call these three, and none is an MCP tool. The `surla` command
prepares the same team with the management operation `ensure_team`.

### `get_team`

`GET /team`. **Output:** `TeamSnapshot` (`team`, `roles`). Registered roles
in guide, builder, test writer order; empty before any is registered. Reading
creates nothing and marks nothing read. Each role has:

| Field | Values |
| --- | --- |
| `state` | `not_started`, `ended`, `starting`, `needs_you`, `working`, `waiting`, `error` or `unknown` |
| `attention` | Why a role needs the user: `none`, `permission`, `question`, `auth`, `silent` (a running agent that reported nothing for 60 seconds) or `unknown` |
| `watcher` | `held` when Chorus reports the agent's watcher, `gone` after 90 seconds of known absence, or `unknown` |
| `assignment` | `unread`, `nothing_owed` or `unknown`; the guide has none |

When Herdr sends no heartbeat or event for 45 seconds, every role reads
`unknown` until fresh facts arrive. A failed assignment read never reads as
nothing owed. `wait_for_changes` also covers these facts.

### `show_team_pane`

`POST /team/pane`. **Input:** `role`. Shows this project's space and focuses
that role's pane, only when the user asks.

| Refusal | When |
| --- | --- |
| `window_busy` (`409`) | A Herdr dialog is open; close it first. Nothing in the window changed. |
| `pane_unavailable` (`409`) | The role has no pane Surla can identify. |
| `team_unavailable` (`503`) | Herdr could not establish the pane: `Herdr did not complete a team command; open the Herdr app, then try again`. |

### `start_team_agents`

`POST /team/agents`. **Output:** `ProjectTeam` (`team`, `space`, `roles`,
`dialog`, `step`). The **Start agents** action: the same team preparation
`surla` runs, opening the Chorus dialog when a listed role is not running.
Refusals: `ambiguous_space` (`409`, close the duplicate Herdr spaces, then
start the agents), `window_busy` (`409`) and `team_unavailable` (`503`), whose
message is the views page's step for its cause (see
[`ensure_team`](#ensure_team-management)). `team_unavailable` can come after
the server saved the team's name and space, registered the guide and wrote
the roster; they stay saved, and the next start reuses them.

### `ensure_team` (management)

`POST /project/team`, called by `surla`. It prepares the same team and
returns `team`, `space`, `roles` (`role`, `handle`, `running`), `dialog`
(`opened`, `already_open` or `not_needed`) and `step`. `surla` prints `step`
unchanged. Its refusals, `ambiguous_space` (with the count), `window_busy`
and `team_unavailable`, are shown with fixed recovery text. As for
`start_team_agents`, `team_unavailable` may leave the team's name, space,
guide and roster saved for the next start.

`team_unavailable` names its `cause`, and `surla` and the views page each
show their own fixed step for it, never the failure's own text. `surla`
adds the HTTP status, and its retry is **Start agents** on the views page,
which opens anyway:

| `cause` | What could not be used | `surla` prints | The views page shows |
| --- | --- | --- | --- |
| `stopping` | The server is shutting down. | `the project server is stopping; run surla again` | `the project server is stopping; run surla in the project folder to start it again` |
| `herdr` | A Herdr command did not complete. | `Herdr did not complete a team command; open the Herdr app, then click Start agents on the views page` | `Herdr did not complete a team command; open the Herdr app, then try again` |
| `roster_folder` | Herdr's launcher roster folder could not be written. | `Herdr's launcher roster folder could not be written; make data/chorus-launcher/rosters in Herdr's app folder (~/.config/herdr-app unless HERDR_APP_HOME is set) writable by this account, then click Start agents on the views page` | The same, ending `then start the agents again` |
| `project_state` | The project's record or an agent credential could not be read or written. | `this project's private state in <state folder> could not be read or written; make that folder and the folders in it private to this account (chmod 700) and its files too (chmod 600), then click Start agents on the views page` | `this project's private state could not be read or written; run surla in the project folder to see which folder to repair` |
| `role_folder` | A role's working folder could not be read. | `a team role's working folder could not be read; make sure this account can open the project folder <project folder> and its build working folders, then click Start agents on the views page` | `a team role's working folder could not be read; make sure this account can open the project folder and its build working folders, then start the agents again` |

A team request from `surla` that runs out of time prints `project team
request did not finish because Herdr did not answer in time; open the Herdr
app, then click Start agents on the views page`.

**Reminders.** The supervision rules allow only fixed text, sent to an idle
agent that owes an assignment read or whose watcher is gone, never in the
first 90 seconds after the server starts. The `surla` command delivers no
reminder: each eligible one is recorded as unavailable. Supervision never
reads screen text and never takes focus without the user's action.

---

## Live target prompt contract

A live target serves one operation for `draft_override`. Surla calls it with
a client it generates from [`design/targetprompt`](../design/targetprompt/design.go);
the target declares the same method in its own Goa design and implements it:

- `PUT /prompts/{prompt_id}` with body `{"template": "<Go text/template>"}`
  and header `X-Live-Target-Key: <key>` answers `200` with
  `{"prompt_id": "...", "version": "..."}`. The version is an opaque,
  non-blank identifier of at most 256 characters, such as
  `sha256:<64 hex digits>`; any other answer is an update Surla cannot
  confirm.
- `401` (`unauthorized`) for a missing or wrong key, `404` (`unknown_prompt`)
  for an unregistered prompt, and `422` (`invalid_prompt`) for a template
  that does not render. The target renders before it saves, so a refused
  template leaves the text in use unchanged.
- The server mints the key for each start. It reaches the program in
  `SURLA_LIVE_TARGET_KEY`, and the program's `Live` function passes it to the
  application through `candidate.Spec.Env`.
- The model choice reaches the program as `SURLA_LIVE_MODEL_PROVIDER` and
  `SURLA_LIVE_MODEL_NAME` for a direct provider, with that provider's key in
  its usual variable, or as `SURLA_LIVE_MODEL_GATEWAY`. Nothing is set when
  the choice is none.
- A test module from the commit, which agents wrote, receives only `PATH`,
  `TMPDIR`, `SURLA_LIVE_TARGET_KEY`, the model variables and the chosen
  provider's key. An evaluator named in stored verification settings
  receives the server's environment with these variables set.
- The starter's test module implements this contract: `override_prompt`
  replaces `<name>.helper.system`, and its `Live` function serves a direct
  provider through its own loopback model gateway so the key stays out of the
  application. It passes a literal loopback gateway address, such as
  `127.0.0.1:9090`, to the application directly, and reaches any other
  gateway, including `localhost`, through a forwarder it owns, because the
  application's sandbox allows only loopback connections and resolves no
  names.
- [`examples/orders`](../examples/orders/README.md) implements it in its
  `prompts` service and replaces `orders.refund.system`; its evaluator's
  `Live` function uses a scripted model and needs no key.

Changing this contract breaks every target that implements it.

---

## MCP resources

- **`surla://golden-path`** (`text/markdown` through MCP `resources/read`;
  a JSON string from `GET /golden-path`). The
  [Golden Path playbook](../internal/goldenpath/GOLDEN_PATH.md) for Goa and
  Goa-AI platforms, followed by the decision components an analysis's
  `ui_composition` may use, generated from the views page's own components
  into [`decision_catalog.md`](../internal/goldenpath/decision_catalog.md), and
  how a composition refers to the analysis's questions and examples by
  1-based position.

---

## Views page

The server serves the views page at `GET /views` on its loopback address.
`surla` opens it with a single-use link in the address fragment
(`/views#link=<link>`), which a browser never sends to a server. On load the
page exchanges the link once for a session, removes it from the address, and
keeps the session only in memory: never in browser storage or a cookie. The
exchange also returns `project_name`, the base name of the project's
physical folder, without its path. The tab's title is the next step and that
name, such as `Confirm change: shop`, from the facts the page last read;
after a failed read of the changes or the team, a lost update connection or
an ended session, it names the recovery instead, such as
`Reconnecting: shop`. Before the exchange succeeds, the title is `Surla`.
Reloading or closing the tab ends the session; run `surla` for a fresh link.
A used or expired link is reported as such. When the server answers `401`, as
it does after a restart, the page keeps everything typed visible and
copyable and says to run `surla` again. Leaving with unsaved edits asks the
browser to confirm.

**Every view.** One line per role that needs the user (for example
`Builder needs you: permission`), with a **Show builder pane** action. The
page keeps one `wait_for_changes` read open and reads again only what
changed. When the server does not answer, the status line says so, every
edit is kept, and the page retries after 5 seconds, doubling up to 30.
Hiding the tab pauses this. A failed read keeps the last facts with a
last-read label. An incoming revision never replaces typed edits: the page
holds it beside them (**Switch to Incoming Card**).

| View | What it shows and does |
| --- | --- |
| **Changes** | The project's changes ([`list_changes`](#list_changes)), opening directly on the only one. With none: `This project has no changes yet. Ask the guide to propose a change; it appears here by itself.` A change with a waiting proposed revision is marked **New revision proposed**. Each change shows its latest saved run, such as **Latest saved run: passed**, or **No saved runs**, and under a run what `inputs_current` says: `Matches the agreed revision and selected tests.` or `Earlier inputs or no current test selection. Ask the builder for a check of the agreed revision and selected tests.` |
| **Team** | Each registered role's state, its Chorus watcher, and whether its assignment is read, with a **Show … pane** action per role, such as **Show guide pane**, and **Start agents**, which says what it returned and when. With no role: `No agents are registered. Start agents prepares the guide; building adds the required roles.` Nothing runs on a refresh. |
| **Story** | The Story saved with the shown revision, one row of step cards per flow: a finished flow is marked **will be tested end to end**, others **draft: not tested** with the reasons. Below it, the current Story evaluation ([`get_story_refresh`](#get_story_refresh)): none, running, failed, or succeeded, and how it differs from the change's Story; a draft is not part of the change until the guide revises the change with it and the user confirms. |
| **Live target** | Whether a live target is starting, running or not confirmed stopped, its commit, address and accepted prompt versions (never the text), and **Stop live target**, which asks first. Below it, the model live targets started afterwards use and a form whose **Apply** sends [`configure_model`](#configure_model); a refusal names a missing key's variable. |
| **Decisions & Approval** | The Change Card: questions to answer, examples, preserved behaviors, and **Confirm Intent & Lock Examples** (on an agreed card, **Refine Confirmed Answers & Examples**). A proposed revision appears above the agreed one with everything adopting it would confirm (changed request and summary, questions and examples marked **New**, **Edited**, **Kept** or **Removed**, Story and design changes), **Adopt this revision** and **Decline**. A stale submission offers **Load Latest Card**. Then the **Build** section and, after approval, the recorded approval with the builder's commit and the tests commit. |
| **Architecture** | [`preview_architecture`](#preview_architecture) for the revision shown. |
| **Proof** | The change's verification runs and each report's scenario outcomes, hidden scenarios anonymous, with **Approve this result** for a passed run. A run of written tests shows the `notice` of its `test_source`. **Candidate and run details** shows the run's candidate commit and its tests commit, labeled **Accepted tests commit** for written tests and **Evaluator commit** for an evaluator named in stored verification settings. |
| **Traces** | A drawer listing recorded errors, with **All statuses** and **Without error status**, older pages, and **Retry from newest** after a failed read. |

**The Build section.** It appears once the change is confirmed or has a
saved build, and then always offers **View team**. While the agreed
revision has no build, it offers one start button: **Build this change**
before any build, **Build current agreed revision** when the saved build is of
an earlier revision, and **Build agreed revision** in either case while a
proposed revision waits. Once the agreed revision is built, it offers **Build
again…** instead, which asks first. Next to each control that sends a start it
says: `Building starts a builder and, unless this project has stored
verification settings, a test writer whose tests Surla runs on this machine
outside the sandbox to check the builder's work.` It refuses to start from an
unconfirmed change (`Confirm the change before building it.`) or an older
revision, and builds the agreed revision even while the editor holds unsaved
edits. It shows when the build started, the required test count, whether
each assignment is read, the test writer's state, the accepted tests commit,
the `not_judged` sentence, and, when tests need changes, the findings read
with [`get_test_findings`](#get_test_findings). A start's team and working-copy
outcomes are shown with the time of that start, not as current health.

**Before the agreed revision changes.** When the agreed revision has a build,
the page says what a different agreed revision does to it, above **Adopt this
revision** and, once an answer or an example is edited, above **Refine
Confirmed Answers & Examples**: the build stays on the earlier revision, and
a build of the changed revision replaces both assignments and clears the test
writer's report and verdict, so its tests must be reported, checked and
accepted again before verification. With stored verification settings it
says instead that such a build updates the builder's assignment and that
verification keeps using those settings. When the page could not read the
build, it gives the same warning for any existing build. A confirmation that
changes nothing keeps the revision and its build.

**Approval on the page.** The recorded approval shows **Builder's commit**
and **Tests' commit**; for written tests it says to merge the tests commit on
branch `surla/tests`. When the server cannot record approvals, **Approval is
unavailable on this server** replaces **Approve this result**.

**What the page never shows or does.** It never shows prompt text or
structural `diff` results, and it starts no live target and sends no prompt
draft: the builder does those. A guide's `ui_composition` is an
illustration, not the Story.

**Browser policy.** The page is a fixed shell with no project data, built
from [`ui/checkpoint/`](../ui/checkpoint/). It sends only the operations
listed in `PAGE_OPERATIONS` in [`ui/checkpoint/build.mjs`](../ui/checkpoint/build.mjs),
whose request types and result validators are generated from the OpenAPI
description, and refuses an answer that does not match. Every page request
must be same-origin. The server sends this `Content-Security-Policy`:

| Directive | What the browser allows |
| --- | --- |
| `script-src 'sha256-<digest>'` | Only the page's one inline script; nothing is evaluated from a string. |
| `style-src 'unsafe-inline'` | Inline styles only; no style sheet loads. |
| `connect-src 'self'` | Requests only to the page's own address. |
| `default-src 'none'` | Nothing else: no images, fonts or frames. |
| `frame-ancestors 'none'` | The page is never framed. |
| `form-action 'none'` | No form submission. |
| `base-uri 'none'` | The base address cannot change. |

---

## Operation diagnostics

The server records safe summaries of the operations callers make, with no
collector to set up. On the views page, open **Traces**; agents use
[`list_traces`](#list_traces).

**What is saved.** Every call by the guide, the builder or the test writer,
every operation that changes something, and the user's reads sent with `POST`
(`preview_architecture`, `diff`, `read_check_output`). The user's `GET` reads,
which the page repeats, are saved only when they fail with an error status or
a panic; refused reads, such as `forbidden` or `not_found`, are expected
answers and are not saved. `wait_for_changes` is never saved. Each HTTP or
MCP call has a `surla.request.<operation>` span; a refusal is recorded
without error status, and an unexpected fault records an error. Calls refused
before authentication or decoding, verification processes, the check helper
and outside services are absent. An `unset` status does not prove success.

**Where it is kept.** The history is `traces.jsonl` in the state folder's
`cards/` folder, created when the first span finishes, append-only, readable
only by your account (`0600`), and read again after a restart. Each record is
at most 64 KiB, with names, summaries and values shortened when needed. The
file holds a SHA-256 hash of each caller for read isolation; neither the
caller nor the hash appears in results or exported spans. The hash is not
anonymization: keep the file private. To discard the history, stop Surla,
then remove the file.

**The server's terminal.** When a `propose_change` or `revise_change` span
ends with an error status, such as a storage fault, the server's Herdr
terminal prints one line with its summary and error messages. That line can
hold private evidence. An analysis refused as `unprocessable` prints nothing.

**Exporting.** Set `OTEL_EXPORTER_OTLP_ENDPOINT` (the collector's base URL) or
`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` (the full trace URL, which wins) in the
server's environment to send full spans to an OpenTelemetry collector over
OTLP/HTTP. Exported spans keep their original fields and status and carry no
caller identity; restrict access to the collector. The check helper exports
its own spans the same way, as children of the call's span. With neither
variable set, Surla sends nothing.

---

## Reference platform, repository checks and layout

### Reference platform: `examples/orders` and `examples/orders-eval`

An order-refund service and its separate evaluator, which checks payment
effects, manager approval, worker recovery and token budgets.

[`examples/orders`](../examples/orders/), the application:

- [`design/`](../examples/orders/design/): the public `orders` HTTP service
  (`chat`, `run_status`, `approve_refund`), the `prompts` service that
  follows the [live target prompt contract](#live-target-prompt-contract),
  internal `commerce` and `payments` services, the `refund` agent
  (`Inject("tenant_id", "customer_id")`, `BoundedResult`, `Confirmation`),
  and the C4 model in [`gen/structurizr/model.json`](../examples/orders/gen/structurizr/model.json).
- [`AGENTS_QUICKSTART.md`](../examples/orders/AGENTS_QUICKSTART.md): the
  generated guide for coding agents.
- [`internal/orders`](../examples/orders/internal/orders/): the refund rule.
  An order total up to \$50.00, tax and shipping included, is refunded as
  store credit at once; a larger total is refunded to the card after a
  manager approves. Prompt changes are accepted only with the live key.
- [`internal/assistant`](../examples/orders/internal/assistant/): runs the
  `refund` agent, resumes after the manager's decision, and keeps runtime
  prompt overrides in memory.
- [`internal/refundflow`](../examples/orders/internal/refundflow/): a durable
  Temporal workflow that refunds each order exactly once.
- [`internal/runstore`](../examples/orders/internal/runstore/): a
  transactional SQLite store for `goa-ai` runs.

[`examples/orders-eval`](../examples/orders-eval/), the evaluator: its
[acceptance tests](../examples/orders-eval/acceptance/acceptance_test.go)
boot the application in the sandbox against
[commerce and payment simulators](../examples/orders-eval/sim/) and a
[scripted model](../examples/orders-eval/scriptedmodel/), and run five
scenarios (`store_credit_within_limit`, `card_refund_needs_manager`,
`refund_survives_worker_crash`, `pending_approval_survives_api_restart`, and
the hidden `other_customers_order_refused`) plus a token-budget regression.
See [`examples/orders/README.md`](../examples/orders/README.md) for a
walkthrough.

### Repository checks

The [CI workflow](../.github/workflows/ci.yml) runs its short checks on pull
requests and on pushes to the branches its `push` trigger names: `main`, and
an integration branch for as long as pull requests target one. The short
checks are:

- Vet and short race tests of each Go module on macOS: the root,
  [`story`](../story/README.md), `examples/orders` and `examples/orders-eval`.
  The root module's tests run in two jobs so that each stays short:
  **Go (., page tests)** runs the views page's tests (those named
  `TestViewsPage…` and `TestCheckpoint…`) and **Go (.)** runs all the others,
  so every test runs in exactly one of the two.
- A Linux build and vet of the root module, once without build tags and once
  with each of `e2e`, `scaffold`, `herdr`, `browser` and `docker`, and the
  Linux tests of the packages that need no sandbox.
- Formatting, generated-code and views page type checks.

To run the short tests locally, run `go test -race -short ./...` in each
module.

The full suite runs nightly at 07:17 UTC and on a manual **Run workflow**
request (`CI=true gh workflow run ci.yml --ref <branch> </dev/null`). It runs
each module's complete tests and the verifier, generation, starter and orders
acceptance suites as parallel jobs. A passing short run does not prove the
sandboxed paths; changes to candidate execution or starter generation need
the full suite. On macOS, with the Temporal CLI for the first three:

```bash
(cd examples/orders-eval && go test -race -tags e2e ./acceptance)
go test -race -tags e2e ./internal/verify
go test -race -count=1 -tags e2e ./cmd/surla
go test -race -count=1 -timeout 60m -tags scaffold ./internal/scaffold
```

The views page's real-browser check runs only on request; CI compiles it but
never runs it. It needs Chrome or Chromium and Node 22 or later; set
`SURLA_TEST_BROWSER` to the browser when it is not in a standard place. It
checks that the browser enforces the page's policy, that the session ends on
reload and nothing is stored, that a long row of Story steps scrolls inside
the Story view, and that the Live target view fits a 375-pixel-wide screen:

```bash
go test -race -count=1 -tags browser ./internal/project
```

### Project layout

```text
.
├── .github/workflows/ci.yml     # Short checks on pull requests and pushes; full suites nightly or on request
├── cmd/surla/                   # The surla command: project start and stop, settings, init, preview, diff, gen, verify, approve
├── design/                      # Goa design of the server's contract
│   ├── design.go                # Change Cards, checks, live target, verification, approval
│   ├── changes.go               # The change list and the views page's waiting read
│   ├── builds.go                # Builds and the builder's and test writer's assignments
│   ├── build_start.go           # A build start's team and working-copy outcomes and refusals
│   ├── team.go                  # The user's team read and actions
│   ├── project.go               # Management: identity, team, stop, browser link
│   ├── story_refresh.go, traces.go, cp1_analysis.go
│   └── targetprompt/            # The live target prompt contract
├── docs/                        # How it works, reference, architecture playbook, product direction, Story test design
├── examples/
│   ├── orders/                  # Reference order-refund platform
│   └── orders-eval/             # Its evaluator, simulators and acceptance scenarios
├── gen/                         # Generated HTTP, JSON-RPC and MCP code (never edited by hand)
├── internal/
│   ├── controlplane/            # Change Cards, builds, judging reports, checks, live target, verification, approval, operation policy, views page
│   ├── project/                 # Project state, credentials, browser sessions, the team and its supervision
│   ├── testwriter/              # The test writer's working copy, the check of its tests, and their commit
│   ├── verify/                  # Verification runs, the builder view of reports, and approval checks
│   ├── verifyrun/               # Supervised verification and live-target processes and run directories
│   ├── designeval/              # Sandboxed design evaluation and the C4 extractor
│   ├── diff/                    # Structural design and dependency diff
│   ├── goldenpath/              # The Golden Path playbook and decision component catalog
│   ├── scaffold/                # The surla init starter and its templates
│   ├── traceview/               # The saved operation diagnostics
│   ├── genpublish/              # Atomic replacement of generated folders
│   ├── targetprompt/            # Generated client of the live target prompt contract
│   ├── procgroup/               # Process groups for sandboxed children
│   ├── abspath/, renameat/, shquote/   # Path resolution, atomic renames, shell quoting
│   └── testmodule/              # Go module fixtures for tests
├── story/                       # The Story language, a separate Go module
├── ui/checkpoint/               # Views page source; build.mjs writes checkpoint.html, its types and the decision catalog
└── verify/
    ├── candidate/               # The macOS sandbox for building and running agent-written code
    └── suite/                   # The evaluator suite command contract (suite.Main)
```
