package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectStartSerializesAndRecovers proves actual writer-lock and authenticated HTTP attach decisions.
func TestProjectStartSerializesAndRecovers(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	var starts atomic.Int32
	var server *http.Server
	var cp *controlplane.ControlPlane
	var active *project.State
	launch := func(ctx context.Context, state *project.State) error {
		starts.Add(1)
		var err error
		cp, err = controlplane.New(controlplane.Options{CardsDir: state.CardsDir()})
		if err != nil {
			return err
		}
		management := project.NewServer(state, func() {})
		ln, err := net.Listen("tcp", state.Address())
		if err != nil {
			return err
		}
		active = state
		if err := state.SaveRunning(project.Running{PID: 123, Terminal: "test"}); err != nil {
			return err
		}
		server = &http.Server{Handler: management.Handler(controlplane.NewHTTPHandler(cp, management.Authenticate)), ReadHeaderTimeout: time.Second}
		go func() {
			if err := server.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				assert.NoError(t, err)
			}
		}()
		return nil
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
			_, _, err = attachOrStartProject(ctx, state, func(context.Context, *project.State) error { called = true; return nil })
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
	before, err := configureServeWorkspace(serveConfig{})
	require.NoError(t, err)
	require.NotNil(t, before.verification)
	assert.Equal(t, "/candidate.git", before.verification.Plan.CandidateRepo)
	assertOutsideDirectory(t, before.verification.RunsDir, dir)
	require.NoError(t, os.WriteFile(file, []byte(`{"runs_dir":"/unowned"}`), 0o600))
	assert.Error(t, runProjectSettings("verification", []string{"set", file}, &discardOutput{}))
	after, err := configureServeWorkspace(serveConfig{})
	require.NoError(t, err)
	require.NotNil(t, after.verification)
	assert.Equal(t, before.verification.Plan, after.verification.Plan)
	require.NoError(t, runProjectSettings("verification", []string{"remove"}, &discardOutput{}))
	current, err := configureServeWorkspace(serveConfig{})
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
		config, err := configureServeWorkspace(serveConfig{})
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
	current, err := configureServeWorkspace(serveConfig{})
	require.NoError(t, err)
	assert.Nil(t, current.promotionTarget)
}
