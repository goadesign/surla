// This file verifies user actions with fresh recorded ownership and no real window effects.
package project

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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
			}
			err := o.server.ShowPane(t.Context(), role)
			if scenario == "owned" {
				require.NoError(t, err)
				assert.Equal(t, TeamSpace{ID: "space", Folder: o.selection.Root, Tabs: []TeamTab{{ID: "tab", Terminals: []TeamPane{{Terminal: "terminal"}}}}}, host.focused)
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

// TestTeamUnavailableStepIsBounded records a fixed owner step and no private cause.
func TestTeamUnavailableStepIsBounded(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous); assert.NoError(t, provider.Shutdown(context.Background())) })
	ctx, span := otel.Tracer("test").Start(t.Context(), "test")
	err := teamUnavailable(ctx, "overlays")
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
