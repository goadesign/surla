// This file verifies user actions with fresh recorded ownership and no real window effects.
package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	genproject "github.com/goadesign/surla/gen/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	goa "goa.design/goa/v3/pkg"
)

// TestTeamReadCreatesNothing returns a stable expected name and registered roles only.
func TestTeamReadCreatesNothing(t *testing.T) {
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	host := deps.Host.(*teamFixture)
	server, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	before, err := os.ReadDir(state.Dir)
	require.NoError(t, err)
	first, err := server.ReadTeam(t.Context())
	require.NoError(t, err)
	second, err := server.ReadTeam(t.Context())
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.NotEmpty(t, first.Team)
	assert.Empty(t, first.Roles)
	assert.Empty(t, host.calls)
	assert.Empty(t, state.Record.Team)
	assert.Empty(t, state.Record.Agents)
	after, err := os.ReadDir(state.Dir)
	require.NoError(t, err)
	assert.Equal(t, len(before), len(after))
}

// TestEnsureTeamTerminalOwnership preserves the team-start owner's fresh label
// checks independently of the supervisor's checks. Each wrong label alone must
// prevent a live non-shell terminal from counting as this project's running role.
func TestEnsureTeamTerminalOwnership(t *testing.T) {
	for _, label := range []string{"owned", "chorus.project", "herdr.space", "chorus.handle"} {
		t.Run(label, func(t *testing.T) {
			state := testState(t)
			deps := fixtureTeamDependencies(state)
			host := deps.Host.(*teamFixture)
			server, err := NewServer(state, func() {}, deps)
			require.NoError(t, err)
			first, err := server.EnsureTeam(teamContext(state))
			require.NoError(t, err)
			host.terminals = []TeamTerminal{{State: "live", Foreground: &TeamForeground{Argv: []string{"codex"}}, Labels: map[string]string{
				"chorus.project": state.Record.Directory,
				"herdr.space":    first.Space,
				"herdr.tab":      "tab",
				"chorus.handle":  first.Roles[0].Handle,
			}}}
			if label != "owned" {
				host.terminals[0].Labels[label] = "another-owner"
			}
			result, err := server.EnsureTeam(teamContext(state))
			require.NoError(t, err)
			require.Len(t, result.Roles, 1)
			assert.Equal(t, label == "owned", result.Roles[0].Running)
			if label == "owned" {
				assert.Equal(t, "not_needed", result.Dialog)
			} else {
				assert.Equal(t, "opened", result.Dialog)
			}
		})
	}
}

// TestTeamShowPaneOverlayAndOwnership refuses all open overlays before any
// discovery/window operation, and never focuses a differently owned fresh pane.
func TestTeamShowPaneOverlayAndOwnership(t *testing.T) {
	for _, scenario := range []string{"owned", "launcher", "other overlay", "wrong project", "wrong space", "wrong handle", "ended", "unregistered", "space folder changed"} {
		t.Run(scenario, func(t *testing.T) {
			o, f, _ := newSupervisionFixture(t)
			host := o.server.team.dependencies.Host.(*teamFixture)
			host.spaces = []TeamSpace{{ID: "space", Folder: o.selection.Root, Tabs: []TeamTab{{ID: "tab", Terminals: []TeamPane{{Terminal: "other-pane"}, {Terminal: "terminal"}}}}}}
			role := "builder"
			switch scenario {
			case "launcher":
				host.overlays = []string{"chorus-launcher"}
			case "other overlay":
				host.overlays = []string{"frame-palette"}
			case "wrong project":
				f.terminal.Labels["chorus.project"] = "/other"
			case "wrong space":
				f.terminal.Labels["herdr.space"] = "other"
			case "wrong handle":
				f.terminal.Labels["chorus.handle"] = "other"
			case "ended":
				f.terminal.State = "finished"
			case "unregistered":
				role = "guide"
			case "space folder changed":
				host.spaces[0].Folder = "/other"
				host.spaces[0].Name = "renamed by user"
			}
			err := o.server.ShowPane(t.Context(), role)
			if scenario == "owned" || scenario == "space folder changed" {
				require.NoError(t, err)
				assert.Equal(t, TeamSpace{ID: "space", Name: host.spaces[0].Name, Folder: host.spaces[0].Folder, Tabs: []TeamTab{{ID: "tab", Terminals: []TeamPane{{Terminal: "terminal"}}}}}, host.focused)
				assert.Equal(t, []string{"overlays", "spaces", "focus"}, host.calls)
				assert.Equal(t, []string{"terminal"}, f.reads)
			} else {
				require.Error(t, err)
				assert.NotContains(t, host.calls, "focus")
				assert.Empty(t, host.focused.ID)
			}
			if scenario == "launcher" || scenario == "other overlay" {
				assert.Equal(t, []string{"overlays"}, host.calls)
				assert.Empty(t, f.reads)
			}
		})
	}
}

// TestShowPaneHerdrFailureNamesRetry gives a failed Show pane the Herdr step without
// sending the user to start agents that are already running: the step retries the
// action the user took.
func TestShowPaneHerdrFailureNamesRetry(t *testing.T) {
	o, _, _ := newSupervisionFixture(t)
	host := o.server.team.dependencies.Host.(*teamFixture)
	o.server.team.dependencies.Host = failingOverlays{host}
	err := o.server.ShowPane(t.Context(), "builder")
	var page *goa.ServiceError
	require.ErrorAs(t, err, &page)
	assert.Equal(t, "team_unavailable", page.Name)
	assert.Contains(t, page.Message, "open the Herdr app, then try again")
	assert.NotContains(t, page.Message, "start the agents")
}

// TestTeamUserStartReusesPreparation proves the same space, registry, roster
// and declared management refusals serve the user's action without a new flow.
func TestTeamUserStartReusesPreparation(t *testing.T) {
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	host := deps.Host.(*teamFixture)
	server, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	first, err := server.StartAgents(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "opened", first.Dialog)
	second, err := server.EnsureTeam(teamContext(state))
	require.NoError(t, err)
	assert.Equal(t, first.Team, second.Team)
	assert.Equal(t, first.Space, second.Space)
	assert.Len(t, state.Record.Agents, 1)
	assert.Equal(t, 1, host.creates)
	for _, overlay := range []string{"chorus-launcher", "frame-palette"} {
		host.overlays = []string{overlay}
		host.calls = nil
		result, err := server.StartAgents(t.Context())
		if overlay == "chorus-launcher" {
			require.NoError(t, err)
			assert.Equal(t, "already_open", result.Dialog)
		} else {
			assert.Error(t, err)
		}
		assert.NotContains(t, host.calls, "focus")
		assert.NotContains(t, host.calls, "open")
	}
	_, err = os.Stat(filepath.Join(deps.RosterDirectory, first.Space+".json"))
	require.NoError(t, err)
}

// TestTeamUserStartPreservesSpaceConflict exercises the real space owner through
// userTeamError when the saved space is missing and two replacements match.
func TestTeamUserStartPreservesSpaceConflict(t *testing.T) {
	state := testState(t)
	state.Record.Space = "missing-space"
	deps := fixtureTeamDependencies(state)
	host := deps.Host.(*teamFixture)
	for _, id := range []string{"first", "second"} {
		host.spaces = append(host.spaces, TeamSpace{
			ID: id, Name: teamName(filepath.Base(state.Record.Directory), state.Record.ID),
			Folder: state.Record.Directory,
		})
	}
	server, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	result, err := server.StartAgents(t.Context())
	assert.Nil(t, result)
	var conflict *genproject.TeamSpaceConflict
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "ambiguous_space", conflict.Name)
	assert.Equal(t, 2, conflict.Count)
	assert.Empty(t, state.Record.Agents)
	assert.Zero(t, host.creates)
	assert.Equal(t, []string{"overlays", "spaces"}, host.calls)
}

// TestUserTeamErrorBoundary preserves the original typed error, including its
// wrapping, while keeping peer-authored details out of other user failures.
func TestUserTeamErrorBoundary(t *testing.T) {
	conflict := &genproject.TeamSpaceConflict{Name: "ambiguous_space", Count: 2}
	wrapped := fmt.Errorf("private peer output: %w", conflict)
	assert.Same(t, conflict, userTeamError(conflict))
	assert.Same(t, wrapped, userTeamError(wrapped))
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"window_busy", genproject.MakeWindowBusy(errors.New("private peer output"))},
		{"team_unavailable", &genproject.TeamUnavailable{Name: "team_unavailable", Cause: "herdr"}},
		{"team_unavailable", errors.New("private peer output")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var failure *goa.ServiceError
			require.ErrorAs(t, userTeamError(tc.err), &failure)
			assert.Equal(t, tc.name, failure.Name)
			assert.NotContains(t, failure.Message, "private peer output")
			assert.NotContains(t, failure.Message, "check Herdr")
		})
	}
}

// TestTeamUnavailableStepIsBounded records a fixed owner step and no private cause.
func TestTeamUnavailableStepIsBounded(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous); assert.NoError(t, provider.Shutdown(context.Background())) })
	ctx, span := otel.Tracer("test").Start(t.Context(), "test")
	err := teamUnavailable(ctx, "overlays", "herdr")
	span.End()
	assert.Error(t, err)
	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	var step string
	for _, attribute := range spans[0].Attributes {
		if attribute.Key == "team.failed_step" {
			step = attribute.Value.AsString()
		}
	}
	assert.Equal(t, "overlays", step)
	for _, event := range spans[0].Events {
		assert.NotContains(t, event.Name, "/")
		for _, attribute := range event.Attributes {
			assert.NotContains(t, attribute.Value.AsString(), "/Users/")
		}
	}
}

// failingOverlays stands in for a Herdr window whose status command fails.
type failingOverlays struct{ *teamFixture }

// Overlays reports a failed Herdr status command.
func (failingOverlays) Overlays(context.Context) ([]string, error) {
	return nil, errors.New("fixture Herdr status failed")
}

// TestTeamFailureNamesItsCause gives each team failure the cause that selects its
// step, so neither the CLI nor the page reduces every failure to checking Herdr.
func TestTeamFailureNamesItsCause(t *testing.T) {
	for _, tc := range []struct {
		name, cause, page string
		damage            func(*State, *TeamDependencies)
	}{
		{
			name: "herdr command", cause: "herdr", page: "open the Herdr app",
			damage: func(_ *State, deps *TeamDependencies) {
				deps.Host = failingOverlays{&teamFixture{}}
			},
		},
		{
			name: "roster folder", cause: "roster_folder", page: "data/chorus-launcher/rosters",
			damage: func(state *State, deps *TeamDependencies) {
				file := filepath.Join(state.Dir, "not-a-folder")
				require.NoError(t, os.WriteFile(file, nil, 0o600))
				deps.RosterDirectory = filepath.Join(file, "rosters")
			},
		},
		{
			name: "agent credential", cause: "project_state", page: "run surla in the project folder",
			damage: func(state *State, _ *TeamDependencies) {
				for handle := range state.Record.Agents {
					require.NoError(t, os.Chmod(state.AgentFile(handle), 0o644))
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := testState(t)
			deps := fixtureTeamDependencies(state)
			server, err := NewServer(state, func() {}, deps)
			require.NoError(t, err)
			_, err = server.EnsureTeam(teamContext(state))
			require.NoError(t, err, "the first start registers the guide")
			tc.damage(state, &deps)
			server, err = NewServer(state, func() {}, deps)
			require.NoError(t, err)
			_, err = server.EnsureTeam(teamContext(state))
			var unavailable *genproject.TeamUnavailable
			require.ErrorAs(t, err, &unavailable)
			assert.Equal(t, "team_unavailable", unavailable.Name)
			assert.Equal(t, tc.cause, unavailable.Cause)
			var page *goa.ServiceError
			require.ErrorAs(t, userTeamError(err), &page)
			assert.Contains(t, page.Message, tc.page)
			assert.NotContains(t, page.Message, "check Herdr")
		})
	}
}
