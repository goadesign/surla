// This file verifies the reviewed team-start boundaries with strict persisted fixtures.
package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	genproject "github.com/goadesign/surla/gen/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestTeamWrittenRosterClosedContract compares every field delivered to the actual launcher path.
func TestTeamWrittenRosterClosedContract(t *testing.T) {
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	host := deps.Host.(*teamFixture)
	server, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	team, err := server.EnsureTeam(teamContext(state))
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(deps.RosterDirectory, team.Space+".json"))
	require.NoError(t, err)
	handle := "guide-" + state.Record.ID[:12]
	message := "Work through the Story and the change with the user in this project's pane. Use your surla connection, starting with the surla://golden-path guidance resource and list_changes/get_change. Only the user confirms intent and approves results, on the views page."
	expected := fmt.Sprintf(`{"root":%q,"swarm":%q,"roles":[{"role":"guide","title":"Guide","handle":%q,"folder":%q,"first_message":%q,"connections":[{"name":"surla","url":"http://127.0.0.1:%d/mcp","credential_file":%q}],"place":null}]}`, state.Record.Directory, team.Team, handle, state.Record.Directory, message, state.Record.Port, state.AgentFile(handle))
	assert.JSONEq(t, expected, string(raw))
	assert.Equal(t, []string{"overlays", "spaces", "create", "terminals", "focus", "open"}, host.calls, "observe overlays and focus this space before opening its launcher")
}

// TestThreeRoleWrittenRoster validates all placements and connection fields after atomic publication.
func TestThreeRoleWrittenRoster(t *testing.T) {
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	server, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	state.Record.Team = teamName("— 123 ! a very long project folder name", state.Record.ID)
	var roles []launcherRole
	var expectedRoles []map[string]any
	for i, role := range []string{"guide", "builder", "test_writer"} {
		handle := role + "-" + state.Record.ID[:12]
		_, err := server.AddAgent(teamContext(state), &genproject.AddAgentPayload{ActorID: handle, Role: role})
		require.NoError(t, err)
		roles = append(roles, launcherRole{Role: role, Title: role, Handle: handle, Folder: state.Record.Directory, FirstMessage: "Use the Surla connection."})
		var place any
		if i == 1 {
			place = map[string]string{"beside": "guide-" + state.Record.ID[:12], "direction": "right"}
		}
		if i == 2 {
			place = map[string]string{"beside": "builder-" + state.Record.ID[:12], "direction": "down"}
		}
		expectedRoles = append(expectedRoles, map[string]any{"role": role, "title": role, "handle": handle, "folder": state.Record.Directory, "first_message": "Use the Surla connection.", "connections": []map[string]string{{"name": "surla", "url": fmt.Sprintf("http://127.0.0.1:%d/mcp", state.Record.Port), "credential_file": state.AgentFile(handle)}}, "place": place})
	}
	roster, err := server.teamRoster(roles)
	require.NoError(t, err)
	destination := filepath.Join(deps.RosterDirectory, "three-roles.json")
	require.NoError(t, os.MkdirAll(deps.RosterDirectory, 0o700))
	require.NoError(t, writeJSON(destination, roster))
	written, err := os.ReadFile(destination)
	require.NoError(t, err)
	expected, err := json.Marshal(map[string]any{"root": state.Record.Directory, "swarm": state.Record.Team, "roles": expectedRoles})
	require.NoError(t, err)
	assert.JSONEq(t, string(expected), string(written))
	assert.Regexp(t, `^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`, state.Record.Team)
}

// TestOpenOverlayLeavesWindowUntouched refuses unprepared projects without saving anything.
func TestOpenOverlayLeavesWindowUntouched(t *testing.T) {
	for _, overlay := range []string{"chorus-launcher", "frame-palette"} {
		t.Run(overlay, func(t *testing.T) {
			state := testState(t)
			deps := fixtureTeamDependencies(state)
			host := deps.Host.(*teamFixture)
			host.overlays = []string{overlay}
			before, err := os.ReadFile(filepath.Join(state.Dir, "project.json"))
			require.NoError(t, err)
			server, err := NewServer(state, func() {}, deps)
			require.NoError(t, err)
			_, err = server.EnsureTeam(teamContext(state))
			require.Error(t, err)
			assert.Zero(t, host.creates)
			assert.Empty(t, state.Record.Agents)
			assert.Empty(t, state.Record.Team)
			assert.Empty(t, state.Record.Space)
			after, err := os.ReadFile(filepath.Join(state.Dir, "project.json"))
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.NotContains(t, host.calls, "focus")
			assert.NotContains(t, host.calls, "open")
			assert.NotContains(t, host.calls, "create")
			_, err = os.Stat(filepath.Join(state.Dir, "agents"))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

// TestPreparedLauncherNeverChangesFocus preserves a possibly unrelated launcher's captured project.
func TestPreparedLauncherNeverChangesFocus(t *testing.T) {
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	host := deps.Host.(*teamFixture)
	server, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	_, err = server.EnsureTeam(teamContext(state))
	require.NoError(t, err)
	host.calls = nil
	host.overlays = []string{"chorus-launcher"}
	team, err := server.EnsureTeam(teamContext(state))
	require.NoError(t, err)
	assert.Equal(t, "already_open", team.Dialog)
	assert.NotContains(t, host.calls, "focus")
	assert.NotContains(t, host.calls, "open")
	assert.NotContains(t, host.calls, "create")
	assert.Equal(t, "overlays", host.calls[0])
}

// TestSavedSpaceWinsNameMatches keeps a renamed saved space despite duplicate name/folder matches.
func TestSavedSpaceWinsNameMatches(t *testing.T) {
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	host := deps.Host.(*teamFixture)
	state.Record.Team = teamName(filepath.Base(state.Record.Directory), state.Record.ID)
	state.Record.Space = "saved"
	require.NoError(t, state.save())
	host.spaces = []TeamSpace{{ID: "candidate1", Name: state.Record.Team, Folder: state.Record.Directory}, {ID: "candidate2", Name: state.Record.Team, Folder: state.Record.Directory}, {ID: "saved", Name: "renamed by user", Folder: "/different", Tabs: []TeamTab{{ID: "tab", Terminals: []TeamPane{{Terminal: "terminal"}}}}}}
	server, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	team, err := server.EnsureTeam(teamContext(state))
	require.NoError(t, err)
	assert.Equal(t, "saved", team.Space)
	assert.Zero(t, host.creates)
}

// TestRosterOmitsMissingWorkingFolders keeps valid roles and rejects invalid existing inputs.
func TestRosterOmitsMissingWorkingFolders(t *testing.T) {
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	server, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	state.Record.Team = teamName("team", state.Record.ID)
	var roles []launcherRole
	for _, role := range []string{"guide", "builder", "test_writer"} {
		handle := role + "-" + state.Record.ID[:12]
		_, err := server.AddAgent(teamContext(state), &genproject.AddAgentPayload{ActorID: handle, Role: role})
		require.NoError(t, err)
		folder := state.Record.Directory
		if role == "test_writer" {
			folder = filepath.Join(state.Record.Directory, "missing-test-writer")
		}
		roles = append(roles, launcherRole{Role: role, Title: role, Handle: handle, Folder: folder, FirstMessage: "Use Surla."})
	}
	roster, err := server.teamRoster(roles)
	require.NoError(t, err)
	require.Len(t, roster.Roles, 2)
	assert.Equal(t, "builder", roster.Roles[1].Role)
	assert.Equal(t, "right", roster.Roles[1].Place.Direction)
	existingFile := filepath.Join(state.Record.Directory, "not-a-folder")
	require.NoError(t, os.WriteFile(existingFile, []byte("fixture"), 0o600))
	roles[2].Folder = existingFile
	_, err = server.teamRoster(roles)
	require.Error(t, err)
	cwd, err := os.Getwd()
	require.NoError(t, err)
	relative, err := filepath.Rel(cwd, state.Record.Directory)
	require.NoError(t, err)
	roles[2].Folder = relative
	_, err = server.teamRoster(roles)
	require.Error(t, err)
	roles[2].Folder = state.Record.Directory
	require.NoError(t, os.Remove(state.AgentFile(roles[2].Handle)))
	_, err = server.teamRoster(roles)
	require.Error(t, err)
}

// TestTeamErrorsKeepPrivatePathsInsideServer checks the public error rather than CLI filtering.
func TestTeamErrorsKeepPrivatePathsInsideServer(t *testing.T) {
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	server, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	handle := "guide-" + state.Record.ID[:12]
	state.Record.Agents[handle] = "guide"
	_, err = server.EnsureTeam(teamContext(state))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), state.Dir)
	assert.NotContains(t, err.Error(), state.Record.Directory)
	assert.NotContains(t, err.Error(), strings.Join([]string{"--label", "chorus.project=" + state.Record.Directory}, " "))
}

// TestTeamSpansReportBoundedOutcomes records state without exporting project paths or command arguments.
func TestTeamSpansReportBoundedOutcomes(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer otel.SetTracerProvider(previous)
	defer func() { assert.NoError(t, provider.Shutdown(context.Background())) }()
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	host := deps.Host.(*teamFixture)
	server, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	team, err := server.EnsureTeam(teamContext(state))
	require.NoError(t, err)
	host.overlays = []string{"chorus-launcher"}
	_, err = server.EnsureTeam(teamContext(state))
	require.NoError(t, err)
	host.overlays = nil
	host.terminals = []TeamTerminal{{State: "live", Labels: map[string]string{"chorus.handle": team.Roles[0].Handle, "chorus.project": state.Record.Directory, "herdr.space": team.Space, "herdr.tab": "tab"}, Foreground: &TeamForeground{Argv: []string{"codex"}}}}
	_, err = server.EnsureTeam(teamContext(state))
	require.NoError(t, err)
	host.overlays = []string{"frame-palette"}
	_, err = server.EnsureTeam(teamContext(state))
	require.Error(t, err)
	host.overlays = nil
	require.NoError(t, os.Remove(state.AgentFile(team.Roles[0].Handle)))
	_, err = server.EnsureTeam(teamContext(state))
	require.Error(t, err)
	spans := recorder.Ended()
	require.Len(t, spans, 5)
	assert.Equal(t, codes.Unset, spans[3].Status().Code)
	assert.Empty(t, spans[3].Events())
	assert.Equal(t, codes.Error, spans[4].Status().Code)
	assert.NotEmpty(t, spans[4].Events())
	for _, span := range spans {
		for _, event := range span.Events() {
			for _, field := range event.Attributes {
				assert.NotContains(t, field.Value.AsString(), state.Dir)
				assert.NotContains(t, field.Value.AsString(), state.Record.Directory)
			}
		}
	}

	for i, dialog := range []string{"opened", "already_open", "not_needed"} {
		attrs := map[string]attribute.Value{}
		for _, value := range spans[i].Attributes() {
			attrs[string(value.Key)] = value.Value
		}
		assert.Equal(t, dialog, attrs["team.dialog"].AsString())
		assert.Equal(t, int64(1), attrs["team.roles"].AsInt64())
		running := int64(0)
		if i == 2 {
			running = 1
		}
		assert.Equal(t, running, attrs["team.roles_running"].AsInt64())
		assert.Equal(t, codes.Unset, spans[i].Status().Code)
	}
}

// failedOverlayHost supplies a hostile external error without making any window changes.
type failedOverlayHost struct{ TeamHost }

// Overlays refuses with a fabricated private cause that must never reach span events or public errors.
func (failedOverlayHost) Overlays(context.Context) ([]string, error) {
	return nil, errors.New("/private/hostile-project/credential --selected-argument secret-placeholder")
}

// TestTeamFailureEventsAreBounded inspects exception messages as well as ordinary span attributes.
func TestTeamFailureEventsAreBounded(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer otel.SetTracerProvider(previous)
	defer func() { assert.NoError(t, provider.Shutdown(context.Background())) }()
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	deps.Host = failedOverlayHost{TeamHost: deps.Host}
	server, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	_, err = server.EnsureTeam(teamContext(state))
	require.Error(t, err)
	spans := recorder.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	require.NotEmpty(t, spans[0].Events())
	for _, event := range spans[0].Events() {
		for _, field := range event.Attributes {
			value := field.Value.AsString()
			for _, private := range []string{"hostile-project", "--selected-argument", "secret-placeholder", state.Dir} {
				assert.NotContains(t, value, private)
			}
			if field.Key == "exception.message" {
				assert.Equal(t, "project team could not be prepared", value)
			}
		}
	}
	assert.NotContains(t, err.Error(), "hostile-project")
}
