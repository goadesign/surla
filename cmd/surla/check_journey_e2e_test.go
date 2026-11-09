//go:build e2e

// This file drives the dependency-repair journey an agent follows on one
// running server: build and test the working tree, add a dependency, repair
// the module files and test again, through the authenticated MCP endpoint
// and the local Docker engine, without restarting the server or committing.
package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goahttp "goa.design/goa/v3/http"

	genmcpclient "github.com/goadesign/surla/gen/jsonrpc/mcp_surla/client"
	genmcpsurla "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
)

// journeyCallTimeout bounds one MCP call; the first check pulls the image and
// downloads the starter's modules.
const journeyCallTimeout = 10 * time.Minute

// goCmpRequire adds a real third-party test dependency the way a developer
// edits go.mod by hand: a requirement with no go.sum entry yet.
const goCmpRequire = "\nrequire github.com/google/go-cmp v0.6.0\n"

// goCmpTest is a test in package dependencycheck that uses go-cmp.
const goCmpTest = `package dependencycheck

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestDiff(t *testing.T) {
	if d := cmp.Diff("a", "a"); d != "" {
		t.Fatal(d)
	}
}
`

// syncRequire and syncTest add a second, different dependency to a go.work
// member, so the root module's go-cmp sums cannot satisfy it.
const (
	syncRequire = "\nrequire golang.org/x/sync v0.7.0\n"
	syncTest    = `package dependencycheck

import (
	"testing"

	"golang.org/x/sync/errgroup"
)

func TestGroup(t *testing.T) {
	var g errgroup.Group
	g.Go(func() error { return nil })
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
}
`
)

// TestCheckProjectRepairsDependenciesOnOneServer initializes a starter
// through MCP, then runs the missing go.sum and go.work workspace journeys
// against the same running server and the local Docker engine. The server is
// the embedded composition root (serveOptions and controlplane.New), not the
// standalone surla serve process.
func TestCheckProjectRepairsDependenciesOnOneServer(t *testing.T) {
	isolateLocalWork(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Surla Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Surla Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", liveModelGatewayEnv, liveModelProviderEnv, liveModelNameEnv} {
		t.Setenv(name, "")
	}
	gatewayAddr := startModelGateway(t, &scriptedCP1Provider{})
	workspace := t.TempDir()
	opts, err := serveOptions(serveConfig{
		checkTimeout:     defaultCheckTimeout,
		projectDir:       workspace,
		projectSet:       true,
		cardsDir:         t.TempDir(),
		cardsDirExplicit: true,
		gatewayAddr:      gatewayAddr,
	}, func(string) string { return "" }, os.Environ())
	require.NoError(t, err)
	opts.Analyzer = trustedSuiteAnalyzer{}
	// A test executable has no released module version in its build info.
	opts.SurlaVersion = starterSurlaVersion
	cp, err := controlplane.New(opts)
	require.NoError(t, err)
	t.Cleanup(func() { closeTrustedSuiteControlPlane(t, cp) })
	srv := httptest.NewServer(controlplane.NewHTTPHandler(cp, controlplane.NewTokenAuthenticator(map[string]controlplane.Principal{
		"test-token": {ID: "operator@example.com"},
	})))
	t.Cleanup(srv.Close)
	scheme, host, err := parseServerOrigin(srv.URL)
	require.NoError(t, err)
	transport := genmcpclient.NewClient(scheme, host, &bearerDoer{client: srv.Client(), token: "test-token"},
		goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	client := &genmcpsurla.Client{ToolsCallEndpoint: transport.ToolsCall()}

	args, err := gensurla.EncodeInitProjectPayload(&gensurla.InitProjectPayload{ModulePath: "example.com/shop"})
	require.NoError(t, err)
	initialized, err := gensurla.DecodeInitProjectResult(journeyCall(t, client, "init_project", args))
	require.NoError(t, err)
	dir := initialized.Dir

	t.Run("the initialized starter passes", func(t *testing.T) {
		res := journeyCheck(t, client)
		assert.True(t, res.Passed, res.Message)
	})

	t.Run("missing go.sum entry is repaired in place", func(t *testing.T) {
		appendFile(t, filepath.Join(dir, "go.mod"), goCmpRequire)
		writeJourneyFile(t, filepath.Join(dir, "dependencycheck", "diff_test.go"), goCmpTest)
		goSumBefore := readJourneyFile(t, filepath.Join(dir, "go.sum"))

		failed := journeyCheck(t, client)
		require.False(t, failed.Passed, failed.Message)
		assert.Contains(t, failed.Message, "call update_dependencies")
		assert.Contains(t, journeyOutput(t, client, failed.OpID), "missing go.sum entry")
		assert.Equal(t, goSumBefore, readJourneyFile(t, filepath.Join(dir, "go.sum")), "check_project never writes the workspace")

		repaired := journeyUpdate(t, client)
		assert.Contains(t, repaired.FilesWritten, "go.sum", repaired.Message)
		assert.Contains(t, readJourneyFile(t, filepath.Join(dir, "go.sum")), "github.com/google/go-cmp v0.6.0 h1:")

		passed := journeyCheck(t, client)
		assert.True(t, passed.Passed, passed.Message)
		assertUncommitted(t, dir, "go.mod", "go.sum", "dependencycheck/")
	})

	t.Run("go.work workspace member dependency is repaired in place", func(t *testing.T) {
		evalDir := filepath.Join(dir, "eval")
		goWork := "go " + goVersion(t, filepath.Join(dir, "go.mod")) + "\n\nuse (\n\t.\n\t./eval\n)\n"
		writeJourneyFile(t, filepath.Join(dir, "go.work"), goWork)
		appendFile(t, filepath.Join(evalDir, "go.mod"), syncRequire)
		writeJourneyFile(t, filepath.Join(evalDir, "dependencycheck", "group_test.go"), syncTest)

		failed := journeyCheck(t, client)
		require.False(t, failed.Passed, failed.Message)
		assert.Contains(t, failed.Message, "call update_dependencies")

		repaired := journeyUpdate(t, client)
		assert.Contains(t, repaired.FilesWritten, "eval/go.sum", repaired.Message)
		assert.Equal(t, goWork, readJourneyFile(t, filepath.Join(dir, "go.work")), "go.work is never written")
		// Whether the workspace graph needs go.work.sum entries is Go's
		// decision; record what it did.
		if workSum, err := os.ReadFile(filepath.Join(dir, "go.work.sum")); err == nil {
			assert.Contains(t, repaired.FilesWritten, "go.work.sum")
			t.Logf("go.work.sum written with %d lines", strings.Count(string(workSum), "\n"))
		} else {
			require.ErrorIs(t, err, os.ErrNotExist)
			assert.NotContains(t, repaired.FilesWritten, "go.work.sum")
			t.Log("go.work.sum not needed for this workspace graph")
		}

		passed := journeyCheck(t, client)
		assert.True(t, passed.Passed, passed.Message)
		assertUncommitted(t, dir, "go.work", "eval/go.mod", "eval/go.sum", "eval/dependencycheck/")
	})
}

// journeyCall runs one MCP tool call that must complete without a tool error
// and returns its structured result.
func journeyCall(t *testing.T, client *genmcpsurla.Client, name string, args []byte) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), journeyCallTimeout)
	defer cancel()
	result, err := client.ToolsCall(ctx, &genmcpsurla.ToolsCallPayload{Name: name, Arguments: args})
	require.NoError(t, err)
	complete, ok := result.Outcome.AsComplete()
	require.True(t, ok, "%s completes in the same MCP call", name)
	require.False(t, complete.IsError != nil && *complete.IsError, "%s: %+v", name, complete.Content)
	return complete.StructuredContent
}

// journeyCheck calls check_project.
func journeyCheck(t *testing.T, client *genmcpsurla.Client) *gensurla.CheckProjectResult {
	t.Helper()
	res, err := gensurla.DecodeCheckProjectResult(journeyCall(t, client, "check_project", []byte("{}")))
	require.NoError(t, err)
	return res
}

// journeyUpdate calls update_dependencies.
func journeyUpdate(t *testing.T, client *genmcpsurla.Client) *gensurla.UpdateDependenciesResult {
	t.Helper()
	res, err := gensurla.DecodeUpdateDependenciesResult(journeyCall(t, client, "update_dependencies", []byte("{}")))
	require.NoError(t, err)
	return res
}

// journeyOutput reads every page of an operation's output.
func journeyOutput(t *testing.T, client *genmcpsurla.Client, opID string) string {
	t.Helper()
	var out strings.Builder
	payload := &gensurla.ReadCheckOutputPayload{OpID: opID}
	for {
		args, err := gensurla.EncodeReadCheckOutputPayload(payload)
		require.NoError(t, err)
		page, err := gensurla.DecodeCheckOutputPage(journeyCall(t, client, "read_check_output", args))
		require.NoError(t, err)
		out.WriteString(page.Text)
		if page.NextCursor == nil {
			return out.String()
		}
		payload.Cursor = page.NextCursor
	}
}

// assertUncommitted checks that the journey's edits are still uncommitted
// working-tree changes: nothing was committed to make them testable.
func assertUncommitted(t *testing.T, dir string, paths ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "status", "--porcelain", "--untracked-files=all")
	cmd.Dir = dir
	out, err := cmd.Output()
	require.NoError(t, err)
	for _, p := range paths {
		assert.Contains(t, string(out), p)
	}
}

// goVersion returns the version on the go line of the go.mod at p.
func goVersion(t *testing.T, p string) string {
	t.Helper()
	for _, line := range strings.Split(readJourneyFile(t, p), "\n") {
		if v, ok := strings.CutPrefix(line, "go "); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("%s has no go line", p)
	return ""
}

// appendFile appends text to the file at p.
func appendFile(t *testing.T, p, text string) {
	t.Helper()
	writeJourneyFile(t, p, readJourneyFile(t, p)+text)
}

// writeJourneyFile writes content to p, creating its directory.
func writeJourneyFile(t *testing.T, p, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
}

// readJourneyFile returns the content of p.
func readJourneyFile(t *testing.T, p string) string {
	t.Helper()
	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	return string(raw)
}
