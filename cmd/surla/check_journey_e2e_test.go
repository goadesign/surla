//go:build e2e && docker

// This file drives the dependency-repair journey an agent follows on one
// running server: build and test the working tree, add a dependency, repair
// the module files and test again, through the authenticated MCP endpoint
// and the local Docker engine, without restarting the server or committing.
// It needs a running local Docker engine, so it builds only with the docker
// tag as well: go test -tags e2e,docker.
package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
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

// Journey A adds rsc.io/binaryregexp v0.2.0, a small published module with
// no dependencies, the way a developer edits go.mod by hand: a requirement
// with no go.sum entry yet.
const (
	regexpModule  = "rsc.io/binaryregexp"
	regexpRequire = "\nrequire rsc.io/binaryregexp v0.2.0\n"
	regexpTest    = `package dependencycheck

import (
	"testing"

	"rsc.io/binaryregexp"
)

func TestMatch(t *testing.T) {
	if !binaryregexp.MustCompile("a+").MatchString("aa") {
		t.Fatal("no match")
	}
}
`
)

// Journey B: the root and eval modules each replace example.com/workdep
// with a dependency-free base copy, so their own go.sum files need nothing
// for it. go.work replaces the same module with a workspace copy whose
// identical API imports rsc.io/qr v0.2.0. Only the workspace graph needs
// rsc.io/qr, so its checksums can come only from go.work.sum: the read-only
// check must fail on writing go.work.sum, and update_dependencies must write
// it. The testdata directories are replace targets, not checked modules.
const (
	qrModule      = "rsc.io/qr"
	workdepModule = "module example.com/workdep\n\ngo 1.21\n"
	workdepBase   = `package workdep

// Name names the copy in use.
func Name() string { return "base" }
`
	workdepWorkspace = `package workdep

import "rsc.io/qr"

// Name names the copy in use.
func Name() string {
	if _, err := qr.Encode("surla", qr.L); err != nil {
		return err.Error()
	}
	return "workspace"
}
`
	workdepRequire = "\nrequire example.com/workdep v0.0.0\n"
	workdepTest    = `package workdepcheck

import (
	"testing"

	"example.com/workdep"
)

func TestName(t *testing.T) {
	if workdep.Name() == "" {
		t.Fatal("empty name")
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
	gatewayAddr := startModelGateway(t, &scriptedModelProvider{})
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
	// A test executable has no released module version in its build info.
	opts.SurlaVersion = starterSurlaVersion
	cp, err := controlplane.New(opts)
	require.NoError(t, err)
	// Registered after the temporary directories, so it runs before they
	// are removed.
	t.Cleanup(func() { closeRetaining(cp, workspace, opts.CardsDir) })
	srv := httptest.NewServer(controlplane.NewHTTPHandler(cp, controlplane.NewTokenAuthenticator(map[string]controlplane.Principal{
		"test-token": {ID: "operator@example.com"},
	})))
	t.Cleanup(srv.Close)
	serverURL, err := url.Parse(srv.URL)
	require.NoError(t, err)
	transport := genmcpclient.NewClient(serverURL.Scheme, serverURL.Host, &bearerDoer{client: srv.Client(), token: "test-token"},
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
		goSumBefore := readJourneyFile(t, filepath.Join(dir, "go.sum"))
		require.NotContains(t, goSumBefore, regexpModule, "premise: the starter has no checksum for the new dependency")
		appendFile(t, filepath.Join(dir, "go.mod"), regexpRequire)
		writeJourneyFile(t, filepath.Join(dir, "dependencycheck", "match_test.go"), regexpTest)

		failed := journeyCheck(t, client)
		require.False(t, failed.Passed, failed.Message)
		assert.Contains(t, failed.Message, "call update_dependencies")
		assert.Contains(t, journeyOutput(t, client, failed.OpID), "missing go.sum entry")
		assert.Equal(t, goSumBefore, readJourneyFile(t, filepath.Join(dir, "go.sum")), "check_project never writes the workspace")

		repaired := journeyUpdate(t, client)
		assert.Contains(t, repaired.FilesWritten, "go.sum", repaired.Message)
		assert.Contains(t, readJourneyFile(t, filepath.Join(dir, "go.sum")), regexpModule+" v0.2.0 h1:")

		passed := journeyCheck(t, client)
		assert.True(t, passed.Passed, passed.Message)
		assertUncommitted(t, dir, "go.mod", "go.sum", "dependencycheck/")
	})

	t.Run("workspace-only checksums are repaired into go.work.sum", func(t *testing.T) {
		evalDir := filepath.Join(dir, "eval")
		writeJourneyFile(t, filepath.Join(dir, "testdata", "deps", "base", "go.mod"), workdepModule)
		writeJourneyFile(t, filepath.Join(dir, "testdata", "deps", "base", "workdep.go"), workdepBase)
		writeJourneyFile(t, filepath.Join(dir, "testdata", "deps", "workspace", "go.mod"), workdepModule+"\nrequire rsc.io/qr v0.2.0\n")
		writeJourneyFile(t, filepath.Join(dir, "testdata", "deps", "workspace", "workdep.go"), workdepWorkspace)
		appendFile(t, filepath.Join(dir, "go.mod"), workdepRequire+"\nreplace example.com/workdep => ./testdata/deps/base\n")
		appendFile(t, filepath.Join(evalDir, "go.mod"), workdepRequire+"\nreplace example.com/workdep => ../testdata/deps/base\n")
		writeJourneyFile(t, filepath.Join(dir, "workdepcheck", "name_test.go"), workdepTest)
		writeJourneyFile(t, filepath.Join(evalDir, "workdepcheck", "name_test.go"), workdepTest)

		// Standalone, each module resolves workdep to the base copy and
		// passes with its current files.
		standalone := journeyCheck(t, client)
		require.True(t, standalone.Passed, "premise: the modules pass without the workspace: %s", standalone.Message)
		for _, sum := range []string{filepath.Join(dir, "go.sum"), filepath.Join(evalDir, "go.sum")} {
			require.NotContains(t, readJourneyFile(t, sum), qrModule, "premise: %s has no checksum for the workspace-only module", sum)
		}
		require.NoFileExists(t, filepath.Join(dir, "go.work.sum"))

		goWork := "go " + goVersion(t, filepath.Join(dir, "go.mod")) +
			"\n\nuse (\n\t.\n\t./eval\n)\n\nreplace example.com/workdep => ./testdata/deps/workspace\n"
		writeJourneyFile(t, filepath.Join(dir, "go.work"), goWork)

		failed := journeyCheck(t, client)
		require.False(t, failed.Passed, failed.Message)
		assert.Contains(t, failed.Message, "call update_dependencies")
		output := journeyOutput(t, client, failed.OpID)
		assert.Contains(t, output, "go.work.sum")
		assert.Contains(t, output, "read-only file system")
		require.NoFileExists(t, filepath.Join(dir, "go.work.sum"), "check_project never writes the workspace")

		repaired := journeyUpdate(t, client)
		assert.Contains(t, repaired.FilesWritten, "go.work.sum", repaired.Message)
		assert.Contains(t, readJourneyFile(t, filepath.Join(dir, "go.work.sum")), qrModule+" v0.2.0 h1:")
		assert.Equal(t, goWork, readJourneyFile(t, filepath.Join(dir, "go.work")), "go.work is never written")

		passed := journeyCheck(t, client)
		assert.True(t, passed.Passed, passed.Message)
		assertUncommitted(t, dir, "go.work", "go.work.sum", "testdata/deps/", "workdepcheck/")
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
	require.False(t, complete.IsError != nil && *complete.IsError, "%s: %s", name, toolErrorText(complete.Content))
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
