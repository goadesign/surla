//go:build e2e

package main

import (
	"bytes"
	"context"
	"io"
	"maps"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
	"github.com/goadesign/surla/internal/testmodule"
)

// starterSurlaVersion is a published Surla version the starter's evaluator
// module requires, as the scaffold tests use.
const starterSurlaVersion = "v0.0.0-20261104215521-580509970fb6"

// headerCommand matches the command line Goa writes in generated file
// headers, which names the program that generated them.
var headerCommand = regexp.MustCompile(`(?m)^// \$ .*$`)

// TestGenMatchesGoaGenByteForByte compares surla gen with the module-pinned
// host goa gen on Surla's own trusted test design: every path and byte must
// match, apart from the header's command line.
func TestGenMatchesGoaGenByteForByte(t *testing.T) {
	snapshot := testmodule.Snapshot(t, completeSourcesGoa, genDesign)
	hostDir := writeModule(t, snapshot)
	cmd := exec.CommandContext(t.Context(), "go", "run", "goa.design/goa/v3/cmd/goa", "gen", "example.com/design/design")
	cmd.Dir = hostDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	surlaDir := writeModule(t, snapshot)
	require.NoError(t, runGen([]string{"-dir", surlaDir}, io.Discard, io.Discard))

	want, got := normalized(t, hostDir), normalized(t, surlaDir)
	assert.Equal(t, keys(want), keys(got), "the same paths")
	for p, w := range want {
		assert.Equal(t, w, got[p], p)
	}
}

// TestGenOnAFreshStarterChangesNothingAndKeepsModulesApart creates a
// starter through one running server's authenticated MCP endpoint, checks
// that the server adopts its default child directory and surla gen in either
// module of the untouched project changes no byte anywhere in it, and that
// regenerating after a design edit in one module leaves the other module
// byte-for-byte unchanged.
func TestGenOnAFreshStarterChangesNothingAndKeepsModulesApart(t *testing.T) {
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
	provider := &scriptedCP1Provider{}
	gatewayAddr := startModelGateway(t, provider)
	workspace := t.TempDir()
	cfg := serveConfig{
		projectDir:       workspace,
		projectSet:       true,
		cardsDir:         t.TempDir(),
		cardsDirExplicit: true,
		gatewayAddr:      gatewayAddr,
	}
	opts, err := serveOptions(cfg, func(string) string { return "" }, os.Environ())
	require.NoError(t, err)
	opts.Analyzer = trustedSuiteAnalyzer{}
	opts.Live.StartTimeout = 3 * time.Minute
	// A test executable has no released module version in its build info.
	opts.SurlaVersion = starterSurlaVersion
	cp, err := controlplane.New(opts)
	require.NoError(t, err)
	t.Cleanup(func() { closeTrustedSuiteControlPlane(t, cp) })
	auth := controlplane.NewTokenAuthenticator(map[string]controlplane.Principal{
		"test-token": {ID: "operator@example.com"},
	})
	srv := httptest.NewServer(controlplane.NewHTTPHandler(cp, auth))
	t.Cleanup(srv.Close)
	scheme, host, err := parseServerOrigin(srv.URL)
	require.NoError(t, err)
	transport := genmcpclient.NewClient(scheme, host,
		&bearerDoer{client: srv.Client(), token: "test-token"},
		goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	client := &genmcpsurla.Client{ToolsCallEndpoint: transport.ToolsCall()}
	args, err := gensurla.EncodeInitProjectPayload(&gensurla.InitProjectPayload{
		ModulePath: "example.com/shop",
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	call := &genmcpsurla.ToolsCallPayload{Name: "init_project", Arguments: args}
	result, err := client.ToolsCall(ctx, call)
	require.NoError(t, err)
	complete, ok := result.Outcome.AsComplete()
	require.True(t, ok, "initialization completes in the same MCP call")
	require.False(t, complete.IsError != nil && *complete.IsError, "%+v", complete.Content)
	initialized, err := gensurla.DecodeInitProjectResult(complete.StructuredContent)
	require.NoError(t, err)
	physicalWorkspace, err := filepath.EvalSymlinks(workspace)
	require.NoError(t, err)
	dir := filepath.Join(physicalWorkspace, "shop")
	assert.Equal(t, dir, initialized.Dir, "omitting dir uses the module's service name")
	assert.Equal(t, "example.com/shop", initialized.ModulePath)
	assert.Equal(t, "shop", initialized.ServiceName)
	assert.Regexp(t, "^[0-9a-f]{40}$", initialized.Commit)
	headCmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "HEAD^{commit}")
	headOut, err := headCmd.CombinedOutput()
	require.NoError(t, err, string(headOut))
	assert.Equal(t, initialized.Commit, strings.TrimSpace(string(headOut)))
	entries, err := os.ReadDir(workspace)
	require.NoError(t, err)
	require.Len(t, entries, 1, "server state and compiler work stay outside the builder workspace")
	assert.Equal(t, "shop", entries[0].Name())

	// A second call must observe the first call's active workspace without
	// restarting the server or running another starter build.
	result, err = client.ToolsCall(ctx, call)
	require.NoError(t, err)
	complete, ok = result.Outcome.AsComplete()
	require.True(t, ok)
	require.NotNil(t, complete.IsError)
	require.True(t, *complete.IsError)
	require.Len(t, complete.Content, 1)
	require.NotNil(t, complete.Content[0].Text)
	assert.Contains(t, *complete.Content[0].Text, "already initialized at "+dir)
	evalDir := filepath.Join(dir, "eval")
	created := files(t, dir, ".")
	require.Contains(t, created, quickstartGuide, "the product design declares an agent, so goa-ai writes its guide")
	assert.Contains(t, created["gen/shop/service.go"], "// $ surla gen", "surla init generates with the same generator")

	for _, module := range []string{dir, evalDir} {
		var out bytes.Buffer
		require.NoError(t, runGen([]string{"-dir", module}, &out, io.Discard))
		assert.Contains(t, out.String(), "added: 0\nchanged: 0\nremoved: 0\n", module)
	}
	assert.Equal(t, created, files(t, dir, "."), "regenerating an untouched starter changes no byte")

	// A server started on the already committed project must keep its startup
	// suite, too. Reuse this same project while its HEAD is still trusted A.
	startupCfg := cfg
	startupCfg.projectDir, startupCfg.cardsDir = dir, t.TempDir()
	startup := newTrustedSuiteServer(t, startupCfg)
	commentPath := filepath.Join(evalDir, "design", "design.go")
	source, err := os.ReadFile(commentPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(commentPath, append(source, []byte("\n// Builder-only comment; evaluator behavior stays unchanged.\n")...), 0o644))
	candidateCommit := trustedSuiteCommit(t, dir)
	require.NotEqual(t, initialized.Commit, candidateCommit)

	assertTrustedSuiteJourney(t, client, opts.CardsDir, opts.RunsDir, candidateCommit, initialized.Commit, "initialized")
	srv.Close()
	closeTrustedSuiteControlPlane(t, cp)
	// Reconstruct from the original empty parent, using its saved private
	// state; the child project now has builder commit B at HEAD.
	restored := newTrustedSuiteServer(t, cfg)
	assertTrustedSuiteJourney(t, restored.client, restored.cardsDir, restored.runsDir, candidateCommit, initialized.Commit, "restored")
	restored.close(t)
	assertTrustedSuiteJourney(t, startup.client, startup.cardsDir, startup.runsDir, candidateCommit, initialized.Commit, "startup")
	startup.close(t)

	edits := []struct {
		// module is the module directory surla gen runs in.
		module string
		// design is the edited design file, relative to dir.
		design string
		// old and new are the replaced description text.
		old, new string
		// other is the other module's directory, relative to dir, which
		// must not change.
		other string
	}{
		{module: dir, design: "design/design.go", old: "Answers price questions", new: "Answers every price question", other: "eval"},
		{module: evalDir, design: "eval/design/design.go", old: "Proves through the public API", new: "Shows through the public API", other: "gen"},
	}
	for _, e := range edits {
		before := files(t, dir, e.other)
		path := filepath.Join(dir, filepath.FromSlash(e.design))
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Contains(t, string(src), e.old)
		require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(src), e.old, e.new, 1)), 0o644))
		var out bytes.Buffer
		require.NoError(t, runGen([]string{"-dir", e.module}, &out, io.Discard))
		assert.NotContains(t, out.String(), "changed: 0\n", "the edit changes generated files in %s", e.module)
		assert.Equal(t, before, files(t, dir, e.other), "regenerating %s leaves %s unchanged", e.module, e.other)
	}
}

// normalized returns every file under gen/ and the agent guide, with header
// command lines replaced by a placeholder.
func normalized(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := files(t, dir, "gen")
	if guide, err := os.ReadFile(dir + "/" + quickstartGuide); err == nil {
		out[quickstartGuide] = string(guide)
	}
	for p, content := range out {
		out[p] = headerCommand.ReplaceAllString(content, "// $ <command>")
	}
	return out
}

// keys returns m's keys, sorted.
func keys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}
