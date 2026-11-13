// This file binds Herdr and Chorus commands at the executable's composition boundary.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/goadesign/surla/internal/modelsettings"
	"github.com/goadesign/surla/internal/project"
	"golang.org/x/mod/semver"
)

type (
	// teamCommands keeps argv typed, closes stdin, and never returns external output in errors.
	// Every command runs with an explicit environment: run and start receive it from the caller.
	teamCommands struct {
		core, app, chorus string
		run               func(ctx context.Context, env []string, program string, args ...string) ([]byte, error)
		// start detaches a GUI/service starter; readiness, never process ownership, bounds startup.
		start func(env []string, program string, args ...string) (<-chan error, error)
		// herdrEnv is the environment of Herdr's own programs (herdr-app, herdr-core): the
		// surla command's, or the server's original one, unchanged, because a Herdr core
		// these programs start keeps it for every terminal, the agents' included.
		herdrEnv []string
		// chorusEnv is the environment of Chorus commands: the surla command's without any
		// model provider family (withoutProviderFamilies).
		chorusEnv []string
	}
)

// minimumTeamChorus is the first release containing the roster and idle-shell launcher contracts.
const minimumTeamChorus = "v0.11.0"

// runTeamCommand bounds one external command run with env; stderr is never echoed to the caller.
func runTeamCommand(ctx context.Context, env []string, program string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	command.Env = env
	raw, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, teamCommandExit{code: exit.ExitCode()}
		}
		return nil, fmt.Errorf("%s command failed", filepath.Base(program))
	}
	return raw, nil
}

// resolveTeamCommands requires the exact executable names consumed by the Chorus launcher.
func resolveTeamCommands(
	find func(string) (string, error),
	run func(context.Context, []string, string, ...string) ([]byte, error),
) (teamCommands, error) {
	programs := []struct{ name, install string }{
		{"herdr-core", "curl -fsSL https://releases.herdr.dev/core/install.sh | sh"},
		{"herdr-app", "curl -fsSL https://releases.herdr.dev/app/install.sh | sh"},
		{"chorus", "curl -fsSL https://raw.githubusercontent.com/goadesign/chorus/main/scripts/install.sh | sh"},
	}
	found := make([]string, 0, len(programs))
	for _, program := range programs {
		path, err := find(program.name)
		if err != nil {
			return teamCommands{}, fmt.Errorf("%s is required; install it with: %s", program.name, program.install)
		}
		found = append(found, path)
	}
	return teamCommands{core: found[0], app: found[1], chorus: found[2], run: run}, nil
}

// preflightTeam checks versions without creating state or starting any service or window.
func preflightTeam(commands teamCommands) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := commands.run(ctx, commands.chorusEnv, commands.chorus, "version")
	if err != nil {
		return errors.New("chorus version failed; update with: chorus update -yes")
	}
	version := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(raw)), "chorus "))
	if !semver.IsValid(version) || semver.Compare(version, minimumTeamChorus) < 0 {
		return fmt.Errorf("Chorus %s or newer is required; update with: chorus update -yes", minimumTeamChorus)
	}
	return nil
}

// startTeamPrograms starts only absent programs after the project server has answered.
func startTeamPrograms(ctx context.Context, commands teamCommands) error {
	// Service and window readiness each own a full deadline; ensure_team gets its own later.
	chorusCtx, stopChorus := context.WithTimeout(ctx, 35*time.Second)
	err := startTeamProgram(chorusCtx, commands, commands.chorus, commands.chorusEnv, []string{"up"})
	stopChorus()
	if err != nil {
		return err
	}
	windowCtx, stopWindow := context.WithTimeout(ctx, 35*time.Second)
	defer stopWindow()
	return startTeamProgram(windowCtx, commands, commands.app, commands.herdrEnv, nil)
}

// startTeamProgram starts one absent dependency with env without ever signaling its process.
func startTeamProgram(ctx context.Context, commands teamCommands, program string, env, args []string) error {
	if teamProgramReady(ctx, commands, program) {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s status did not become ready; %s", filepath.Base(program), teamProgramNextStep(commands, program))
	}
	commandName := "chorus up"
	if program == commands.app {
		commandName = "herdr-app"
	}
	exited, err := commands.start(env, program, args...)
	if err != nil {
		return fmt.Errorf("%s could not start; %s", commandName, teamProgramNextStep(commands, program))
	}
	pollCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		ready := teamProgramReady(pollCtx, commands, program)
		// Observe an already-reported starter failure before declaring readiness.
		select {
		case err := <-exited:
			if err != nil {
				return fmt.Errorf("%s failed to start; %s", commandName, teamProgramNextStep(commands, program))
			}
			exited = nil
		default:
		}
		if ready {
			return nil
		}
		select {
		case err := <-exited:
			if err != nil {
				return fmt.Errorf("%s failed to start; %s", commandName, teamProgramNextStep(commands, program))
			}
			exited = nil // A successful short-lived launcher is allowed to leave its window/service running.
		case <-pollCtx.Done():
			return fmt.Errorf("%s status did not become ready; %s", filepath.Base(program), teamProgramNextStep(commands, program))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// teamProgramReady requires the same Chorus status evidence as its launcher.
func teamProgramReady(ctx context.Context, commands teamCommands, program string) bool {
	if program == commands.app {
		_, err := commands.Overlays(ctx)
		return err == nil
	}
	raw, err := commands.run(ctx, commands.chorusEnv, program, "status")
	return err == nil && strings.Contains(string(raw), " is running")
}

// teamProgramNextStep gives fixed recovery wording without command output or selected paths.
func teamProgramNextStep(commands teamCommands, program string) string {
	if program == commands.app {
		return "open Herdr, then run surla again"
	}
	return "run chorus up, then run surla again"
}

// startDetachedTeamProgram gives the window or service starter its own session, env and no pipes.
// Context deadlines apply only to status polling: Surla never signals these user processes.
func startDetachedTeamProgram(env []string, program string, args ...string) (<-chan error, error) {
	command := exec.Command(program, args...)
	command.Env = env
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return nil, errors.New("program could not start")
	}
	exited := make(chan error, 1)
	go reapTeamProgram(command, exited)
	return exited, nil
}

// reapTeamProgram reports an exit without blocking startup or inheriting output pipes.
func reapTeamProgram(command *exec.Cmd, exited chan<- error) { exited <- command.Wait(); close(exited) }

// projectTeamDependencies binds the server login environment, never paths supplied by the CLI caller.
// Its HERDR_APP_HOME (default ~/.config/herdr-app) may differ from the running window’s environment.
// herdrEnv is the server's original environment, which serve kept before removing every model
// provider family from its own process; only Herdr's own programs run with it, unchanged.
func projectTeamDependencies(ctx context.Context, herdrEnv []string) (project.TeamDependencies, error) {
	// Resolve only Herdr, when a team operation actually makes a command. Server startup
	// and unrelated management operations remain available without these programs on login PATH.
	commands := teamCommands{core: "herdr-core", app: "herdr-app", run: runServerTeamCommand, herdrEnv: herdrEnv}
	home := os.Getenv("HERDR_APP_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return project.TeamDependencies{}, err
		}
		home = filepath.Join(userHome, ".config", "herdr-app")
	}
	return project.TeamDependencies{Lifetime: ctx, Host: commands, RosterDirectory: filepath.Join(home, "data", "chorus-launcher", "rosters"), Shell: os.Getenv("SHELL"), Observer: teamObservations{commands: commands, stream: runTeamStream, client: &http.Client{Timeout: 65 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, chorusURL: "http://127.0.0.1:9440"}, Sender: unboundTeamSender{}}, nil
}

// json decodes command replies and checks JSON-level errors as well as process status.
func (c teamCommands) json(ctx context.Context, program string, result any, args ...string) error {
	raw, err := c.run(ctx, c.herdrEnv, program, args...)
	if err != nil {
		return err
	}
	var envelope *struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return errors.New("Herdr returned invalid JSON")
	}
	if envelope == nil {
		return errors.New("Herdr returned an empty result")
	}
	errorField := envelope.Error
	if len(errorField) > 0 && string(errorField) != "null" && string(errorField) != "false" && string(errorField) != `""` {
		return errors.New("Herdr reported a command error")
	}
	if err := json.Unmarshal(raw, result); err != nil {
		return errors.New("Herdr returned an invalid result")
	}
	return nil
}

// do sends exactly one JSON argument, never shell syntax or a stdin protocol.
func (c teamCommands) do(ctx context.Context, command string, payload any, result any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.json(ctx, c.app, result, "do", command, string(raw))
}

// Spaces reads local spaces and their tab/terminal mapping without changing window state.
func (c teamCommands) Spaces(ctx context.Context) ([]project.TeamSpace, error) {
	var result struct {
		Spaces *[]project.TeamSpace `json:"spaces"`
	}
	if err := c.json(ctx, c.app, &result, "ls"); err != nil {
		return nil, err
	}
	if result.Spaces == nil {
		return nil, errors.New("Herdr omitted its space listing")
	}
	local := make([]project.TeamSpace, 0, len(*result.Spaces))
	for _, space := range *result.Spaces {
		if !strings.ContainsAny(space.ID, "/\\") {
			local = append(local, space)
		}
	}
	return local, nil
}

// CreateSpace uses the space, tab and terminal identifiers returned by space.new.
func (c teamCommands) CreateSpace(ctx context.Context, name, folder string) (project.TeamSpace, error) {
	var result struct {
		Space    string `json:"space"`
		Tab      string `json:"tab"`
		Terminal string `json:"terminal"`
	}
	if err := c.do(ctx, "space.new", struct {
		Name   string `json:"name"`
		Folder string `json:"folder"`
	}{name, folder}, &result); err != nil {
		return project.TeamSpace{}, err
	}
	if result.Space == "" || result.Tab == "" || result.Terminal == "" {
		return project.TeamSpace{}, errors.New("space.new returned incomplete identifiers")
	}
	return project.TeamSpace{ID: result.Space, Name: name, Folder: folder, Tabs: []project.TeamTab{{ID: result.Tab, Terminals: []project.TeamPane{{Terminal: result.Terminal}}}}}, nil
}

// Terminals selects each registered handle with both owner labels before reading facts.
func (c teamCommands) Terminals(ctx context.Context, root, space string, handles []string) ([]project.TeamTerminal, error) {
	result := make([]project.TeamTerminal, 0, len(handles))
	for _, handle := range handles {
		var listing struct {
			Terminals *[]struct {
				Address string `json:"address"`
			} `json:"terminals"`
		}
		if err := c.json(ctx, c.core, &listing, "ls", "--json", "--label", "chorus.project="+root, "--label", "herdr.space="+space, "--label", "chorus.handle="+handle); err != nil {
			return nil, err
		}
		if listing.Terminals == nil {
			return nil, errors.New("Herdr omitted its terminal listing")
		}
		for _, item := range *listing.Terminals {
			if item.Address == "" {
				return nil, errors.New("Herdr returned a missing terminal identifier")
			}
			var terminal teamTerminalFacts
			if err := c.json(ctx, c.core, &terminal, "get", item.Address, "--json"); err != nil {
				return nil, err
			}
			result = append(result, terminal.terminal())
		}
	}
	return result, nil
}

// Focus shows the owned space and focuses one of its terminals before launcher capture.
func (c teamCommands) Focus(ctx context.Context, space project.TeamSpace) error {
	var result map[string]json.RawMessage
	if err := c.do(ctx, "space.select", struct {
		ID string `json:"id"`
	}{space.ID}, &result); err != nil {
		return err
	}
	for _, tab := range space.Tabs {
		for _, pane := range tab.Terminals {
			if pane.Terminal != "" {
				return c.do(ctx, "pane.focus", struct {
					Terminal string `json:"terminal"`
				}{pane.Terminal}, &result)
			}
		}
	}
	return errors.New("Herdr space has no terminal to focus")
}

// Overlays reads the names reported by herdr-app status; opening a mod is a toggle.
func (c teamCommands) Overlays(ctx context.Context) ([]string, error) {
	var result struct {
		Overlays *[]string `json:"overlays"`
	}
	if err := c.json(ctx, c.app, &result, "status"); err != nil {
		return nil, err
	}
	if result.Overlays == nil {
		return nil, errors.New("Herdr status omitted overlay names")
	}
	return *result.Overlays, nil
}

// OpenLauncher opens only the fixed Chorus launcher after the owner has checked overlays.
func (c teamCommands) OpenLauncher(ctx context.Context) error {
	var result map[string]json.RawMessage
	return c.do(ctx, "mod.open", struct {
		Name string `json:"name"`
	}{"chorus-launcher"}, &result)
}

// runServerTeamCommand resolves a required Herdr program at the external operation boundary
// and runs it with env.
func runServerTeamCommand(ctx context.Context, env []string, program string, args ...string) ([]byte, error) {
	resolved, err := exec.LookPath(program)
	if err != nil {
		return nil, errors.New("required Herdr program is unavailable on the server login PATH")
	}
	return runTeamCommand(ctx, env, resolved, args...)
}

// withoutProviderFamilies returns environ without any model provider family variable. The
// surla command gives it to the programs that must never receive the shell's provider
// settings: Chorus and the browser opener. Herdr's programs keep the environment unchanged.
func withoutProviderFamilies(environ []string) []string {
	_, rest := modelsettings.Split(environ)
	return rest
}

// takeProviderFamilies is serve's first step: it returns a copy of the server's original
// environment, for Herdr's own programs only, and removes every model provider family from
// this process, so no later read or child takes a provider setting from Herdr's environment.
// Removal changes what children inherit, not the server's initial environment block.
func takeProviderFamilies() ([]string, error) {
	original := os.Environ()
	family, _ := modelsettings.Split(original)
	for _, kv := range family {
		name, _, _ := strings.Cut(kv, "=")
		if err := os.Unsetenv(name); err != nil {
			return nil, fmt.Errorf("remove model provider settings from the project server's environment: %w", err)
		}
	}
	return original, nil
}
