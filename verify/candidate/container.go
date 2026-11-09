package candidate

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type (
	// ContainerRunner runs Go commands against candidate source in Linux
	// containers on the local Docker engine, using the docker command-line
	// tool with direct argv and no shell. check_project and
	// update_dependencies use it; it is the only way Surla runs candidate tests
	// in a container. A ContainerRunner holds no engine state: each operation
	// calls Open, so a docker installed or started after the server started
	// works without a restart.
	ContainerRunner struct {
		// lookPath finds the docker executable; tests replace it.
		lookPath func(string) (string, error)
		// command runs one docker invocation; tests replace it with a
		// scripted runner.
		command commandFunc
	}

	// Engine is one operation's resolved Docker engine: the docker executable,
	// the local endpoint every command of the operation targets with --host,
	// and the operation's private, empty Docker configuration directory. The
	// empty configuration keeps the CLI from injecting the operator's proxy
	// variables or registry credentials into containers.
	Engine struct {
		// runner is the ContainerRunner that resolved this engine.
		runner *ContainerRunner
		// dockerPath is the absolute path of the docker executable.
		dockerPath string
		// host is the resolved local endpoint, such as unix:///var/run/docker.sock.
		host string
		// configDir is the operation-owned empty DOCKER_CONFIG directory.
		configDir string
	}

	// Mount is one bind mount of a host directory into a container.
	Mount struct {
		// Source is the absolute host directory.
		Source string
		// Target is the absolute path inside the container.
		Target string
		// ReadOnly mounts the directory read-only.
		ReadOnly bool
	}

	// ContainerSpec describes one container that runs the Go command.
	ContainerSpec struct {
		// Name is the operation-owned container name, chosen before create so
		// a lost create reply can still be removed by name.
		Name string
		// Operation is the owning operation ID, recorded as the
		// surla.check label.
		Operation string
		// Network is "none" for running candidate tests, or "bridge" for
		// module downloads.
		Network string
		// User is the "uid:gid" the container runs as.
		User string
		// WorkDir is the working directory inside the container.
		WorkDir string
		// Mounts are the bind mounts.
		Mounts []Mount
		// Env is the complete container environment; nothing else is passed.
		Env []string
		// Args are the arguments to the go command, such as ["test", "./..."].
		Args []string
	}

	// CleanupError reports a container whose removal could not be confirmed.
	// The caller must keep every directory the container mounts until a
	// later removal succeeds.
	CleanupError struct {
		// Name is the container name to remove with docker rm -f.
		Name string
		// Err is the removal failure.
		Err error
	}

	// commandFunc runs docker with args and env, writing its output to stdout
	// and stderr. It returns the process's exit error, if any.
	commandFunc func(ctx context.Context, docker string, args, env []string, stdout, stderr io.Writer) error
)

const (
	// CheckImage is the official Go image every check container runs, pinned
	// by its multi-arch index digest. Containers run with GOTOOLCHAIN=auto, so
	// Go itself selects and installs any newer toolchain a module requires.
	CheckImage = "golang:1.27.3@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e"
	// checkLabel labels every check container with its operation ID.
	checkLabel = "surla.check"
	// removeTimeout bounds one docker rm -f, run with a fresh context so it
	// survives the cancellation that triggered it.
	removeTimeout = 30 * time.Second
	// pidsLimit bounds the processes one check container may start.
	pidsLimit = "4096"
	// goEntrypoint is the Go command inside CheckImage.
	goEntrypoint = "/usr/local/go/bin/go"
)

// ErrEngineUnavailable reports that no usable local Docker engine was
// reached: docker is not installed, the engine does not answer, the
// endpoint is not local, or the check image cannot be pulled.
var ErrEngineUnavailable = errors.New("local Docker engine unavailable")

// NewContainerRunner returns a runner that uses the docker executable found
// on PATH at each Open.
func NewContainerRunner() *ContainerRunner {
	return &ContainerRunner{lookPath: exec.LookPath, command: runDocker}
}

// Open resolves the docker executable and the local engine endpoint for one
// operation, using the operator's Docker configuration only for this
// resolution. Every later command uses configDir, which the caller creates
// empty and owns. It fails with ErrEngineUnavailable when docker is missing,
// the endpoint is not a local unix socket (a remote engine resolves bind
// mounts on its own machine), or the engine does not answer.
func (r *ContainerRunner) Open(ctx context.Context, configDir string) (*Engine, error) {
	docker, err := r.lookPath("docker")
	if err != nil {
		return nil, fmt.Errorf("%w: docker is not installed or not on PATH for surla serve: %v", ErrEngineUnavailable, err)
	}
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		var out bytes.Buffer
		args := []string{"context", "inspect", "--format", "{{.Endpoints.docker.Host}}"}
		if err := r.command(ctx, docker, args, operatorEnv(), &out, io.Discard); err != nil {
			return nil, fmt.Errorf("%w: resolve the active Docker context: %v", ErrEngineUnavailable, err)
		}
		host = strings.TrimSpace(out.String())
	}
	if !strings.HasPrefix(host, "unix://") {
		return nil, fmt.Errorf("%w: check_project needs a local Docker engine, but the endpoint is %q", ErrEngineUnavailable, host)
	}
	e := r.Reopen(docker, host, configDir)
	var out bytes.Buffer
	if err := e.docker(ctx, &out, io.Discard, "version", "--format", "{{.Server.Os}}"); err != nil {
		return nil, fmt.Errorf("%w: the Docker engine at %s does not answer; start Docker: %v", ErrEngineUnavailable, host, err)
	}
	if engineOS := strings.TrimSpace(out.String()); engineOS != "linux" {
		return nil, fmt.Errorf("%w: the Docker engine at %s runs %q containers, not linux", ErrEngineUnavailable, host, engineOS)
	}
	return e, nil
}

// Reopen returns the engine for a recorded docker path and endpoint, such as
// the ones an earlier operation saved for retrying a container removal.
func (r *ContainerRunner) Reopen(docker, host, configDir string) *Engine {
	return &Engine{runner: r, dockerPath: docker, host: host, configDir: configDir}
}

// Docker returns the docker executable path this engine uses.
func (e *Engine) Docker() string {
	return e.dockerPath
}

// Host returns the resolved endpoint every command of the operation targets.
func (e *Engine) Host() string {
	return e.host
}

// EnsureImage makes CheckImage available on the engine, pulling it by digest
// only when it is missing. A failed pull is ErrEngineUnavailable.
func (e *Engine) EnsureImage(ctx context.Context) error {
	if err := e.docker(ctx, io.Discard, io.Discard, "image", "inspect", CheckImage); err == nil {
		return nil
	}
	var stderr bytes.Buffer
	if err := e.docker(ctx, io.Discard, &stderr, "pull", CheckImage); err != nil {
		return fmt.Errorf("%w: pull %s: %v: %s", ErrEngineUnavailable, CheckImage, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Run creates the container spec describes, streams its combined output to
// out from the first byte, waits for it, and removes it. It returns the
// container's exit code from docker wait. Removal always runs, with a fresh
// bounded context; when it cannot be confirmed, Run returns a *CleanupError
// and the caller must keep the container's mounted directories.
func (e *Engine) Run(ctx context.Context, spec ContainerSpec, out io.Writer) (exitCode int, err error) {
	defer func() {
		if rmErr := e.Remove(spec.Name); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
	}()
	create, err := e.createArgs(spec)
	if err != nil {
		return -1, err
	}
	var stderr bytes.Buffer
	if err := e.docker(ctx, io.Discard, &stderr, create...); err != nil {
		return -1, fmt.Errorf("create container %s: %w: %s", spec.Name, err, strings.TrimSpace(stderr.String()))
	}
	// docker start --attach streams the output. When the container exits
	// non-zero, start exits with the same status; any other start failure
	// means the stream, and so the captured evidence, is incomplete.
	startErr := e.docker(ctx, out, out, "start", "--attach", spec.Name)
	if startErr != nil && ctx.Err() != nil {
		return -1, fmt.Errorf("run container %s: %w", spec.Name, ctx.Err())
	}
	var waitOut bytes.Buffer
	stderr.Reset()
	if err := e.docker(ctx, &waitOut, &stderr, "wait", spec.Name); err != nil {
		return -1, fmt.Errorf("wait for container %s: %w: %s", spec.Name, err, strings.TrimSpace(stderr.String()))
	}
	code, err := strconv.Atoi(strings.TrimSpace(waitOut.String()))
	if err != nil {
		return -1, fmt.Errorf("container %s exit status %q: %w", spec.Name, waitOut.String(), err)
	}
	if startErr != nil && !mirrorsExit(startErr, code) {
		return -1, fmt.Errorf("stream the output of container %s, which exited %d: %w", spec.Name, code, startErr)
	}
	return code, nil
}

// Remove force-removes the named container with a fresh context bounded by
// removeTimeout. A container that does not exist counts as removed. Any
// other failure is a *CleanupError.
func (e *Engine) Remove(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()
	var stderr bytes.Buffer
	err := e.docker(ctx, io.Discard, &stderr, "rm", "--force", name)
	if err == nil || strings.Contains(stderr.String(), "No such container") {
		return nil
	}
	return &CleanupError{Name: name, Err: fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))}
}

// Error implements error.
func (e *CleanupError) Error() string {
	return fmt.Sprintf("container %s may still exist; remove it with docker rm -f %s: %v", e.Name, e.Name, e.Err)
}

// Unwrap returns the removal failure.
func (e *CleanupError) Unwrap() error {
	return e.Err
}

// operatorEnv is the environment for resolving the operator's Docker
// context: the server's own environment, which holds DOCKER_CONFIG and
// DOCKER_CONTEXT when the operator set them.
func operatorEnv() []string {
	return os.Environ()
}

// runDocker is the production commandFunc.
func runDocker(ctx context.Context, docker string, args, env []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, docker, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

// createArgs builds the docker create arguments for spec. Every container
// is read-only with no capabilities, no privilege escalation, a process
// limit, no Docker-kept logs, and only spec's environment and mounts.
func (e *Engine) createArgs(spec ContainerSpec) ([]string, error) {
	args := []string{
		"create",
		"--name", spec.Name,
		"--label", checkLabel + "=" + spec.Operation,
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", pidsLimit,
		"--log-driver", "none",
		"--network", spec.Network,
		"--user", spec.User,
		"--entrypoint", goEntrypoint,
		"--workdir", spec.WorkDir,
	}
	for _, m := range spec.Mounts {
		opt, err := mountOption(m)
		if err != nil {
			return nil, err
		}
		args = append(args, "--mount", opt)
	}
	for _, kv := range spec.Env {
		args = append(args, "--env", kv)
	}
	args = append(args, CheckImage)
	return append(args, spec.Args...), nil
}

// mirrorsExit reports whether err is a docker start --attach exit that only
// repeats the container's non-zero exit code.
func mirrorsExit(err error, code int) bool {
	var exitErr *exec.ExitError
	return code != 0 && errors.As(err, &exitErr) && exitErr.ExitCode() == code
}

// mountOption encodes m as a docker --mount value. Docker splits the value
// as one CSV record, so each field is CSV-encoded and a path containing a
// comma or quote stays one field.
func mountOption(m Mount) (string, error) {
	fields := []string{"type=bind", "source=" + m.Source, "target=" + m.Target}
	if m.ReadOnly {
		fields = append(fields, "readonly")
	}
	var b strings.Builder
	if err := csv.NewWriter(&b).WriteAll([][]string{fields}); err != nil {
		return "", fmt.Errorf("encode mount %s: %w", m.Source, err)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// docker runs one command against this engine's endpoint with only the
// operation's empty configuration directory and the server's PATH.
func (e *Engine) docker(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
	full := append([]string{"--host", e.host}, args...)
	env := []string{"DOCKER_CONFIG=" + e.configDir, "PATH=" + os.Getenv("PATH")}
	return e.runner.command(ctx, e.dockerPath, full, env, stdout, stderr)
}
