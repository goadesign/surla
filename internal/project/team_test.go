// This file exercises team ownership through recorded Herdr effects and temporary rosters.
package project

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	genproject "github.com/goadesign/surla/gen/project"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// teamFixture records all host effects and supplies deterministic space/terminal observations.
type teamFixture struct {
	spaces    []TeamSpace
	terminals []TeamTerminal
	overlays  []string
	calls     []string
	creates   int
	focused   TeamSpace
}

// Spaces returns the fixture listing without reading Herdr.
func (f *teamFixture) Spaces(context.Context) ([]TeamSpace, error) {
	f.calls = append(f.calls, "spaces")
	return f.spaces, nil
}

// CreateSpace records the requested local space and returns its terminal mapping.
func (f *teamFixture) CreateSpace(_ context.Context, name, folder string) (TeamSpace, error) {
	f.creates++
	s := TeamSpace{ID: fmt.Sprintf("space-%d", f.creates), Name: name, Folder: folder, Tabs: []TeamTab{{ID: "tab", Terminals: []TeamPane{{Terminal: "terminal"}}}}}
	f.spaces = append(f.spaces, s)
	f.calls = append(f.calls, "create")
	return s, nil
}

// Terminals returns only fixture foreground and label observations.
func (f *teamFixture) Terminals(context.Context, string, string, []string) ([]TeamTerminal, error) {
	f.calls = append(f.calls, "terminals")
	return f.terminals, nil
}

// Focus records the requested fixture focus without touching a window.
func (f *teamFixture) Focus(_ context.Context, space TeamSpace) error {
	f.focused = space
	f.calls = append(f.calls, "focus")
	return nil
}

// Overlays returns the fixture dialog names.
func (f *teamFixture) Overlays(context.Context) ([]string, error) {
	f.calls = append(f.calls, "overlays")
	return f.overlays, nil
}

// OpenLauncher records the request without opening a real dialog.
func (f *teamFixture) OpenLauncher(context.Context) error {
	f.calls = append(f.calls, "open")
	return nil
}

// fixtureTeamDependencies prevents all external effects in server authentication/lifecycle tests.
func fixtureTeamDependencies(state *State) TeamDependencies {
	return TeamDependencies{Lifetime: context.Background(), Host: &teamFixture{}, RosterDirectory: filepath.Join(state.Dir, "test-rosters"), Shell: "/bin/zsh"}
}

// teamContext grants management through the same trusted principal boundary used by HTTP.
func teamContext(state *State) context.Context {
	return controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: state.Record.Owner, CallerID: "management", Kind: controlplane.CallerManagement})
}

// TestEnsureTeamLifecycle verifies the server or command contract using only temporary state and stand-ins.
func TestEnsureTeamLifecycle(t *testing.T) {
	state := testState(t)
	dependencies := fixtureTeamDependencies(state)
	host := dependencies.Host.(*teamFixture)
	server, err := NewServer(state, func() {}, dependencies)
	require.NoError(t, err)
	first, err := server.EnsureTeam(teamContext(state))
	require.NoError(t, err)
	assert.Equal(t, "opened", first.Dialog)
	assert.Equal(t, 1, host.creates)
	assert.Len(t, state.Record.Agents, 1)
	path := filepath.Join(dependencies.RosterDirectory, first.Space+".json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var roster map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &roster))
	assert.ElementsMatch(t, []string{"root", "swarm", "roles"}, keys(roster))
	var roles []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(roster["roles"], &roles))
	require.Len(t, roles, 1)
	assert.ElementsMatch(t, []string{"role", "title", "handle", "folder", "first_message", "connections", "place"}, keys(roles[0]))
	secret, err := os.ReadFile(state.AgentFile(first.Roles[0].Handle))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), string(secret))
	host.calls = nil
	host.overlays = []string{"chorus-launcher"}
	second, err := server.EnsureTeam(teamContext(state))
	require.NoError(t, err)
	assert.Equal(t, "already_open", second.Dialog)
	assert.NotContains(t, host.calls, "open")
	assert.Equal(t, 1, host.creates)
	assert.Len(t, state.Record.Agents, 1)
	host.terminals = []TeamTerminal{{State: "live", Labels: map[string]string{"chorus.handle": first.Roles[0].Handle, "chorus.project": state.Record.Directory, "herdr.space": first.Space, "herdr.tab": "tab"}, Foreground: &TeamForeground{Argv: []string{"codex"}}}}
	host.calls = nil
	host.overlays = nil
	running, err := server.EnsureTeam(teamContext(state))
	require.NoError(t, err)
	assert.Equal(t, "not_needed", running.Dialog)
	assert.True(t, running.Roles[0].Running)
	assert.NotContains(t, host.calls, "focus")
	host.spaces = nil
	host.terminals = nil
	host.overlays = nil
	replacement, err := server.EnsureTeam(teamContext(state))
	require.NoError(t, err)
	assert.Equal(t, first.Team, replacement.Team)
	assert.NotEqual(t, first.Space, replacement.Space)
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestEnsureTeamAdoptionConflictAndAccess verifies the server or command contract using only temporary state and stand-ins.
func TestEnsureTeamAdoptionConflictAndAccess(t *testing.T) {
	for _, kind := range []controlplane.CallerKind{controlplane.CallerUser, controlplane.CallerGuide, controlplane.CallerBuilder, controlplane.CallerTestWriter} {
		t.Run(string(kind), func(t *testing.T) {
			state := testState(t)
			deps := fixtureTeamDependencies(state)
			s, err := NewServer(state, func() {}, deps)
			require.NoError(t, err)
			ctx := controlplane.WithPrincipal(t.Context(), controlplane.Principal{ID: state.Record.Owner, Kind: kind})
			_, err = s.EnsureTeam(ctx)
			require.Error(t, err)
			assert.Empty(t, deps.Host.(*teamFixture).calls)
			assert.Empty(t, state.Record.Agents)
			assert.Empty(t, state.Record.Team)
		})
	}
	for _, count := range []int{1, 2} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			state := testState(t)
			deps := fixtureTeamDependencies(state)
			host := deps.Host.(*teamFixture)
			for i := 0; i < count; i++ {
				host.spaces = append(host.spaces, TeamSpace{ID: string(rune('a' + i)), Name: teamName(filepath.Base(state.Record.Directory), state.Record.ID), Folder: state.Record.Directory, Tabs: []TeamTab{{ID: "tab", Terminals: []TeamPane{{Terminal: "terminal"}}}}})
			}
			s, err := NewServer(state, func() {}, deps)
			require.NoError(t, err)
			result, err := s.EnsureTeam(teamContext(state))
			if count == 2 {
				var conflict *genproject.TeamSpaceConflict
				require.ErrorAs(t, err, &conflict)
				assert.Equal(t, 2, conflict.Count)
				assert.Empty(t, state.Record.Agents)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "a", result.Space)
			}
			assert.Zero(t, host.creates)
		})
	}
}

// TestEnsureTeamOtherDialog verifies the server or command contract using only temporary state and stand-ins.
func TestEnsureTeamOtherDialog(t *testing.T) {
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	host := deps.Host.(*teamFixture)
	host.overlays = []string{"frame-palette"}
	s, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	_, err = s.EnsureTeam(teamContext(state))
	require.Error(t, err)
	assert.NotContains(t, host.calls, "open")
}

// TestTeamNamesAndThreeRoleRoster verifies the server or command contract using only temporary state and stand-ins.
func TestTeamNamesAndThreeRoleRoster(t *testing.T) {
	state := testState(t)
	state.Record.Team = teamName("— 123 ! a very long project folder name", state.Record.ID)
	deps := fixtureTeamDependencies(state)
	s, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	var roles []launcherRole
	for _, role := range []string{"guide", "builder", "test_writer"} {
		handle := teamName(role, state.Record.ID)
		assert.LessOrEqual(t, len(handle), 32)
		assert.Regexp(t, `^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`, handle)
		_, err := registerTestAgent(s, handle, role, s.state.Record.Directory)
		require.NoError(t, err)
		roles = append(roles, launcherRole{Role: role, Title: role, Handle: handle, Folder: state.Record.Directory, FirstMessage: "Use your Surla connection."})
	}
	roster, err := s.teamRoster(roles)
	require.NoError(t, err)
	assert.Nil(t, roster.Roles[0].Place)
	assert.Equal(t, &launcherPlace{Beside: roles[0].Handle, Direction: "right"}, roster.Roles[1].Place)
	assert.Equal(t, &launcherPlace{Beside: roles[1].Handle, Direction: "down"}, roster.Roles[2].Place)
	other := testState(t)
	assert.NotEqual(t, state.Record.Team, teamName("— 123 ! a very long project folder name", other.Record.ID))
	assert.LessOrEqual(t, len(state.Record.Team), 32)
	assert.Regexp(t, `^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`, state.Record.Team)
}

// TestRoleRunningSharedContract verifies the server or command contract using only temporary state and stand-ins.
func TestRoleRunningSharedContract(t *testing.T) {
	for _, test := range []struct {
		name, state string
		argv        []string
		want        bool
	}{{"no foreground", "live", nil, true}, {"empty", "live", []string{}, true}, {"finished", "finished", []string{"codex"}, false}, {"agent", "live", []string{"/usr/bin/claude"}, true}, {"shell", "live", []string{"/bin/zsh", "-l"}, false}, {"login", "live", []string{"-zsh", "-li"}, false}, {"custom", "live", []string{"/bin/custom"}, false}, {"wrapper", "live", []string{"zsh", "-lic", "exec codex"}, true}, {"command", "live", []string{"bash", "-c", "codex"}, true}, {"second flag", "live", []string{"sh", "-l", "-c"}, true}, {"long flag", "live", []string{"sh", "--command"}, false}, {"nonflag", "live", []string{"sh", "abc"}, false}, {"xonsh", "live", []string{"xonsh"}, false}, {"elvish", "live", []string{"elvish"}, false}} {
		t.Run(test.name, func(t *testing.T) {
			terminal := TeamTerminal{State: test.state}
			if test.argv != nil {
				terminal.Foreground = &TeamForeground{Argv: test.argv}
			}
			assert.Equal(t, test.want, roleRunning(terminal, "/bin/custom"))
		})
	}
}

// keys supports strict schema comparisons without depending on JSON key ordering.
func keys[T any](values map[string]T) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	return result
}

// TestEnsureTeamCleanupRetry retains the old space until its stale roster is removed.
func TestEnsureTeamCleanupRetry(t *testing.T) {
	state := testState(t)
	state.Record.Team = teamName(filepath.Base(state.Record.Directory), state.Record.ID)
	state.Record.Space = "old"
	require.NoError(t, state.save())
	deps := fixtureTeamDependencies(state)
	old := filepath.Join(deps.RosterDirectory, "old.json")
	require.NoError(t, os.MkdirAll(old, 0o700))
	blocker := filepath.Join(old, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("retry fixture"), 0o600))
	s, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	_, err = s.EnsureTeam(teamContext(state))
	require.Error(t, err)
	assert.Equal(t, "old", state.Record.Space)
	loaded, err := Discover(filepath.Dir(state.Dir), state.Record.Directory)
	require.NoError(t, err)
	assert.Equal(t, "old", loaded.Record.Space)
	require.NoError(t, os.Remove(blocker))
	result, err := s.EnsureTeam(teamContext(state))
	require.NoError(t, err)
	assert.Equal(t, "space-1", result.Space)
	assert.Equal(t, 1, deps.Host.(*teamFixture).creates)
	_, err = os.Stat(old)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestEnsureTeamOwnedLifetime distinguishes caller disconnection from server shutdown.
func TestEnsureTeamOwnedLifetime(t *testing.T) {
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	s, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	caller, cancel := context.WithCancel(teamContext(state))
	cancel()
	_, err = s.EnsureTeam(caller)
	require.NoError(t, err)
	state = testState(t)
	deps = fixtureTeamDependencies(state)
	owner, cancel := context.WithCancel(context.Background())
	deps.Lifetime = owner
	cancel()
	s, err = NewServer(state, func() {}, deps)
	require.NoError(t, err)
	_, err = s.EnsureTeam(teamContext(state))
	require.Error(t, err)
	assert.Empty(t, deps.Host.(*teamFixture).calls)
}

// TestEnsureTeamGuideInvariant rejects mismatched roles and missing credentials without a roster.
func TestEnsureTeamGuideInvariant(t *testing.T) {
	for _, scenario := range []string{"role", "credential"} {
		t.Run(scenario, func(t *testing.T) {
			state := testState(t)
			deps := fixtureTeamDependencies(state)
			s, err := NewServer(state, func() {}, deps)
			require.NoError(t, err)
			handle := teamName("guide", state.Record.ID)
			role := "guide"
			if scenario == "role" {
				role = "builder"
			}
			_, err = registerTestAgent(s, handle, role, s.state.Record.Directory)
			require.NoError(t, err)
			if scenario == "credential" {
				require.NoError(t, os.Remove(state.AgentFile(handle)))
			}
			_, err = s.EnsureTeam(teamContext(state))
			require.Error(t, err)
			_, err = os.Stat(deps.RosterDirectory)
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

// TestEnsureTeamPreservesTraceParent verifies accepted work remains a child of the request.
func TestEnsureTeamPreservesTraceParent(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer otel.SetTracerProvider(previous)
	defer func() { assert.NoError(t, provider.Shutdown(context.Background())) }()
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	s, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	parent := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled})
	ctx, cancel := context.WithCancel(trace.ContextWithSpanContext(teamContext(state), parent))
	cancel()
	_, err = s.EnsureTeam(ctx)
	require.NoError(t, err)
	spans := recorder.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, parent.TraceID(), spans[0].SpanContext().TraceID())
	assert.Equal(t, parent.SpanID(), spans[0].Parent().SpanID())
}

// TestTwoProjectTeams keeps spaces, handles and roster paths distinct under one Herdr owner.
func TestTwoProjectTeams(t *testing.T) {
	host := &teamFixture{}
	rosters := t.TempDir()
	var results []*genproject.ProjectTeam
	for i := 0; i < 2; i++ {
		state := testState(t)
		deps := fixtureTeamDependencies(state)
		deps.Host = host
		deps.RosterDirectory = rosters
		s, err := NewServer(state, func() {}, deps)
		require.NoError(t, err)
		result, err := s.EnsureTeam(teamContext(state))
		require.NoError(t, err)
		results = append(results, result)
		_, err = os.Stat(filepath.Join(rosters, result.Space+".json"))
		require.NoError(t, err)
	}
	assert.NotEqual(t, results[0].Team, results[1].Team)
	assert.NotEqual(t, results[0].Space, results[1].Space)
	assert.NotEqual(t, results[0].Roles[0].Handle, results[1].Roles[0].Handle)
}

// TestMissingForegroundPreservesLiveGuide mirrors older Herdr's absent/null foreground replies.
func TestMissingForegroundPreservesLiveGuide(t *testing.T) {
	for _, foreground := range []string{"", `,"foreground":null`, `,"foreground":{}`, `,"foreground":{"argv":[]}`, `,"foreground":{"argv":[""]}`} {
		t.Run(foreground, func(t *testing.T) {
			state := testState(t)
			deps := fixtureTeamDependencies(state)
			host := deps.Host.(*teamFixture)
			s, err := NewServer(state, func() {}, deps)
			require.NoError(t, err)
			team, err := s.EnsureTeam(teamContext(state))
			require.NoError(t, err)
			raw := fmt.Sprintf(`{"state":"live","labels":{"chorus.handle":%q,"chorus.project":%q,"herdr.space":%q,"herdr.tab":"tab"}%s}`, team.Roles[0].Handle, state.Record.Directory, team.Space, foreground)
			var terminal TeamTerminal
			require.NoError(t, json.Unmarshal([]byte(raw), &terminal))
			assert.True(t, roleRunning(terminal, deps.Shell))
			host.terminals = []TeamTerminal{terminal}
			host.calls = nil
			result, err := s.EnsureTeam(teamContext(state))
			require.NoError(t, err)
			assert.True(t, result.Roles[0].Running)
			assert.Equal(t, "not_needed", result.Dialog)
			assert.NotContains(t, host.calls, "focus")
			assert.NotContains(t, host.calls, "open")
		})
	}
}

// testAgent exposes credential paths to package tests without a management API.
type testAgent struct {
	ActorID        string
	CredentialFile string
}

// registerTestAgent exercises the owning private admission with explicit saved folders.
func registerTestAgent(server *Server, handle, role, folder string) (*testAgent, error) {
	actual, err := server.registerAgent(handle, agentRecord{Role: role, Folder: folder})
	if err != nil {
		return nil, err
	}
	return &testAgent{ActorID: actual, CredentialFile: server.state.AgentFile(actual)}, nil
}

// TestTeamResultNextStep covers each real owner outcome and the user-facing conversion.
func TestTeamResultNextStep(t *testing.T) {
	for _, tc := range []struct{ dialog, step string }{
		{"opened", "Choose an agent for each role in the open Chorus dialog in Herdr."},
		{"already_open", `Click "Reload team" in the open Chorus dialog, then choose the agents.`},
		{"not_needed", "Every listed role is running."},
	} {
		t.Run(tc.dialog, func(t *testing.T) {
			state := testState(t)
			deps := fixtureTeamDependencies(state)
			host := deps.Host.(*teamFixture)
			server, err := NewServer(state, func() {}, deps)
			require.NoError(t, err)
			first, err := server.EnsureTeam(teamContext(state))
			require.NoError(t, err)
			switch tc.dialog {
			case "already_open":
				host.overlays = []string{"chorus-launcher"}
			case "not_needed":
				host.terminals = []TeamTerminal{{State: "live", Labels: map[string]string{
					"chorus.project": state.Record.Directory,
					"herdr.space":    first.Space,
					"herdr.tab":      "tab",
					"chorus.handle":  first.Roles[0].Handle,
				}}}
			}
			team, err := server.EnsureTeam(teamContext(state))
			require.NoError(t, err)
			assert.Equal(t, tc.dialog, team.Dialog)
			assert.Equal(t, tc.step, team.Step)
			userTeam, err := server.StartAgents(t.Context())
			require.NoError(t, err)
			assert.Equal(t, team.Step, userTeam.Step, "the user operation must carry the owner's step unchanged")
		})
	}
}
