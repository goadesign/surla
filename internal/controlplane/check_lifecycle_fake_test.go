// This file provides the scripted fake docker executable, in-process fake
// daemon, and HTTP/MCP client helpers shared by check_lifecycle_test.go.
package controlplane

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	genhttpclient "github.com/goadesign/surla/gen/http/surla/client"
	genmcpclient "github.com/goadesign/surla/gen/jsonrpc/mcp_surla/client"
	genmcpsurla "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/verify/candidate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goahttp "goa.design/goa/v3/http"
)

type (
	// fakeDockerCall is the JSON request/response envelope between fake docker and fakeDockerDaemon.
	fakeDockerCall struct {
		// Args and Env carry the CLI invocation from the fake docker process.
		Args, Env []string
		// Stdout and Stderr hold the raw bytes written back by the CLI.
		Stdout, Stderr []byte
		// ExitCode is the process exit status returned by the fake docker CLI.
		ExitCode int
	}

	// recordedContainer holds the parsed specification and state of one fake container.
	recordedContainer struct {
		// Name, Label, Network, and WorkDir record their respective create flags.
		Name, Label, Network, WorkDir string
		// Mounts lists all bind mounts passed via --mount in flag order.
		Mounts []candidate.Mount
		// Env and Args hold the --env entries and Go arguments after CheckImage.
		Env, Args []string
		// DockerConfig is the DOCKER_CONFIG directory passed in the CLI environment.
		DockerConfig string
		// State is "created", "running" or "exited". A labelled listing
		// reports a container in every state until it is removed.
		State string
		// Removed reports whether docker rm --force confirmed removal.
		Removed bool
		// ExitCode is the exit status recorded when docker start --attach finished.
		ExitCode int
	}

	// containerStartFunc simulates container execution during docker start --attach.
	containerStartFunc func(c *recordedContainer) (string, string, int)

	// beforeCallFunc runs, outside the daemon's lock, when a docker invocation
	// arrives and before it is handled. ctx ends when the docker process that
	// sent it is gone, so a test can hold one verb mid-request and see whether
	// its sender was killed.
	beforeCallFunc func(ctx context.Context, verb string)

	// fakeDockerDaemon serves scripted Docker CLI responses for one unix://
	// endpoint file inside t.TempDir without talking to a real container engine.
	fakeDockerDaemon struct {
		// serverURL and hostURI store the HTTP server URL and unix:// --host URI.
		serverURL, hostURI string
		// pulls, contextInspects, and removes count pull, context, and rm calls.
		pulls, contextInspects, removes atomic.Int64
		// calls counts every docker invocation that reached the daemon.
		calls atomic.Int64
		// mu guards all fields below.
		mu sync.Mutex
		// imagePresent reports whether CheckImage has been pulled onto this daemon.
		imagePresent bool
		// containers records every container created on this daemon in order.
		containers []*recordedContainer
		// rmCounts counts docker rm --force invocations per container name.
		rmCounts map[string]int
		// failRemove maps container names whose docker rm --force should fail.
		failRemove map[string]bool
		// onStart runs outside mu during docker start --attach.
		onStart containerStartFunc
		// beforeCall runs outside mu before each invocation is handled.
		beforeCall beforeCallFunc
		// failCreate makes docker create fail with a daemon error, creating
		// nothing.
		failCreate bool
		// failList makes docker container ls fail.
		failList bool
	}

	// checkClients bundles a test server's ControlPlane and HTTP/MCP clients.
	checkClients struct {
		// cp is the ControlPlane service under test.
		cp *ControlPlane
		// httpClient calls the generated Goa HTTP endpoints with bearer auth.
		httpClient *gensurla.Client
		// mcpClient calls the generated MCP JSON-RPC endpoints with bearer auth.
		mcpClient *genmcpsurla.Client
	}
)

// fakeDockerSource is a standalone Go binary compiled into t.TempDir and placed
// on PATH so candidate.NewContainerRunner resolves and runs a real executable.
const fakeDockerSource = `package main
import ("bytes"; "encoding/json"; "fmt"; "net/http"; "os"; "path/filepath"; "strings")
func must(err error) { if err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(1) } }
func main() {
	ep := filepath.Join(filepath.Dir(os.Args[0]), "context_endpoint.txt")
	if len(os.Args) >= 3 && os.Args[1] == "--host" {
		if !strings.HasPrefix(os.Args[2], "unix://") { must(fmt.Errorf("bad host %q", os.Args[2])) }
		ep = strings.TrimPrefix(os.Args[2], "unix://")
	}
	rawURL, err := os.ReadFile(ep); must(err)
	body, err := json.Marshal(struct{ Args, Env []string }{os.Args[1:], os.Environ()}); must(err)
	resp, err := http.Post(strings.TrimSpace(string(rawURL))+"/invoke", "application/json", bytes.NewReader(body)); must(err)
	defer resp.Body.Close()
	var out struct{ Stdout, Stderr []byte; ExitCode int }
	must(json.NewDecoder(resp.Body).Decode(&out))
	_, err = os.Stdout.Write(out.Stdout); must(err)
	_, err = os.Stderr.Write(out.Stderr); must(err)
	os.Exit(out.ExitCode)
}
`

// installFakeDocker compiles fakeDockerSource into t.TempDir and prepends it to PATH.
func installFakeDocker(t *testing.T) string {
	t.Helper()
	root := realTempDir(t)
	srcDir, binDir := filepath.Join(root, "src"), filepath.Join(root, "bin")
	writeTree(t, srcDir, map[string]string{
		"go.mod":  "module example.com/fakedocker\n\ngo 1.24\n",
		"main.go": fakeDockerSource,
	})
	require.NoError(t, os.MkdirAll(binDir, 0o700))
	dockerBin := filepath.Join(binDir, "docker")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", dockerBin, ".")
	cmd.Dir, cmd.Env = srcDir, append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "build fake docker: %s", string(out))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dockerBin
}

// newFakeDockerDaemon starts an in-process fakeDockerDaemon backed by httptest.Server.
func newFakeDockerDaemon(t *testing.T) *fakeDockerDaemon {
	t.Helper()
	d := &fakeDockerDaemon{rmCounts: map[string]int{}, failRemove: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(d.handleInvoke))
	t.Cleanup(srv.Close)
	ep := filepath.Join(realTempDir(t), "docker.sock")
	require.NoError(t, os.WriteFile(ep, []byte(srv.URL+"\n"), 0o600))
	d.serverURL, d.hostURI = srv.URL, "unix://"+ep
	return d
}

// validateStrippedDockerEnv verifies that Engine.docker passed only DOCKER_CONFIG and PATH.
func validateStrippedDockerEnv(env []string) (string, error) {
	var dockerConfig string
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		switch {
		case !ok, k == "PATH", k == "PWD", k == "_", k == "SHLVL":
		case k == "DOCKER_CONFIG":
			dockerConfig = v
		default:
			return "", fmt.Errorf("unexpected environment variable %q passed to docker CLI", k)
		}
	}
	if dockerConfig == "" {
		return "", fmt.Errorf("missing DOCKER_CONFIG in docker CLI env")
	}
	return dockerConfig, nil
}

// parseCreateContainer parses docker create flags and enforces container hardening.
func parseCreateContainer(args []string, dockerConfig string) (*recordedContainer, error) {
	c := &recordedContainer{DockerConfig: dockerConfig}
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var readOnly bool
	got := map[string]string{}
	fs.StringVar(&c.Name, "name", "", "")
	fs.StringVar(&c.Label, "label", "", "")
	fs.BoolVar(&readOnly, "read-only", false, "")
	fs.StringVar(&c.Network, "network", "", "")
	fs.StringVar(&c.WorkDir, "workdir", "", "")
	for _, name := range []string{"cap-drop", "security-opt", "pids-limit", "log-driver", "user", "entrypoint"} {
		fs.Func(name, "", func(v string) error { got[name] = v; return nil })
	}
	fs.Func("mount", "", func(raw string) error {
		fields, err := csv.NewReader(strings.NewReader(raw)).Read()
		var m candidate.Mount
		for _, f := range fields {
			if k, v, ok := strings.Cut(f, "="); ok && k == "source" {
				m.Source = v
			} else if ok && k == "target" {
				m.Target = v
			} else if f == "readonly" {
				m.ReadOnly = true
			}
		}
		c.Mounts = append(c.Mounts, m)
		return err
	})
	fs.Func("env", "", func(kv string) error { c.Env = append(c.Env, kv); return nil })
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	rest := fs.Args()
	if len(rest) == 0 || !readOnly || got["cap-drop"] != "ALL" || got["security-opt"] != "no-new-privileges" ||
		got["pids-limit"] != "4096" || got["log-driver"] != "none" || got["user"] == "" ||
		got["entrypoint"] != "/usr/local/go/bin/go" || rest[0] != candidate.CheckImage {
		return nil, fmt.Errorf("unexpected container hardening flags in %v", args)
	}
	c.Args = slices.Clone(rest[1:])
	return c, nil
}

// testCheckHelper starts this test binary in TestMain's check-run role, the
// check helper surla serve would start as `surla check-run`.
func testCheckHelper(t testing.TB) CheckHelper {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	return func() *exec.Cmd { return exec.Command(self, "check-run") }
}

// newCheckTestServer mounts NewHTTPHandler with ContainerRunner defaults and bearer auth.
func newCheckTestServer(t *testing.T, opts Options) checkClients {
	t.Helper()
	opts.CardsDir = filepath.Join(realTempDir(t), "cards")
	opts.Checker = candidate.NewContainerRunner()
	opts.CheckHelper = testCheckHelper(t)
	opts.CheckTimeout = 30 * time.Second
	cp := newControlPlane(t, opts)
	srv := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{
		"generation-token": {ID: "developer@example.com"},
	})))
	t.Cleanup(srv.Close)
	origin, err := url.Parse(srv.URL)
	require.NoError(t, err)
	doer := generationDoer{client: srv.Client()}
	httpT := genhttpclient.NewClient(origin.Scheme, origin.Host, doer, goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	mcpT := genmcpclient.NewClient(origin.Scheme, origin.Host, doer, goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	httpC := &gensurla.Client{
		CheckProjectEndpoint:       httpT.CheckProject(),
		UpdateDependenciesEndpoint: httpT.UpdateDependencies(),
		ReadCheckOutputEndpoint:    httpT.ReadCheckOutput(),
	}
	mcpC := &genmcpsurla.Client{ToolsCallEndpoint: mcpT.ToolsCall(), ToolsListEndpoint: mcpT.ToolsList()}
	return checkClients{cp: cp, httpClient: httpC, mcpClient: mcpC}
}

// openCheckCP opens a ControlPlane configured with ContainerRunner without t.Cleanup.
func openCheckCP(t *testing.T, cardsDir, workspace string) *ControlPlane {
	t.Helper()
	opts := Options{
		CardsDir:     cardsDir,
		WorkspaceDir: workspace,
		Checker:      candidate.NewContainerRunner(),
		CheckHelper:  testCheckHelper(t),
		CheckTimeout: 30 * time.Second,
	}
	cp, err := New(opts)
	require.NoError(t, err)
	return cp
}

// newShopModule creates a single-module Go workspace in a symlink-resolved TempDir.
func newShopModule(t *testing.T, extra map[string]string) string {
	t.Helper()
	workspace := realTempDir(t)
	files := map[string]string{
		"go.mod":       "module example.com/shop\n\ngo 1.24\n",
		"shop_test.go": "package shop\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	writeTree(t, workspace, files)
	return workspace
}

// callMCPTool invokes name over MCP JSON-RPC and decodes its structured result.
func callMCPTool[T any](
	t *testing.T, mcpClient *genmcpsurla.Client, name, args string, decode func([]byte) (T, error),
) T {
	t.Helper()
	call, err := mcpClient.ToolsCall(t.Context(), &genmcpsurla.ToolsCallPayload{
		Name:      name,
		Arguments: json.RawMessage(args),
	})
	require.NoError(t, err)
	complete, ok := call.Outcome.AsComplete()
	require.True(t, ok)
	require.False(t, complete.IsError != nil && *complete.IsError, "%+v", complete.Content)
	res, err := decode(complete.StructuredContent)
	require.NoError(t, err)
	return res
}

// findMount returns the candidate.Mount targeting target inside c.
func findMount(t *testing.T, c *recordedContainer, target string) candidate.Mount {
	t.Helper()
	for _, m := range c.Mounts {
		if m.Target == target {
			return m
		}
	}
	assert.Failf(t, "missing mount", "no mount for target %q in %+v", target, c.Mounts)
	return candidate.Mount{}
}

// assertFileContent asserts that path exists and holds want.
func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if assert.NoError(t, err) {
		assert.Equal(t, want, string(got))
	}
}

// dirEntryNames returns the entry names inside dir.
func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// readOwnerRecord strictly decodes opDir/owner.json via decodeCheckOwner.
func readOwnerRecord(t *testing.T, opDir string) checkOwner {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(opDir, ownerFile))
	require.NoError(t, err)
	owner, err := decodeCheckOwner(raw, filepath.Base(opDir))
	require.NoError(t, err)
	return owner
}

// handleInvoke processes one CLI invocation from the fake docker executable.
func (d *fakeDockerDaemon) handleInvoke(w http.ResponseWriter, r *http.Request) {
	var req fakeDockerCall
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.calls.Add(1)
	d.mu.Lock()
	before := d.beforeCall
	d.mu.Unlock()
	if before != nil {
		args := req.Args
		if len(args) >= 2 && args[0] == "--host" {
			args = args[2:]
		}
		if len(args) > 0 {
			before(r.Context(), args[0])
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(d.dispatch(req)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// dispatch executes one Docker CLI subcommand against the daemon's state.
func (d *fakeDockerDaemon) dispatch(req fakeDockerCall) fakeDockerCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	args := req.Args
	if len(args) >= 2 && args[0] == "context" && args[1] == "inspect" {
		d.contextInspects.Add(1)
		return fakeDockerCall{Stdout: []byte(d.hostURI + "\n")}
	}
	if len(args) < 3 || args[0] != "--host" {
		return fakeDockerCall{Stderr: []byte("missing --host\n"), ExitCode: 1}
	}
	dockerConfig, envErr := validateStrippedDockerEnv(req.Env)
	if envErr != nil {
		return fakeDockerCall{Stderr: []byte(envErr.Error() + "\n"), ExitCode: 1}
	}
	sub := args[2:]
	switch sub[0] {
	case "version":
		return fakeDockerCall{Stdout: []byte("linux\n")}
	case "image":
		if !d.imagePresent {
			return fakeDockerCall{Stderr: []byte("Error: No such image\n"), ExitCode: 1}
		}
		return fakeDockerCall{Stdout: []byte("[]\n")}
	case "pull":
		if len(sub) != 2 || sub[1] != candidate.CheckImage {
			return fakeDockerCall{Stderr: []byte("unexpected pull args\n"), ExitCode: 1}
		}
		d.imagePresent = true
		d.pulls.Add(1)
		return fakeDockerCall{Stdout: []byte("pulled\n")}
	case "create":
		if d.failCreate {
			return fakeDockerCall{Stderr: []byte("Error response from daemon: mounts denied\n"), ExitCode: 1}
		}
		c, err := parseCreateContainer(sub[1:], dockerConfig)
		if err != nil {
			return fakeDockerCall{Stderr: []byte(err.Error() + "\n"), ExitCode: 1}
		}
		c.State = "created"
		d.containers = append(d.containers, c)
		return fakeDockerCall{Stdout: []byte(c.Name + "\n")}
	case "start":
		if len(sub) != 3 || sub[1] != "--attach" {
			return fakeDockerCall{Stderr: []byte("unexpected start args\n"), ExitCode: 1}
		}
		c := d.findContainer(sub[2], false)
		if c == nil {
			return fakeDockerCall{Stderr: []byte("No such container\n"), ExitCode: 1}
		}
		var stdout, stderr string
		var code int
		c.State = "running"
		if onStart := d.onStart; onStart != nil {
			d.mu.Unlock()
			stdout, stderr, code = onStart(c)
			d.mu.Lock()
		}
		c.State = "exited"
		c.ExitCode = code
		return fakeDockerCall{Stdout: []byte(stdout), Stderr: []byte(stderr), ExitCode: code}
	case "wait":
		if len(sub) == 2 {
			if c := d.findContainer(sub[1], false); c != nil {
				return fakeDockerCall{Stdout: fmt.Appendf(nil, "%d\n", c.ExitCode)}
			}
		}
		return fakeDockerCall{Stderr: []byte("No such container\n"), ExitCode: 1}
	case "container":
		label, ok := strings.CutPrefix(strings.Join(sub, " "), "container ls --all --filter label=")
		label, format, cut := strings.Cut(label, " --format ")
		if !ok || !cut || format != "{{.Names}}" || strings.Contains(label, " ") {
			return fakeDockerCall{Stderr: []byte("unexpected container args\n"), ExitCode: 1}
		}
		if d.failList {
			return fakeDockerCall{Stderr: []byte("Error response from daemon: list failed\n"), ExitCode: 1}
		}
		var names strings.Builder
		for _, c := range d.containers {
			if c.Label == label && !c.Removed {
				names.WriteString(c.Name + "\n")
			}
		}
		return fakeDockerCall{Stdout: []byte(names.String())}
	case "rm":
		if len(sub) != 3 || sub[1] != "--force" {
			return fakeDockerCall{Stderr: []byte("unexpected rm args\n"), ExitCode: 1}
		}
		name := sub[2]
		d.removes.Add(1)
		d.rmCounts[name]++
		if d.failRemove[name] {
			return fakeDockerCall{Stderr: []byte("daemon blocked removal\n"), ExitCode: 1}
		}
		if c := d.findContainer(name, true); c != nil {
			c.Removed = true
			return fakeDockerCall{Stdout: []byte(name + "\n")}
		}
		// Docker can report a missing container this way: exit 0, no
		// output.
		return fakeDockerCall{}
	default:
		return fakeDockerCall{Stderr: []byte("unknown subcommand\n"), ExitCode: 1}
	}
}

// findContainer looks up a recorded container by name while d.mu is held.
func (d *fakeDockerDaemon) findContainer(name string, activeOnly bool) *recordedContainer {
	for _, c := range d.containers {
		if c.Name == name && (!activeOnly || !c.Removed) {
			return c
		}
	}
	return nil
}

// createdContainers returns a snapshot of the containers created on d.
func (d *fakeDockerDaemon) createdContainers() []recordedContainer {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]recordedContainer, len(d.containers))
	for i, c := range d.containers {
		cp := *c
		cp.Mounts, cp.Env, cp.Args = slices.Clone(c.Mounts), slices.Clone(c.Env), slices.Clone(c.Args)
		out[i] = cp
	}
	return out
}

// resetContainers clears the recorded container list while preserving image cache state.
func (d *fakeDockerDaemon) resetContainers() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.containers = nil
}

// setOnStart installs the callback invoked during docker start --attach.
func (d *fakeDockerDaemon) setOnStart(fn containerStartFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onStart = fn
}

// setBeforeCall installs the hook run before each invocation is handled.
func (d *fakeDockerDaemon) setBeforeCall(fn beforeCallFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.beforeCall = fn
}

// setFailCreate configures whether docker create fails with a daemon error.
func (d *fakeDockerDaemon) setFailCreate(fail bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failCreate = fail
}

// setFailList configures whether docker container ls fails.
func (d *fakeDockerDaemon) setFailList(fail bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failList = fail
}

// addRunning records a container that is running on the daemon, as one an
// earlier server created and started would be.
func (d *fakeDockerDaemon) addRunning(name, label string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.containers = append(d.containers, &recordedContainer{Name: name, Label: label, State: "running"})
}

// addCreated records a container that was created and never started, as a
// create answered after its sender stopped waiting would leave.
func (d *fakeDockerDaemon) addCreated(name, label string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.containers = append(d.containers, &recordedContainer{Name: name, Label: label, State: "created"})
}

// container returns a snapshot of the recorded container name, or false.
func (d *fakeDockerDaemon) container(name string) (recordedContainer, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.findContainer(name, false); c != nil {
		return *c, true
	}
	return recordedContainer{}, false
}

// setFailRemove configures whether docker rm --force fails for container name.
func (d *fakeDockerDaemon) setFailRemove(name string, fail bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failRemove[name] = fail
}

// removeCount returns how many times docker rm --force targeted name on d.
func (d *fakeDockerDaemon) removeCount(name string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rmCounts[name]
}
