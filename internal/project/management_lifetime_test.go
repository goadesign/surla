package project

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	genproject "github.com/goadesign/surla/gen/project"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestManagementLifetime keeps durable agent identity independent of server management authority.
func TestManagementLifetime(t *testing.T) {
	state := testState(t)
	_, err := os.Stat(filepath.Join(state.Dir, "management"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	first, err := NewServer(state, func() {})
	require.NoError(t, err)
	require.NoError(t, first.PublishRunning(os.Getpid(), "test"))
	original, err := state.ReadRunning()
	require.NoError(t, err)
	principal := controlplane.WithPrincipal(t.Context(), controlplane.Principal{ID: state.Record.Owner, CallerID: "management", Kind: controlplane.CallerManagement})
	agent, err := first.AddAgent(principal, &genproject.AddAgentPayload{ActorID: "guide", Role: "guide"})
	require.NoError(t, err)
	agentCredential, err := os.ReadFile(agent.CredentialFile)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodGet, state.Origin()+"/project/identity", nil)
	request.Header.Set("Authorization", "Bearer "+original.ManagementCredential)
	_, err = first.Authenticate(request)
	require.NoError(t, err)

	// Replacing the private record cannot grant or revoke authority in a live server.
	replacement := original
	replacement.ManagementCredential = strings.Repeat("f", 64)
	require.NoError(t, state.SaveRunning(replacement))
	_, err = first.Authenticate(request)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+replacement.ManagementCredential)
	_, err = first.Authenticate(request)
	assert.Error(t, err)
	require.NoError(t, first.RevokeManagement())
	request.Header.Set("Authorization", "Bearer "+original.ManagementCredential)
	_, err = first.Authenticate(request)
	assert.Error(t, err)
	assert.ErrorContains(t, first.PublishRunning(os.Getpid(), "test"), "has ended")
	require.NoError(t, state.SaveRunning(original))
	_, err = first.Authenticate(request)
	assert.Error(t, err, "disk replacement cannot undo revocation")

	second, err := NewServer(state, func() {})
	require.NoError(t, err)
	require.NoError(t, second.PublishRunning(os.Getpid(), "test"))
	current, err := state.ReadRunning()
	require.NoError(t, err)
	assert.False(t, credentialEqual(original.ManagementCredential, current.ManagementCredential), "restart must rotate management authority")
	_, err = second.Authenticate(request)
	assert.Error(t, err)
	request.Header.Set("Authorization", "Bearer "+current.ManagementCredential)
	_, err = second.Authenticate(request)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+string(agentCredential))
	actor, err := second.Authenticate(request)
	require.NoError(t, err)
	assert.Equal(t, "guide", actor.CallerID)
	assert.Equal(t, controlplane.CallerGuide, actor.Kind)
	assert.Equal(t, state.Record.Owner, actor.ID)
	require.NoError(t, second.RevokeManagement())
}

// TestRunningCredentialValidation proves only complete private current-format records are consumed.
func TestRunningCredentialValidation(t *testing.T) {
	for _, value := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("z", 64)} {
		t.Run(fmt.Sprintf("invalid-format-%d", len(value)), func(t *testing.T) {
			state := testState(t)
			require.NoError(t, state.SaveRunning(Running{ManagementCredential: value}))
			_, err := state.ReadRunning()
			assert.ErrorContains(t, err, "invalid running management credential")
		})
	}
	state := testState(t)
	require.NoError(t, state.SaveRunning(Running{ManagementCredential: strings.Repeat("a", 64)}))
	path := filepath.Join(state.Dir, "running.json")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.NoError(t, os.Chmod(path, 0o644))
	_, err = state.ReadRunning()
	assert.ErrorContains(t, err, "must be private")
	require.NoError(t, os.Remove(path))
	target := filepath.Join(t.TempDir(), "record")
	require.NoError(t, os.WriteFile(target, []byte(`{"management_credential":"`+strings.Repeat("a", 64)+`"}`), 0o600))
	require.NoError(t, os.Symlink(target, path))
	_, err = state.ReadRunning()
	assert.Error(t, err, "a credential record must not follow a link")
}
