// This file proves the actual team operation preserves every registered trusted role.
package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEnsureTeamPreservesRegisteredRoles exercises real roster publication across repeated starts.
func TestEnsureTeamPreservesRegisteredRoles(t *testing.T) {
	for _, missingFolder := range []bool{false, true} {
		t.Run(map[bool]string{false: "all folders", true: "test writer absent"}[missingFolder], func(t *testing.T) {
			state := testState(t)
			deps := fixtureTeamDependencies(state)
			host := deps.Host.(*teamFixture)
			testFolder := filepath.Join(t.TempDir(), "nested-project")
			if !missingFolder {
				require.NoError(t, os.Mkdir(testFolder, 0o700))
			}
			server, err := NewServer(state, func() {}, deps)
			require.NoError(t, err)
			handles := []string{"existing-guide", "existing-builder", "existing-test-writer"}
			for i, role := range []string{"guide", "builder", "test_writer"} {
				folder := state.Record.Directory
				if role == "test_writer" {
					folder = testFolder
				}
				_, err := registerTestAgent(server, handles[i], role, folder)
				require.NoError(t, err)
			}
			original := map[string]agentRecord{}
			for actor, role := range state.Record.Agents {
				original[actor] = role
			}
			// Guide is already running. With a real test folder, builder is missing; with
			// no test folder both listed roles run and the omitted role must not open a dialog.
			host.terminals = []TeamTerminal{{State: "live", Labels: map[string]string{"chorus.project": state.Record.Directory, "herdr.space": "space-1", "herdr.tab": "tab", "chorus.handle": handles[0]}, Foreground: &TeamForeground{Argv: []string{"codex"}}}}
			if missingFolder {
				host.terminals = append(host.terminals, TeamTerminal{State: "live", Labels: map[string]string{"chorus.project": state.Record.Directory, "herdr.space": "space-1", "herdr.tab": "tab", "chorus.handle": handles[1]}, Foreground: &TeamForeground{Argv: []string{"claude"}}}, TeamTerminal{State: "live", Labels: map[string]string{"chorus.project": state.Record.Directory, "herdr.space": "space-1", "herdr.tab": "tab", "chorus.handle": handles[2]}, Foreground: &TeamForeground{Argv: []string{"codex"}}})
			}
			var first []byte
			for attempt := 0; attempt < 2; attempt++ {
				host.calls = nil
				result, err := server.EnsureTeam(teamContext(state))
				require.NoError(t, err)
				require.Len(t, result.Roles, 3)
				assert.Equal(t, original, state.Record.Agents, "existing actor IDs and authorization roles are immutable")
				for i, role := range []string{"guide", "builder", "test_writer"} {
					assert.Equal(t, role, result.Roles[i].Role)
					assert.Equal(t, handles[i], result.Roles[i].Handle)
				}
				assert.True(t, result.Roles[0].Running)
				assert.Equal(t, missingFolder, result.Roles[1].Running)
				assert.Equal(t, missingFolder, result.Roles[2].Running, "running is independent of roster membership")
				if missingFolder {
					assert.Equal(t, "not_needed", result.Dialog)
					assert.NotContains(t, host.calls, "focus")
					assert.NotContains(t, host.calls, "open")
					_, err = os.Stat(testFolder)
					assert.ErrorIs(t, err, os.ErrNotExist)
				} else {
					assert.Equal(t, "opened", result.Dialog)
					assert.Contains(t, host.calls, "focus")
					assert.Contains(t, host.calls, "open")
				}
				raw, err := os.ReadFile(filepath.Join(deps.RosterDirectory, result.Space+".json"))
				require.NoError(t, err)
				var written launcherRoster
				require.NoError(t, json.Unmarshal(raw, &written))
				count := 3
				if missingFolder {
					count = 2
				}
				require.Len(t, written.Roles, count)
				assert.Equal(t, "guide", written.Roles[0].Role)
				assert.Equal(t, handles[0], written.Roles[0].Handle)
				assert.Nil(t, written.Roles[0].Place)
				assert.Equal(t, "builder", written.Roles[1].Role)
				assert.Equal(t, handles[1], written.Roles[1].Handle)
				assert.Equal(t, &launcherPlace{Beside: handles[0], Direction: "right"}, written.Roles[1].Place)
				if !missingFolder {
					assert.Equal(t, "test_writer", written.Roles[2].Role)
					assert.Equal(t, handles[2], written.Roles[2].Handle)
					assert.Equal(t, testFolder, written.Roles[2].Folder)
					assert.Equal(t, &launcherPlace{Beside: handles[1], Direction: "down"}, written.Roles[2].Place)
				}
				if attempt == 0 {
					first = raw
				} else {
					assert.JSONEq(t, string(first), string(raw), "repeating team start must never drop registered rows")
				}
			}
		})
	}
}

// TestAgentRoleAdmissionIsIdempotent preserves one stable handle and credential per role.
func TestAgentRoleAdmissionIsIdempotent(t *testing.T) {
	for _, role := range []string{"guide", "builder", "test_writer"} {
		t.Run(role, func(t *testing.T) {
			state := testState(t)
			server, err := NewServer(state, func() {}, fixtureTeamDependencies(state))
			require.NoError(t, err)
			first, err := registerTestAgent(server, role+"-first", role, state.Record.Directory)
			require.NoError(t, err)
			before, err := os.ReadFile(first.CredentialFile)
			require.NoError(t, err)
			second, err := registerTestAgent(server, role+"-second", role, state.Record.Directory)
			require.NoError(t, err)
			assert.Equal(t, first, second)
			after, err := os.ReadFile(first.CredentialFile)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			require.Len(t, state.Record.Agents, 1)
		})
	}
}

// TestStoredDuplicateRoleIsRejected validates the same invariant when a project record is loaded.
func TestStoredDuplicateRoleIsRejected(t *testing.T) {
	state := testState(t)
	state.Record.Agents = map[string]agentRecord{"first": {Role: "guide", Folder: state.Record.Directory}, "second": {Role: "guide", Folder: state.Record.Directory}}
	require.NoError(t, state.save())
	_, err := Discover(filepath.Dir(state.Dir), state.Record.Directory)
	require.ErrorContains(t, err, `invalid stored role "guide"`)
}

// TestExistingCustomGuideIsReused keeps its stable actor ID and persistent credential.
func TestExistingCustomGuideIsReused(t *testing.T) {
	state := testState(t)
	server, err := NewServer(state, func() {}, fixtureTeamDependencies(state))
	require.NoError(t, err)
	actor, err := registerTestAgent(server, "long-lived-guide", "guide", server.state.Record.Directory)
	require.NoError(t, err)
	before, err := os.ReadFile(actor.CredentialFile)
	require.NoError(t, err)
	team, err := server.EnsureTeam(teamContext(state))
	require.NoError(t, err)
	require.Len(t, team.Roles, 1)
	assert.Equal(t, actor.ActorID, team.Roles[0].Handle)
	require.Len(t, state.Record.Agents, 1)
	after, err := os.ReadFile(actor.CredentialFile)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	_, err = os.Stat(state.AgentFile(teamName("guide", state.Record.ID)))
	assert.ErrorIs(t, err, os.ErrNotExist)
}
