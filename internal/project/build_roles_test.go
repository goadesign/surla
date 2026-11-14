// This file proves build admission persists roles independently of window readiness.
package project

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildRegistrationPersistsFoldersAndCredentials exercises admission, restart and real roster publication.
func TestBuildRegistrationPersistsFoldersAndCredentials(t *testing.T) {
	for _, storedEvaluator := range []bool{false, true} {
		t.Run(map[bool]string{false: "builder and test writer", true: "stored evaluator"}[storedEvaluator], func(t *testing.T) {
			state := testState(t)
			deps := fixtureTeamDependencies(state)
			server, err := NewServer(state, func() {}, deps)
			require.NoError(t, err)
			builder := filepath.Join(state.Record.Directory, "service")
			require.NoError(t, os.Mkdir(builder, 0o700))
			tests := t.TempDir()
			_, err = registerTestAgent(server, "custom-guide", "guide", state.Record.Directory)
			require.NoError(t, err)
			_, err = registerTestAgent(server, "custom-builder", "builder", state.Record.Directory)
			require.NoError(t, err)
			if !storedEvaluator {
				_, err = registerTestAgent(server, "custom-tests", "test_writer", state.Record.Directory)
				require.NoError(t, err)
			}
			credentials := map[string][]byte{}
			for handle := range state.Record.Agents {
				credentials[handle], err = os.ReadFile(state.AgentFile(handle))
				require.NoError(t, err)
			}
			var folders controlplane.BuildFolders = controlplane.BuilderOnly{BuilderFolder: builder}
			if !storedEvaluator {
				folders = controlplane.BuilderAndTestWriter{BuilderFolder: builder, TestWriterFolder: tests}
			}
			for attempt := 0; attempt < 2; attempt++ {
				err := server.RegisterBuildRoles(t.Context(), folders)
				require.NoError(t, err)
				assert.Len(t, state.Record.Agents, len(credentials))
			}
			assert.Equal(t, agentRecord{Role: "guide", Folder: state.Record.Directory}, state.Record.Agents["custom-guide"])
			assert.Equal(t, agentRecord{Role: "builder", Folder: builder}, state.Record.Agents["custom-builder"])
			if !storedEvaluator {
				assert.Equal(t, agentRecord{Role: "test_writer", Folder: tests}, state.Record.Agents["custom-tests"])
			}
			for handle, original := range credentials {
				raw, err := os.ReadFile(state.AgentFile(handle))
				require.NoError(t, err)
				assert.Equal(t, original, raw)
				request := httptest.NewRequest(http.MethodGet, state.Origin()+"/mcp", nil)
				request.Header.Set("Authorization", "Bearer "+string(raw))
				principal, err := server.Authenticate(request)
				require.NoError(t, err)
				assert.Equal(t, controlplane.CallerKind(state.Record.Agents[handle].Role), principal.Kind)
			}
			loaded, err := Discover(filepath.Dir(state.Dir), builder)
			require.NoError(t, err)
			assert.Equal(t, state.Record, loaded.Record)
			fresh, err := NewServer(loaded, func() {}, deps)
			require.NoError(t, err)
			team, err := fresh.EnsureTeam(teamContext(loaded))
			require.NoError(t, err)
			var roster launcherRoster
			require.NoError(t, readJSON(filepath.Join(deps.RosterDirectory, team.Space+".json"), &roster))
			require.Len(t, roster.Roles, len(credentials))
			assert.Equal(t, builder, roster.Roles[1].Folder)
			assert.Equal(t, &launcherPlace{Beside: "custom-guide", Direction: "right"}, roster.Roles[1].Place)
			if !storedEvaluator {
				assert.Equal(t, tests, roster.Roles[2].Folder)
				assert.Equal(t, &launcherPlace{Beside: "custom-builder", Direction: "down"}, roster.Roles[2].Place)
			}
			assert.Equal(t, 1, deps.Host.(*teamFixture).creates)
		})
	}
}

// TestBuildRegistrationIgnoresWindowState proves the documented start-agents recovery step.
func TestBuildRegistrationIgnoresWindowState(t *testing.T) {
	for _, outcome := range []string{"busy", "unavailable", "ambiguous"} {
		t.Run(outcome, func(t *testing.T) {
			state := testState(t)
			deps := fixtureTeamDependencies(state)
			host := deps.Host.(*teamFixture)
			if outcome == "busy" {
				host.overlays = []string{"palette"}
			} else if outcome == "unavailable" {
				deps.Host = unavailableAdmissionHost{TeamHost: host}
			} else {
				name := teamName(filepath.Base(state.Record.Directory), state.Record.ID)
				host.spaces = []TeamSpace{{ID: "one", Name: name, Folder: state.Record.Directory}, {ID: "two", Name: name, Folder: state.Record.Directory}}
			}
			server, err := NewServer(state, func() {}, deps)
			require.NoError(t, err)
			folders := controlplane.BuilderAndTestWriter{BuilderFolder: state.Record.Directory, TestWriterFolder: t.TempDir()}
			for attempt := 0; attempt < 2; attempt++ {
				require.NoError(t, server.RegisterBuildRoles(t.Context(), folders))
			}
			loaded, err := Discover(filepath.Dir(state.Dir), state.Record.Directory)
			require.NoError(t, err)
			require.Len(t, loaded.Record.Agents, 2)
			assert.Equal(t, folders.BuilderFolder, loaded.Record.Agents[teamName("builder", state.Record.ID)].Folder)
			assert.Equal(t, folders.TestWriterFolder, loaded.Record.Agents[teamName("test_writer", state.Record.ID)].Folder)
			assert.Empty(t, host.calls, "registration must never inspect or change the window")
			assert.Zero(t, host.creates)
			server.team.dependencies.Host = host
			host.overlays, host.spaces = nil, nil
			team, err := server.StartAgents(t.Context())
			require.NoError(t, err)
			assert.Len(t, team.Roles, 3)
			assert.Equal(t, "opened", team.Dialog)
			assert.Equal(t, 1, host.creates)
			assert.Equal(t, []string{"overlays", "spaces", "create", "terminals", "focus", "open"}, host.calls)
		})
	}
}

// unavailableAdmissionHost refuses observation without touching the desktop.
type unavailableAdmissionHost struct{ TeamHost }

// Overlays models a stopped or unavailable Herdr host.
func (unavailableAdmissionHost) Overlays(context.Context) ([]string, error) {
	return nil, errors.New("host stopped")
}

// TestBuildRegistrationRejectsInvalidConstructedFolders checks all inputs before registry or host effects.
func TestBuildRegistrationRejectsInvalidConstructedFolders(t *testing.T) {
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	server, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	for _, folders := range []controlplane.BuildFolders{
		nil,
		controlplane.BuilderOnly{},
		controlplane.BuilderOnly{BuilderFolder: "relative"},
		controlplane.BuilderAndTestWriter{BuilderFolder: state.Record.Directory, TestWriterFolder: "relative"},
	} {
		err := server.RegisterBuildRoles(t.Context(), folders)
		assert.Error(t, err)
		assert.Empty(t, state.Record.Agents)
		assert.Empty(t, deps.Host.(*teamFixture).calls)
	}
}

// TestBuildRegistrationCompletesAfterPhaseCancellation keeps a saved build recoverable.
func TestBuildRegistrationCompletesAfterPhaseCancellation(t *testing.T) {
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	server, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.NoError(t, server.RegisterBuildRoles(ctx, controlplane.BuilderAndTestWriter{BuilderFolder: state.Record.Directory, TestWriterFolder: t.TempDir()}))
	assert.Len(t, state.Record.Agents, 2)
	assert.Empty(t, deps.Host.(*teamFixture).calls)
	team, err := server.StartAgents(t.Context())
	require.NoError(t, err)
	assert.Len(t, team.Roles, 3)
}

// TestBuilderOnlyRevokesTestWriter keeps the exact roles selected by stored verification settings.
func TestBuilderOnlyRevokesTestWriter(t *testing.T) {
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	server, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	require.NoError(t, server.RegisterBuildRoles(t.Context(), controlplane.BuilderAndTestWriter{BuilderFolder: state.Record.Directory, TestWriterFolder: t.TempDir()}))
	_, err = server.StartAgents(t.Context())
	require.NoError(t, err)
	writer := teamName("test_writer", state.Record.ID)
	credential, err := os.ReadFile(state.AgentFile(writer))
	require.NoError(t, err)
	preserved := map[string][]byte{}
	for handle, agent := range state.Record.Agents {
		if agent.Role != "test_writer" {
			preserved[handle], err = os.ReadFile(state.AgentFile(handle))
			require.NoError(t, err)
		}
	}
	require.NoError(t, server.RegisterBuildRoles(t.Context(), controlplane.BuilderOnly{BuilderFolder: state.Record.Directory}))
	_, err = os.Stat(state.AgentFile(writer))
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.NotContains(t, state.Record.Agents, writer)
	request := httptest.NewRequest(http.MethodGet, state.Origin()+"/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+string(credential))
	_, err = server.Authenticate(request)
	assert.Error(t, err)
	for handle, original := range preserved {
		raw, err := os.ReadFile(state.AgentFile(handle))
		require.NoError(t, err)
		assert.Equal(t, original, raw)
	}
	loaded, err := Discover(filepath.Dir(state.Dir), state.Record.Directory)
	require.NoError(t, err)
	assert.Equal(t, state.Record, loaded.Record)
	team, err := server.StartAgents(t.Context())
	require.NoError(t, err)
	require.Len(t, team.Roles, 2)
	assert.Equal(t, "guide", team.Roles[0].Role)
	assert.Equal(t, "builder", team.Roles[1].Role)
	var roster launcherRoster
	require.NoError(t, readJSON(filepath.Join(deps.RosterDirectory, team.Space+".json"), &roster))
	assert.Len(t, roster.Roles, 2)
	assert.Equal(t, "guide", roster.Roles[0].Role)
	assert.Equal(t, "builder", roster.Roles[1].Role)
	// Returning to Surla's tests admits a fresh credential; revocation stays final.
	require.NoError(t, server.RegisterBuildRoles(t.Context(), controlplane.BuilderAndTestWriter{BuilderFolder: state.Record.Directory, TestWriterFolder: t.TempDir()}))
	freshCredential, err := os.ReadFile(state.AgentFile(writer))
	require.NoError(t, err)
	assert.NotEqual(t, credential, freshCredential)
	_, err = server.Authenticate(request)
	assert.Error(t, err)
	request.Header.Set("Authorization", "Bearer "+string(freshCredential))
	principal, err := server.Authenticate(request)
	require.NoError(t, err)
	assert.Equal(t, controlplane.CallerTestWriter, principal.Kind)

}

// TestTeamOperationsSerialize shares one registry, roster and window transition among every entry.
func TestTeamOperationsSerialize(t *testing.T) {
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	server, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	var workers sync.WaitGroup
	for range 6 {
		workers.Go(func() {
			err := server.RegisterBuildRoles(t.Context(), controlplane.BuilderOnly{BuilderFolder: state.Record.Directory})
			assert.NoError(t, err)
		})
		workers.Go(func() {
			_, err := server.EnsureTeam(teamContext(state))
			assert.NoError(t, err)
		})
		workers.Go(func() { assert.Error(t, server.ShowPane(t.Context(), "guide")) })
	}
	workers.Wait()
	assert.Equal(t, 1, deps.Host.(*teamFixture).creates)
	assert.Len(t, state.Record.Agents, 2)
}

// TestStrictAgentRecord rejects the former string shape and invalid structured records.
func TestStrictAgentRecord(t *testing.T) {
	for _, agent := range []string{`"builder"`, `{"role":"builder"}`, `{"role":"manager","folder":"/work"}`, `{"role":"builder","folder":"relative"}`, `{"role":"builder","folder":"/work\u0000bad"}`, `{"role":"builder","folder":"/work","first_message":"stale"}`} {
		t.Run(agent, func(t *testing.T) {
			state := testState(t)
			raw, err := json.Marshal(state.Record)
			require.NoError(t, err)
			var object map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &object))
			object["agents"] = json.RawMessage(`{"builder":` + agent + `}`)
			raw, err = json.Marshal(object)
			require.NoError(t, err)
			require.NoError(t, writePrivate(filepath.Join(state.Dir, "project.json"), raw))
			_, err = Discover(filepath.Dir(state.Dir), state.Record.Directory)
			assert.Error(t, err)
		})
	}
}

// TestFirstMessagesNameOnlyAllowedOperations checks each named operation against the invocation policy.
// File paths such as eval/design/story_tests.go name files, not operations.
func TestFirstMessagesNameOnlyAllowedOperations(t *testing.T) {
	names := regexp.MustCompile(`\b[a-z]+(?:_[a-z]+)+\b`)
	files := regexp.MustCompile(`\S+\.go\b`)
	for _, role := range []string{"guide", "builder", "test_writer"} {
		_, message := roleFirstMessage(role)
		assert.True(t, controlplane.OperationAllowed(controlplane.CallerKind(role), "golden_path"))
		for _, operation := range names.FindAllString(files.ReplaceAllString(message, ""), -1) {
			assert.Contains(t, gensurla.MethodNames, operation)
			assert.True(t, controlplane.OperationAllowed(controlplane.CallerKind(role), operation), "%s may not call %s", role, operation)
		}
	}
}

// TestProjectRecordRejectsTrailingData preserves the single strict stored-document contract.
func TestProjectRecordRejectsTrailingData(t *testing.T) {
	state := testState(t)
	raw, err := json.Marshal(state.Record)
	require.NoError(t, err)
	raw = append(raw, []byte("\n{}")...)
	require.NoError(t, writePrivate(filepath.Join(state.Dir, "project.json"), raw))
	_, err = Discover(filepath.Dir(state.Dir), state.Record.Directory)
	assert.ErrorContains(t, err, "trailing project state")
}

// TestTheTestWriterMayRegenerateItsTestModule checks the test writer's first
// message: Surla writes the Story table and the test writer leaves it as it
// is, reporting or regenerating after a design change, because regenerating
// keeps the required tests. No role is told that regenerating drops a test.
func TestTheTestWriterMayRegenerateItsTestModule(t *testing.T) {
	_, message := roleFirstMessage("test_writer")
	for _, sentence := range []string{
		"Surla writes eval/design/story_tests.go, which declares the required tests, from the agreed Story",
		"leave that file as it is",
		"report to have it generated again or, when your shell is not sandboxed, run surla gen -dir eval in the working folder first",
		"regenerating keeps the required tests",
	} {
		assert.Contains(t, message, sentence)
	}
	for _, role := range []string{"guide", "builder", "test_writer"} {
		_, message := roleFirstMessage(role)
		assert.NotContains(t, message, "drop", role)
		assert.NotContains(t, message, "never regenerate", role)
	}
}
