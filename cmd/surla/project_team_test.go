// This file supplies bounded stand-ins for every new startup effect, including tagged tests.
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
	"testing"
	"time"

	"github.com/goadesign/surla/internal/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureTeamHost keeps runtime helper effects inside each test-owned state root.
type fixtureTeamHost struct{ space project.TeamSpace }

// Spaces returns the fixture listing without reading Herdr.
func (f *fixtureTeamHost) Spaces(context.Context) ([]project.TeamSpace, error) {
	if f.space.ID == "" {
		return nil, nil
	}
	return []project.TeamSpace{f.space}, nil
}

// CreateSpace records the requested local space and returns its terminal mapping.
func (f *fixtureTeamHost) CreateSpace(_ context.Context, name, folder string) (project.TeamSpace, error) {
	f.space = project.TeamSpace{ID: "fixture-space", Name: name, Folder: folder, Tabs: []project.TeamTab{{ID: "fixture-tab", Terminals: []project.TeamPane{{Terminal: "fixture-terminal"}}}}}
	return f.space, nil
}

// Terminals returns only fixture foreground and label observations.
func (f *fixtureTeamHost) Terminals(context.Context, string, string, []string) ([]project.TeamTerminal, error) {
	return nil, nil
}

// Focus records the requested fixture focus without touching a window.
func (f *fixtureTeamHost) Focus(context.Context, project.TeamSpace) error { return nil }

// Overlays returns the fixture dialog names.
func (f *fixtureTeamHost) Overlays(context.Context) ([]string, error) { return nil, nil }

// OpenLauncher records the request without opening a real dialog.
func (f *fixtureTeamHost) OpenLauncher(context.Context) error { return nil }

// fixtureTeamDependencies binds only the caller's temporary state directory and recorded effects.
func fixtureTeamDependencies(state *project.State) project.TeamDependencies {
	return project.TeamDependencies{Lifetime: context.Background(), Host: &fixtureTeamHost{}, RosterDirectory: filepath.Join(state.Dir, "fixture-rosters"), Shell: "/bin/zsh"}
}

// bindFixtureTeamEffects explicitly opts a journey into test-owned team effects.
// When SURLA_TEST_PRESENCE_DIR names a folder, the team step reports there, by
// names and booleans only, which provider variables the server's process and
// Herdr's copy of its environment hold (writePresence).
func bindFixtureTeamEffects(runtime *projectRuntime) {
	runtime.startTeamPrograms = func(context.Context) error { return nil }
	runtime.teamDependencies = func(ctx context.Context, herdrEnv []string) (project.TeamDependencies, error) {
		if dir := os.Getenv("SURLA_TEST_PRESENCE_DIR"); dir != "" {
			if err := errors.Join(
				writePresence(filepath.Join(dir, "server-process"), os.Environ()),
				writePresence(filepath.Join(dir, "server-herdr"), herdrEnv),
			); err != nil {
				return project.TeamDependencies{}, err
			}
		}
		root, err := runtime.configRoot()
		if err != nil {
			return project.TeamDependencies{}, err
		}
		return project.TeamDependencies{Lifetime: ctx, Host: &fixtureTeamHost{}, RosterDirectory: filepath.Join(root, "fixture-rosters"), Shell: "/bin/zsh"}, nil
	}
}

// TestTeamPreflightVersionAndPrograms verifies the server or command contract using only temporary state and stand-ins.
func TestTeamPreflightVersionAndPrograms(t *testing.T) {
	for _, missing := range []string{"herdr-core", "herdr-app", "chorus"} {
		t.Run(missing, func(t *testing.T) {
			calls := 0
			_, err := resolveTeamCommands(func(name string) (string, error) {
				if name == missing {
					return "", assert.AnError
				}
				return name, nil
			}, func(context.Context, []string, string, ...string) ([]byte, error) { calls++; return nil, nil })
			require.ErrorContains(t, err, missing)
			assert.Contains(t, err.Error(), "install it with:")
			assert.Zero(t, calls)
		})
	}
	for _, version := range []string{"v0.10.1", "v0.11.0", "v0.12.0", "invalid", "v0.11.0-rc.1"} {
		t.Run(version, func(t *testing.T) {
			commands := teamCommands{chorus: "chorus", run: func(_ context.Context, _ []string, name string, args ...string) ([]byte, error) {
				assert.Equal(t, "chorus", name)
				assert.Equal(t, []string{"version"}, args)
				return []byte("chorus " + version + "\n"), nil
			}}
			err := preflightTeam(commands)
			if version == "v0.11.0" || version == "v0.12.0" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, "chorus update -yes")
			}
		})
	}
}

// TestTeamProgramStartup records service/window starts and all readiness failures.
func TestTeamProgramStartup(t *testing.T) {
	for _, scenario := range []string{"ready", "absent", "chorus up fails", "window start fails", "status deadline", "json error"} {
		t.Run(scenario, func(t *testing.T) {
			var calls []string
			chorusReady, appReady := scenario == "ready", scenario == "ready"
			commands := teamCommands{chorus: "chorus", app: "herdr-app"}
			commands.start = func(_ []string, name string, args ...string) (<-chan error, error) {
				call := name + " " + strings.Join(args, " ")
				calls = append(calls, strings.TrimSpace(call))
				if name == commands.chorus {
					if scenario == "chorus up fails" {
						return nil, assert.AnError
					}
					chorusReady = true
				} else {
					if scenario == "window start fails" {
						return nil, assert.AnError
					}
					appReady = true
				}
				done := make(chan error, 1)
				done <- nil
				return done, nil
			}
			commands.run = func(ctx context.Context, _ []string, name string, args ...string) ([]byte, error) {
				call := name + " " + strings.Join(args, " ")
				calls = append(calls, strings.TrimSpace(call))
				if name == "chorus" && len(args) > 0 && args[0] == "status" {
					if !chorusReady {
						return nil, assert.AnError
					}
					return []byte("Chorus v0.11.0 is running"), nil
				}
				if name == "chorus" {
					if scenario == "chorus up fails" {
						return nil, assert.AnError
					}
					chorusReady = true
					return nil, nil
				}
				if len(args) == 0 {
					if scenario == "window start fails" {
						return nil, assert.AnError
					}
					appReady = true
					return nil, nil
				}
				if scenario == "json error" {
					return []byte(`{"error":"private peer error"}`), nil
				}
				if !appReady || scenario == "status deadline" {
					return nil, assert.AnError
				}
				return []byte(`{"overlays":[]}`), nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			err := startTeamPrograms(ctx, commands)
			if scenario == "ready" || scenario == "absent" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "private peer error")
			}
			if scenario == "ready" {
				assert.Equal(t, []string{"chorus status", "herdr-app status"}, calls)
			}
			if scenario == "absent" {
				assert.Equal(t, []string{"chorus status", "chorus up", "chorus status", "herdr-app status", "herdr-app", "herdr-app status"}, calls)
			}
		})
	}
}

// TestTeamCommandProtocol verifies one JSON argument and process/JSON failure handling.
func TestTeamCommandProtocol(t *testing.T) {
	var calls [][]string
	c := teamCommands{core: "herdr-core", app: "herdr-app", run: func(_ context.Context, _ []string, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		switch args[0] {
		case "do":
			if args[1] == "space.new" {
				return []byte(`{"space":"s1","tab":"t1","terminal":"term1"}`), nil
			}
			return []byte(`{}`), nil
		case "ls":
			if name == "herdr-app" {
				return []byte(`{"spaces":[{"id":"s1","name":"team","folder":"/project","tabs":[{"id":"t1","terminals":[{"terminal":"term1"}]}]},{"id":"remote/s2"}]}`), nil
			}
			return []byte(`{"terminals":[{"address":"term1"}]}`), nil
		case "get":
			return []byte(`{"state":"live","labels":{"chorus.handle":"guide"},"foreground":{"argv":["codex"]}}`), nil
		default:
			return []byte(`{"overlays":["chorus-launcher"]}`), nil
		}
	}}
	spaces, err := c.Spaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	created, err := c.CreateSpace(t.Context(), "project's team", "/project with spaces")
	require.NoError(t, err)
	assert.Equal(t, "s1", created.ID)
	require.NoError(t, c.Focus(t.Context(), created))
	require.NoError(t, c.OpenLauncher(t.Context()))
	terminals, err := c.Terminals(t.Context(), "/project", "s1", []string{"guide"})
	require.NoError(t, err)
	require.Len(t, terminals, 1)
	overlays, err := c.Overlays(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"chorus-launcher"}, overlays)
	for _, call := range calls {
		if len(call) > 1 && call[1] == "do" {
			require.Len(t, call, 4)
			var payload map[string]string
			assert.NoError(t, json.Unmarshal([]byte(call[3]), &payload))
		}
	}
	for _, reply := range []string{`{"error":"private"}`, `broken`, `{}`, `null`} {
		c.run = func(context.Context, []string, string, ...string) ([]byte, error) { return []byte(reply), nil }
		_, err := c.Spaces(t.Context())
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "private")
	}
	assert.Equal(t, time.Second, projectHTTPClient().Timeout)
	assert.Equal(t, 50*time.Second, teamHTTPClient().Timeout)
}

// TestEnsureTeamDeclaredErrorsAreFixed drives the generated CLI decoder with hostile messages.
func TestEnsureTeamDeclaredErrorsAreFixed(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{{"ambiguous", `{"name":"ambiguous_space","count":3}`, "3 Herdr spaces match", 409}, {"busy", `{"name":"window_busy","id":"fixture","message":"private peer output","temporary":false,"timeout":false,"fault":false}`, "a dialog is open in Herdr", 409}, {"unavailable", `{"name":"team_unavailable","id":"fixture","message":"private peer output","temporary":false,"timeout":false,"fault":true}`, "project team could not be prepared", 503}} {
		t.Run(tc.name, func(t *testing.T) {
			isolateServeWorkspace(t)
			state, err := fixtureProjectState(t.TempDir())
			require.NoError(t, err)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("goa-error", strings.Split(tc.body, `"`)[3])
				w.WriteHeader(tc.status)
				_, err := io.WriteString(w, tc.body)
				assert.NoError(t, err)
			}))
			defer api.Close()
			state.Record.Port = api.Listener.Addr().(*net.TCPAddr).Port
			require.NoError(t, state.SaveRunning(project.Running{ManagementCredential: strings.Repeat("a", 64)}))
			_, err = projectClient(state).EnsureTeam(t.Context())
			require.ErrorContains(t, err, tc.want)
			assert.NotContains(t, err.Error(), "private peer output")
			assert.Contains(t, err.Error(), fmt.Sprintf("HTTP %d", tc.status))
		})
	}
}

// TestTeamPreflightHasNoStateEffects proves each installation/version failure exits before discovery.
func TestTeamPreflightHasNoStateEffects(t *testing.T) {
	for _, scenario := range []string{"herdr-core", "herdr-app", "chorus", "old Chorus"} {
		t.Run(scenario, func(t *testing.T) {
			runtime := testProjectRuntimeAt(t.TempDir())
			runtime.configRoot = func() (string, error) { t.Fatal("preflight failure must not discover or create state"); return "", nil }
			runtime.startTerminal = func(context.Context, *project.State) (string, error) {
				t.Fatal("preflight failure must not start a terminal")
				return "", nil
			}
			runtime.startTeamPrograms = func(context.Context) error { t.Fatal("preflight failure must not start services/windows"); return nil }
			runtime.preflight = func() error {
				commands, err := resolveTeamCommands(func(name string) (string, error) {
					if name == scenario {
						return "", assert.AnError
					}
					return name, nil
				}, func(context.Context, []string, string, ...string) ([]byte, error) {
					return []byte("chorus v0.10.1"), nil
				})
				if err != nil {
					return err
				}
				return preflightTeam(commands)
			}
			var out strings.Builder
			err := runtime.start(&out)
			require.Error(t, err)
			assert.Empty(t, out.String())
			if scenario == "old Chorus" {
				assert.Contains(t, err.Error(), "chorus update -yes")
			} else {
				assert.Contains(t, err.Error(), scenario+" is required; install it with:")
			}
		})
	}
}
