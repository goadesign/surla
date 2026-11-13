package main

import (
	"bytes"
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
	runtimepkg "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	genproject "github.com/goadesign/surla/gen/project"
	goahttp "goa.design/goa/v3/http"
	"testing"
	"time"

	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectStartSerializesAndRecovers proves writer-lock and authenticated HTTP attach decisions.
// Ephemeral port selection is a bind/release snapshot; another listener can take it before launch.
func TestProjectStartSerializesAndRecovers(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	var starts atomic.Int32
	var server *http.Server
	var cp *controlplane.ControlPlane
	var active *project.State
	launch := func(ctx context.Context, state *project.State) (string, error) {
		starts.Add(1)
		var err error
		cp, err = controlplane.New(controlplane.Options{CardsDir: state.CardsDir()})
		if err != nil {
			return "", err
		}
		management, err := project.NewServer(state, func() {}, fixtureTeamDependencies(state))
		if err != nil {
			return "", err
		}
		ln, err := net.Listen("tcp", state.Address())
		if err != nil {
			return "", err
		}
		active = state
		_, recordErr := state.ReadRunning()
		assert.ErrorIs(t, recordErr, os.ErrNotExist, "stale record must be removed before launch")
		if err := management.PublishRunning(123, "test"); err != nil {
			return "", err
		}
		server = &http.Server{Handler: management.Handler(controlplane.NewHTTPHandler(cp, management.Authenticate)), ReadHeaderTimeout: time.Second}
		go func() {
			if err := server.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				assert.NoError(t, err)
			}
		}()
		return "stand-in", nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first, err := project.Discover(root, dir)
	require.NoError(t, err)
	second, err := project.Discover(root, dir)
	require.NoError(t, err)
	var group sync.WaitGroup
	for _, state := range []*project.State{first, second} {
		group.Go(func() { _, _, err := attachOrStartProject(ctx, state, launch); assert.NoError(t, err) })
	}
	group.Wait()
	require.EqualValues(t, 1, starts.Load())
	oldPort := active.Record.Port
	require.NoError(t, server.Close())
	require.NoError(t, cp.Close())
	// An abrupt exit leaves running.json; the free store lock is the recovery authority.
	recovered, err := project.Discover(root, dir)
	require.NoError(t, err)
	_, _, err = attachOrStartProject(ctx, recovered, launch)
	require.NoError(t, err)
	assert.EqualValues(t, 2, starts.Load())
	assert.Equal(t, oldPort, active.Record.Port)
	require.NoError(t, server.Close())
	require.NoError(t, cp.Close())
	require.NoError(t, active.RemoveRunning())
}

// TestProjectStartRefusesTakenPortAndHeldUnansweringStore proves neither case launches another server.
func TestProjectStartRefusesTakenPortAndHeldUnansweringStore(t *testing.T) {
	for _, test := range []string{"taken port", "held store"} {
		t.Run(test, func(t *testing.T) {
			state, err := project.Discover(t.TempDir(), t.TempDir())
			require.NoError(t, err)
			lock, err := state.StartLock(t.Context())
			require.NoError(t, err)
			_, err = state.Initialize("owner")
			require.NoError(t, err)
			require.NoError(t, lock.Close())
			if test == "taken port" {
				ln, err := net.Listen("tcp", state.Address())
				require.NoError(t, err)
				defer func() { require.NoError(t, ln.Close()) }()
			} else {
				cp, err := controlplane.New(controlplane.Options{CardsDir: state.CardsDir()})
				require.NoError(t, err)
				defer func() { require.NoError(t, cp.Close()) }()
			}
			called := false
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			_, _, err = attachOrStartProject(ctx, state, func(context.Context, *project.State) (string, error) { called = true; return "stand-in", nil })
			assert.Error(t, err)
			assert.False(t, called)
		})
	}
}

// TestTakenPortNamesHolderAndStep names what holds a stopped project's saved port and
// the step that frees it: another Surla project's running server, by its folder, or
// another program. Projects saved before new ones avoided each other's ports can share one.
func TestTakenPortNamesHolderAndStep(t *testing.T) {
	for _, holder := range []string{"another program", "another project", "another stopped project"} {
		t.Run(holder, func(t *testing.T) {
			root := t.TempDir()
			state := initializedProject(t, root, t.TempDir())
			ln, err := net.Listen("tcp", state.Address())
			require.NoError(t, err)
			defer func() { assert.NoError(t, ln.Close()) }()
			otherFolder := ""
			if holder != "another program" {
				other := initializedProject(t, root, t.TempDir())
				otherFolder = other.Record.Directory
				other.Record.Port = state.Record.Port
				raw, err := json.Marshal(other.Record)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(other.Dir, "project.json"), raw, 0o600))
				if holder == "another project" {
					cp, err := controlplane.New(controlplane.Options{CardsDir: other.CardsDir()})
					require.NoError(t, err)
					defer func() { assert.NoError(t, cp.Close()) }()
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, _, err = attachOrStartProject(ctx, state, func(context.Context, *project.State) (string, error) {
				t.Error("no server starts on a taken port")
				return "", nil
			})
			require.Error(t, err)
			if holder == "another project" {
				assert.Contains(t, err.Error(), "the Surla server of the project in "+otherFolder)
				assert.Contains(t, err.Error(), "run surla stop in that folder")
				return
			}
			assert.Contains(t, err.Error(), fmt.Sprintf("another program is listening on 127.0.0.1:%d", state.Record.Port))
			assert.Contains(t, err.Error(), "stop that program, then run surla")
		})
	}
}

// initializedProject creates the project for directory under root as surla does.
func initializedProject(t *testing.T, root, directory string) *project.State {
	t.Helper()
	state, err := project.Discover(root, directory)
	require.NoError(t, err)
	lock, err := state.StartLock(t.Context())
	require.NoError(t, err)
	_, err = state.Initialize("owner")
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	return state
}

// TestProjectSettingsPersistAndRejectStorageFields proves stopped-server management and startup snapshot behavior.
func TestProjectSettingsPersistAndRejectStorageFields(t *testing.T) {
	dir := isolateServeWorkspace(t)
	file := filepath.Join(t.TempDir(), "verification.json")
	content := `{"candidate_repo":"/candidate.git","candidate_module":".","binaries":{"app":"./cmd/app"},"suite_repo":"/suite.git","suite_commit":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","suite_module":".","suite_command":"./cmd/eval","timeout":"30m"}`
	require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
	require.NoError(t, runProjectSettings("verification", []string{"show"}, &discardOutput{}))
	unknown, err := discoverProject()
	require.NoError(t, err)
	assert.Empty(t, unknown.Record.ID)
	invalid := filepath.Join(t.TempDir(), "invalid.json")
	require.NoError(t, os.WriteFile(invalid, []byte(`{"runs_dir":"unowned"}`), 0o600))
	assert.Error(t, runProjectSettings("verification", []string{"set", invalid}, &discardOutput{}))
	unknown, err = discoverProject()
	require.NoError(t, err)
	assert.Empty(t, unknown.Record.ID, "invalid settings cannot create project state")
	require.NoError(t, runProjectSettings("verification", []string{"set", file}, &discardOutput{}))
	before, err := configureServeWorkspace()
	require.NoError(t, err)
	require.NotNil(t, before.verification)
	assert.Equal(t, "/candidate.git", before.verification.Plan.CandidateRepo)
	assertOutsideDirectory(t, before.verification.RunsDir, dir)
	require.NoError(t, os.WriteFile(file, []byte(`{"runs_dir":"/unowned"}`), 0o600))
	assert.Error(t, runProjectSettings("verification", []string{"set", file}, &discardOutput{}))
	after, err := configureServeWorkspace()
	require.NoError(t, err)
	require.NotNil(t, after.verification)
	assert.Equal(t, before.verification.Plan, after.verification.Plan)
	require.NoError(t, runProjectSettings("verification", []string{"remove"}, &discardOutput{}))
	current, err := configureServeWorkspace()
	require.NoError(t, err)
	assert.Nil(t, current.verification)
	require.NoError(t, runProjectSettings("verification", []string{"show"}, &discardOutput{}))
}

type discardOutput struct{}

func (*discardOutput) Write(p []byte) (int, error) { return len(p), nil }

// TestPromotionSettingsSurviveRestart proves startup-owned target facts and refusal preserve the prior snapshot.
func TestPromotionSettingsSurviveRestart(t *testing.T) {
	isolateServeWorkspace(t)
	release := t.TempDir()
	commit := strings.Repeat("c", 40)
	publish(t, release, commit, map[string]string{"app": "verified binary"})
	file := filepath.Join(t.TempDir(), "promotion.json")
	raw := fmt.Sprintf(`{"release_dir":%q,"suite_id":"suite","suite_commit":%q,"environment":"sim","binaries":["app"]}`, release, strings.Repeat("b", 40))
	require.NoError(t, os.WriteFile(file, []byte(raw), 0o600))
	var output bytes.Buffer
	require.NoError(t, runProjectSettings("promotion", []string{"set", file}, &output))
	assert.Contains(t, output.String(), "next time")
	load := func() controlplane.PromotionInputs {
		config, err := configureServeWorkspace()
		require.NoError(t, err)
		require.NotNil(t, config.promotionTarget)
		cp, err := controlplane.New(controlplane.Options{CardsDir: config.cardsDir, PromotionTarget: config.promotionTarget})
		require.NoError(t, err)
		inputs, err := config.promotionTarget.CurrentInputs(t.Context(), commit)
		require.NoError(t, err)
		require.NoError(t, cp.Close())
		return inputs
	}
	before := load()
	assert.Equal(t, digestOf("verified binary"), before.Artifacts["app"])
	// Both fields removed from settings are rejected; no alternate report store can be introduced.
	require.NoError(t, os.WriteFile(file, []byte(strings.TrimSuffix(raw, "}")+`,"report_dir":"/unowned"}`), 0o600))
	assert.Error(t, runProjectSettings("promotion", []string{"set", file}, &output))
	assert.Equal(t, before, load())
	output.Reset()
	require.NoError(t, runProjectSettings("promotion", []string{"show"}, &output))
	assert.Contains(t, output.String(), `"suite_id":"suite"`)
	assert.NotContains(t, output.String(), "report_dir")
	require.NoError(t, runProjectSettings("promotion", []string{"remove"}, &output))
	current, err := configureServeWorkspace()
	require.NoError(t, err)
	assert.Nil(t, current.promotionTarget)
}

// TestProjectSettingsRejectsWorkspaceAndRepositoryPaths proves management cannot
// publish builder-editable settings or release inputs, including physical aliases.
func TestProjectSettingsRejectsWorkspaceAndRepositoryPaths(t *testing.T) {
	for _, kind := range []string{"verification", "promotion"} {
		for _, location := range []string{"workspace file", "repository file", "symlink file", "workspace release", "repository release", "symlink release"} {
			if kind == "verification" && strings.HasSuffix(location, "release") {
				continue
			}
			t.Run(kind+"/"+location, func(t *testing.T) {
				isolateServeWorkspace(t)
				repo := newGitRepo(t)
				workspace := filepath.Join(repo, "services", "app")
				require.NoError(t, os.MkdirAll(workspace, 0o700))
				t.Chdir(workspace)
				outside := t.TempDir()
				path := filepath.Join(outside, "settings.json")
				release := filepath.Join(outside, "release")
				switch location {
				case "workspace file":
					path = filepath.Join(workspace, "settings.json")
				case "repository file":
					path = filepath.Join(repo, "settings.json")
				case "symlink file":
					actual := filepath.Join(workspace, "settings.json")
					require.NoError(t, os.Symlink(actual, path))
				case "workspace release":
					release = filepath.Join(workspace, "release")
				case "repository release":
					release = filepath.Join(repo, "release")
				case "symlink release":
					alias := filepath.Join(outside, "alias")
					require.NoError(t, os.Symlink(workspace, alias))
					release = filepath.Join(alias, "release")
				}
				content := `{"candidate_repo":"/c.git","candidate_module":".","binaries":{"app":"./cmd/app"},"suite_repo":"/s.git","suite_commit":"` + strings.Repeat("b", 40) + `","suite_module":".","suite_command":"./cmd/evals","timeout":"30m"}`
				if kind == "promotion" {
					content = `{"release_dir":"` + release + `","suite_id":"suite","suite_commit":"` + strings.Repeat("b", 40) + `","environment":"sim","binaries":["app"]}`
				}
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
				err := runProjectSettings(kind, []string{"set", path}, io.Discard)
				require.ErrorContains(t, err, "must be outside the workspace")
				state, err := discoverProject()
				require.NoError(t, err)
				assert.Empty(t, state.Record.ID, "rejected setup creates no project record")
				_, err = state.ReadSettings(kind)
				assert.ErrorIs(t, err, os.ErrNotExist)
			})
		}
	}
}

// TestProjectStartPreflightPrecedesState proves unavailable platform effects do not create a project.
func TestProjectStartPreflightPrecedesState(t *testing.T) {
	runtime := testProjectRuntime()
	runtime.preflight = func() error { return errors.New("install required platform program") }
	runtime.configRoot = func() (string, error) { t.Fatal("state reached before preflight"); return "", nil }
	assert.ErrorContains(t, runWithRuntime(nil, io.Discard, runtime), "install required platform program")
}

// TestProjectStartPortTakeoverRemovesOnlyFailedTerminal proves the actual bind owns
// availability and a post-check takeover never changes the saved project or port.
func TestProjectStartPortTakeoverRemovesOnlyFailedTerminal(t *testing.T) {
	isolateServeWorkspace(t)
	state, err := fixtureProjectState("")
	require.NoError(t, err)
	port := state.Record.Port
	identity := state.Record.ID
	runtime := testProjectRuntime()
	runtime.preflight = func() error { return nil }
	runtime.startTerminal = func(ctx context.Context, state *project.State) (string, error) {
		// attachOrStartProject's precheck already closed its socket. Another owner now takes it.
		listener, err := net.Listen("tcp", state.Address())
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, listener.Close()) })
		_, err = net.Listen("tcp", state.Address())
		require.Error(t, err, "the server's actual bind fails")
		return "owned-failed-terminal", nil
	}
	runtime.waitTerminal = func(context.Context, string) (bool, error) { return true, nil }
	removed := ""
	runtime.removeTerminal = func(ctx context.Context, id string) error { removed = id; return nil }
	err = runWithRuntime(nil, io.Discard, runtime)
	require.ErrorContains(t, err, "owned-failed-terminal")
	assert.Equal(t, "owned-failed-terminal", removed)
	after, err := discoverProject()
	require.NoError(t, err)
	assert.Equal(t, port, after.Record.Port)
	assert.Equal(t, identity, after.Record.ID)
	_, err = after.ReadRunning()
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestProjectStartShowsEndedServerOutput proves a server whose terminal ends while starting
// reports its own last lines, bounded and printable, read before its terminal is removed.
func TestProjectStartShowsEndedServerOutput(t *testing.T) {
	// numbered returns lines "line <n>", filled with dots to width bytes when width is set.
	numbered := func(first, last, width int) []string {
		var lines []string
		for index := first; index <= last; index++ {
			line := fmt.Sprintf("line %d", index)
			lines = append(lines, line+strings.Repeat(".", max(0, width-len(line))))
		}
		return lines
	}
	indent := func(lines []string) string { return "  " + strings.Join(lines, "\n  ") }
	const stopped = "the project server stopped while starting, and its last lines were:\n"
	const fallback = "the project server in Herdr terminal t7 did not start answering: "
	remedy := " run surla verification set <file>."
	for _, tc := range []struct {
		name    string
		output  string
		readErr error
		// want is the exact error; wantParts are required parts when the cause varies.
		want      string
		wantParts []string
	}{
		{
			name:   "final error",
			output: "surla: serving on http://127.0.0.1:1\nsurla: decode verification settings: unexpected end of JSON input;" + remedy + "\n\n   \n",
			want:   stopped + "  surla: serving on http://127.0.0.1:1\n  surla: decode verification settings: unexpected end of JSON input;" + remedy,
		},
		{
			name:   "escape sequences and control characters",
			output: "\x1b[1;31msurla:\x1b[0m bad\tsettings\r\n\x1b]0;title\x07\a\x00\x7f\u009b\u202eok\xff\x1b(B\n",
			want:   stopped + "  surla: bad settings\n  ok",
		},
		{
			name:   "many lines keep the end",
			output: strings.Join(numbered(1, 30, 0), "\n") + "\n",
			want:   stopped + indent(numbered(11, 30, 0)),
		},
		{
			name:   "many long lines keep whole lines at the end",
			output: strings.Join(numbered(1, 25, 250), "\n"),
			want:   stopped + indent(numbered(10, 25, 250)),
		},
		{
			name:   "one long line keeps its end",
			output: strings.Repeat("é", 3000) + remedy,
			want:   stopped + "  …" + strings.Repeat("é", 2030) + remedy,
		},
		{
			name:      "unreadable output",
			readErr:   errors.New("read Herdr terminal t7: exit status 1"),
			wantParts: []string{fallback, "\nthe project server's terminal output could not be read: read Herdr terminal t7: exit status 1"},
		},
		{
			name:      "no printable output",
			output:    "\x1b[0m\n \n\t\n",
			wantParts: []string{fallback, "\nthe project server's terminal output could not be read: it showed no printable text"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateServeWorkspace(t)
			runtime := testProjectRuntime()
			runtime.preflight = func() error { return nil }
			runtime.startTerminal = func(context.Context, *project.State) (string, error) { return "t7", nil }
			runtime.waitTerminal = func(context.Context, string) (bool, error) { return true, nil }
			var effects []string
			var readCtx context.Context
			runtime.readTerminal = func(ctx context.Context, id string) (string, error) {
				assert.NoError(t, ctx.Err(), "the read has its own cleanup deadline")
				readCtx = ctx
				effects = append(effects, "read "+id)
				return tc.output, tc.readErr
			}
			runtime.removeTerminal = func(ctx context.Context, id string) error {
				// A read that used up its deadline must not leave the removal without one:
				// the read's context has ended by now and the removal's has not.
				require.NotNil(t, readCtx, "the read comes first")
				assert.Error(t, readCtx.Err(), "the read's deadline ends with the read")
				assert.NoError(t, ctx.Err(), "the removal has its own deadline")
				effects = append(effects, "remove "+id)
				return nil
			}
			err := runWithRuntime(nil, io.Discard, runtime)
			require.Error(t, err)
			if tc.want != "" {
				assert.Equal(t, tc.want, err.Error())
				assert.Less(t, len(err.Error()), len(stopped)+startOutputBytes+startOutputLines*3, "shown lines stay within the bound")
			}
			for _, part := range tc.wantParts {
				assert.Contains(t, err.Error(), part)
			}
			assert.Equal(t, []string{"read t7", "remove t7"}, effects, "read before removal, exactly once")
		})
	}
}

// TestProjectStartLeavesLiveTerminalUnread proves a server that never answered but whose
// terminal is still running is neither read nor removed, and the error names its terminal.
func TestProjectStartLeavesLiveTerminalUnread(t *testing.T) {
	isolateServeWorkspace(t)
	runtime := testProjectRuntime()
	runtime.preflight = func() error { return nil }
	runtime.startTerminal = func(_ context.Context, state *project.State) (string, error) {
		// Published access with no listener makes the readiness probe fail at once.
		return "t7", state.SaveRunning(project.Running{ManagementCredential: strings.Repeat("a", 64)})
	}
	runtime.waitTerminal = func(ctx context.Context, _ string) (bool, error) { <-ctx.Done(); return false, nil }
	runtime.readTerminal = func(context.Context, string) (string, error) {
		t.Error("a live terminal must not be read")
		return "", nil
	}
	runtime.removeTerminal = func(context.Context, string) error {
		t.Error("a live terminal must stay for the user to look at")
		return nil
	}
	err := runWithRuntime(nil, io.Discard, runtime)
	require.ErrorContains(t, err, "the project server in Herdr terminal t7 did not start answering: ")
	assert.NotContains(t, err.Error(), "stopped while starting")
}

// TestProjectStartThatAnswersReadsNothing proves a normal start never reads its terminal.
func TestProjectStartThatAnswersReadsNothing(t *testing.T) {
	t.Chdir(t.TempDir())
	runtime := projectDispatchRuntime(t)
	runtime.readTerminal = func(context.Context, string) (string, error) {
		t.Error("an answering server's terminal must not be read")
		return "", nil
	}
	runtime.removeTerminal = func(context.Context, string) error {
		t.Error("an answering server's terminal must not be removed")
		return nil
	}
	var output bytes.Buffer
	require.NoError(t, runWithRuntime(nil, &output, runtime))
	assert.Contains(t, output.String(), "Surla is running for")
}

// TestReadProjectTerminalArguments runs an owned hc stand-in, never the real one, to pin
// the exact read arguments and how its reply and failures reach the start error.
func TestReadProjectTerminalArguments(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   string
		exit    int
		want    string
		wantErr string
	}{
		{name: "text", reply: `{"complete":true,"how":"history","text":"surla: first\nsurla: second"}`, want: "surla: first\nsurla: second"},
		{name: "failure", reply: "", exit: 3, wantErr: "read Herdr terminal t7: exit status 3"},
		{name: "not JSON", reply: "not JSON", wantErr: "decode Herdr terminal t7 output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			arguments := filepath.Join(bin, "arguments")
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nprintf '%%s' '%s'\nexit %d\n", arguments, tc.reply, tc.exit)
			hc := filepath.Join(bin, "hc")
			require.NoError(t, os.WriteFile(hc, []byte(script), 0o700))
			text, err := readProjectTerminal(t.Context(), "t7", func() (string, error) { return hc, nil })
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, text)
			}
			argv, err := os.ReadFile(arguments)
			require.NoError(t, err)
			assert.Equal(t, "read\n--json\n--lines\n20\nt7\n", string(argv))
		})
	}
}

// TestInternalServerTakesOnlyItsStateFolder keeps all configuration in its owning workflow:
// serve's one input is the state folder its launcher found, and every other option is refused.
func TestInternalServerTakesOnlyItsStateFolder(t *testing.T) {
	stateDir, err := parseServeArgs([]string{"-state", "/owned/project-state"})
	require.NoError(t, err)
	assert.Equal(t, "/owned/project-state", stateDir)
	for _, args := range [][]string{nil, {"-state", ""}} {
		_, err := parseServeArgs(args)
		assert.ErrorContains(t, err, "serve requires -state; start the project with surla", args)
	}
	for _, option := range []string{"-model-provider", "-model-name", "-model-gateway-addr", "-check-timeout", "-principal", "-token", "-addr", "-cards-dir", "-project", "-verification-config", "-promotion-config"} {
		_, err := parseServeArgs([]string{"-state", "/owned/project-state", option, "value"})
		assert.ErrorContains(t, err, "flag provided but not defined", option)
	}
	_, err = parseServeArgs([]string{"-state", "/owned/project-state", "directory"})
	assert.ErrorContains(t, err, "unexpected arguments")
}

// TestServeReadsOnlyItsStateFolder starts the real serve entry from a working directory that
// belongs to no project, with a runtime whose configuration folder must never be asked for:
// the server publishes its management record in exactly the folder named by -state.
func TestServeReadsOnlyItsStateFolder(t *testing.T) {
	isolateServeWorkspace(t)
	state, err := fixtureProjectState("")
	require.NoError(t, err)
	t.Chdir(t.TempDir())
	runtime := testProjectRuntime()
	// The shared team fixture keeps its rosters under the configuration folder, so this
	// test binds its own rosters folder and leaves the configuration folder unreachable.
	rosters := t.TempDir()
	runtime.teamDependencies = func(ctx context.Context) (project.TeamDependencies, error) {
		return project.TeamDependencies{Lifetime: ctx, Host: &fixtureTeamHost{}, RosterDirectory: rosters, Shell: "/bin/zsh"}, nil
	}
	runtime.configRoot = func() (string, error) {
		t.Error("serve must not resolve a configuration folder")
		return "", errors.New("configuration folder is not available to serve")
	}
	done := make(chan error, 1)
	go func() { done <- runWithRuntime([]string{"serve", "-state", state.Dir}, io.Discard, runtime) }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	running, err := waitForRunning(ctx, state)
	require.NoError(t, err)
	assert.Len(t, running.ManagementCredential, 64)
	require.NoError(t, projectClient(state).Stop(ctx))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("serve did not stop")
	}
}

// TestMissingBrowserOpenerFailsBeforeProjectCreation checks the production preflight
// with an empty owned executable search path, without invoking any program.
func TestMissingBrowserOpenerFailsBeforeProjectCreation(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	runtime := productionProjectRuntime()
	runtime.preflight = func() error { return preflightProjectStart(func() (string, error) { return "owned-hc-stand-in", nil }) }
	runtime.configRoot = func() (string, error) { t.Fatal("state must remain untouched"); return "", nil }
	var output bytes.Buffer
	err := runWithRuntime(nil, &output, runtime)
	opener := "xdg-open"
	if runtimepkg.GOOS == "darwin" {
		opener = "open"
	}
	require.ErrorContains(t, err, opener+" is required to open Surla views")
	assert.Empty(t, output.String(), "never print a browser link on setup failure")
}

// TestReadinessStopsOnAnyReceivedHTTPResponse proves credentials are never resent
// after a response, including redirects, refusals, and malformed identity bodies.
func TestReadinessStopsOnAnyReceivedHTTPResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"wrong project", http.StatusOK, `{"project_id":"another-project","directory":"/another","owner":"another"}`},
		{"unauthorized", http.StatusUnauthorized, `{"message":"refused"}`},
		{"not found", http.StatusNotFound, "not found"},
		{"non JSON", http.StatusOK, "not JSON"},
		{"redirect", http.StatusFound, "redirect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateServeWorkspace(t)
			state, err := fixtureProjectState("")
			require.NoError(t, err)
			var probes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probes.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(tc.status)
				_, err := io.WriteString(w, tc.body)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			state.Record.Port = server.Listener.Addr().(*net.TCPAddr).Port
			require.NoError(t, state.SaveRunning(project.Running{ManagementCredential: strings.Repeat("a", 64)}))
			client := projectClient(state)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, err = confirmProject(ctx, state, client)
			require.Error(t, err)
			assert.EqualValues(t, 1, probes.Load(), "no redirect or retry may resend the credential")
			assert.NoError(t, ctx.Err(), "a reply ends polling before the deadline")
		})
	}
}

// TestReadinessDoesNotResendAfterConnectionFailure bounds credential transmission.
func TestReadinessDoesNotResendAfterConnectionFailure(t *testing.T) {
	isolateServeWorkspace(t)
	state, err := fixtureProjectState("")
	require.NoError(t, err)
	probes := 0
	client := &genproject.Client{IdentityEndpoint: func(context.Context, any) (any, error) {
		probes++
		if probes == 1 {
			return nil, goahttp.ErrRequestError("project", "identity", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED})
		}
		return &genproject.ProjectIdentity{ProjectID: state.Record.ID, Directory: state.Record.Directory, Owner: state.Record.Owner}, nil
	}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err = confirmProject(ctx, state, client)
	require.Error(t, err)
	assert.Equal(t, 1, probes)
}

// TestRuntimeRootsAreIndependent verifies local work and project management
// retain each runtime's injected root even when the test environment changes.
func TestRuntimeRootsAreIndependent(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	firstRoot, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	secondRoot, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runtimes := []projectRuntime{testProjectRuntimeAt(firstRoot), testProjectRuntimeAt(secondRoot)}
	t.Setenv("SURLA_TEST_USER_CONFIG_DIR", t.TempDir())
	input := filepath.Join(t.TempDir(), "verification.json")
	content := `{"candidate_repo":"/candidate.git","candidate_module":".","binaries":{"app":"./cmd/app"},"suite_repo":"/suite.git","suite_commit":"` + strings.Repeat("b", 40) + `","suite_module":".","suite_command":"./cmd/evals","timeout":"30m"}`
	require.NoError(t, os.WriteFile(input, []byte(content), 0o600))
	var identities []string
	for index, runtime := range runtimes {
		expected := []string{firstRoot, secondRoot}[index]
		evaluation, err := runtime.localEvalWorkDir(workspace)
		require.NoError(t, err)
		staging, err := runtime.localInitWorkDir(workspace)
		require.NoError(t, err)
		for _, work := range []string{evaluation, staging} {
			assert.True(t, strings.HasPrefix(work, expected+string(os.PathSeparator)), work)
		}
		require.NoError(t, runtime.settings("verification", []string{"set", input}, io.Discard))
		state, err := runtime.discover()
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(state.Dir, expected+string(os.PathSeparator)))
		assert.NotEmpty(t, state.Record.ID)
		identities = append(identities, state.Record.ID)
		raw, err := state.ReadSettings("verification")
		require.NoError(t, err)
		assert.Contains(t, string(raw), "/candidate.git")
	}
	assert.NotEqual(t, identities[0], identities[1])
}
