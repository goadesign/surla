package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goahttp "goa.design/goa/v3/http"
)

// TestStartOpensOnlyProjectViewsLinks drives the generated management consumer and
// actual CLI start boundary while replacing the browser opener with a recording stand-in.
func TestStartOpensOnlyProjectViewsLinks(t *testing.T) {
	for _, scenario := range []string{"valid", "foreign origin", "other scheme", "filesystem path", "wrong page", "short token", "nonhex token", "extra fragment", "query string"} {
		t.Run(scenario, func(t *testing.T) {
			isolateServeWorkspace(t)
			state, err := fixtureProjectState("")
			require.NoError(t, err)
			cp, err := controlplane.New(controlplane.Options{CardsDir: state.CardsDir()})
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, cp.Close()) })
			token := strings.Repeat("a", 64)
			url := state.Origin() + "/views#link=" + token
			switch scenario {
			case "foreign origin":
				url = "http://foreign.invalid/views#link=" + token
			case "other scheme":
				url = "https://" + state.Address() + "/views#link=" + token
			case "filesystem path":
				url = "/private/untrusted/views#link=" + token
			case "wrong page":
				url = state.Origin() + "/another#link=" + token
			case "short token":
				url = state.Origin() + "/views#link=" + token[:63]
			case "nonhex token":
				url = state.Origin() + "/views#link=" + strings.Repeat("g", 64)
			case "extra fragment":
				url += "#another"
			case "query string":
				url = state.Origin() + "/views?other=1#link=" + token
			}
			api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/project/identity" {
					_, err := fmt.Fprintf(w, `{"project_id":%q,"directory":%q,"owner":%q}`, state.Record.ID, state.Record.Directory, state.Record.Owner)
					assert.NoError(t, err)
					return
				}
				if r.URL.Path == "/project/team" {
					_, err := io.WriteString(w, `{"team":"fixture-team","space":"fixture-space","roles":[{"role":"guide","handle":"guide-fixture","running":true}],"dialog":"not_needed"}`)
					assert.NoError(t, err)
					return
				}
				assert.Equal(t, "/project/browser-link", r.URL.Path)
				w.WriteHeader(http.StatusCreated)
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{"url": url, "expires_at": time.Now().Add(time.Minute).Format(time.RFC3339Nano)}))
			}))
			require.NoError(t, api.Listener.Close())
			api.Listener, err = net.Listen("tcp", state.Address())
			require.NoError(t, err)
			api.Start()
			t.Cleanup(api.Close)
			require.NoError(t, state.SaveRunning(project.Running{ManagementCredential: strings.Repeat("b", 64)}))
			runtime := testProjectRuntime()
			bindFixtureTeamEffects(&runtime)
			runtime.preflight = func() error { return nil }
			opened := false
			runtime.openViews = func(_ context.Context, address string) error {
				opened = true
				assert.True(t, address == url)
				return nil
			}
			err = runtime.start(io.Discard)
			if scenario == "valid" {
				assert.NoError(t, err)
				assert.True(t, opened)
			} else {
				require.ErrorContains(t, err, "invalid views link")
				assert.False(t, opened)
				assert.False(t, strings.Contains(err.Error(), url), "an untrusted URL must not be echoed")
			}
		})
	}
}

// TestUnexpectedManagementResponseDoesNotEchoCredential exercises the CLI-facing
// error through an actual response that echoes the request's Authorization header.
func TestUnexpectedManagementResponseDoesNotEchoCredential(t *testing.T) {
	isolateServeWorkspace(t)
	state, err := fixtureProjectState("")
	require.NoError(t, err)
	cp, err := controlplane.New(controlplane.Options{CardsDir: state.CardsDir()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	key := strings.Repeat("a", 64)
	api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, err := io.WriteString(w, r.Header.Get("Authorization"))
		assert.NoError(t, err)
	}))
	require.NoError(t, api.Listener.Close())
	api.Listener, err = net.Listen("tcp", state.Address())
	require.NoError(t, err)
	api.Start()
	t.Cleanup(api.Close)
	require.NoError(t, state.SaveRunning(project.Running{Terminal: "owned-terminal", ManagementCredential: key}))
	runtime := testProjectRuntime()
	bindFixtureTeamEffects(&runtime)
	runtime.preflight = func() error { return nil }
	err = runWithRuntime(nil, io.Discard, runtime)
	require.Error(t, err)
	var failure *goahttp.ClientError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "invalid_response", failure.Name)
	assert.Contains(t, err.Error(), "502")
	assert.False(t, strings.Contains(err.Error(), key), "CLI error must never echo a management credential")
	assert.False(t, strings.Contains(err.Error(), "Bearer"))
	assert.Contains(t, err.Error(), "holds this project")
	assert.Contains(t, err.Error(), "owned-terminal")
}

// TestManagementErrorsUseOnlyCLIText covers declared errors, malformed bodies and
// malformed HTTP itself. The peer deliberately copies the sent credential into each reply.
func TestManagementErrorsUseOnlyCLIText(t *testing.T) {
	for _, scenario := range []string{"forbidden", "window_busy", "unprocessable", "unknown name", "invalid JSON", "malformed status", "malformed header"} {
		t.Run(scenario, func(t *testing.T) {
			isolateServeWorkspace(t)
			state, err := fixtureProjectState("")
			require.NoError(t, err)
			key := strings.Repeat("c", 64)
			marker := "peer-must-never-print"
			status := http.StatusForbidden
			if scenario == "window_busy" {
				status = http.StatusConflict
			}
			if scenario == "unprocessable" {
				status = http.StatusUnprocessableEntity
			}
			if scenario == "invalid JSON" {
				status = http.StatusOK
			}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(scenario, "malformed") {
					conn, writer, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					defer func() { assert.NoError(t, conn.Close()) }()
					if scenario == "malformed status" {
						_, err = fmt.Fprintf(writer, "HTTP/1.1 %s %s\r\n\r\n", key, marker)
					} else {
						_, err = fmt.Fprintf(writer, "HTTP/1.1 200 OK\r\n%s %s\r\n\r\n", key, marker)
					}
					assert.NoError(t, err)
					assert.NoError(t, writer.Flush())
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("goa-error", scenario)
				w.WriteHeader(status)
				if scenario == "invalid JSON" {
					_, err := fmt.Fprintf(w, "%s %s", r.Header.Get("Authorization"), marker)
					assert.NoError(t, err)
					return
				}
				name := scenario
				if scenario == "unknown name" {
					name = marker + key
				}
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"name": name, "id": marker + key, "message": marker + r.Header.Get("Authorization")}))
			}))
			t.Cleanup(api.Close)
			state.Record.Port = api.Listener.Addr().(*net.TCPAddr).Port
			require.NoError(t, state.SaveRunning(project.Running{ManagementCredential: key}))
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if scenario == "window_busy" {
				_, err = projectClient(state).EnsureTeam(ctx)
			} else {
				_, err = projectClient(state).Identity(ctx)
			}
			require.Error(t, err)
			assert.False(t, strings.Contains(err.Error(), key), "the CLI must never echo the sent credential")
			assert.False(t, strings.Contains(err.Error(), marker), "the CLI must never print peer-authored text")
			if strings.HasPrefix(scenario, "malformed") {
				assert.Contains(t, err.Error(), "project server connection failed")
			} else {
				assert.Contains(t, err.Error(), fmt.Sprintf("HTTP %d", status))
			}
		})
	}
}

// TestManagementResponseStatusIsRequestLocal detects accidental cross-talk between
// concurrent generated operations sharing the same native HTTP client.
func TestManagementResponseStatusIsRequestLocal(t *testing.T) {
	isolateServeWorkspace(t)
	state, err := fixtureProjectState("")
	require.NoError(t, err)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := http.StatusTeapot
		if r.URL.Path == "/project/browser-link" {
			status = http.StatusBadGateway
		}
		w.WriteHeader(status)
		_, err := io.WriteString(w, r.Header.Get("Authorization"))
		assert.NoError(t, err)
	}))
	t.Cleanup(api.Close)
	state.Record.Port = api.Listener.Addr().(*net.TCPAddr).Port
	key := strings.Repeat("a", 64)
	require.NoError(t, state.SaveRunning(project.Running{ManagementCredential: key}))
	client := projectClient(state)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var group sync.WaitGroup
	group.Go(func() {
		_, err := client.Identity(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "418")
		assert.False(t, strings.Contains(err.Error(), key))
	})
	group.Go(func() {
		_, err := client.BrowserLink(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "502")
		assert.False(t, strings.Contains(err.Error(), key))
	})
	group.Wait()
}

// TestServeWithdrawsRunningBeforeStoreRelease observes the actual composition root:
// its owned listener removes the record while the control plane still holds its lock.
func TestServeWithdrawsRunningBeforeStoreRelease(t *testing.T) {
	isolateServeWorkspace(t)
	state, err := fixtureProjectState("")
	require.NoError(t, err)
	runtime := testProjectRuntime()
	bindFixtureTeamEffects(&runtime)
	closed := make(chan struct{})
	runtime.listen = func(network, address string) (net.Listener, error) {
		listener, err := net.Listen(network, address)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() {
			// A fixture failure must still unblock the server's accept loop.
			select {
			case <-closed:
			default:
				assert.NoError(t, listener.Close())
			}
		})
		return &closeProbe{Listener: listener, beforeClose: func() {
			_, err := state.ReadRunning()
			assert.ErrorIs(t, err, os.ErrNotExist)
			held, err := controlplane.CardStoreInUse(state.CardsDir())
			assert.NoError(t, err)
			assert.True(t, held, "record withdrawal must precede the actual store-lock release")
			close(closed)
		}}, nil
	}
	done := make(chan error, 1)
	go func() { done <- runtime.serve() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = waitForRunning(ctx, state)
	require.NoError(t, err)
	require.NoError(t, projectClient(state).Stop(ctx))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("actual serve did not close")
	}
	select {
	case <-closed:
	default:
		t.Fatal("actual listener close was not observed")
	}
	held, err := controlplane.CardStoreInUse(state.CardsDir())
	require.NoError(t, err)
	assert.False(t, held)
	_, err = state.ReadRunning()
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestProjectTerminalLookupUsesOnlyInjectedProgram forbids fallback to a user's hc
// even when the owned executable is absent or the supplied lookup fails.
func TestProjectTerminalLookupUsesOnlyInjectedProgram(t *testing.T) {
	isolateServeWorkspace(t)
	state, err := fixtureProjectState("")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	calls := 0
	_, err = startProjectTerminal(ctx, state, func() (string, error) { calls++; return "", errors.New("owned lookup unavailable") })
	assert.ErrorContains(t, err, "owned lookup unavailable")
	assert.Equal(t, 1, calls)
	_, err = startProjectTerminal(ctx, state, func() (string, error) { return filepath.Join(t.TempDir(), "absent-hc"), nil })
	assert.Error(t, err, "missing owned executable must not reach production fallback")
}

// TestManagementLocalFailuresPreserveIdentity keeps file and deadline errors useful.
func TestManagementLocalFailuresPreserveIdentity(t *testing.T) {
	isolateServeWorkspace(t)
	state, err := fixtureProjectState("")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = projectClient(state).Identity(ctx)
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorContains(t, err, "has not published management access")
	require.NoError(t, os.WriteFile(filepath.Join(state.Dir, "running.json"), []byte("not JSON"), 0o600))
	_, err = projectClient(state).Identity(t.Context())
	assert.ErrorContains(t, err, "decode project state")
}
