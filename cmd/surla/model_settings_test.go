// This file proves what the surla command hands over and prints: the request
// settings it sends, the line naming what the server holds, the programs that
// receive or never receive the shell's provider variables, and that no reply
// of the project server makes it print a value.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/modelsettings"
	"github.com/goadesign/surla/internal/project"
)

// shellFamily stands for the provider variables a user exported in the
// shell that runs surla; none is a real credential.
var shellFamily = map[string]string{
	"ANTHROPIC_API_KEY":   "sentinel-handed-key",
	"ANTHROPIC_BASE_URL":  "https://gateway.invalid/sentinel-url-credential/",
	"ANTHROPIC_ADMIN_KEY": "sentinel-shell-admin",
	"OPENAI_BASE_URL":     "https://openai.invalid/sentinel-openai-url/",
}

// TestModelSettingsLineNamesWhatIsHeld covers the line in each case: this
// shell, an earlier surla, both mixed, nothing held but something not sent,
// nothing at all, and the model live targets use.
func TestModelSettingsLineNamesWhatIsHeld(t *testing.T) {
	anthropicDefault := directChoice("anthropic", "claude-sonnet-4-6")
	for _, tc := range []struct {
		name   string
		sel    modelsettings.Selection
		result *gensurla.ModelSettingsResult
		want   string
	}{
		{
			name: "this shell, with an unlisted variable",
			sel: modelsettings.Selection{
				Send:    map[string]map[string]string{"anthropic": {"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "u"}},
				NotSent: []modelsettings.Unsent{{Name: "ANTHROPIC_ADMIN_KEY"}},
			},
			result: &gensurla.ModelSettingsResult{Held: map[string][]string{"anthropic": {"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"}}, Connection: anthropicDefault},
			want: "Model settings held by the project server: ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL (from this shell); not sent: ANTHROPIC_ADMIN_KEY. " +
				"Live targets use anthropic/claude-sonnet-4-6; change it on the views page. surla stop makes the project server forget them.",
		},
		{
			name:   "an earlier surla",
			sel:    modelsettings.Selection{Send: map[string]map[string]string{}},
			result: &gensurla.ModelSettingsResult{Held: map[string][]string{"anthropic": {"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"}}, Connection: anthropicDefault},
			want: "Model settings held by the project server: ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL (from an earlier surla). " +
				"Live targets use anthropic/claude-sonnet-4-6; change it on the views page. surla stop makes the project server forget them.",
		},
		{
			name: "mixed, with a setting whose key is not set",
			sel: modelsettings.Selection{
				Send:    map[string]map[string]string{"openai": {"OPENAI_API_KEY": "k"}},
				NotSent: []modelsettings.Unsent{{Name: "ANTHROPIC_AUTH_TOKEN", MissingKey: "ANTHROPIC_API_KEY"}, {Name: "ANTHROPIC_BASE_URL", MissingKey: "ANTHROPIC_API_KEY"}, {Name: "OPENAI_ADMIN_KEY"}},
			},
			result: &gensurla.ModelSettingsResult{Held: map[string][]string{"anthropic": {"ANTHROPIC_API_KEY"}, "openai": {"OPENAI_API_KEY"}}, Connection: anthropicDefault},
			want: "Model settings held by the project server: ANTHROPIC_API_KEY (from an earlier surla), OPENAI_API_KEY (from this shell); " +
				"not sent: ANTHROPIC_AUTH_TOKEN, ANTHROPIC_BASE_URL (ANTHROPIC_API_KEY is not set), OPENAI_ADMIN_KEY. " +
				"Live targets use anthropic/claude-sonnet-4-6; change it on the views page. surla stop makes the project server forget them.",
		},
		{
			name:   "both from this shell",
			sel:    modelsettings.Selection{Send: map[string]map[string]string{"anthropic": {"ANTHROPIC_API_KEY": "k"}, "openai": {"OPENAI_API_KEY": "k"}}},
			result: &gensurla.ModelSettingsResult{Held: map[string][]string{"anthropic": {"ANTHROPIC_API_KEY"}, "openai": {"OPENAI_API_KEY"}}, Connection: directChoice("openai", "gpt-5.4-mini")},
			want: "Model settings held by the project server: ANTHROPIC_API_KEY, OPENAI_API_KEY (from this shell). " +
				"Live targets use openai/gpt-5.4-mini; change it on the views page. surla stop makes the project server forget them.",
		},
		{
			name:   "nothing held, a setting without its key",
			sel:    modelsettings.Selection{Send: map[string]map[string]string{}, NotSent: []modelsettings.Unsent{{Name: "ANTHROPIC_BASE_URL", MissingKey: "ANTHROPIC_API_KEY"}}},
			result: &gensurla.ModelSettingsResult{Held: map[string][]string{}, Connection: noChoice()},
			want: "No model settings are held by the project server; not sent: ANTHROPIC_BASE_URL (ANTHROPIC_API_KEY is not set). " +
				"Live targets have no model: export a provider key in a shell and run surla there, or choose a model gateway on the views page.",
		},
		{
			name:   "nothing held, nothing unsent, a gateway",
			sel:    modelsettings.Selection{Send: map[string]map[string]string{}},
			result: &gensurla.ModelSettingsResult{Held: map[string][]string{}, Connection: gatewayChoice("127.0.0.1:9090")},
			want:   "",
		},
		{
			name:   "a gateway and settings; a model text from the server is shown printable",
			sel:    modelsettings.Selection{Send: map[string]map[string]string{}},
			result: &gensurla.ModelSettingsResult{Held: map[string][]string{"openai": {"OPENAI_API_KEY"}}, Connection: gatewayChoice("127.0.0.1:9090\x1b[2J")},
			want: "Model settings held by the project server: OPENAI_API_KEY (from an earlier surla). " +
				"Live targets use the model gateway at 127.0.0.1:9090; change it on the views page. surla stop makes the project server forget them.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, modelSettingsLine(tc.sel, tc.result))
		})
	}
}

// TestHandOverSendsTheShellsRequestSettings runs the real start against a
// real project server: the request carries the key and base URL, never the
// admin key or a base URL whose key is unset; the line names them; a second
// start from a shell without a key keeps them and says they came from an
// earlier surla.
func TestHandOverSendsTheShellsRequestSettings(t *testing.T) {
	isolateServeWorkspace(t)
	isolateProviderFamilies(t)
	for name, value := range shellFamily {
		t.Setenv(name, value)
	}
	state, err := fixtureProjectState("")
	require.NoError(t, err)
	cp, bodies := startRecordingProjectServer(t, state)
	errStop := errors.New("stopped after the hand-over")
	runtime := testProjectRuntime()
	bindFixtureTeamEffects(&runtime)
	runtime.preflight = func() error { return nil }
	runtime.startTeamPrograms = func(context.Context) error { return errStop }

	var first bytes.Buffer
	require.ErrorIs(t, runtime.start(&first), errStop)
	require.Len(t, *bodies, 1, "one hand-over per start")
	var sent map[string]map[string]string
	require.NoError(t, json.Unmarshal([]byte((*bodies)[0]), &sent))
	assert.Equal(t, map[string]map[string]string{"anthropic": {
		"ANTHROPIC_API_KEY": shellFamily["ANTHROPIC_API_KEY"], "ANTHROPIC_BASE_URL": shellFamily["ANTHROPIC_BASE_URL"],
	}}, sent, "the request carries the key and its request settings only")
	assert.Equal(t, "Model settings held by the project server: ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL (from this shell); "+
		"not sent: OPENAI_BASE_URL (OPENAI_API_KEY is not set), ANTHROPIC_ADMIN_KEY. Live targets use anthropic/claude-sonnet-4-6; "+
		"change it on the views page. surla stop makes the project server forget them.\n", first.String())
	read, err := cp.GetModelConnection(controlplane.WithPrincipal(t.Context(), controlplane.Principal{ID: state.Record.Owner, Kind: controlplane.CallerUser}))
	require.NoError(t, err)
	assert.Equal(t, directChoice("anthropic", "claude-sonnet-4-6"), read.Connection)

	isolateProviderFamilies(t)
	var second bytes.Buffer
	require.ErrorIs(t, runtime.start(&second), errStop)
	require.Len(t, *bodies, 2)
	assert.JSONEq(t, `{}`, (*bodies)[1], "a shell without a key sends nothing")
	assert.Equal(t, "Model settings held by the project server: ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL (from an earlier surla). "+
		"Live targets use anthropic/claude-sonnet-4-6; change it on the views page. surla stop makes the project server forget them.\n", second.String())
	for _, out := range []string{first.String(), second.String()} {
		assertNoShellValue(t, out)
	}
}

// TestHandOverRepliesNeverPrintAValue forces each reply the project server
// can give the hand-over, through the generated client, and checks the error
// text and output: refused with the credential unchanged and changed, an
// older server's 404, a 400, a 500, a refused connection and a deadline.
func TestHandOverRepliesNeverPrintAValue(t *testing.T) {
	isolateProviderFamilies(t)
	for name, value := range shellFamily {
		t.Setenv(name, value)
	}
	sel := modelsettings.Select(os.Environ())
	result := `{"held":{"anthropic":["ANTHROPIC_API_KEY","ANTHROPIC_BASE_URL"]},"connection":{"kind":"direct","provider":"anthropic","model_name":"claude-sonnet-4-6"}}`
	for _, tc := range []struct {
		name    string
		replies []int
		rotate  bool
		closed  bool
		hang    bool
		want    string
		output  string
	}{
		{name: "success", replies: []int{http.StatusOK}, output: "Model settings held by the project server: ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL (from this shell)"},
		{name: "refused with an unchanged credential", replies: []int{http.StatusUnauthorized}, want: "project server response could not be used (HTTP 401); run surla again"},
		{name: "refused, then accepted with the changed credential", replies: []int{http.StatusUnauthorized, http.StatusOK}, rotate: true, output: "(from this shell)"},
		{name: "refused twice with a changed credential", replies: []int{http.StatusUnauthorized, http.StatusUnauthorized}, rotate: true, want: "[surla hand_over_model_settings]: invalid response code 401"},
		{name: "older server", replies: []int{http.StatusNotFound}, want: "this project’s server was started by another version; run surla stop, then surla"},
		{name: "value too long", replies: []int{http.StatusBadRequest}, want: "the project server refused the model settings from this shell (HTTP 400); each ANTHROPIC_ and OPENAI_ request setting must be at most 4096 characters"},
		{name: "server fault", replies: []int{http.StatusInternalServerError}, want: "[surla hand_over_model_settings]: invalid response code 500"},
		{name: "connection refused", closed: true, want: "project server connection failed; run surla again"},
		{name: "deadline", hang: true, want: "project server request did not finish"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateServeWorkspace(t)
			state, err := fixtureProjectState("")
			require.NoError(t, err)
			var mu sync.Mutex
			var bodies []string
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				mu.Lock()
				attempt := len(bodies)
				bodies = append(bodies, string(raw))
				mu.Unlock()
				if tc.hang {
					<-release
					return
				}
				if tc.rotate && attempt == 0 {
					assert.NoError(t, state.SaveRunning(project.Running{ManagementCredential: strings.Repeat("b", 64)}))
				}
				status := tc.replies[attempt]
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				body := `{"name":"peer","message":"` + shellFamily["ANTHROPIC_API_KEY"] + `"}`
				if status == http.StatusOK {
					body = result
				}
				_, err = io.WriteString(w, body)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			t.Cleanup(func() { close(release) })
			state.Record.Port = server.Listener.Addr().(*net.TCPAddr).Port
			if tc.closed {
				server.Close()
			}
			require.NoError(t, state.SaveRunning(project.Running{ManagementCredential: strings.Repeat("a", 64)}))
			// A hanging server meets the caller's deadline before the
			// client's own one-second timeout.
			deadline := 2 * time.Second
			if tc.hang {
				deadline = 300 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), deadline)
			defer cancel()
			var out bytes.Buffer
			err = handOverModelSettings(ctx, state, sel, &out)
			if tc.want != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.want)
				assertNoShellValue(t, err.Error())
			} else {
				require.NoError(t, err)
			}
			assert.Contains(t, out.String(), tc.output)
			assertNoShellValue(t, out.String())
			mu.Lock()
			defer mu.Unlock()
			for _, body := range bodies[min(len(bodies), 1):] {
				assert.Equal(t, bodies[0], body, "a retry resends the same request")
			}
		})
	}
}

// TestRefusedHandOverStopsStartBeforeTheTeam runs the real start against a
// project server that answers for the project but refuses the hand-over:
// start fails with the refusal's fixed text, prints nothing, and never
// starts the team programs, asks for the team, mints a browser link or opens
// the views.
func TestRefusedHandOverStopsStartBeforeTheTeam(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{name: "older server", status: http.StatusNotFound, want: "this project’s server was started by another version; run surla stop, then surla"},
		{name: "value too long", status: http.StatusBadRequest, want: "the project server refused the model settings from this shell (HTTP 400)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateServeWorkspace(t)
			isolateProviderFamilies(t)
			t.Setenv("ANTHROPIC_API_KEY", shellFamily["ANTHROPIC_API_KEY"])
			state, err := fixtureProjectState("")
			require.NoError(t, err)
			// The held card store makes start attach to the answering server.
			cp, err := controlplane.New(controlplane.Options{CardsDir: state.CardsDir()})
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, cp.Close()) })
			var mu sync.Mutex
			var requested []string
			api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requested = append(requested, r.URL.Path)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/project/identity":
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{
						"project_id": state.Record.ID, "directory": state.Record.Directory, "owner": state.Record.Owner,
					}))
				case "/cp1/model/settings":
					w.WriteHeader(tc.status)
					_, err := io.WriteString(w, `{"name":"peer","message":"`+shellFamily["ANTHROPIC_API_KEY"]+`"}`)
					assert.NoError(t, err)
				default:
					w.WriteHeader(http.StatusInternalServerError)
				}
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
			teamStarted, opened := false, false
			runtime.startTeamPrograms = func(context.Context) error { teamStarted = true; return nil }
			runtime.openViews = func(context.Context, string) error { opened = true; return nil }

			var out bytes.Buffer
			err = runtime.start(&out)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assertNoShellValue(t, err.Error())
			assert.Empty(t, out.String(), "nothing is printed after a refused hand-over")
			assert.False(t, teamStarted, "no team program starts")
			assert.False(t, opened, "the views are not opened")
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, []string{"/project/identity", "/cp1/model/settings"}, requested, "neither ensure_team nor browser_link is asked for")
		})
	}
}

// TestCLIChildrenReceiveTheShellsProviderVariablesOnlyWhereAllowed runs the
// command's real program launches against presence stand-ins: Herdr's
// programs (hc and herdr-app) get the shell's environment unchanged, while
// Chorus and the browser opener get none of its provider variables.
func TestCLIChildrenReceiveTheShellsProviderVariablesOnlyWhereAllowed(t *testing.T) {
	isolateServeWorkspace(t)
	isolateProviderFamilies(t)
	reports, bin := t.TempDir(), t.TempDir()
	t.Setenv("SURLA_TEST_PRESENCE_DIR", reports)
	t.Setenv("SURLA_TEST_STAND_IN_DIR", bin)
	t.Setenv("ANTHROPIC_API_KEY", "sentinel-handed-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sentinel-shell-token")
	t.Setenv("OPENAI_API_KEY", "sentinel-shell-openai")
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	// Each stand-in reports under its name and first argument; the opener's
	// argument is a link, so it reports as "opener".
	standIns := map[string]string{
		"chorus":     `case "$1" in version) echo "chorus v0.11.0";; status) [ -f "$SURLA_TEST_STAND_IN_DIR/chorus-up" ] && echo "Chorus v0.11.0 is running" || exit 1;; up) : > "$SURLA_TEST_STAND_IN_DIR/chorus-up";; esac`,
		"herdr-app":  `case "$1" in status) [ -f "$SURLA_TEST_STAND_IN_DIR/app-up" ] && printf '{"overlays":[]}' || exit 1;; "") : > "$SURLA_TEST_STAND_IN_DIR/app-up";; esac`,
		"herdr-core": `exit 0`,
		"hc":         `case "$1" in run) printf '{"address":"stand-in"}';; wait) printf '{"ended":false}';; rm) :;; esac`,
		opener:       `:`,
	}
	for name, body := range standIns {
		report := name + "-${1:-start}"
		if name == opener {
			report = "opener"
		}
		script := "#!/bin/sh\nname=" + report + "\n/bin/sh -c '" + strings.ReplaceAll(presenceScript, "'", `'\''`) + "' \"$name\" \"$@\"\n" + body + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700))
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")

	commands, err := cliTeamCommands()
	require.NoError(t, err)
	require.NoError(t, preflightTeam(commands))
	require.NoError(t, startTeamPrograms(t.Context(), commands))
	require.NoError(t, openProjectViews(t.Context(), "http://127.0.0.1:1/views#link=private"))
	state, err := fixtureProjectState("")
	require.NoError(t, err)
	terminal, err := startProjectTerminal(t.Context(), state, projectHerdrProgram)
	require.NoError(t, err)
	_, err = waitProjectTerminal(t.Context(), terminal)
	require.NoError(t, err)
	require.NoError(t, removeProjectTerminal(t.Context(), terminal))

	present := "ANTHROPIC_API_KEY=handed\nANTHROPIC_AUTH_TOKEN=set\nANTHROPIC_BASE_URL=\nANTHROPIC_ADMIN_KEY=\nOPENAI_API_KEY=set\nARGS_HOLD_SENTINEL=no\n"
	absent := "ANTHROPIC_API_KEY=\nANTHROPIC_AUTH_TOKEN=\nANTHROPIC_BASE_URL=\nANTHROPIC_ADMIN_KEY=\nOPENAI_API_KEY=\nARGS_HOLD_SENTINEL=no\n"
	for _, report := range []string{"chorus-version", "chorus-status", "chorus-up", "opener"} {
		assert.Equal(t, absent, readPresence(t, reports, report), "%s gets no provider variable", report)
	}
	for _, report := range []string{"herdr-app-status", "herdr-app-start", "hc-run", "hc-wait", "hc-rm"} {
		assert.Equal(t, present, readPresence(t, reports, report), "%s gets the shell's environment unchanged", report)
	}
}

// startRecordingProjectServer serves state's project with a real control
// plane and project server on its address, recording each hand-over
// request body, and publishes its management credential.
func startRecordingProjectServer(t *testing.T, state *project.State) (*controlplane.ControlPlane, *[]string) {
	t.Helper()
	cp, err := controlplane.New(controlplane.Options{CardsDir: state.CardsDir()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	management, err := project.NewServer(state, func() {}, fixtureTeamDependencies(state))
	require.NoError(t, err)
	var bodies []string
	handler := management.Handler(controlplane.NewHTTPHandler(cp, management.Authenticate))
	recorder := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cp1/model/settings" {
			raw, err := io.ReadAll(r.Body)
			if !assert.NoError(t, err) {
				return
			}
			bodies = append(bodies, string(raw))
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		handler.ServeHTTP(w, r)
	})
	api := httptest.NewUnstartedServer(recorder)
	require.NoError(t, api.Listener.Close())
	api.Listener, err = net.Listen("tcp", state.Address())
	require.NoError(t, err)
	api.Start()
	t.Cleanup(api.Close)
	require.NoError(t, management.PublishRunning(os.Getpid(), "fixture"))
	return cp, &bodies
}

// assertNoShellValue fails for every shell value, or part of one, text holds.
func assertNoShellValue(t *testing.T, text string) {
	t.Helper()
	assert.NotContains(t, text, "sentinel")
	for _, value := range shellFamily {
		assert.NotContains(t, text, value)
	}
}
