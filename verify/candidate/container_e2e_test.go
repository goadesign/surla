//go:build e2e && docker

// These tests run small Go fixtures through the real Docker runner. They need
// a local Linux Docker engine and the e2e,docker tags; no fixture is built or
// executed on the host.
package candidate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type (
	// containerFixture owns one operation's source, cache, writable scratch,
	// captured logs and unique container names. Runs are sequential.
	containerFixture struct {
		// engine is the local engine resolved before any fixture starts.
		engine *Engine
		// root holds every fixture file under t.TempDir.
		root string
		// source is the only candidate source directory mounted in the engine.
		source string
		// id is the fixture's unique surla.check label.
		id string
		// next assigns a distinct name and log to each command.
		next int
		// toolchain selects the Go launcher policy for this fixture.
		toolchain string
	}

	// containerRun is the completed runner result delivered after cancellation.
	containerRun struct {
		// code is the container exit code when a completed command supplied one.
		code int
		// err reports engine, cancellation or cleanup failure.
		err error
	}
)

const (
	// containerCommandTimeout bounds one real Go command; removal has its
	// own fresh removeTimeout deadline after cancellation.
	containerCommandTimeout = 4 * time.Minute
	// containerSource is the private source mount inside the fixture container.
	containerSource = "/src"
)

// containerCGoSource needs the image's C compiler during the offline test phase.
const containerCGoSource = `package containerproof
/* static int proof_value(void) { return 7; } */
import "C"
func Value() int { return int(C.proof_value()) }
`

// containerBehaviorTests deliberately exercise only the assigned container
// filesystem, subprocesses and network namespace, plus one failing/hanging test
// selected separately by the host driver.
const containerBehaviorTests = `package containerproof
import (
 "net"
 "os"
 "os/exec"
 "syscall"
 "errors"
 "testing"
 "time"
)
func TestBehavior(t *testing.T) {
 if Value() != 7 { t.Fatal("C compiler did not produce the expected value") }
 if err := os.WriteFile("test-artifact.txt", []byte("written inside copied source"), 0600); err != nil { t.Fatal(err) }
 if err := os.WriteFile("/tmp/surla-readonly-root", []byte("forbidden"), 0600); !errors.Is(err, syscall.EROFS) { t.Fatalf("root write returned %v, want EROFS", err) }
 if err := os.WriteFile("/gomodcache/surla-readonly-cache", []byte("forbidden"), 0600); !errors.Is(err, syscall.EROFS) { t.Fatalf("module cache write returned %v, want EROFS", err) }
 if _, err := os.ReadFile(os.Getenv("SURLA_E2E_PROTECTED_FILE")); err == nil { t.Fatal("read the unmounted operator file") }
 for _, name := range []string{"SURLA_E2E_HOST_SECRET", "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "FTP_PROXY", "ftp_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
  if os.Getenv(name) != "" { t.Fatalf("unexpected inherited variable %s", name) }
 }
 interfaces, err := net.Interfaces(); if err != nil { t.Fatal(err) }
 for _, iface := range interfaces { if iface.Flags & net.FlagUp != 0 && iface.Flags & net.FlagLoopback == 0 { t.Fatalf("non-loopback network interface %s is up", iface.Name) } }
 listener, err := net.Listen("tcp", "127.0.0.1:0"); if err != nil { t.Fatal(err) }
 defer listener.Close()
 connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second); if err != nil { t.Fatal(err) }
 if err := connection.Close(); err != nil { t.Fatal(err) }
 out, err := exec.Command("/bin/sh", "-c", "printf shell-ok").CombinedOutput(); if err != nil || string(out) != "shell-ok" { t.Fatalf("shell: %q, %v", out, err) }
}
func TestFailure(t *testing.T) { t.Fatal("intentional container failure") }
func TestHang(t *testing.T) { time.Sleep(2*time.Minute) }
`

// TestContainerEngineEndToEnd verifies real execution and resource cleanup.
// A missing engine or image is a setup failure, never a skipped proof.
func TestContainerEngineEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	t.Setenv("SURLA_E2E_HOST_SECRET", "must-not-reach-container")
	engine, err := NewContainerRunner().Open(ctx, containerTempDir(t))
	require.NoError(t, err, "start a local Linux Docker engine before running the e2e tag")
	t.Logf("resolved local engine %s; ensuring pinned Go image", engine.Host())
	require.NoError(t, engine.EnsureImage(ctx))

	basic := newContainerFixture(t, engine, map[string]string{
		"go.mod":        "module example.com/containerproof\n\ngo 1.27.3\n",
		"value.go":      containerCGoSource,
		"proof_test.go": containerBehaviorTests,
	})
	t.Run("networked_readonly_preparation", func(t *testing.T) {
		code, output := basic.run(t, ctx, ".", "bridge", true, "list", "-deps", "-test", "./...")
		assert.Zero(t, code, output)
	})
	t.Run("cgo_fixture_loopback_shell_and_isolation", func(t *testing.T) {
		code, output := basic.run(t, ctx, ".", "none", false, "test", "-v", "-run", "^TestBehavior$", "./...")
		assert.Zero(t, code, output)
		assert.Contains(t, output, "PASS: TestBehavior")
		written, err := os.ReadFile(filepath.Join(basic.source, "test-artifact.txt"))
		require.NoError(t, err)
		assert.Equal(t, "written inside copied source", string(written))
	})
	t.Run("test_failure_is_an_exit_outcome", func(t *testing.T) {
		code, output := basic.run(t, ctx, ".", "none", false, "test", "-run", "^TestFailure$", "./...")
		assert.Equal(t, 1, code, output)
		assert.Contains(t, output, "intentional container failure")
	})
	t.Run("cancellation_removes_the_running_container", func(t *testing.T) {
		basic.cancelRunning(t, ctx)
	})
	t.Run("vendor_uses_go_defaults", func(t *testing.T) {
		fixture := newContainerFixture(t, engine, map[string]string{
			"go.mod":                          "module example.com/vendorproof\n\ngo 1.27.3\n\nrequire example.com/dep v1.0.0\n",
			"proof_test.go":                   "package vendorproof\nimport (\"testing\"; \"example.com/dep\")\nfunc TestVendor(t *testing.T) { if dep.Value != 9 { t.Fatal(dep.Value) } }\n",
			"vendor/modules.txt":              "# example.com/dep v1.0.0\n## explicit; go 1.27.3\nexample.com/dep\n",
			"vendor/example.com/dep/value.go": "package dep\nconst Value = 9\n",
		})
		code, output := fixture.run(t, ctx, ".", "bridge", true, "list", "-deps", "-test", "./...")
		require.Zero(t, code, output)
		code, output = fixture.run(t, ctx, ".", "none", false, "test", "./...")
		assert.Zero(t, code, output)
	})
	t.Run("workspace_and_downloaded_toolchain_work_offline", func(t *testing.T) {
		// A published older version exercises an actual toolchain download and
		// later cache execution. It does not claim a newer release exists.
		fixture := newContainerFixture(t, engine, map[string]string{
			"go.work":           "go 1.26.0\n\ntoolchain go1.26.0\n\nuse (\n ./app\n ./lib\n)\n",
			"app/go.mod":        "module example.com/app\n\ngo 1.26.0\n\nrequire example.com/lib v0.0.0\n",
			"app/proof_test.go": "package app\nimport (\"runtime\"; \"testing\"; \"example.com/lib\")\nfunc TestWorkspace(t *testing.T) { if lib.Value != 4 { t.Fatal(lib.Value) }; if runtime.Version() != \"go1.26.0\" { t.Fatal(runtime.Version()) } }\n",
			"lib/go.mod":        "module example.com/lib\n\ngo 1.26.0\n",
			"lib/value.go":      "package lib\nconst Value = 4\n",
		})
		fixture.toolchain = "go1.26.0+auto"
		code, output := fixture.run(t, ctx, "app", "bridge", true, "list", "-deps", "-test", "./...")
		require.Zero(t, code, output)
		code, output = fixture.run(t, ctx, "app", "none", false, "test", "-v", "./...")
		assert.Zero(t, code, output)
		assert.Contains(t, output, "PASS: TestWorkspace")
	})
	t.Run("operator_docker_proxy_values_are_not_injected", func(t *testing.T) {
		operatorConfig := containerTempDir(t)
		require.NoError(t, os.WriteFile(filepath.Join(operatorConfig, "config.json"), []byte(`{"proxies":{"default":{"httpProxy":"http://private-proxy.invalid:8080","httpsProxy":"http://private-proxy.invalid:8080","ftpProxy":"http://private-proxy.invalid:8080","allProxy":"http://private-proxy.invalid:8080","noProxy":"private.example"}}}`), 0o600))
		t.Setenv("DOCKER_CONFIG", operatorConfig)
		t.Setenv("DOCKER_HOST", engine.Host())
		privateEngine, err := NewContainerRunner().Open(ctx, containerTempDir(t))
		require.NoError(t, err)
		original := basic.engine
		basic.engine = privateEngine
		t.Cleanup(func() { basic.engine = original })
		code, output := basic.run(t, ctx, ".", "none", false, "test", "-count=1", "-run", "^TestBehavior$", "./...")
		assert.Zero(t, code, output)
	})
}

// newContainerFixture writes trusted test data but never runs it on the host.
func newContainerFixture(t *testing.T, engine *Engine, files map[string]string) *containerFixture {
	t.Helper()
	root := containerTempDir(t)
	var id [16]byte
	_, err := rand.Read(id[:])
	require.NoError(t, err)
	f := &containerFixture{
		engine: engine, root: root, source: filepath.Join(root, "src, quoted\""),
		id: hex.EncodeToString(id[:]), toolchain: "auto",
	}
	for _, dir := range []string{f.source, filepath.Join(root, "cache")} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	for name, text := range files {
		dest := filepath.Join(f.source, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o700))
		require.NoError(t, os.WriteFile(dest, []byte(text), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "operator-private.txt"), []byte("not mounted"), 0o600))
	return f
}

// containerTempDir returns the actual existing path, including on macOS where
// temporary-directory aliases otherwise confuse engine bind mounts.
func containerTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return path
}

// stopOnUncertainRemoval exits before testing can delete t.TempDir directories
// that may still be mounted. The outer bounded capture records the failure and
// the exact resource and directory to retain for cleanup.
func stopOnUncertainRemoval(f *containerFixture, name string, err error) {
	var cleanup *CleanupError
	if errors.As(err, &cleanup) {
		fmt.Fprintf(os.Stderr, "container removal is unconfirmed; retained fixture %s; remove only container %s on %s: %v\n", f.root, name, f.engine.Host(), err)
		os.Exit(2)
	}
}

// executeContainer delivers one result without calling testing helpers from a
// worker goroutine. runContainer owns removal on every return path.
func executeContainer(
	ctx context.Context, engine *Engine, spec ContainerSpec, log *os.File,
	done chan<- containerRun,
) {
	code, err := runContainer(ctx, engine, spec, log)
	done <- containerRun{code: code, err: err}
}

// runContainer creates, starts and always removes one container, as the
// check helper does, and joins an unconfirmed removal to the result.
func runContainer(ctx context.Context, engine *Engine, spec ContainerSpec, out io.Writer) (code int, err error) {
	defer func() {
		if rmErr := engine.Remove(spec.Name); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
	}()
	if err := engine.Create(ctx, spec); err != nil {
		return -1, err
	}
	return engine.Start(ctx, spec.Name, out)
}

// run invokes the production runner and checks no labelled container remains.
func (f *containerFixture) run(
	t *testing.T, parent context.Context, module, network string, readonly bool,
	args ...string,
) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, containerCommandTimeout)
	defer cancel()
	spec, log := f.command(t, module, network, readonly, args...)
	code, err := runContainer(ctx, f.engine, spec, log)
	stopOnUncertainRemoval(f, spec.Name, err)
	closeErr := log.Close()
	f.requireRemoved(t)
	require.NoError(t, closeErr)
	output, readErr := os.ReadFile(log.Name())
	require.NoError(t, readErr)
	require.NoError(t, err, string(output))
	return code, string(output)
}

// command prepares only explicit mounts and environment for one invocation.
func (f *containerFixture) command(
	t *testing.T, module, network string, readonly bool, args ...string,
) (ContainerSpec, *os.File) {
	t.Helper()
	name := "surla-check-" + f.id + "-" + strconv.Itoa(f.next)
	work := filepath.Join(f.root, "work-"+strconv.Itoa(f.next))
	f.next++
	require.NoError(t, os.MkdirAll(filepath.Join(work, "tmp"), 0o700))
	log, err := os.Create(filepath.Join(f.root, name+".log"))
	require.NoError(t, err)
	proxy := "https://proxy.golang.org"
	if network == "none" {
		proxy = "off"
	}
	return ContainerSpec{
		Name: name, Operation: f.id, Network: network,
		User:    fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		WorkDir: filepath.Join(containerSource, module),
		Mounts: []Mount{
			{Source: f.source, Target: containerSource, ReadOnly: readonly},
			{Source: filepath.Join(f.root, "cache"), Target: "/gomodcache", ReadOnly: network == "none"},
			{Source: work, Target: "/work"},
		},
		Env: []string{
			"HOME=/work", "TMPDIR=/work/tmp", "GOCACHE=/work/gocache", "GOPATH=/work/gopath",
			"GOMODCACHE=/gomodcache", "GOTOOLCHAIN=" + f.toolchain, "GOTELEMETRY=off",
			"PATH=/usr/local/go/bin:/usr/bin:/bin", "GOPROXY=" + proxy,
			"SURLA_E2E_PROTECTED_FILE=" + filepath.Join(f.root, "operator-private.txt"),
		},
		Args: args,
	}, log
}

// requireRemoved checks only this fixture's random operation label.
func (f *containerFixture) requireRemoved(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out, stderr bytes.Buffer
	err := f.engine.docker(ctx, &out, &stderr, "container", "ls", "--all", "--filter", "label="+checkLabel+"="+f.id, "--format", "{{.Names}}")
	if err != nil || strings.TrimSpace(out.String()) != "" {
		fmt.Fprintf(os.Stderr, "could not confirm empty operation %s on %s; retained fixture %s; containers %q; error %v: %s\n", f.id, f.engine.Host(), f.root, out.String(), err, stderr.String())
		os.Exit(2)
	}
}

// cancelRunning cancels only after the engine reports this fixture running,
// then bounds the wait for the runner's independent removal deadline.
func (f *containerFixture) cancelRunning(t *testing.T, parent context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, containerCommandTimeout)
	defer cancel()
	spec, log := f.command(t, ".", "none", false, "test", "-run", "^TestHang$", "./...")
	done := make(chan containerRun, 1)
	go executeContainer(ctx, f.engine, spec, log, done)
	early, readyErr := f.waitRunning(ctx, done)
	cancel()
	var result containerRun
	if early != nil {
		result = *early
	} else {
		select {
		case result = <-done:
		case <-time.After(removeTimeout + 10*time.Second):
			fmt.Fprintf(os.Stderr, "runner did not return after cancellation; retained fixture %s and container %s on %s\n", f.root, spec.Name, f.engine.Host())
			os.Exit(2)
		}
	}
	stopOnUncertainRemoval(f, spec.Name, result.err)
	closeErr := log.Close()
	f.requireRemoved(t)
	require.NoError(t, closeErr)
	output, err := os.ReadFile(log.Name())
	require.NoError(t, err)
	require.NoError(t, readyErr, string(output))
	require.ErrorIs(t, result.err, context.Canceled)
}

// waitRunning accepts only creation followed by running; every unexpected
// state or engine error stops the wait immediately, and the deadline is hard.
func (f *containerFixture) waitRunning(
	parent context.Context, done <-chan containerRun,
) (*containerRun, error) {
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case result := <-done:
			return &result, errors.Join(fmt.Errorf("runner exited before cancellation with code %d", result.code), result.err)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
			var out, stderr bytes.Buffer
			if err := f.engine.docker(ctx, &out, &stderr, "container", "ls", "--all", "--filter", "label="+checkLabel+"="+f.id, "--format", "{{.State}}"); err != nil {
				return nil, fmt.Errorf("inspect fixture readiness: %w: %s", err, stderr.String())
			}
			switch state := strings.TrimSpace(out.String()); state {
			case "running":
				return nil, nil
			case "", "created":
				// The same unique operation may still be entering its container.
			default:
				return nil, fmt.Errorf("container entered unexpected state %q", state)
			}
		}
	}
}
