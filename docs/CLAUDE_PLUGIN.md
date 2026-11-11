# Use Surla from Claude Code

The Surla plugin for Claude Code makes Claude Code start Surla for the project
you have open. You do not start a server, choose a port, or create a token:
Claude Code runs `surla mcp` when a session opens and stops it when the session
closes. Your coding agent then drives Surla through its MCP tools.

This page covers installing the plugin, checking that it works, and recovering
when it does not. The [reference](REFERENCE.md) describes every tool.

## Where it works

| Host | Supported |
|---|---|
| Claude Code in a terminal, on macOS | Yes. Tool results appear as text. |
| The Code tab of the Claude desktop app, local sessions, on macOS | Yes. It reads the same plugin settings as the terminal. |
| The chat tab of the Claude desktop app | No. It does not load Claude Code plugins. |
| claude.ai, cloud sessions, remote sessions over SSH, Linux, Windows | No. |

Surla's tools return an interactive checkpoint viewer for hosts that can show
MCP Apps. Claude Code in a terminal does not show it. Whether the desktop
app's Code tab shows it for a server that a plugin starts has not been verified,
so do not rely on it. The plugin does not change how the viewer is delivered
over HTTP: `surla serve` still serves it as the
[reference](REFERENCE.md#mcp-resources) describes.

## Before you install

You need:

- macOS.
- The Go toolchain that `go.mod` in this repository declares.
- Docker, for the `check_project` and `update_dependencies` tools.
- Claude Code.

## Install

1. Install the `surla` command at the commit this page was written for:

   ```sh
   go install github.com/goadesign/surla/cmd/surla@998b5168621da29f6a6ae40e8eae1db88374af23
   ```

2. Check that your shell finds it and that it has the `mcp` command:

   ```sh
   command -v surla
   surla help
   ```

   `surla help` must list this command:

   ```
   mcp             run Surla as an MCP server over standard input and output
   ```

   If it does not, an older `surla` is first on
   your `PATH`: remove it or reinstall, then check again. The Claude desktop
   app takes `PATH` from your login shell profile, so set it there.

3. From the root of the project you want to use Surla in, add the catalog and
   install the plugin for that project:

   ```sh
   claude plugin marketplace add goadesign/surla --scope project
   claude plugin install surla@surla --scope project
   ```

   Project scope records the plugin in `.claude/settings.json` in that
   project. Commit that file to offer the plugin to everyone who works in the
   repository; each collaborator still runs the two commands once in their
   own clone.

## Check that it works

1. Open the project in Claude Code and run `/mcp`. The server
   `plugin:surla:surla` is listed as connected.
2. Ask the agent to read `surla://golden-path`, then to propose a change.
   Claude Code asks you to approve the first tool call. The result is a Change
   Card: Surla's saved record of what the change must do, the examples that
   become checks, and the behavior it must keep.

Surla saves Change Cards on your machine, in a directory it keeps for that
project, so closing and reopening the session continues where you stopped.

## When something goes wrong

**The server is listed as failed.** Claude Code could not start `surla mcp`.
Run the checks in step 2 of [Install](#install) in a new terminal. After you
fix the cause, choose **Reconnect** for the server in `/mcp`; Claude Code does
not retry a failed plugin server by itself for several minutes.

**You upgraded `surla` while a session was open.** The running server keeps
the old program. Choose **Reconnect** in `/mcp`, or close and reopen the
session.

**Another session already has the project open.** One Surla process at a time
owns a project's Change Cards. A second one stops with
`surla: cards directory <path> is in use by another process`. Close the other
session, or keep working in it.

**You have Change Cards saved under another owner.** `surla mcp` identifies you
by your local user account, as `uid:<number>`. To keep using cards that an
earlier setup saved under a different owner, register `surla mcp` yourself
with its `-principal` flag instead of using this plugin, as the
[reference](REFERENCE.md) describes.

## What the plugin does and does not do

The plugin declares one local server, `surla mcp -project <project root>`. It
adds no hooks, skills, agents, or settings, and it does not include the `surla`
program.

Claude Code runs the main agent and every subagent of a session through the
same Surla server, as the same user, with the same file access. The plugin does
not hide files from any agent and does not give agents separate identities.
Surla itself removes holdout scenario details from the results it returns to
the building agent, and builds and runs candidate code in its own sandbox.
Keeping holdout files out of an agent's reach is the operator's job: use Claude
Code permission rules or a separate user account.

## Update or remove

Updates are not automatic. To take a newer plugin:

```sh
claude plugin marketplace update surla
```

To remove it from a project:

```sh
claude plugin uninstall surla@surla --scope project
```
