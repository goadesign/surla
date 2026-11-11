//go:build e2e && docker

// This file checks, against the local Docker engine, the engine behavior the
// check helper's cleanup relies on: a listing filtered by the surla.check
// label shows a created and an exited container until they are removed, a
// container created with createArgs has restart policy "no", and
// rm --force exits 0 for a container that does not exist, so its exit
// status cannot prove absence. It goes through Engine's own docker method
// and createArgs against the default endpoint, creates exactly two
// containers with no mounts and no network, fails instead of pulling when
// CheckImage is missing, and removes only the names it created.
package candidate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type (
	// factsProbe owns one run of TestLocalEngineContainerLifecycleFacts: the
	// resolved engine, the run's random label value and the exact names the
	// run may create and remove.
	factsProbe struct {
		// t is the running test.
		t *testing.T
		// ctx bounds the whole run by containerCommandTimeout.
		ctx context.Context
		// engine is the default local engine resolved by Open.
		engine *Engine
		// id is the run's random 32-hex surla.check label value.
		id string
		// names are the only container names this run creates or removes.
		names []string
	}

	// dockerResult is one docker invocation's recorded outcome.
	dockerResult struct {
		// code is the process exit code, or -1 when the command did not exit
		// on its own (killed at its deadline or never started).
		code int
		// stdout and stderr are the command's complete output.
		stdout, stderr string
		// err is the error runDocker returned.
		err error
	}

	// listedContainer is one row of a label-filtered container listing.
	listedContainer struct {
		// name is the container name without a leading slash.
		name string
		// state is the engine's one-word state, such as created or exited.
		state string
	}
)

// TestLocalEngineContainerLifecycleFacts checks, on the default local
// engine, that a label-filtered `container ls --all` shows a created container
// right after create exits 0 and an exited container after it ran, and
// neither once removed; that a created container has restart policy "no";
// and that `rm --force` exits 0 both for a container that exists and for one
// that does not. It stops at the first unexpected state. A missing CheckImage
// is a failure: the test never pulls.
func TestLocalEngineContainerLifecycleFacts(t *testing.T) {
	require.Empty(t, os.Getenv("DOCKER_HOST"), "this proof uses the default Docker context; unset DOCKER_HOST")
	ctx, cancel := context.WithTimeout(t.Context(), containerCommandTimeout)
	defer cancel()
	p := newFactsProbe(t, ctx)

	image := p.call("image", "inspect", "--format", "{{.Id}}", CheckImage)
	require.Zero(t, image.code, "CheckImage is not present on the engine; this proof never pulls")
	require.Empty(t, p.list(), "no container may carry this run's label before it starts")

	// 1. A created container is listed at once, in state created, with
	// restart policy "no".
	created := p.create(0)
	require.Zero(t, created.code, "create %s", p.names[0])
	require.Equal(t, []listedContainer{{name: p.names[0], state: "created"}}, p.list())
	inspect := p.call("container", "inspect", "--format", "{{.State.Status}} {{.HostConfig.RestartPolicy.Name}}", p.names[0])
	require.Zero(t, inspect.code)
	require.Equal(t, "created no", strings.TrimSpace(inspect.stdout))

	// 2. Removing it, then removing the same name again once it is gone.
	removed := p.call("rm", "--force", p.names[0])
	require.Zero(t, removed.code, "rm --force of an existing container")
	require.Empty(t, p.list(), "the removed container must no longer be listed")
	missing := p.call("rm", "--force", p.names[0])
	require.Zero(t, missing.code, "rm --force of a missing container exits 0, so its exit status cannot prove absence")

	// 3. A container that ran and exited is listed in state exited.
	require.Zero(t, p.create(1).code, "create %s", p.names[1])
	require.Zero(t, p.call("start", p.names[1]).code, "start %s", p.names[1])
	waited := p.call("wait", p.names[1])
	require.Zero(t, waited.code, "wait %s", p.names[1])
	require.Equal(t, "0", strings.TrimSpace(waited.stdout), "go version exit status")
	require.Equal(t, []listedContainer{{name: p.names[1], state: "exited"}}, p.list())
	require.Zero(t, p.call("rm", "--force", p.names[1]).code, "rm --force of an exited container")
	require.Empty(t, p.list(), "the removed container must no longer be listed")
}

// newFactsProbe resolves the default engine and registers the cleanup that
// removes only this run's exact names and then fails on any container still
// carrying the run's label.
func newFactsProbe(t *testing.T, ctx context.Context) *factsProbe {
	t.Helper()
	engine, err := NewContainerRunner().Open(ctx, containerTempDir(t))
	require.NoError(t, err, "resolve the default local engine")
	var raw [16]byte
	_, err = rand.Read(raw[:])
	require.NoError(t, err)
	id := hex.EncodeToString(raw[:])
	p := &factsProbe{t: t, ctx: ctx, engine: engine, id: id}
	for n := range 2 {
		p.names = append(p.names, fmt.Sprintf("surla-check-%s-%d", id, n))
	}
	t.Logf("endpoint %s; label %s=%s; names %v", engine.Host(), checkLabel, id, p.names)
	t.Cleanup(p.cleanup)
	return p
}

// create creates container n of this run without starting it: no mounts, no
// network, the image's go command with "version" as its only argument.
func (p *factsProbe) create(n int) dockerResult {
	args, err := p.engine.createArgs(ContainerSpec{
		Name:      p.names[n],
		Operation: p.id,
		Network:   "none",
		User:      fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		WorkDir:   "/",
		Env:       []string{"PATH=/usr/local/go/bin:/usr/bin:/bin", "GOTOOLCHAIN=local", "GOTELEMETRY=off"},
		Args:      []string{"version"},
	})
	require.NoError(p.t, err)
	return p.call(args...)
}

// list returns the containers carrying this run's label, in every state,
// sorted by name. A failed listing stops the test.
func (p *factsProbe) list() []listedContainer {
	r := p.call("container", "ls", "--all", "--filter", "label="+checkLabel+"="+p.id, "--format", "{{.Names}}\t{{.State}}")
	require.Zero(p.t, r.code, "list containers labelled %s=%s", checkLabel, p.id)
	return parseListing(r.stdout)
}

// call runs one docker command against the engine, bounded by removeTimeout
// within the run's deadline, and logs its exit code and complete output.
func (p *factsProbe) call(args ...string) dockerResult {
	ctx, cancel := context.WithTimeout(p.ctx, removeTimeout)
	defer cancel()
	r := runRecorded(ctx, p.engine, args)
	p.t.Logf("docker %s: exit=%d stdout=%q stderr=%q err=%v", strings.Join(args, " "), r.code, r.stdout, r.stderr, r.err)
	return r
}

// cleanup removes, with fresh bounded contexts, only those of this run's
// exact names that a listing by this run's label still shows, then lists
// the label again and fails naming every container that remains.
func (p *factsProbe) cleanup() {
	listing := func() ([]listedContainer, error) {
		ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
		defer cancel()
		r := runRecorded(ctx, p.engine, []string{"container", "ls", "--all", "--filter", "label=" + checkLabel + "=" + p.id, "--format", "{{.Names}}\t{{.State}}"})
		if r.code != 0 {
			return nil, fmt.Errorf("list exit %d: %v: %s", r.code, r.err, r.stderr)
		}
		return parseListing(r.stdout), nil
	}
	before, err := listing()
	if err != nil {
		p.t.Errorf("cleanup could not list label %s=%s; check containers %v by hand: %v", checkLabel, p.id, p.names, err)
		return
	}
	for _, c := range before {
		if !slices.Contains(p.names, c.name) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
		r := runRecorded(ctx, p.engine, []string{"rm", "--force", c.name})
		cancel()
		p.t.Logf("cleanup rm --force %s: exit=%d stdout=%q stderr=%q", c.name, r.code, r.stdout, r.stderr)
	}
	after, err := listing()
	switch {
	case err != nil:
		p.t.Errorf("cleanup could not confirm label %s=%s is empty; check containers %v by hand: %v", checkLabel, p.id, p.names, err)
	case len(after) > 0:
		p.t.Errorf("LEFTOVER containers labelled %s=%s on %s: %v", checkLabel, p.id, p.engine.Host(), after)
	default:
		p.t.Logf("cleanup confirmed no container labelled %s=%s remains", checkLabel, p.id)
	}
}

// runRecorded runs one docker command and returns its exit code and output.
func runRecorded(ctx context.Context, e *Engine, args []string) dockerResult {
	var stdout, stderr bytes.Buffer
	err := e.docker(ctx, &stdout, &stderr, args...)
	r := dockerResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		r.code = exitErr.ExitCode()
	default:
		r.code = -1
	}
	return r
}

// parseListing parses `{{.Names}}\t{{.State}}` rows, sorted by name.
func parseListing(out string) []listedContainer {
	var rows []listedContainer
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		name, state, _ := strings.Cut(line, "\t")
		rows = append(rows, listedContainer{name: name, state: state})
	}
	slices.SortFunc(rows, func(a, b listedContainer) int { return strings.Compare(a.name, b.name) })
	return rows
}
