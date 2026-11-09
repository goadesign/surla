// This file exercises the end-to-end check_project, update_dependencies, and
// read_check_output lifecycle through the public ControlPlane service and
// generated HTTP/MCP handlers using a trusted scripted fake docker executable
// on PATH.
package controlplane

import (
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
	"unicode/utf8"

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
		// Removed reports whether docker rm --force confirmed removal.
		Removed bool
		// ExitCode is the exit status recorded when docker start --attach finished.
		ExitCode int
	}

	// containerStartFunc simulates container execution during docker start --attach.
	containerStartFunc func(c *recordedContainer) (string, string, int)

	// pinnedCheck holds the retained state from a CheckProject call whose rm failed.
	pinnedCheck struct {
		// cp is the open ControlPlane that ran the pinned check.
		cp *ControlPlane
		// name and opDir identify the unremoved container and retained directory.
		name, opDir string
	}

	// fakeDockerDaemon serves scripted Docker CLI responses for one unix://
	// endpoint file inside t.TempDir without talking to a real container engine.
	fakeDockerDaemon struct {
		// serverURL and hostURI store the HTTP server URL and unix:// --host URI.
		serverURL, hostURI string
		// pulls, contextInspects, and removes count pull, context, and rm calls.
		pulls, contextInspects, removes atomic.Int64
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

// TestCheckProjectLifecycle verifies context-inspect resolution when DOCKER_HOST
// is unset, digest pull on cache miss, Phase 1 (networked go list, read-only
// source, writable modcache) before Phase 2 (offline go test, writable source
// copy, read-only modcache), check -> update_dependencies -> check repair, live
// workspace preservation, and UTF-8/invalid-byte output budgeting over MCP/HTTP.
func TestCheckProjectLifecycle(t *testing.T) {
	ctx := t.Context()
	dockerBin := installFakeDocker(t)
	daemon := newFakeDockerDaemon(t)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(dockerBin), "context_endpoint.txt"), []byte(daemon.serverURL+"\n"), 0o600))
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("SURLA_OPERATOR_SECRET", "must-not-leak-to-docker")

	workspace := newShopModule(t, map[string]string{
		"go.sum":                      "repaired-sum\n",
		"eval/go.mod":                 "module example.com/shop/eval\n\ngo 1.24\n",
		"eval/eval_test.go":           "package eval\n",
		"testdata/fixture/go.mod":     "module example.com/ignored\n",
		".surla/cards/c1.json":        "{}\n",
		"reports/" + reportName:       "secret holdout report\n",
		"reports/public-fixture.json": "public fixture\n",
	})
	runGitForTest(t, workspace, "init", "-q")
	clients := newCheckTestServer(t, Options{
		WorkspaceDir:    workspace,
		ImportedReports: []string{filepath.Join(workspace, "reports", reportName)},
		GoProxyEnv:      []string{"GOPROXY=https://proxy.example.test", "GOPRIVATE=example.com/*"},
	})

	tools, err := clients.mcpClient.ToolsList(ctx, &genmcpsurla.ToolsListPayload{})
	require.NoError(t, err)
	for _, want := range []string{"check_project", "update_dependencies", "read_check_output"} {
		assert.True(t, slices.ContainsFunc(tools.Tools, func(ti *genmcpsurla.ToolInfo) bool {
			return ti.Name == want
		}), want)
	}

	evalDir := filepath.Join(workspace, "eval")
	var failEvalTest atomic.Bool
	daemon.setOnStart(func(c *recordedContainer) (string, string, int) {
		srcMount, modMount := findMount(t, c, workspace), findMount(t, c, containerModCache)
		assert.False(t, findMount(t, c, containerWork).ReadOnly)
		assert.NoFileExists(t, filepath.Join(srcMount.Source, ".git", "HEAD"))
		assert.NoFileExists(t, filepath.Join(srcMount.Source, ".surla", "cards", "c1.json"))
		assert.NoFileExists(t, filepath.Join(srcMount.Source, "reports", reportName))
		assert.FileExists(t, filepath.Join(srcMount.Source, "reports", "public-fixture.json"))
		assert.FileExists(t, filepath.Join(srcMount.Source, "shop_test.go"))

		isEval := c.WorkDir == evalDir
		marker := filepath.Base(c.WorkDir) + "-cached"
		switch {
		case slices.Equal(c.Args, []string{"mod", "tidy"}):
			if !isEval {
				assert.NoError(t, os.WriteFile(filepath.Join(srcMount.Source, "go.sum"), []byte("repaired-sum\n"), 0o644))
			}
			return "tidied\n", "", 0
		case slices.Equal(c.Args, []string{"list", "-deps", "-test", "./..."}):
			assert.Equal(t, "bridge", c.Network)
			assert.True(t, srcMount.ReadOnly)
			assert.False(t, modMount.ReadOnly)
			assert.Subset(t, c.Env, []string{"GOPROXY=https://proxy.example.test", "GOPRIVATE=example.com/*"})
			if !isEval {
				sum, rerr := os.ReadFile(filepath.Join(srcMount.Source, "go.sum"))
				if rerr != nil || string(sum) != "repaired-sum\n" {
					return "", "go: updates to go.mod needed; to update it:\n\tgo mod tidy\n", 1
				}
			}
			assert.NoError(t, os.WriteFile(filepath.Join(modMount.Source, marker), []byte("ok"), 0o600))
			return "prepared " + marker + "\n", "", 0
		default:
			assert.Equal(t, []string{"test", "./..."}, c.Args)
			assert.Equal(t, "none", c.Network)
			assert.False(t, srcMount.ReadOnly)
			assert.True(t, modMount.ReadOnly)
			assert.Contains(t, c.Env, "GOPROXY=off")
			assert.NotContains(t, c.Env, "GOPRIVATE=example.com/*")
			assert.FileExists(t, filepath.Join(modMount.Source, marker))
			assert.NoError(t, os.WriteFile(filepath.Join(srcMount.Source, "test_scratch.tmp"), []byte("ephemeral"), 0o600))
			assert.NoError(t, os.WriteFile(filepath.Join(srcMount.Source, "reports", "public-fixture.json"), []byte("mutated\n"), 0o600))
			if isEval && failEvalTest.Load() {
				return strings.Repeat("log-✓-", 3200) + strings.Repeat("\xff", 6000) + "\n--- FAIL: TestEvalScenario (0.01s)\n", "", 1
			}
			return "ok  \t" + c.WorkDir + "\n", "", 0
		}
	})

	// 1. Passing check_project over MCP pulls CheckImage once, runs Phase 1 ->
	// Phase 2 per module, leaves live files untouched, and cleans scratch.
	res := callMCPTool(t, clients.mcpClient, "check_project", `{}`, gensurla.DecodeCheckProjectResult)
	assert.True(t, res.Passed)
	assert.Equal(t, "tested 2 module(s): ., eval. All passed.", res.Message)
	assert.Equal(t, int64(1), daemon.pulls.Load())
	assert.GreaterOrEqual(t, daemon.contextInspects.Load(), int64(1))
	created := daemon.createdContainers()
	require.Len(t, created, 4)
	for i, wantDir := range []string{workspace, workspace, evalDir, evalDir} {
		assert.Equal(t, wantDir, created[i].WorkDir)
		assert.True(t, created[i].Removed)
	}
	assert.Equal(t, []string{"list", "-deps", "-test", "./..."}, created[0].Args)
	assert.Equal(t, []string{"test", "./..."}, created[1].Args)
	assert.NoFileExists(t, filepath.Join(workspace, "test_scratch.tmp"))
	assertFileContent(t, filepath.Join(workspace, "reports", "public-fixture.json"), "public fixture\n")
	assert.ElementsMatch(t, []string{outputFile, outputMetaFile, ownerFile}, dirEntryNames(t, filepath.Join(clients.cp.checkWorkDir, res.OpID)))

	// 2. Stale go.sum fails Phase 1 on ".", skips Phase 2 for "." while still
	// testing "eval", then repairs via update_dependencies on the same server.
	daemon.resetContainers()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "go.sum"), []byte("stale-sum\n"), 0o644))
	resDownloadFail, err := clients.httpClient.CheckProject(ctx)
	require.NoError(t, err)
	assert.False(t, resDownloadFail.Passed)
	assert.Contains(t, resDownloadFail.Message, "Dependencies for . could not be prepared (go list exited 1)")
	assert.Contains(t, resDownloadFail.Message, "call update_dependencies and check again")
	assert.Equal(t, int64(1), daemon.pulls.Load())
	assert.GreaterOrEqual(t, daemon.contextInspects.Load(), int64(2))
	created = daemon.createdContainers()
	require.Len(t, created, 3)
	assert.Equal(t, []string{workspace, evalDir, evalDir}, []string{created[0].WorkDir, created[1].WorkDir, created[2].WorkDir})

	repairRes := callMCPTool(t, clients.mcpClient, "update_dependencies", `{}`, gensurla.DecodeUpdateDependenciesResult)
	assert.Equal(t, []string{"go.sum"}, repairRes.FilesWritten)
	assert.True(t, callMCPTool(t, clients.mcpClient, "check_project", `{}`, gensurla.DecodeCheckProjectResult).Passed)

	// 3. Multi-page UTF-8 and invalid-byte failure output stays <= 16 KiB per
	// decoded MCP Message and read_check_output page without splitting valid runes.
	daemon.resetContainers()
	failEvalTest.Store(true)
	resTestFail := callMCPTool(t, clients.mcpClient, "check_project", `{}`, gensurla.DecodeCheckProjectResult)
	assert.False(t, resTestFail.Passed)
	assert.LessOrEqual(t, len(resTestFail.Message), maxCheckMessageBytes)
	assert.True(t, utf8.ValidString(resTestFail.Message))
	assert.Contains(t, resTestFail.Message, "eval failed: go test exited 1.")
	assert.Contains(t, resTestFail.Message, "\uFFFD")
	assert.Contains(t, resTestFail.Message, "--- FAIL: TestEvalScenario")

	var combined strings.Builder
	pages := 0
	for cursor := (*string)(nil); ; {
		args, err := gensurla.EncodeReadCheckOutputPayload(&gensurla.ReadCheckOutputPayload{OpID: resTestFail.OpID, Cursor: cursor})
		require.NoError(t, err)
		page := callMCPTool(t, clients.mcpClient, "read_check_output", string(args), gensurla.DecodeCheckOutputPage)
		assert.LessOrEqual(t, len(page.Text), maxCheckMessageBytes)
		assert.True(t, utf8.ValidString(page.Text))
		combined.WriteString(page.Text)
		assert.Zero(t, page.DroppedBytes)
		pages++
		if page.NextCursor == nil {
			assert.Equal(t, int64(combined.Len()-2*6000), page.CapturedBytes)
			break
		}
		require.NotEqual(t, cursor, page.NextCursor)
		cursor = page.NextCursor
	}
	assert.GreaterOrEqual(t, pages, 3)
	assert.Contains(t, combined.String(), "==> modules: ., eval")
	assert.Contains(t, combined.String(), strings.Repeat("log-✓-", 3200))
	assert.Contains(t, combined.String(), "--- FAIL: TestEvalScenario (0.01s)")

	httpPage, err := clients.httpClient.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: resTestFail.OpID})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(httpPage.Text), maxCheckMessageBytes)
	assert.NotEmpty(t, httpPage.Text)
}

// TestUpdateDependenciesLifecycle verifies that update_dependencies runs
// go mod tidy with network and writable source mounts, publishes only changed
// manifests of modules whose tidy succeeded without touching unchanged or
// failed modules' files, and when go.work is present in an enclosing grant,
// also runs go list -deps -test ./... and publishes go.work.sum.
func TestUpdateDependenciesLifecycle(t *testing.T) {
	ctx := t.Context()
	installFakeDocker(t)
	daemon := newFakeDockerDaemon(t)
	t.Setenv("DOCKER_HOST", daemon.hostURI)

	t.Run("multi_module_without_gowork_publishes_changed_passing_modules", func(t *testing.T) {
		workspace := newShopModule(t, map[string]string{
			"go.sum":        "old-root-sum\n",
			"eval/go.mod":   "module example.com/shop/eval\n\ngo 1.24\n",
			"broken/go.mod": "module example.com/shop/broken\n\ngo 1.24\n",
			"broken/go.sum": "keep-broken-sum\n",
		})
		clients := newCheckTestServer(t, Options{
			WorkspaceDir: workspace,
			GoProxyEnv:   []string{"GOPROXY=https://proxy.example.test"},
		})
		daemon.setOnStart(func(c *recordedContainer) (string, string, int) {
			srcMount := findMount(t, c, workspace)
			assert.Equal(t, "bridge", c.Network)
			assert.False(t, srcMount.ReadOnly)
			assert.False(t, findMount(t, c, containerModCache).ReadOnly)
			assert.Contains(t, c.Env, "GOPROXY=https://proxy.example.test")
			assert.Equal(t, []string{"mod", "tidy"}, c.Args)
			rel, err := filepath.Rel(workspace, c.WorkDir)
			assert.NoError(t, err)
			stagedSum := filepath.Join(srcMount.Source, rel, "go.sum")
			switch filepath.ToSlash(rel) {
			case ".":
				assert.NoError(t, os.WriteFile(stagedSum, []byte("new-root-sum\n"), 0o644))
				return "tidied root\n", "", 0
			case "eval":
				assert.NoError(t, os.WriteFile(stagedSum, []byte("created-eval-sum\n"), 0o644))
				return "tidied eval\n", "", 0
			default:
				assert.NoError(t, os.WriteFile(stagedSum, []byte("corrupted-by-failed-tidy\n"), 0o644))
				return "", "example.com/shop/broken: missing package\n", 1
			}
		})

		res := callMCPTool(t, clients.mcpClient, "update_dependencies", `{}`, gensurla.DecodeUpdateDependenciesResult)
		assert.Equal(t, []string{"eval/go.sum", "go.sum"}, res.FilesWritten)
		assert.Contains(t, res.Message, "Wrote 2 file(s): eval/go.sum, go.sum.")
		assert.Contains(t, res.Message, "go tidy failed for broken (exit 1); its files were not written.")
		assertFileContent(t, filepath.Join(workspace, "go.sum"), "new-root-sum\n")
		assertFileContent(t, filepath.Join(workspace, "eval", "go.sum"), "created-eval-sum\n")
		assertFileContent(t, filepath.Join(workspace, "broken", "go.sum"), "keep-broken-sum\n")
	})

	t.Run("enclosing_grant_with_gowork_runs_list_and_publishes_gowork_sum", func(t *testing.T) {
		grant := realTempDir(t)
		runGitForTest(t, grant, "init", "-q")
		workspace := filepath.Join(grant, "services", "shop")
		writeTree(t, grant, map[string]string{
			"go.work":               "go 1.24\n\nuse ./services/shop\n",
			"services/shop/go.mod":  "module example.com/shop\n\ngo 1.24\n",
			"services/shop/go.sum":  "old-sum\n",
			"services/shop/main.go": "package main\n",
		})
		clients := newCheckTestServer(t, Options{
			WorkspaceDir: workspace,
			GoProxyEnv:   []string{"GOPROXY=https://proxy.example.test"},
		})
		daemon.setOnStart(func(c *recordedContainer) (string, string, int) {
			srcMount := findMount(t, c, grant)
			assert.Equal(t, "bridge", c.Network)
			assert.False(t, srcMount.ReadOnly)
			assert.Equal(t, workspace, c.WorkDir)
			if slices.Equal(c.Args, []string{"mod", "tidy"}) {
				assert.NoError(t, os.WriteFile(filepath.Join(srcMount.Source, "services", "shop", "go.sum"), []byte("tidied-shop-sum\n"), 0o644))
				return "", "", 0
			}
			assert.Equal(t, []string{"list", "-deps", "-test", "./..."}, c.Args)
			assert.NoError(t, os.WriteFile(filepath.Join(srcMount.Source, "go.work.sum"), []byte("work-sum-from-list\n"), 0o644))
			return "", "", 0
		})

		res, err := clients.httpClient.UpdateDependencies(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{"../../go.work.sum", "go.sum"}, res.FilesWritten)
		assert.Contains(t, res.Message, "Wrote 2 file(s): ../../go.work.sum, go.sum.")
		assertFileContent(t, filepath.Join(workspace, "go.sum"), "tidied-shop-sum\n")
		assertFileContent(t, filepath.Join(grant, "go.work.sum"), "work-sum-from-list\n")
	})
}

// TestCheckOperationsConcurrentIsolation verifies disjoint per-operation
// sandboxes and output cursors across concurrent check_project and
// update_dependencies calls, plus live directory-lock serialization across two
// servers updating dependencies on the same workspace.
func TestCheckOperationsConcurrentIsolation(t *testing.T) {
	installFakeDocker(t)
	daemon := newFakeDockerDaemon(t)
	t.Setenv("DOCKER_HOST", daemon.hostURI)
	t.Run("check_and_update_use_disjoint_sandboxes", func(t *testing.T) {
		testConcurrentCheckAndUpdate(t, daemon)
	})
	t.Run("two_servers_serialize_live_directory_lock", func(t *testing.T) {
		testTwoServerUpdateContention(t, daemon)
	})
}

// TestCheckOperationsRecoveryAndRetainedOwner verifies retained-owner scratch
// preservation on container-removal failure, exact recorded-endpoint retry on
// Close and next startup, unavailable refusal after Close, non-interference
// across distinct CardsDir servers on the same workspace, and startup cleanup
// of ownerless pre-container scratch.
func TestCheckOperationsRecoveryAndRetainedOwner(t *testing.T) {
	authCtx := WithPrincipal(t.Context(), Principal{ID: "alice@example.com"})
	dockerBin := installFakeDocker(t)

	t.Run("failed_close_isolates_peer_server_and_recovers_on_restart", func(t *testing.T) {
		recordedDaemon, otherDaemon := newFakeDockerDaemon(t), newFakeDockerDaemon(t)
		workspace := newShopModule(t, nil)
		cardsA, cardsB := filepath.Join(realTempDir(t), "cards-a"), filepath.Join(realTempDir(t), "cards-b")

		t.Setenv("DOCKER_HOST", recordedDaemon.hostURI)
		pA := runPinned(t, workspace, cardsA, recordedDaemon)
		opID := filepath.Base(pA.opDir)
		for _, sub := range []string{"src", "modcache", "work-0", "docker-config"} {
			assert.DirExists(t, filepath.Join(pA.opDir, sub))
		}
		assert.Equal(t, checkOwner{Docker: dockerBin, Host: recordedDaemon.hostURI, Containers: []string{pA.name}}, readOwnerRecord(t, pA.opDir))

		// A separate server on cardsB sharing workspace never retries pA.cp's container.
		cpB := openCheckCP(t, cardsB, workspace)
		resB, err := cpB.CheckProject(authCtx)
		require.NoError(t, err)
		assert.True(t, resB.Passed)
		require.NoError(t, cpB.Close())
		assert.Equal(t, 1, recordedDaemon.removeCount(pA.name))

		// Switch DOCKER_HOST to otherDaemon while recordedDaemon still blocks rm:
		// Close retries recordedDaemon, returns the cleanup error, keeps scratch,
		// and refuses subsequent check_project and update_dependencies calls.
		t.Setenv("DOCKER_HOST", otherDaemon.hostURI)
		closeErr := pA.cp.Close()
		require.Error(t, closeErr)
		assert.Contains(t, closeErr.Error(), pA.name)
		assert.Zero(t, otherDaemon.removes.Load())
		assert.Equal(t, 2, recordedDaemon.removeCount(pA.name))
		assert.DirExists(t, filepath.Join(pA.opDir, "src"))

		_, err = pA.cp.CheckProject(authCtx)
		assertServiceError(t, err, "unavailable")
		_, err = pA.cp.UpdateDependencies(authCtx)
		assertServiceError(t, err, "unavailable")
		assert.Equal(t, []string{opID}, dirEntryNames(t, filepath.Join(cardsA, "check-work")))

		// Unblock recordedDaemon and start cpA2 with DOCKER_HOST still on
		// otherDaemon: startup recovery contacts recordedDaemon, cleans scratch,
		// clears owner.Containers, and preserves ReadCheckOutput.
		recordedDaemon.setFailRemove(pA.name, false)
		cpA2 := openCheckCP(t, cardsA, workspace)
		defer func() { assert.NoError(t, cpA2.Close()) }()
		assert.Zero(t, otherDaemon.removes.Load())
		assert.Equal(t, 3, recordedDaemon.removeCount(pA.name))
		assert.ElementsMatch(t, []string{outputFile, outputMetaFile, ownerFile}, dirEntryNames(t, pA.opDir))
		assert.Empty(t, readOwnerRecord(t, pA.opDir).Containers)

		page, err := cpA2.ReadCheckOutput(authCtx, &gensurla.ReadCheckOutputPayload{OpID: opID})
		require.NoError(t, err)
		assert.Contains(t, page.Text, "pinned check output")
	})

	t.Run("close_cleans_scratch_via_recorded_endpoint", func(t *testing.T) {
		recordedDaemon, otherDaemon := newFakeDockerDaemon(t), newFakeDockerDaemon(t)
		t.Setenv("DOCKER_HOST", recordedDaemon.hostURI)
		p := runPinned(t, newShopModule(t, nil), filepath.Join(realTempDir(t), "cards"), recordedDaemon)
		recordedDaemon.setFailRemove(p.name, false)
		t.Setenv("DOCKER_HOST", otherDaemon.hostURI)
		require.NoError(t, p.cp.Close())
		assert.Zero(t, otherDaemon.removes.Load())
		assert.ElementsMatch(t, []string{outputFile, outputMetaFile, ownerFile}, dirEntryNames(t, p.opDir))
		assert.Empty(t, readOwnerRecord(t, p.opDir).Containers)
	})

	t.Run("ownerless_pre_container_operation_cleans_scratch_and_keeps_output", func(t *testing.T) {
		daemon := newFakeDockerDaemon(t)
		t.Setenv("DOCKER_HOST", daemon.hostURI)
		cardsDir := filepath.Join(realTempDir(t), "cards")
		opID := strings.Repeat("cd", 16)
		opDir := filepath.Join(cardsDir, "check-work", opID)
		const savedText = "note: socket skipped before crash\n"
		writeTree(t, opDir, map[string]string{
			"src/main.go":                          "package main\n",
			"modcache/example.com/mod@v1.0.0/m.go": "package mod\n",
			outputFile:                             savedText,
			outputMetaFile:                         fmt.Sprintf(`{"captured":%d,"dropped":0}`, len(savedText)),
		})
		require.NoError(t, os.Chmod(filepath.Join(opDir, "modcache/example.com/mod@v1.0.0"), 0o500))

		cp := openCheckCP(t, cardsDir, newShopModule(t, nil))
		defer func() { assert.NoError(t, cp.Close()) }()
		assert.Zero(t, daemon.removes.Load())
		assert.ElementsMatch(t, []string{outputFile, outputMetaFile}, dirEntryNames(t, opDir))

		page, err := cp.ReadCheckOutput(authCtx, &gensurla.ReadCheckOutputPayload{OpID: opID})
		require.NoError(t, err)
		assert.Equal(t, savedText, page.Text)
	})
}

// testConcurrentCheckAndUpdate launches CheckProject and UpdateDependencies
// concurrently against a barrier in daemon.onStart, verifies disjoint mounts
// and labels while both are active, edits live go.sum mid-flight, and checks
// snapshot isolation, manifest-conflict refusal, and cross-operation cursors.
func testConcurrentCheckAndUpdate(t *testing.T, daemon *fakeDockerDaemon) {
	t.Helper()
	ctx := t.Context()
	workspace := newShopModule(t, map[string]string{"go.sum": "initial-sum\n"})
	httpClient := newCheckTestServer(t, Options{WorkspaceDir: workspace}).httpClient
	bothEntered, releaseBoth := make(chan struct{}), make(chan struct{})
	var (
		enteredMu           sync.Mutex
		active              []*recordedContainer
		checkRes            *gensurla.CheckProjectResult
		updateRes           *gensurla.UpdateDependenciesResult
		checkErr, updateErr error
		wg                  sync.WaitGroup
	)
	daemon.setOnStart(func(c *recordedContainer) (string, string, int) {
		srcMount := findMount(t, c, workspace)
		if slices.Equal(c.Args, []string{"list", "-deps", "-test", "./..."}) ||
			slices.Equal(c.Args, []string{"mod", "tidy"}) {
			enteredMu.Lock()
			active = append(active, c)
			if len(active) == 2 {
				close(bothEntered)
			}
			enteredMu.Unlock()
			<-releaseBoth
		}
		if slices.Equal(c.Args, []string{"mod", "tidy"}) {
			assert.NoError(t, os.WriteFile(filepath.Join(srcMount.Source, "go.sum"), []byte("tidy-staged-sum\n"), 0o644))
			return "tidy output " + strings.Repeat("x", maxCheckMessageBytes) + "\n", "", 0
		}
		assertFileContent(t, filepath.Join(srcMount.Source, "go.sum"), "initial-sum\n")
		return "check step " + strings.Join(c.Args, " ") + "\n", "", 0
	})

	wg.Add(2)
	go func() { defer wg.Done(); checkRes, checkErr = httpClient.CheckProject(ctx) }()
	go func() { defer wg.Done(); updateRes, updateErr = httpClient.UpdateDependencies(ctx) }()

	<-bothEntered
	enteredMu.Lock()
	require.Len(t, active, 2)
	c0, c1 := active[0], active[1]
	enteredMu.Unlock()
	assert.NotEqual(t, c0.Label, c1.Label)
	assert.NotEqual(t, c0.DockerConfig, c1.DockerConfig)
	for _, target := range []string{workspace, containerModCache, containerWork} {
		assert.NotEqual(t, findMount(t, c0, target).Source, findMount(t, c1, target).Source)
	}

	require.NoError(t, os.WriteFile(filepath.Join(workspace, "go.sum"), []byte("concurrent-user-edit\n"), 0o644))
	close(releaseBoth)
	wg.Wait()
	require.NoError(t, checkErr)
	require.NoError(t, updateErr)
	assert.True(t, checkRes.Passed)
	assert.Empty(t, updateRes.FilesWritten)
	assert.Contains(t, updateRes.Message, "go.sum was not written: manifest conflict: go.sum changed during update_dependencies; call again")
	assertFileContent(t, filepath.Join(workspace, "go.sum"), "concurrent-user-edit\n")

	checkPage, err := httpClient.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: checkRes.OpID})
	require.NoError(t, err)
	assert.Contains(t, checkPage.Text, "check step test ./...")
	updatePage, err := httpClient.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: updateRes.OpID})
	require.NoError(t, err)
	assert.Contains(t, updatePage.Text, "tidy output")
	require.NotNil(t, updatePage.NextCursor)
	_, err = httpClient.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: checkRes.OpID, Cursor: updatePage.NextCursor})
	assertServiceError(t, err, "unprocessable")
}

// testTwoServerUpdateContention runs UpdateDependencies concurrently on two
// distinct servers bound to the same workspace, holding srvA inside writeAtomic
// (while srvA holds the live directory flock) until srvB attempts publication.
func testTwoServerUpdateContention(t *testing.T, daemon *fakeDockerDaemon) {
	t.Helper()
	ctx := t.Context()
	workspace := newShopModule(t, map[string]string{"go.sum": "initial-sum\n"})
	srvA := newCheckTestServer(t, Options{WorkspaceDir: workspace})
	srvB := newCheckTestServer(t, Options{WorkspaceDir: workspace})
	bothTidying, srvAInWrite := make(chan struct{}), make(chan struct{})
	srvADone, srvBDone := make(chan struct{}), make(chan struct{})
	var (
		tidyMu      sync.Mutex
		tidyEntered int
		once        sync.Once
		resA, resB  *gensurla.UpdateDependenciesResult
		errA, errB  error
	)
	priorFsync := fsyncDir
	fsyncDir = func(fd int) error {
		once.Do(func() { close(srvAInWrite); <-srvBDone })
		return priorFsync(fd)
	}
	t.Cleanup(func() { fsyncDir = priorFsync })

	daemon.setOnStart(func(c *recordedContainer) (string, string, int) {
		srcMount := findMount(t, c, workspace)
		tidyMu.Lock()
		tidyEntered++
		if tidyEntered == 2 {
			close(bothTidying)
		}
		tidyMu.Unlock()
		<-bothTidying
		if strings.HasPrefix(srcMount.Source, srvA.cp.checkWorkDir) {
			assert.NoError(t, os.WriteFile(filepath.Join(srcMount.Source, "go.sum"), []byte("sum-from-a\n"), 0o644))
			return "tidied a\n", "", 0
		}
		assert.NoError(t, os.WriteFile(filepath.Join(srcMount.Source, "go.sum"), []byte("sum-from-b\n"), 0o644))
		select {
		case <-srvAInWrite:
		case <-srvADone:
		}
		return "tidied b\n", "", 0
	})

	go func() { defer close(srvADone); resA, errA = srvA.httpClient.UpdateDependencies(ctx) }()
	go func() { defer close(srvBDone); resB, errB = srvB.httpClient.UpdateDependencies(ctx) }()
	<-srvADone
	<-srvBDone
	require.NoError(t, errA)
	require.NoError(t, errB)
	assert.Equal(t, []string{"go.sum"}, resA.FilesWritten)
	assert.Empty(t, resB.FilesWritten)
	assert.Contains(t, resB.Message, "go.sum was not written: manifest conflict: another update_dependencies is writing the directory of go.sum; call again")
	assertFileContent(t, filepath.Join(workspace, "go.sum"), "sum-from-a\n")
}

// runPinned starts a ControlPlane at cards and runs one CheckProject call whose
// first container fails docker rm -f, returning the retained pinnedCheck.
func runPinned(t *testing.T, ws, cards string, d *fakeDockerDaemon) pinnedCheck {
	t.Helper()
	cp := openCheckCP(t, cards, ws)
	var pinned atomic.Value
	d.setOnStart(func(c *recordedContainer) (string, string, int) {
		if pinned.CompareAndSwap(nil, c.Name) {
			d.setFailRemove(c.Name, true)
		}
		return "pinned check output\n", "", 0
	})
	_, err := cp.CheckProject(WithPrincipal(t.Context(), Principal{ID: "alice@example.com"}))
	require.Error(t, err)
	name, ok := pinned.Load().(string)
	require.True(t, ok)
	assert.Contains(t, err.Error(), "because container "+name+" may still exist; remove it with docker rm -f "+name)
	names := dirEntryNames(t, filepath.Join(cards, "check-work"))
	require.Len(t, names, 1)
	return pinnedCheck{cp: cp, name: name, opDir: filepath.Join(cards, "check-work", names[0])}
}

// installFakeDocker compiles fakeDockerSource into t.TempDir and prepends it to PATH.
func installFakeDocker(t *testing.T) string {
	t.Helper()
	root := realTempDir(t)
	srcDir, binDir := filepath.Join(root, "src"), filepath.Join(root, "bin")
	writeTree(t, srcDir, map[string]string{"go.mod": "module example.com/fakedocker\n\ngo 1.24\n", "main.go": fakeDockerSource})
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

// newCheckTestServer mounts NewHTTPHandler with ContainerRunner defaults and bearer auth.
func newCheckTestServer(t *testing.T, opts Options) checkClients {
	t.Helper()
	opts.CardsDir = filepath.Join(realTempDir(t), "cards")
	opts.Checker = candidate.NewContainerRunner()
	opts.CheckTimeout = 30 * time.Second
	cp := newControlPlane(t, opts)
	srv := httptest.NewServer(NewHTTPHandler(cp, NewTokenAuthenticator(map[string]Principal{
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
	opts := Options{CardsDir: cardsDir, WorkspaceDir: workspace, Checker: candidate.NewContainerRunner(), CheckTimeout: 30 * time.Second}
	cp, err := New(opts)
	require.NoError(t, err)
	return cp
}

// newShopModule creates a single-module Go workspace in a symlink-resolved TempDir.
func newShopModule(t *testing.T, extra map[string]string) string {
	t.Helper()
	workspace := realTempDir(t)
	files := map[string]string{"go.mod": "module example.com/shop\n\ngo 1.24\n", "shop_test.go": "package shop\n"}
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
	call, err := mcpClient.ToolsCall(t.Context(), &genmcpsurla.ToolsCallPayload{Name: name, Arguments: json.RawMessage(args)})
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
		c, err := parseCreateContainer(sub[1:], dockerConfig)
		if err != nil {
			return fakeDockerCall{Stderr: []byte(err.Error() + "\n"), ExitCode: 1}
		}
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
		if onStart := d.onStart; onStart != nil {
			d.mu.Unlock()
			stdout, stderr, code = onStart(c)
			d.mu.Lock()
		}
		c.ExitCode = code
		return fakeDockerCall{Stdout: []byte(stdout), Stderr: []byte(stderr), ExitCode: code}
	case "wait":
		if len(sub) == 2 {
			if c := d.findContainer(sub[1], false); c != nil {
				return fakeDockerCall{Stdout: fmt.Appendf(nil, "%d\n", c.ExitCode)}
			}
		}
		return fakeDockerCall{Stderr: []byte("No such container\n"), ExitCode: 1}
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
		return fakeDockerCall{Stderr: []byte("No such container: " + name + "\n"), ExitCode: 1}
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
