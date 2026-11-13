package main

import (
	"bytes"
	"context"
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

// TestInternalServerAcceptsNoCallerOptions keeps all configuration in its owning workflow.
func TestInternalServerAcceptsNoCallerOptions(t *testing.T) {
	require.NoError(t, parseProjectArgs("serve", nil))
	for _, option := range []string{"-model-provider", "-model-name", "-model-gateway-addr", "-check-timeout", "-principal", "-token", "-addr", "-cards-dir", "-project", "-verification-config", "-promotion-config"} {
		assert.ErrorContains(t, parseProjectArgs("serve", []string{option, "value"}), "flag provided but not defined", option)
	}
	assert.ErrorContains(t, parseProjectArgs("serve", []string{"directory"}), "unexpected arguments")
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
			require.Error(t, confirmProject(ctx, state, client))
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
	require.Error(t, confirmProject(ctx, state, client))
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
