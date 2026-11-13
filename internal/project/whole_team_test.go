// This file proves the actual team operation preserves every registered trusted role.
package project

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	genproject "github.com/goadesign/surla/gen/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goa "goa.design/goa/v3/pkg"
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
			reads := 0
			deps.TestWriterFolder = func(context.Context) (string, error) { reads++; return testFolder, nil }
			server, err := NewServer(state, func() {}, deps)
			require.NoError(t, err)
			handles := []string{"existing-guide", "existing-builder", "existing-test-writer"}
			for i, role := range []string{"guide", "builder", "test_writer"} {
				_, err := server.AddAgent(teamContext(state), &genproject.AddAgentPayload{ActorID: handles[i], Role: role})
				require.NoError(t, err)
			}
			original := map[string]string{}
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
			assert.Equal(t, 2, reads, "working-folder ownership is read once per operation without creating a checkout")
		})
	}
}

// TestAgentRolesAreUniqueAtAdmission refuses duplicate purpose before a credential or record changes.
func TestAgentRolesAreUniqueAtAdmission(t *testing.T) {
	for _, role := range []string{"guide", "builder", "test_writer"} {
		t.Run(role, func(t *testing.T) {
			state := testState(t)
			server, err := NewServer(state, func() {}, fixtureTeamDependencies(state))
			require.NoError(t, err)
			first := role + "-first"
			second := role + "-second"
			_, err = server.AddAgent(teamContext(state), &genproject.AddAgentPayload{ActorID: first, Role: role})
			require.NoError(t, err)
			before, err := os.ReadFile(filepath.Join(state.Dir, "project.json"))
			require.NoError(t, err)
			_, err = server.AddAgent(teamContext(state), &genproject.AddAgentPayload{ActorID: second, Role: role})
			var conflict *goa.ServiceError
			require.ErrorAs(t, err, &conflict)
			assert.Equal(t, "conflict", conflict.Name)
			after, err := os.ReadFile(filepath.Join(state.Dir, "project.json"))
			require.NoError(t, err)
			assert.Equal(t, before, after)
			_, err = os.Stat(state.AgentFile(second))
			assert.ErrorIs(t, err, os.ErrNotExist)
			require.Len(t, state.Record.Agents, 1)
		})
	}
}

// TestStoredDuplicateRoleIsRejected validates the same invariant when a project record is loaded.
func TestStoredDuplicateRoleIsRejected(t *testing.T) {
	state := testState(t)
	state.Record.Agents = map[string]string{"first": "guide", "second": "guide"}
	require.NoError(t, state.save())
	_, err := Discover(filepath.Dir(state.Dir), state.Record.Directory)
	require.ErrorContains(t, err, `invalid stored role "guide"`)
}

// TestExistingCustomGuideIsReused keeps its stable actor ID and persistent credential.
func TestExistingCustomGuideIsReused(t *testing.T) {
	state := testState(t)
	server, err := NewServer(state, func() {}, fixtureTeamDependencies(state))
	require.NoError(t, err)
	actor, err := server.AddAgent(teamContext(state), &genproject.AddAgentPayload{ActorID: "long-lived-guide", Role: "guide"})
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

// TestServerRequiresNoncreatingFolderRead rejects a missing dependency rather than inventing a path.
func TestServerRequiresNoncreatingFolderRead(t *testing.T) {
	state := testState(t)
	deps := fixtureTeamDependencies(state)
	deps.TestWriterFolder = nil
	_, err := NewServer(state, func() {}, deps)
	require.ErrorContains(t, err, "test-writer folder reader")
}
