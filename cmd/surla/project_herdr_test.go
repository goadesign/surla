//go:build herdr

// This file proves the project server's real parentless Herdr lifetime without starting an agent or model.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	genproject "github.com/goadesign/surla/gen/project"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// init enables production launch effects only in the explicitly tagged test binary.
// TestMain still requires the isolated configuration root before starting its helper.
func init() {
	herdrTestRuntime = productionProjectRuntime
}

// TestHC isolates the Herdr core, starts only the real server, and cleans up exact terminals.
func TestHC(t *testing.T) {
	if testing.Short() {
		t.Skip("real isolated Herdr server lifecycle")
	}
	hc, err := exec.LookPath("hc")
	if err != nil {
		home, homeErr := os.UserHomeDir()
		require.NoError(t, homeErr)
		hc = filepath.Join(home, ".local", "bin", "hc")
		if _, err := os.Stat(hc); err != nil {
			t.Skip("Herdr hc executable unavailable")
		}
	}
	scratch := t.TempDir()
	session := fmt.Sprintf("surla-lifecycle-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Setenv("HC_SESSION", session)
	t.Setenv("HC_SOCKET", filepath.Join(scratch, "s"))
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		dir := filepath.Join(scratch, key)
		require.NoError(t, os.Mkdir(dir, 0o700))
		t.Setenv(key, dir)
	}
	t.Setenv("HC_TERMINAL_ID", "")
	bin := filepath.Join(scratch, "bin")
	require.NoError(t, os.Mkdir(bin, 0o700))
	for _, name := range []string{"open", "xdg-open"} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o700))
	}
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	fakeOpen := filepath.Join(bin, opener)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	config := filepath.Join(scratch, "surla-config")
	require.NoError(t, os.Mkdir(config, 0o700))
	t.Setenv("SURLA_TEST_USER_CONFIG_DIR", config)
	prior := userConfigDir
	userConfigDir = func() (string, error) { return config, nil }
	t.Cleanup(func() { userConfigDir = prior })
	workspace := filepath.Join(scratch, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	t.Chdir(workspace)
	var terminals []string
	var current *project.State
	callerTerminal := ""
	coreStarted := false
	// Cleanup remains confined to this unique session and the terminal IDs recorded below.
	t.Cleanup(func() {
		if !coreStarted {
			return
		}
		if current != nil {
			client, err := projectClient(current)
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				stopErr := client.Stop(ctx)
				cancel()
				if stopErr != nil {
					held, probeErr := controlplane.CardStoreInUse(current.CardsDir())
					if probeErr == nil && held {
						t.Log("server stop needed terminal cleanup")
					}
				}
			}
		}
		if callerTerminal != "" {
			_, killErr := isolatedHerdr(hc, "kill", "--json", callerTerminal)
			if killErr != nil {
				t.Log("caller terminal already exited")
			}
			_, removeErr := isolatedHerdr(hc, "rm", "--json", callerTerminal)
			assert.NoError(t, removeErr)
		}
		for _, id := range terminals {
			_, killErr := isolatedHerdr(hc, "kill", "--json", id)
			if killErr != nil {
				raw, getErr := isolatedHerdr(hc, "get", "--json", id)
				assert.NoError(t, getErr)
				assert.Contains(t, string(raw), `"finished"`)
			}
			_, removeErr := isolatedHerdr(hc, "rm", "--json", id)
			assert.NoError(t, removeErr)
		}
		raw, listErr := isolatedHerdr(hc, "ls", "--json", "--all")
		assert.NoError(t, listErr)
		assert.NotContains(t, string(raw), `"live"`)
		_, stopErr := isolatedHerdr(hc, "server", "stop", "--json")
		assert.NoError(t, stopErr)
	})
	raw, err := isolatedHerdr(hc, "server", "start", "--json")
	require.NoError(t, err, string(raw))
	coreStarted = true
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// Verify the isolated core resolves the harmless browser stand-in before invoking the full CLI.
	resolved, err := isolatedHerdr(hc, "exec", "--", "/usr/bin/which", opener)
	require.NoError(t, err)
	require.Equal(t, fakeOpen, strings.TrimSpace(string(resolved)))
	self, err := os.Executable()
	require.NoError(t, err)
	launched, err := isolatedHerdr(hc, "run", "--json", "--no-parent", "--no-notify", "--bg", "--cwd", workspace, "--", self, "test-project-start")
	require.NoError(t, err)
	var caller struct {
		Address string `json:"address"`
	}
	require.NoError(t, json.Unmarshal(launched, &caller))
	require.NotEmpty(t, caller.Address)
	callerTerminal = caller.Address
	waited, err := isolatedHerdr(hc, "wait", "--json", "--timeout", "10", callerTerminal)
	require.NoError(t, err)
	var outcome struct {
		Ended    bool `json:"ended"`
		ExitCode *int `json:"exit_code"`
	}
	require.NoError(t, json.Unmarshal(waited, &outcome))
	require.True(t, outcome.Ended)
	require.NotNil(t, outcome.ExitCode)
	require.Zero(t, *outcome.ExitCode)
	_, err = isolatedHerdr(hc, "rm", "--json", callerTerminal)
	require.NoError(t, err)
	callerTerminal = ""
	state, err := discoverProject()
	require.NoError(t, err)
	client, err := projectClient(state)
	require.NoError(t, err)
	launch := func(ctx context.Context, state *project.State) (string, error) {
		return startProjectTerminal(ctx, state, nil)
	}

	current = state
	running, err := state.ReadRunning()
	require.NoError(t, err)
	require.NotEmpty(t, running.Terminal)
	terminals = append(terminals, running.Terminal)
	metadata, err := isolatedHerdr(hc, "get", "--json", running.Terminal)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(metadata, &fields))
	require.Contains(t, fields, "parent")
	var terminal struct {
		Parent *string `json:"parent"`
	}
	require.NoError(t, json.Unmarshal(metadata, &terminal))
	assert.Nil(t, terminal.Parent)
	management, err := state.ManagementCredential()
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(metadata), management), "management credential must not appear in terminal metadata")
	actor, err := client.AddAgent(ctx, &genproject.AddAgentPayload{ActorID: "guide", Role: "guide"})
	require.NoError(t, err)
	credential, err := os.ReadFile(actor.CredentialFile)
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(metadata), string(credential)), "agent credential must not appear in terminal metadata")
	// Inspect current metadata and server output after agent issuance, never printing credentials.
	issuedMetadata, err := isolatedHerdr(hc, "get", "--json", running.Terminal)
	require.NoError(t, err)
	serverOutput, err := isolatedHerdr(hc, "read", "--json", "--lines", "100", running.Terminal)
	require.NoError(t, err)
	for _, data := range [][]byte{issuedMetadata, serverOutput} {
		assert.False(t, strings.Contains(string(data), management), "management credential must not appear in terminal metadata or output")
		assert.False(t, strings.Contains(string(data), string(credential)), "agent credential must not appear in terminal metadata or output")
	}
	assertAgentGuidance(t, state, string(credential))
	originalPort := state.Record.Port
	// The launching CLI call has returned and no parent owns the server terminal.
	require.NoError(t, client.Stop(ctx))
	waitForStoreRelease(t, state)
	second, err := discoverProject()
	require.NoError(t, err)
	client, _, err = attachOrStartProject(ctx, second, launch)
	require.NoError(t, err)
	current = second
	restarted, err := second.ReadRunning()
	require.NoError(t, err)
	terminals = append(terminals, restarted.Terminal)
	assert.Equal(t, originalPort, second.Record.Port)
	assertAgentGuidance(t, second, string(credential))
	// Handled HUP attempts the same ordered close as authenticated stop.
	require.NoError(t, syscall.Kill(restarted.PID, syscall.SIGHUP))
	waitForStoreRelease(t, second)
	_, err = second.ReadRunning()
	assert.ErrorIs(t, err, os.ErrNotExist)
	client, _, err = attachOrStartProject(ctx, second, launch)
	require.NoError(t, err)
	restarted, err = second.ReadRunning()
	require.NoError(t, err)
	terminals = append(terminals, restarted.Terminal)
	assertAgentGuidance(t, second, string(credential))
	// An outright kill is recovery through a free store lock and stale record, never called graceful.
	require.NotEqual(t, os.Getpid(), restarted.PID)
	require.NoError(t, syscall.Kill(restarted.PID, syscall.SIGKILL))
	waitForStoreRelease(t, second)
	third, err := discoverProject()
	require.NoError(t, err)
	client, _, err = attachOrStartProject(ctx, third, launch)
	require.NoError(t, err)
	current = third
	recovered, err := third.ReadRunning()
	require.NoError(t, err)
	terminals = append(terminals, recovered.Terminal)
	assert.Equal(t, originalPort, third.Record.Port)
	assertAgentGuidance(t, third, string(credential))
	require.NoError(t, client.Stop(ctx))
	waitForStoreRelease(t, third)
	_, err = third.ReadRunning()
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// isolatedHerdr bounds each command and inherits the test's unique session and scratch XDG environment.
func isolatedHerdr(hc string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, hc, args...)
	command.Stdin = nil
	var out, stderr bytes.Buffer
	command.Stdout = &out
	command.Stderr = &stderr
	err := command.Run()
	if err != nil {
		return append(out.Bytes(), stderr.Bytes()...), err
	}
	return out.Bytes(), nil
}

// waitForStoreRelease waits for the owning process's existing writer lock, not its PID record.
func waitForStoreRelease(t *testing.T, state *project.State) {
	t.Helper()
	require.Eventually(t, func() bool { held, err := controlplane.CardStoreInUse(state.CardsDir()); return err == nil && !held }, 10*time.Second, 25*time.Millisecond)
}

// assertAgentGuidance verifies an existing persistent credential through the actual HTTP consumer after each restart.
func assertAgentGuidance(t *testing.T, state *project.State, token string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, state.Origin()+"/golden-path", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: time.Second}
	response, err := client.Do(request)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, "nosniff", response.Header.Get("X-Content-Type-Options"))
	require.NoError(t, response.Body.Close())
	assert.True(t, strings.HasPrefix(state.Origin(), "http://127.0.0.1:"))
}
