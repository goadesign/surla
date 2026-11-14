// This file verifies detached startup and the actual CLI/runtime boundaries without real dependencies.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestProductionTeamPreflightRejectsOldChorus calls the real composition, not a reconstructed preflight.
func TestProductionTeamPreflightRejectsOldChorus(t *testing.T) {
	bin := t.TempDir()
	effects := filepath.Join(bin, "unexpected-effects")
	script := "#!/bin/sh\nif [ \"$1\" = version ]; then printf 'chorus v0.10.1\\n'; else printf '%s\\n' unexpected > \"$SURLA_TEST_TEAM_EFFECTS\"; fi\n"
	for _, name := range []string{"chorus", "herdr-core", "herdr-app", "hc", "open", "xdg-open"} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700))
	}
	t.Setenv("PATH", bin)
	t.Setenv("SURLA_TEST_TEAM_EFFECTS", effects)
	runtime := productionProjectRuntime()
	runtime.configRoot = func() (string, error) {
		t.Fatal("old Chorus must fail before discovery or state creation")
		return "", nil
	}
	runtime.startTerminal = func(context.Context, *project.State) (string, error) {
		t.Fatal("old Chorus must not start a server terminal")
		return "", nil
	}
	runtime.startTeamPrograms = func(context.Context) error { t.Fatal("old Chorus must not start services/windows"); return nil }
	runtime.openViews = func(context.Context, string) error { t.Fatal("old Chorus must not open a browser"); return nil }
	var output bytes.Buffer
	err := runtime.start(&output)
	require.ErrorContains(t, err, "Chorus v0.11.0 or newer is required; update with: chorus update -yes")
	assert.Empty(t, output.String())
	_, err = os.Stat(effects)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestDetachedTeamStartersSurvivePollingDeadline proves Surla never owns a GUI/service starter's lifetime.
func TestDetachedTeamStartersSurvivePollingDeadline(t *testing.T) {
	for _, dependency := range []string{"chorus", "herdr-app"} {
		t.Run(dependency, func(t *testing.T) {
			directory := t.TempDir()
			pidFile := filepath.Join(directory, "pid")
			signalFile := filepath.Join(directory, "signalled")
			program := filepath.Join(directory, dependency)
			childFile := filepath.Join(directory, "child-pid")
			stdoutPipe := filepath.Join(directory, "stdout-pipe")
			stderrPipe := filepath.Join(directory, "stderr-pipe")
			script := "#!/bin/sh\ntrap 'printf signalled > \"$SURLA_TEST_STARTER_SIGNAL\"; exit 0' TERM HUP INT\nif [ -p /dev/fd/1 ]; then printf pipe > \"$SURLA_TEST_STARTER_STDOUT_PIPE\"; fi\nif [ -p /dev/fd/2 ]; then printf pipe > \"$SURLA_TEST_STARTER_STDERR_PIPE\"; fi\n/bin/sleep 300 &\nchild=$!\nprintf '%s\\n' \"$child\" > \"$SURLA_TEST_STARTER_CHILD\"\nprintf '%s\\n' \"$$\" > \"$SURLA_TEST_STARTER_PID\"\nwait \"$child\"\n"
			require.NoError(t, os.WriteFile(program, []byte(script), 0o700))
			t.Setenv("SURLA_TEST_STARTER_PID", pidFile)
			t.Setenv("SURLA_TEST_STARTER_SIGNAL", signalFile)
			t.Setenv("SURLA_TEST_STARTER_CHILD", childFile)
			t.Setenv("SURLA_TEST_STARTER_STDOUT_PIPE", stdoutPipe)
			t.Setenv("SURLA_TEST_STARTER_STDERR_PIPE", stderrPipe)
			exits := make(chan (<-chan error), 1)
			commands := teamCommands{chorus: "chorus", app: "herdr-app", run: func(context.Context, string, ...string) ([]byte, error) { return nil, assert.AnError }, start: func(name string, args ...string) (<-chan error, error) {
				assert.Equal(t, dependency, name)
				exited, err := startDetachedTeamProgram(program, args...)
				if exited != nil {
					exits <- exited
				}
				return exited, err
			}}
			if dependency == "herdr-app" {
				commands.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
					if name == "chorus" {
						return []byte("Chorus v0.11.0 is running"), nil
					}
					return nil, assert.AnError
				}
			}
			pid := 0
			finished := make(chan error, 1)
			phaseEnded := false
			// Register cleanup before readiness assertions. The private script reports only
			// this test's child PID; verify its separate group before signaling that group.
			t.Cleanup(func() {
				if pid == 0 {
					if !assert.Eventually(t, func() bool {
						raw, err := os.ReadFile(pidFile)
						if err != nil {
							return false
						}
						parsed, err := strconv.Atoi(strings.TrimSpace(string(raw)))
						if err != nil || parsed <= 0 {
							return false
						}
						pid = parsed
						return true
					}, 5*time.Second, 10*time.Millisecond, "owned starter must report its PID for exact cleanup") {
						return
					}
				}
				// A failed session/group assertion must never disable fixture cleanup.
				group, groupErr := syscall.Getpgid(pid)
				if groupErr == nil && group == pid {
					err := syscall.Kill(-pid, syscall.SIGKILL)
					if !errors.Is(err, syscall.ESRCH) {
						assert.NoError(t, err)
					}
				}
				owned := []int{pid}
				childRaw, childErr := os.ReadFile(childFile)
				if assert.NoError(t, childErr) {
					child, parseErr := strconv.Atoi(strings.TrimSpace(string(childRaw)))
					if assert.NoError(t, parseErr) && assert.Positive(t, child) {
						owned = append(owned, child)
					}
				}
				t.Logf("owned detached fixture PIDs: %v", owned)
				for _, ownedPID := range owned {
					err := syscall.Kill(ownedPID, syscall.SIGKILL)
					if !errors.Is(err, syscall.ESRCH) {
						assert.NoError(t, err)
					}
				}

				select {
				case exited := <-exits:
					select {
					case <-exited:
					case <-time.After(5 * time.Second):
						t.Error("owned starter was not reaped")
					}
				case <-time.After(5 * time.Second):
					t.Error("detached starter did not return its exit observation")
				}
				assert.Eventually(t, func() bool {
					for _, ownedPID := range owned {
						if !errors.Is(syscall.Kill(ownedPID, 0), syscall.ESRCH) {
							return false
						}
					}
					return true
				}, 5*time.Second, 10*time.Millisecond, "every recorded fixture process must end and be reaped")

				if !phaseEnded {
					select {
					case <-finished:
					case <-time.After(5 * time.Second):
						t.Error("startup observer did not finish after exact fixture cleanup")
					}
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
			defer cancel()
			go func() { finished <- startTeamPrograms(ctx, commands) }()
			select {
			case err := <-finished:
				phaseEnded = true
				require.ErrorContains(t, err, dependency+" status did not become ready")
			case <-time.After(5 * time.Second):
				t.Fatal("starter was waited beyond the bounded readiness poll")
			}
			require.Eventually(t, func() bool {
				raw, err := os.ReadFile(pidFile)
				if err != nil {
					return false
				}
				parsed, err := strconv.Atoi(strings.TrimSpace(string(raw)))
				if err != nil {
					return false
				}
				pid = parsed
				return pid > 0
			}, 5*time.Second, 10*time.Millisecond)
			group, err := syscall.Getpgid(pid)
			require.NoError(t, err)
			assert.Equal(t, pid, group)
			session, err := unix.Getsid(pid)
			require.NoError(t, err)
			assert.Equal(t, pid, session, "starter must have its own session")
			assert.NoError(t, syscall.Kill(pid, 0), "poll timeout must leave the starter alive")
			_, err = os.Stat(stdoutPipe)
			assert.ErrorIs(t, err, os.ErrNotExist, "starter stdout must not be a pipe")
			_, err = os.Stat(stderrPipe)
			assert.ErrorIs(t, err, os.ErrNotExist, "starter stderr must not be a pipe")
			_, err = os.Stat(signalFile)
			assert.ErrorIs(t, err, os.ErrNotExist, "Surla must never signal the starter")
			if dependency == "herdr-app" {
				assert.Contains(t, teamProgramNextStep(commands, dependency), "open Herdr")
			}
		})
	}
}

// TestChorusReadinessMatchesLauncher requires both exit success and the launcher's literal status marker.
func TestChorusReadinessMatchesLauncher(t *testing.T) {
	for _, tc := range []struct {
		output string
		err    error
		ready  bool
	}{{"Chorus v0.11.0 is running", nil, true}, {"ready", nil, false}, {"Chorus is running", assert.AnError, false}} {
		commands := teamCommands{app: "herdr-app", chorus: "chorus", run: func(context.Context, string, ...string) ([]byte, error) { return []byte(tc.output), tc.err }}
		assert.Equal(t, tc.ready, teamProgramReady(t.Context(), commands, "chorus"))
	}
}

// TestTeamStartupHasIndependentDeadlines prevents machine readiness from consuming ensure_team's timeout.
func TestTeamStartupHasIndependentDeadlines(t *testing.T) {
	var chorusCtx, windowCtx context.Context
	commands := teamCommands{chorus: "chorus", app: "herdr-app", run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "chorus" {
			chorusCtx = ctx
			return []byte("Chorus is running"), nil
		}
		windowCtx = ctx
		return []byte(`{"overlays":[]}`), nil
	}}
	require.NoError(t, startTeamPrograms(t.Context(), commands))
	require.NotNil(t, chorusCtx)
	require.NotNil(t, windowCtx)
	assert.NotEqual(t, chorusCtx, windowCtx)
	assert.ErrorIs(t, chorusCtx.Err(), context.Canceled)
	assert.ErrorIs(t, windowCtx.Err(), context.Canceled)
}

// TestServerTeamDependenciesResolveOnOperation keeps management available without machine programs.
func TestServerTeamDependenciesResolveOnOperation(t *testing.T) {
	isolateServeWorkspace(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HERDR_APP_HOME", t.TempDir())
	state, err := fixtureProjectState(t.TempDir())
	require.NoError(t, err)
	deps, err := projectTeamDependencies(t.Context())
	require.NoError(t, err, "server construction must not resolve Herdr or Chorus")
	server, err := project.NewServer(state, func() {}, deps)
	require.NoError(t, err)
	ctx := controlplane.WithPrincipal(t.Context(), controlplane.Principal{ID: state.Record.Owner, CallerID: "management", Kind: controlplane.CallerManagement})
	_, err = server.Identity(ctx)
	require.NoError(t, err)
	_, err = server.EnsureTeam(ctx)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), state.Record.Directory)
	assert.NotContains(t, err.Error(), "PATH")
}

// TestOlderProjectServerNamesRestartStep exercises the generated 404 consumer with hostile text.
func TestOlderProjectServerNamesRestartStep(t *testing.T) {
	isolateServeWorkspace(t)
	state, err := fixtureProjectState(t.TempDir())
	require.NoError(t, err)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, err := io.WriteString(w, "private peer text")
		assert.NoError(t, err)
	}))
	defer api.Close()
	state.Record.Port = api.Listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, state.SaveRunning(project.Running{ManagementCredential: strings.Repeat("a", 64)}))
	_, err = projectClient(state).EnsureTeam(t.Context())
	require.EqualError(t, err, "this project’s server was started by another version; run surla stop, then surla")
}

// testBuildIdentity hashes this test binary's executable once per test process, as
// project.BuildIdentity reads a running surla's once. Hashing the whole binary takes
// long enough under the race detector to miss a test's bounded wait, so no request
// handler or deadline may include it.
var testBuildIdentity = sync.OnceValues(func() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
})

// testBuild is the build identity of this test binary: the SHA-256 of its executable.
// Call it on the test's goroutine before starting a fake server or a deadline, and
// give handlers the returned value: require must not run on a handler's goroutine.
func testBuild(t *testing.T) string {
	t.Helper()
	build, err := testBuildIdentity()
	require.NoError(t, err)
	return build
}

// serveFixtureIdentity answers the project's identity at its saved address while the card
// store is held, as a running server of the given build would; an empty build omits the
// field, as servers built before it did. Stop requests release the store.
func serveFixtureIdentity(t *testing.T, state *project.State, build string) {
	t.Helper()
	cp, err := controlplane.New(controlplane.Options{CardsDir: state.CardsDir()})
	require.NoError(t, err)
	var closeOnce sync.Once
	closeStore := func() { closeOnce.Do(func() { assert.NoError(t, cp.Close()) }) }
	t.Cleanup(closeStore)
	api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/project/identity":
			identity := map[string]string{"project_id": state.Record.ID, "directory": state.Record.Directory, "owner": state.Record.Owner}
			if build != "" {
				identity["build"] = build
			}
			assert.NoError(t, json.NewEncoder(w).Encode(identity))
		case "/project/stop":
			w.WriteHeader(http.StatusAccepted)
			go closeStore()
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
	require.NoError(t, api.Listener.Close())
	api.Listener, err = net.Listen("tcp", state.Address())
	require.NoError(t, err)
	api.Start()
	t.Cleanup(api.Close)
	require.NoError(t, state.SaveRunning(project.Running{ManagementCredential: strings.Repeat("b", 64)}))
}

// TestServerOfAnotherBuildNamesRestartStep refuses to attach to a running server that
// another build of surla started, such as the one running before an upgrade, and names
// the restart step; the same build attaches.
func TestServerOfAnotherBuildNamesRestartStep(t *testing.T) {
	for _, tc := range []struct{ name, build string }{
		{"same build", ""},
		{"another build", strings.Repeat("0", 64)},
		{"build not reported", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateServeWorkspace(t)
			state, err := fixtureProjectState("")
			require.NoError(t, err)
			build := tc.build
			if tc.name == "same build" {
				build = testBuild(t)
			}
			serveFixtureIdentity(t, state, build)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			_, _, err = attachOrStartProject(ctx, state, func(context.Context, *project.State) (string, error) {
				t.Error("a running server is never started again")
				return "", nil
			})
			if tc.name == "same build" {
				assert.NoError(t, err)
				return
			}
			assert.EqualError(t, err, "this project's server was started by another build of surla; run surla stop, then surla")
		})
	}
}

// TestStopAcceptsServerOfAnyBuild keeps surla stop working against a server that another
// build started, including one built before servers reported their build.
func TestStopAcceptsServerOfAnyBuild(t *testing.T) {
	for _, tc := range []struct{ name, build string }{
		{"another build", strings.Repeat("0", 64)},
		{"build not reported", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateServeWorkspace(t)
			state, err := fixtureProjectState("")
			require.NoError(t, err)
			serveFixtureIdentity(t, state, tc.build)
			var output strings.Builder
			require.NoError(t, testProjectRuntime().stop(nil, &output))
			assert.Equal(t, "Surla stopped.\n", output.String())
		})
	}
}

// TestCLIAlreadyOpenAndCredentialNonleak runs the actual CLI with generated management transport and recorded commands.
func TestCLIAlreadyOpenAndCredentialNonleak(t *testing.T) {
	isolateServeWorkspace(t)
	state, err := fixtureProjectState("")
	require.NoError(t, err)
	cp, err := controlplane.New(controlplane.Options{CardsDir: state.CardsDir()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	var calls []string
	var space *project.TeamSpace
	var overlays []string
	commands := teamCommands{core: "herdr-core", app: "herdr-app", run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "herdr-core" {
			return []byte(`{"terminals":[]}`), nil
		}
		switch args[0] {
		case "status":
			return json.Marshal(map[string]any{"overlays": overlays})
		case "ls":
			spaces := []project.TeamSpace{}
			if space != nil {
				spaces = append(spaces, *space)
			}
			return json.Marshal(map[string]any{"spaces": spaces})
		case "do":
			if args[1] == "space.new" {
				var payload map[string]string
				if err := json.Unmarshal([]byte(args[2]), &payload); err != nil {
					return nil, err
				}
				space = &project.TeamSpace{ID: "space", Name: payload["name"], Folder: payload["folder"], Tabs: []project.TeamTab{{ID: "tab", Terminals: []project.TeamPane{{Terminal: "terminal"}}}}}
				return []byte(`{"space":"space","tab":"tab","terminal":"terminal"}`), nil
			}
			return []byte(`{"ok":true}`), nil
		}
		return nil, errors.New("unexpected recorded command")
	}}
	deps := fixtureTeamDependencies(state)
	deps.Host = commands
	server, err := project.NewServer(state, func() {}, deps)
	require.NoError(t, err)
	overlays = []string{}
	ctx := controlplane.WithPrincipal(t.Context(), controlplane.Principal{ID: state.Record.Owner, CallerID: "management", Kind: controlplane.CallerManagement})
	_, err = server.EnsureTeam(ctx)
	require.NoError(t, err)
	api := httptest.NewUnstartedServer(server.Handler(controlplane.NewHTTPHandler(cp, server.Authenticate)))
	require.NoError(t, api.Listener.Close())
	api.Listener, err = net.Listen("tcp", state.Address())
	require.NoError(t, err)
	api.Start()
	t.Cleanup(api.Close)
	require.NoError(t, server.PublishRunning(os.Getpid(), "fixture"))
	running, err := state.ReadRunning()
	require.NoError(t, err)
	overlays = []string{"chorus-launcher"}
	calls = nil
	runtime := testProjectRuntime()
	bindFixtureTeamEffects(&runtime)
	runtime.preflight = func() error { return nil }
	runtime.startTeamPrograms = func(ctx context.Context) error {
		_, hasDeadline := ctx.Deadline()
		assert.False(t, hasDeadline, "machine phases own separate deadlines")
		return nil
	}
	runtime.openViews = func(context.Context, string) error { return nil }
	var output bytes.Buffer
	require.NoError(t, runtime.start(&output))
	assert.Contains(t, output.String(), `If the open Chorus dialog is for this project, click "Reload team", then choose the agents. If it is for another project, close it, then start the agents again (Start agents on the views page, or run surla).`)
	for actor := range state.Record.Agents {
		secret, err := os.ReadFile(state.AgentFile(actor))
		require.NoError(t, err)
		assert.NotContains(t, strings.Join(calls, "\n"), string(secret))
		assert.NotContains(t, output.String(), string(secret))
	}
	assert.NotContains(t, strings.Join(calls, "\n"), running.ManagementCredential)
	assert.NotContains(t, output.String(), running.ManagementCredential)
	for _, call := range calls {
		assert.NotContains(t, call, "space.select")
		assert.NotContains(t, call, "pane.focus")
		assert.NotContains(t, call, "mod.open")
	}
}

// TestCommandErrorsNeverIncludeArguments verifies server adapters do not export selected paths or argv.
func TestCommandErrorsNeverIncludeArguments(t *testing.T) {
	raw, err := runTeamCommand(t.Context(), "/no-such-program", "private-project-path", "private-argument")
	assert.Nil(t, raw)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "private-project-path")
	assert.NotContains(t, err.Error(), "private-argument")
	assert.Equal(t, "no-such-program command failed", err.Error())
}

// TestStarterExitFailurePrecedesReady preserves a reported command failure even when its service answers.
func TestStarterExitFailurePrecedesReady(t *testing.T) {
	ready := false
	commands := teamCommands{chorus: "chorus", app: "herdr-app", run: func(context.Context, string, ...string) ([]byte, error) {
		if ready {
			return []byte("Chorus is running"), nil
		}
		return nil, assert.AnError
	}, start: func(string, ...string) (<-chan error, error) {
		ready = true
		done := make(chan error, 1)
		done <- assert.AnError
		return done, nil
	}}
	err := startTeamPrograms(t.Context(), commands)
	require.ErrorContains(t, err, "chorus up failed to start; run chorus up, then run surla again")
}

// TestTeamTimeoutHasRecoveryStep preserves deadline identity and leaves other management wording alone.
func TestTeamTimeoutHasRecoveryStep(t *testing.T) {
	for _, operation := range []string{"team", "identity"} {
		t.Run(operation, func(t *testing.T) {
			isolateServeWorkspace(t)
			state, err := fixtureProjectState(t.TempDir())
			require.NoError(t, err)
			release := make(chan struct{})
			api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
			defer api.Close()
			defer close(release)
			state.Record.Port = api.Listener.Addr().(*net.TCPAddr).Port
			require.NoError(t, state.SaveRunning(project.Running{ManagementCredential: strings.Repeat("a", 64)}))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			if operation == "team" {
				_, err = projectClient(state).EnsureTeam(ctx)
				require.ErrorContains(t, err, "open the Herdr app, then click Start agents on the views page")
			} else {
				_, err = projectClient(state).Identity(ctx)
				require.ErrorContains(t, err, "project server request did not finish")
				assert.NotContains(t, err.Error(), "check Herdr")
			}
			assert.ErrorIs(t, err, context.DeadlineExceeded)
		})
	}
}
